package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/steig/worktender/internal/herdrapi"
)

// The completion gate is the hand-off primitive: block until a dispatched
// worker's report satisfies a predicate, then let the coordinator proceed.
//
// Nothing in adopt/staff/resume/prune covers this, because those are about the
// shape of a repository and this is about the order of two agents' work.
//
// THE PREDICATE MAY ONLY READ VALIDATED SLOTS. --until matches `status`, a
// closed set; --require-pr matches the presence of `pr`, a positive integer.
// There is deliberately no way to write a predicate over the note, and adding
// one is not a feature request that can be granted. The note is untrusted text
// authored by whatever the worker swallowed — a GitHub issue body is written by
// anyone who can file one — and a predicate over it would put that author in
// charge of when the coordinator's next agent starts. The report envelope
// exists to keep untrusted text out of the coordinator's control flow; a
// --note-contains flag would hand it the control flow directly.
//
// WHAT THE GATE DOES NOT PROVE. It reads the worker's pane, and a pane is a
// buffer the worker fully controls: a worker that prints seven crafted lines
// produces an envelope that parses, and one that echoes a file containing those
// lines does the same. So the gate authenticates SHAPE and POSITION, never
// authorship — it establishes that a well-formed report appeared in that pane
// after the gate started, and nothing about who composed it. That holds for both
// channels a report can arrive on; see readChannels for why the metadata one is
// no stronger despite being the one `worktender report` writes.
//
// A shared secret would not close that. The obvious fix is a nonce the
// coordinator puts in the dispatch prompt and requires back in the report — but
// the dispatch prompt is IN the worker's context, sitting beside the hostile
// issue body, so any text that can talk the worker into printing a fake report
// can also read the nonce out of the same context and include it. A secret the
// attacker can read is not a secret, so the gate does not pretend to have one.
//
// What remains true regardless is the part that matters: whatever the status
// slot says, the note reaches the coordinator quoted and announced, and the
// gate branched on neither its content nor its length.

// gateDefaultTimeout bounds a wait nobody supplied a bound for.
//
// A gate with no timeout wedges a coordinator with no diagnosis, which is worse
// than no gate at all — so there is no "wait indefinitely" option, unlike
// `herdr agent wait`, which offers one. Fifteen minutes is long enough for a
// worker to finish a slice of real work and short enough that a coordinator
// which loses one gets its terminal back inside a coffee break.
const gateDefaultTimeout = 15 * time.Minute

// gateReadSource is the snapshot the LEGACY half of the report is parsed out
// of. Unwrapped, because a wrapped envelope line is two lines to a parser.
//
// The pane used to be the only channel, and it carried a requirement the
// dispatch prompt had to satisfy: the envelope had to reach the worker's
// TERMINAL, which a Claude Code worker running `worktender report` as a tool call
// never does. metadata.go is the channel that removed the requirement. The pane
// is still read, second, because a worker that reproduces its envelope as reply
// text has reported and must go on being heard — including one whose `worktender
// report` never ran at all.
const gateReadSource = herdrapi.ReadSourceRecentUnwrapped

const gateUsage = "usage: worktender gate --target <agent|pane> [--until planned|blocked|done] [--require-pr] [--timeout 15m]"

// gateOptions is one gate invocation.
type gateOptions struct {
	target    string
	until     []string
	requirePR bool
	timeout   time.Duration
}

// statusFlag collects a repeatable --until, validating each value on arrival so
// a typo is refused rather than quietly matching nothing for fifteen minutes.
type statusFlag []string

func (s *statusFlag) String() string { return strings.Join(*s, ",") }

func (s *statusFlag) Set(v string) error {
	if !isReportStatus(v) {
		return fmt.Errorf("--until %q is not one of %s", v, strings.Join(reportStatuses, "|"))
	}
	*s = append(*s, v)
	return nil
}

func parseGate(args []string) (gateOptions, error) {
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	target := flags.String("target", "", "the agent or pane to wait on")
	requirePR := flags.Bool("require-pr", false, "the report must carry a pull request number")
	// A duration rather than herdr's bare milliseconds: this is worktender's own
	// surface, and "15m" cannot be pasted wrong by a factor of a thousand.
	timeout := flags.Duration("timeout", gateDefaultTimeout, "how long to wait before giving up")
	var until statusFlag
	flags.Var(&until, "until", "release on this status; repeat for more than one")

	if err := flags.Parse(args); err != nil {
		return gateOptions{}, fmt.Errorf("%w; %s", err, gateUsage)
	}
	if rest := flags.Args(); len(rest) > 0 {
		return gateOptions{}, fmt.Errorf("unexpected argument %q; %s", rest[0], gateUsage)
	}
	if *target == "" {
		return gateOptions{}, fmt.Errorf("--target is required; %s", gateUsage)
	}
	if *timeout <= 0 {
		return gateOptions{}, fmt.Errorf("--timeout must be positive; a gate that cannot expire is the wedge this command exists to avoid")
	}
	if len(until) == 0 {
		// The gate's reason for existing is completion, so that is the default.
		until = statusFlag{"done"}
	}
	return gateOptions{target: *target, until: until, requirePR: *requirePR, timeout: *timeout}, nil
}

// satisfies is the whole predicate surface: a status from the closed set, and
// optionally the presence of the validated pr slot. The note is not reachable
// from here and there is no third case to add.
func (o gateOptions) satisfies(r report) bool {
	if !slices.Contains(o.until, r.status) {
		return false
	}
	return !o.requirePR || r.pr > 0
}

// gateCommand waits for a worker to report, then releases.
//
// Like `report`, it takes no repository lock and needs no invocation context: it
// touches neither git nor the reconciler. It does need herdr, because the pane
// it reads and the transitions it waits on are herdr's.
func gateCommand(args []string, out io.Writer) error {
	opts, err := parseGate(args)
	if err != nil {
		return err
	}
	client, err := herdrapi.New()
	if err != nil {
		return err
	}
	return runGate(client, opts, out)
}

func runGate(client *herdrapi.Client, opts gateOptions, out io.Writer) error {
	// Resolving the target first is the cheapest half of the dead-worker
	// answer: herdr returns agent_not_found both for a name that never existed
	// and for a pane whose agent has exited, so a gate pointed at something
	// that will never report fails here instead of at the deadline.
	info, err := client.AgentGet(opts.target)
	if err != nil {
		return fmt.Errorf("gate %s: %w", opts.target, err)
	}
	pane, workspace := info.Agent.PaneID, info.Agent.WorkspaceID
	if info.Agent.AgentStatus == herdrapi.AgentStatusUnknown {
		return fmt.Errorf("gate %s: herdr reports no agent state for pane %s; there is nothing to wait on", opts.target, pane)
	}

	started := time.Now()
	deadline := started.Add(opts.timeout)

	// Subscribe BEFORE reading the baseline. The other order has a hole exactly
	// one round trip wide: a report landing between the read and the
	// subscription would be in neither, and the gate would wait out its whole
	// timeout on a worker that had already answered.
	//
	// The last subscription is the one experience added: closing a workspace does
	// NOT emit pane.exited or pane.closed for the panes inside it, so a gate
	// subscribed only to the pane events sits through its whole timeout after
	// its worker's workspace is torn down. That was observed, not predicted.
	//
	// pane.updated is what makes the metadata channel edge-triggered. Attaching
	// tokens to a pane emits one, measured on a live socket, so a report lands
	// in the gate's lap the instant `worktender report` writes it — rather than
	// whenever the worker's agent status next happens to move, which for a
	// worker that reports mid-turn and keeps working could be minutes later or
	// never.
	stream, err := client.Subscribe([]herdrapi.Subscription{
		{Type: herdrapi.SubscriptionPaneAgentStatusChanged, PaneID: pane},
		{Type: herdrapi.SubscriptionPaneExited, PaneID: pane},
		{Type: herdrapi.SubscriptionPaneClosed, PaneID: pane},
		{Type: herdrapi.SubscriptionPaneUpdated},
		{Type: herdrapi.SubscriptionWorkspaceClosed, WorkspaceID: workspace},
	}, deadline)
	if err != nil {
		return fmt.Errorf("gate %s: %w", opts.target, err)
	}
	defer stream.Close()

	// Whatever is already on either channel is a PREVIOUS task's answer.
	//
	// A worker is dispatched, then gated; anything it had reported before the
	// gate opened was reported about something else. Releasing on it would hand
	// the coordinator a stale `done` — which is precisely the hand-off bug the
	// gate exists to remove, not a shortcut worth taking. The baseline is
	// therefore ignored, and named in the timeout message, so a gate started
	// too late says so instead of just failing.
	//
	// Taking it is the same operation as judging one: what the marks return the
	// first time is exactly what was already there, so a report only ever counts
	// as news once, and the rule that decides it is written in one place.
	var seen gateMarks
	stale := seen.advance(readChannels(client, pane))
	baseline, hasBaseline := report{}, len(stale) > 0
	if hasBaseline {
		baseline = stale[0]
	}

	fmt.Fprintf(out, "gate: waiting on %s (pane %s) for status %s, up to %s\n",
		opts.target, pane, strings.Join(opts.until, "|"), opts.timeout)

	for {
		event, err := stream.Next()
		if errors.Is(err, herdrapi.ErrStreamExpired) {
			return gateExpired(opts, pane, time.Since(started), baseline, hasBaseline)
		}
		if err != nil {
			return fmt.Errorf("gate %s: %w", opts.target, err)
		}

		verdict, gone := gateVerdict(event, pane, workspace)
		if verdict == gateIgnore {
			continue
		}

		// Read before acting on the verdict, so a worker that printed its
		// report and died in the same breath is still heard. A gate that
		// checked liveness first would throw that report away.
		fresh := seen.advance(readChannels(client, pane))

		// Both channels can move between two looks, so a look can turn up more
		// than one new report — and then the ORDER they were read in must not
		// decide the gate. Every one of them is weighed for release before any
		// of them is weighed for blocked, so a `done` on one channel is not lost
		// to a `blocked` that happened to be read first.
		if i := slices.IndexFunc(fresh, opts.satisfies); i >= 0 {
			fmt.Fprintf(out, "gate: released after %s\n", time.Since(started).Round(time.Second))
			fmt.Fprint(out, renderReport(fresh[i]))
			return nil
		}
		// `blocked` is terminal in the direction that matters: a blocked worker
		// does not reach `done` on its own, and the party that unblocks it is
		// the coordinator sitting in this wait. Holding the gate open would
		// spend the whole timeout waiting for something only the waiter can
		// cause.
		if i := slices.IndexFunc(fresh, isBlocked); i >= 0 {
			fmt.Fprint(out, renderReport(fresh[i]))
			return fmt.Errorf("gate %s: the worker reported blocked after %s; it will not reach %s without you",
				opts.target, time.Since(started).Round(time.Second), strings.Join(opts.until, "|"))
		}
		for _, r := range fresh {
			fmt.Fprintf(out, "gate: %s reported %s; still waiting\n", opts.target, r.status)
		}

		if verdict == gateGone {
			return fmt.Errorf("gate %s: %s before it reported %s",
				opts.target, gone, strings.Join(opts.until, "|"))
		}
	}
}

// What one stream frame means to a gate.
type gateEventVerdict int

const (
	// Not about this worker: a workspace.closed for somebody else's workspace,
	// which herdr delivers regardless of the filter it was asked for.
	gateIgnore gateEventVerdict = iota
	// This worker did something; look at the pane.
	gateCheck
	// This worker can no longer report. A gate that waits on one of these out
	// to its deadline turns a crash into fifteen minutes of silence, which is
	// the state that makes a gate worse than no gate at all.
	gateGone
)

// gateVerdict classifies one frame, doing the filtering herdr does not.
func gateVerdict(event herdrapi.StreamEvent, pane, workspace string) (gateEventVerdict, string) {
	switch event.Event {
	case herdrapi.StreamEventPaneExited, herdrapi.StreamEventPaneClosed:
		return gateGone, "the worker's pane ended"

	case herdrapi.StreamEventPaneUpdated:
		// Unfiltered and backlog-replayed, like workspace.closed: the
		// subscription takes no pane id, so this is every pane in the session,
		// and a fresh subscriber is handed the history first.
		//
		// Both are harmless here for the same reason, which is that a frame is
		// only ever a reason to go and look. The payload carries the pane's
		// tokens AS THEY WERE, and a replayed frame therefore carries metadata
		// that has since been replaced; reading it would be how a gate releases
		// on a report from a quarter of an hour ago. So the id is checked here
		// and the report is read from herdr, never from the frame.
		if event.PaneID() != pane {
			return gateIgnore, ""
		}
		return gateCheck, ""

	case herdrapi.StreamEventWorkspaceClosed:
		// Unfiltered and replayed from the session's backlog, so the id has to
		// be checked here or the gate fails on somebody else's history.
		if event.WorkspaceID() != workspace {
			return gateIgnore, ""
		}
		return gateGone, "the worker's workspace was closed"

	case herdrapi.StreamEventPaneAgentStatusChanged:
		data, err := event.AgentStatus()
		if err != nil {
			// A frame we cannot read is not a frame we can act on, and the
			// alternative to saying so is guessing about a live worker.
			return gateIgnore, ""
		}
		// `unknown` is what an agent that has gone away looks like: herdr had a
		// state for this pane a moment ago and has none now.
		if data.AgentStatus == herdrapi.AgentStatusUnknown {
			return gateGone, "herdr lost track of the agent"
		}
		return gateCheck, ""
	}
	return gateIgnore, ""
}

// gateExpired is the diagnosis a caller gets at the deadline.
//
// It names the baseline because the most likely reason a gate expires with a
// report sitting in the pane is that it was started after the worker answered.
// Only the validated slots are quoted back: a timeout message is not a place to
// print untrusted text, and status and pr say everything a coordinator needs to
// decide whether to act on it or dispatch again.
func gateExpired(opts gateOptions, pane string, waited time.Duration, baseline report, hasBaseline bool) error {
	detail := "the pane held no report when the gate opened"
	if hasBaseline {
		detail = fmt.Sprintf("the pane already held status=%s pr=%s when the gate opened, which the gate ignored as a previous task's answer",
			baseline.status, prSlot(baseline))
	}
	return fmt.Errorf("gate %s: no new report reached status %s within %s (pane %s); %s",
		opts.target, strings.Join(opts.until, "|"), waited.Round(time.Second), pane, detail)
}

// prSlot renders the pr slot the same way the envelope does.
func prSlot(r report) string {
	if r.pr > 0 {
		return fmt.Sprint(r.pr)
	}
	return missing
}

// isBlocked is the one status that ends a wait without releasing it.
func isBlocked(r report) bool { return r.status == "blocked" }

// The two channels a report can reach a gate on. Each carries its own counter
// and the two are never compared: one counts writes to a pane's metadata, the
// other counts envelopes in its output, and a number from one says nothing about
// a number from the other.
const (
	channelMetadata = iota
	channelTerminal
	channelCount
)

// gateMarks is the sequence each channel showed the last time the gate looked.
type gateMarks [channelCount]uint64

// observation is what one channel held on one look at the pane.
//
// A channel the gate could not READ produces no observation at all, which is not
// the same as one holding no report. A failed read is not evidence that a report
// went away, and recording it as one would lower that channel's mark and let the
// gate judge a report it has already judged — releasing, the second time, on the
// stale answer the baseline exists to refuse.
type observation struct {
	channel int
	report  report
	seq     uint64
}

// advance returns the reports the gate has not judged yet, and moves the marks
// to what it has just been shown.
//
// THE RELEASE RULE. A report is new exactly when its channel's counter is higher
// than the one that channel last showed. Content is never compared, which is
// what makes a report identical to an earlier one audible: two runs of the same
// fixed template are two reports, and the second one is news.
//
// A counter that goes DOWN re-marks and releases nothing. The terminal's does
// that legitimately — it counts envelopes in a bounded snapshot, so a buffer
// that has scrolled past one holds fewer than it did — and following it down is
// what keeps a long-lived pane audible, at the cost of nothing: a report is
// still news at any count above the mark it lands on.
func (m *gateMarks) advance(obs []observation) []report {
	var fresh []report
	for _, o := range obs {
		if o.seq > m[o.channel] {
			fresh = append(fresh, o.report)
		}
		m[o.channel] = o.seq
	}
	return fresh
}

// readChannels returns what each of a pane's channels holds, and the sequence
// that identifies it there.
//
// BOTH, every look, and neither wins. Metadata is read first because it is where
// a report normally is — it is the channel `worktender report` writes and confirms,
// and the only one that survives a Claude Code tool call — but a worker that
// reproduces its envelope as reply text has reported too, and it is the same
// worker. A reader that stopped at the metadata the moment it held anything made
// the terminal unreachable for the rest of the gate: a worker that reported
// `planned` over the tool call and finished by echoing `done` was never
// released. Reading both and comparing each against its own mark is what lets
// the newer report win whichever channel it arrived on.
//
// WHAT THE METADATA CHANNEL AUTHENTICATES, WHICH IS NOT AUTHORSHIP. It is no
// stronger than the pane here, and this comment used to say it was.
// pane.report_metadata takes an arbitrary pane_id, so nothing binds a write to
// the caller's own pane; `source` is provenance rather than a namespace, as
// metadata.go says where it depends on exactly that; and what arrives is a flat
// map[string]string with no attribution on it, so decodeReport could not check
// an author even in principle. Any process holding the herdr socket can write
// worktender_status and worktender_pr onto another worker's pane and release a
// coordinator's gate.
//
// That is a limit to state, not a hole to engineer around: it needs code already
// running as the user, so it crosses no privilege boundary, and a nonce would
// fail here for the reason the header of this file gives. The counter does not
// narrow it either — a writer that can set the slots can set the number. Both
// channels authenticate SHAPE and POSITION and nothing else. That `worktender
// report` writes only to HERDR_PANE_ID is this plugin's own discipline, which is
// worth keeping and is not a restriction the channel enforces.
//
// applies_to_source would not change that, which is why it is still not sent.
// Whatever it constrains on the way in, nothing on the way out carries a source
// at all: PaneInfo.Tokens is one flat map per pane, so a reader has nothing to
// check a claimed source against. It could only ever be a field set, not a
// guarantee gained.
//
// Neither read is fatal when it fails. A pane can disappear underneath a gate,
// and the event that says so is already on its way; failing here would report a
// vanished pane as a transport fault rather than as a worker that died.
func readChannels(client *herdrapi.Client, pane string) []observation {
	var obs []observation
	if info, err := client.PaneGet(pane); err == nil {
		r, seq, _ := decodeReport(info.Pane.Tokens)
		obs = append(obs, observation{channel: channelMetadata, report: r, seq: seq})
	}
	if read, err := client.PaneRead(pane, gateReadSource); err == nil {
		r, count := envelopesIn(read.Read.Text)
		obs = append(obs, observation{channel: channelTerminal, report: r, seq: count})
	}
	return obs
}

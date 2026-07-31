package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/steig/muster/internal/herdrapi"
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
// after the gate started, and nothing about who composed it.
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

// gateReadSource is the snapshot the report is parsed out of. Unwrapped,
// because a wrapped envelope line is two lines to a parser.
//
// The pane is the channel because it is the one `report` already writes to. It
// carries a REQUIREMENT the dispatch prompt has to satisfy, learnt by watching
// a real agent fail it: the envelope must reach the worker's TERMINAL. A Claude
// Code worker that runs `muster report` as a tool call does not satisfy this —
// its TUI collapses a finished tool call to "Ran 1 shell command" and the
// envelope stays inside its transcript, never reaching the screen the gate
// reads. Telling the worker to reproduce the output as its reply does satisfy
// it, and is what a coordinator has to ask for.
const gateReadSource = herdrapi.ReadSourceRecentUnwrapped

const gateUsage = "usage: muster gate --target <agent|pane> [--until planned|blocked|done] [--require-pr] [--timeout 15m]"

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
	// A duration rather than herdr's bare milliseconds: this is muster's own
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
	stream, err := client.Subscribe([]herdrapi.Subscription{
		{Type: herdrapi.SubscriptionPaneAgentStatusChanged, PaneID: pane},
		{Type: herdrapi.SubscriptionPaneExited, PaneID: pane},
		{Type: herdrapi.SubscriptionPaneClosed, PaneID: pane},
		{Type: herdrapi.SubscriptionWorkspaceClosed, WorkspaceID: workspace},
	}, deadline)
	if err != nil {
		return fmt.Errorf("gate %s: %w", opts.target, err)
	}
	defer stream.Close()

	// Whatever is already in the pane is a PREVIOUS task's answer.
	//
	// A worker is dispatched, then gated; anything it had reported before the
	// gate opened was reported about something else. Releasing on it would hand
	// the coordinator a stale `done` — which is precisely the hand-off bug the
	// gate exists to remove, not a shortcut worth taking. The baseline is
	// therefore ignored, and named in the timeout message, so a gate started
	// too late says so instead of just failing.
	baseline, hasBaseline := readEnvelope(client, pane)

	fmt.Fprintf(out, "gate: waiting on %s (pane %s) for status %s, up to %s\n",
		opts.target, pane, strings.Join(opts.until, "|"), opts.timeout)

	// last is the report the gate has already judged. A parsed envelope always
	// carries a status, so the zero value cannot collide with a real one.
	last := baseline
	for {
		event, err := stream.Next()
		if errors.Is(err, herdrapi.ErrStreamExpired) {
			return gateExpired(opts, pane, time.Since(started), baseline, hasBaseline)
		}
		if err != nil {
			return fmt.Errorf("gate %s: %w", opts.target, err)
		}

		verdict, gone := gateVerdict(event, workspace)
		if verdict == gateIgnore {
			continue
		}

		// Read before acting on the verdict, so a worker that printed its
		// report and died in the same breath is still heard. A gate that
		// checked liveness first would throw that report away.
		current, ok := readEnvelope(client, pane)
		if ok && current != last {
			last = current
			if opts.satisfies(current) {
				fmt.Fprintf(out, "gate: released after %s\n", time.Since(started).Round(time.Second))
				fmt.Fprint(out, renderReport(current))
				return nil
			}
			// `blocked` is terminal in the direction that matters: a blocked
			// worker does not reach `done` on its own, and the party that
			// unblocks it is the coordinator sitting in this wait. Holding the
			// gate open would spend the whole timeout waiting for something
			// only the waiter can cause.
			if current.status == "blocked" {
				fmt.Fprint(out, renderReport(current))
				return fmt.Errorf("gate %s: the worker reported blocked after %s; it will not reach %s without you",
					opts.target, time.Since(started).Round(time.Second), strings.Join(opts.until, "|"))
			}
			fmt.Fprintf(out, "gate: %s reported %s; still waiting\n", opts.target, current.status)
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
func gateVerdict(event herdrapi.StreamEvent, workspace string) (gateEventVerdict, string) {
	switch event.Event {
	case herdrapi.StreamEventPaneExited, herdrapi.StreamEventPaneClosed:
		return gateGone, "the worker's pane ended"

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

// readEnvelope takes a snapshot of the pane and returns the last envelope in it.
//
// A read that fails is treated as "no envelope" rather than as a fatal error:
// the pane can disappear underneath a gate, and the event that says so is
// already on its way. Failing here instead would report a vanished pane as a
// transport fault rather than as a worker that died.
func readEnvelope(client *herdrapi.Client, pane string) (report, bool) {
	read, err := client.PaneRead(pane, gateReadSource)
	if err != nil {
		return report{}, false
	}
	return lastEnvelope(read.Read.Text)
}

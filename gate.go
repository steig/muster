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

// gateDefaultTimeout bounds a wait nobody supplied a bound for. There is no
// "wait indefinitely" option.
const gateDefaultTimeout = 15 * time.Minute

// gateReadSource is the snapshot the terminal channel is parsed out of.
// Unwrapped, because a wrapped envelope line is two lines to a parser.
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
// optionally the presence of the validated pr slot. The note is deliberately
// not reachable from here.
func (o gateOptions) satisfies(r report) bool {
	if !slices.Contains(o.until, r.status) {
		return false
	}
	return !o.requirePR || r.pr > 0
}

// gateCommand waits for a worker to report, then releases. It takes no
// repository lock and needs no invocation context, but it does need herdr.
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
	// A gate pointed at something that will never report fails here rather than
	// at the deadline.
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

	// Subscribe BEFORE reading the baseline, or a report landing between the two
	// is in neither.
	//
	// workspace.closed is subscribed because closing a workspace emits no
	// pane.exited or pane.closed for the panes inside it. pane.updated is what
	// makes the metadata channel edge-triggered: attaching tokens emits one.
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

	// Whatever is already on either channel is a previous task's answer, so it
	// is ignored — and named in the timeout message, so a gate started too late
	// says so instead of just failing.
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

		// Read before acting on the verdict, so a worker that printed its report
		// and died in the same breath is still heard.
		fresh := seen.advance(readChannels(client, pane))

		// A look can turn up more than one new report, so every one is weighed
		// for release before any is weighed for blocked: the order they were
		// read in must not decide the gate.
		if i := slices.IndexFunc(fresh, opts.satisfies); i >= 0 {
			fmt.Fprintf(out, "gate: released after %s\n", time.Since(started).Round(time.Second))
			fmt.Fprint(out, renderReport(fresh[i]))
			return nil
		}
		// A blocked worker does not reach `done` on its own, and the party that
		// unblocks it is the coordinator sitting in this wait.
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
	// Not about this worker; herdr delivers some events regardless of the
	// filter it was asked for.
	gateIgnore gateEventVerdict = iota
	// This worker did something; look at the pane.
	gateCheck
	// This worker can no longer report.
	gateGone
)

// gateVerdict classifies one frame, doing the filtering herdr does not.
func gateVerdict(event herdrapi.StreamEvent, pane, workspace string) (gateEventVerdict, string) {
	switch event.Event {
	case herdrapi.StreamEventPaneExited, herdrapi.StreamEventPaneClosed:
		return gateGone, "the worker's pane ended"

	case herdrapi.StreamEventPaneUpdated:
		// Unfiltered and backlog-replayed, so the id is checked here. A frame is
		// only ever a reason to go and look: the payload carries the pane's
		// tokens as they were, so the report is read from herdr, never from the
		// frame.
		if event.PaneID() != pane {
			return gateIgnore, ""
		}
		return gateCheck, ""

	case herdrapi.StreamEventWorkspaceClosed:
		// Unfiltered and replayed, so the id has to be checked here.
		if event.WorkspaceID() != workspace {
			return gateIgnore, ""
		}
		return gateGone, "the worker's workspace was closed"

	case herdrapi.StreamEventPaneAgentStatusChanged:
		data, err := event.AgentStatus()
		if err != nil {
			// A frame we cannot read is not a frame we can act on.
			return gateIgnore, ""
		}
		// `unknown` is what an agent that has gone away looks like.
		if data.AgentStatus == herdrapi.AgentStatusUnknown {
			return gateGone, "herdr lost track of the agent"
		}
		return gateCheck, ""
	}
	return gateIgnore, ""
}

// gateExpired is the diagnosis a caller gets at the deadline. It names the
// baseline, and quotes only the validated slots — a timeout message is not a
// place to print untrusted text.
func gateExpired(opts gateOptions, pane string, waited time.Duration, baseline report, hasBaseline bool) error {
	detail := "the pane held no report when the gate opened"
	if hasBaseline {
		detail = fmt.Sprintf("the pane already held status=%s pr=%s when the gate opened, which the gate ignored as a previous task's answer",
			baseline.status, prSlot(baseline))
	}
	return fmt.Errorf("gate %s: no new report reached status %s within %s (pane %s); %s",
		opts.target, strings.Join(opts.until, "|"), waited.Round(time.Second), pane, detail)
}

// isBlocked is the one status that ends a wait without releasing it.
func isBlocked(r report) bool { return r.status == "blocked" }

// The two channels a report can reach a gate on. Each carries its own counter
// and the two are never compared.
const (
	channelMetadata = iota
	channelTerminal
	channelCount
)

// gateMarks is the sequence each channel showed the last time the gate looked.
type gateMarks [channelCount]uint64

// observation is what one channel held on one look at the pane. A channel the
// gate could not read produces no observation at all, which is not the same as
// one holding no report.
type observation struct {
	channel int
	report  report
	seq     uint64
}

// advance returns the reports the gate has not judged yet, and moves the marks
// to what it has just been shown.
//
// A report is new exactly when its channel's counter is higher than the one
// that channel last showed; content is never compared, so two byte-identical
// reports are two reports. A counter that goes down re-marks and releases
// nothing — the terminal's does that legitimately as its buffer scrolls.
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
// Both are read on every look and neither wins. Metadata is where a report
// normally is — it is the only channel that survives a Claude Code tool call —
// but a worker that reproduces its envelope as reply text has reported too, and
// a reader that stopped at metadata made the terminal unreachable for the rest
// of the gate.
//
// Neither channel authenticates authorship: any process holding the herdr socket
// can write the slots onto another pane. Both authenticate shape and position
// only. That `worktender report` writes solely to HERDR_PANE_ID is this
// plugin's own discipline, not something the channel enforces.
//
// Neither read is fatal when it fails: a pane can disappear underneath a gate,
// and the event that says so is already on its way.
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

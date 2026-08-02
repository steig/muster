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

const gateUsage = "usage: worktender gate --target <agent|pane> | --any <agent|pane>,... [--until planned|blocked|done] [--require-pr] [--timeout 15m]"

// gateOptions is one gate invocation.
type gateOptions struct {
	// targets is every worker this wait covers, in the order they were named.
	// The first one to satisfy the predicate releases the gate; the rest are
	// left running for the caller to gate on again.
	targets   []string
	until     []string
	requirePR bool
	timeout   time.Duration
}

// names is how a wait covering several workers refers to itself in a message
// about the wait rather than about one worker.
func (o gateOptions) names() string { return strings.Join(o.targets, ",") }

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

// targetFlag collects the workers a gate waits on. `--target` takes one name
// and `--any` takes a comma-separated list, but both fill the same list: a gate
// on one worker is a gate on any of one, so there is a single code path rather
// than a single-target case and a multi-target one beside it.
//
// Both are repeatable and neither overwrites, because flag's own last-one-wins
// would drop a target silently — and a coordinator that believes it is waiting
// on five workers while the gate waits on one has no way to notice.
type targetFlag struct {
	targets *[]string
	// split is what makes --any a list: --target takes its value whole, so a
	// name is never cut on a character herdr allows in one.
	split bool
}

func (f targetFlag) String() string {
	if f.targets == nil {
		return ""
	}
	return strings.Join(*f.targets, ",")
}

func (f targetFlag) Set(v string) error {
	values := []string{v}
	if f.split {
		values = strings.Split(v, ",")
	}
	for _, name := range values {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("%q has an empty target in it", v)
		}
		// A target named twice would be watched twice and counted twice, so its
		// one report could release a gate that believes it heard from two
		// workers.
		if slices.Contains(*f.targets, name) {
			return fmt.Errorf("%q is named more than once", name)
		}
		*f.targets = append(*f.targets, name)
	}
	return nil
}

func parseGate(args []string) (gateOptions, error) {
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var targets []string
	flags.Var(targetFlag{targets: &targets}, "target", "an agent or pane to wait on; repeat for more than one")
	flags.Var(targetFlag{targets: &targets, split: true}, "any", "agents or panes to wait on, comma-separated; the first to satisfy the predicate releases")
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
	if len(targets) == 0 {
		return gateOptions{}, fmt.Errorf("--target or --any is required; %s", gateUsage)
	}
	if *timeout <= 0 {
		return gateOptions{}, fmt.Errorf("--timeout must be positive; a gate that cannot expire is the wedge this command exists to avoid")
	}
	if len(until) == 0 {
		// The gate's reason for existing is completion, so that is the default.
		until = statusFlag{"done"}
	}
	return gateOptions{targets: targets, until: until, requirePR: *requirePR, timeout: *timeout}, nil
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

// gateTarget is one worker a gate is waiting on: what the caller called it,
// where herdr says it lives, and what its channels had already shown.
type gateTarget struct {
	name      string
	pane      string
	workspace string
	// seen is per pane, because the counters that identify a report are per
	// channel per pane and two panes' are never comparable.
	seen        gateMarks
	baseline    report
	hasBaseline bool
}

// gateFresh is one new report and the worker it came from. A gate on several
// workers has to weigh reports from all of them together, so each one carries
// where it came from rather than being judged in its own loop.
type gateFresh struct {
	target int
	report report
}

func runGate(client *herdrapi.Client, opts gateOptions, out io.Writer) error {
	targets, err := resolveTargets(client, opts.targets)
	if err != nil {
		return err
	}

	started := time.Now()
	deadline := started.Add(opts.timeout)

	// Subscribe BEFORE reading the baselines, or a report landing between the
	// two is in neither.
	stream, err := client.Subscribe(gateSubscriptions(targets), deadline)
	if err != nil {
		return fmt.Errorf("gate %s: %w", opts.names(), err)
	}
	defer stream.Close()

	// Whatever is already on either of a worker's channels is a previous task's
	// answer, so it is ignored — and named in the timeout message, so a gate
	// started too late says so instead of just failing.
	for i := range targets {
		stale := targets[i].seen.advance(readChannels(client, targets[i].pane))
		if len(stale) > 0 {
			targets[i].baseline, targets[i].hasBaseline = stale[0], true
		}
	}

	fmt.Fprintf(out, "gate: waiting on %s for status %s, up to %s\n",
		gateWhere(targets), strings.Join(opts.until, "|"), opts.timeout)

	for {
		event, err := stream.Next()
		if errors.Is(err, herdrapi.ErrStreamExpired) {
			return gateExpired(opts, targets, time.Since(started))
		}
		if err != nil {
			return fmt.Errorf("gate %s: %w", opts.names(), err)
		}

		// Every target is asked what the frame means to it: herdr delivers some
		// events regardless of the filter it was asked for, and one workspace
		// closing ends every worker inside it.
		var fresh []gateFresh
		gone, goneWhy := -1, ""
		for i := range targets {
			verdict, why := gateVerdict(event, targets[i].pane, targets[i].workspace)
			if verdict == gateIgnore {
				continue
			}
			// Read before acting on the verdict, so a worker that printed its
			// report and died in the same breath is still heard.
			for _, r := range targets[i].seen.advance(readChannels(client, targets[i].pane)) {
				fresh = append(fresh, gateFresh{target: i, report: r})
			}
			if verdict == gateGone && gone < 0 {
				gone, goneWhy = i, why
			}
		}

		// A look can turn up more than one new report, so every one is weighed
		// for release before any is weighed for blocked, and before any death is
		// acted on: neither the order they were read in nor which worker the
		// frame was about may decide the gate.
		if i := slices.IndexFunc(fresh, func(f gateFresh) bool { return opts.satisfies(f.report) }); i >= 0 {
			// Which worker released is the whole answer when the gate covered
			// several: it is what the caller drops before waiting on the rest.
			fmt.Fprintf(out, "gate: %s released after %s\n",
				targets[fresh[i].target].name, time.Since(started).Round(time.Second))
			fmt.Fprint(out, renderReport(fresh[i].report))
			return nil
		}
		// A blocked worker does not reach `done` on its own, and the party that
		// unblocks it is the coordinator sitting in this wait.
		if i := slices.IndexFunc(fresh, func(f gateFresh) bool { return isBlocked(f.report) }); i >= 0 {
			fmt.Fprint(out, renderReport(fresh[i].report))
			return fmt.Errorf("gate %s: the worker reported blocked after %s; it will not reach %s without you",
				targets[fresh[i].target].name, time.Since(started).Round(time.Second), strings.Join(opts.until, "|"))
		}
		for _, f := range fresh {
			fmt.Fprintf(out, "gate: %s reported %s; still waiting\n", targets[f.target].name, f.report.status)
		}

		// A worker that can no longer report ends the wait even when others are
		// still running, and the failure names which one: the caller's remedy is
		// to drop it and gate on the rest, and it cannot do that without the
		// name. Waiting on in silence would leave the death unreported until the
		// deadline, which is the failure the gate exists to convert into a fast
		// one.
		if gone >= 0 {
			return fmt.Errorf("gate %s: %s before it reported %s",
				targets[gone].name, goneWhy, strings.Join(opts.until, "|"))
		}
	}
}

// resolveTargets turns the names a caller gave into panes. A gate pointed at
// something that will never report fails here rather than at the deadline, and
// it fails before waiting on ANY of them: a coordinator that mistyped one name
// out of five should not find out fifteen minutes later.
func resolveTargets(client *herdrapi.Client, names []string) ([]gateTarget, error) {
	targets := make([]gateTarget, 0, len(names))
	for _, name := range names {
		info, err := client.AgentGet(name)
		if err != nil {
			return nil, fmt.Errorf("gate %s: %w", name, err)
		}
		t := gateTarget{name: name, pane: info.Agent.PaneID, workspace: info.Agent.WorkspaceID}
		if info.Agent.AgentStatus == herdrapi.AgentStatusUnknown {
			return nil, fmt.Errorf("gate %s: herdr reports no agent state for pane %s; there is nothing to wait on", name, t.pane)
		}
		// An agent and its own pane id are two names for one worker, and herdr
		// resolves both. Waiting on both would count one worker as two.
		if i := slices.IndexFunc(targets, func(o gateTarget) bool { return o.pane == t.pane }); i >= 0 {
			return nil, fmt.Errorf("gate %s: %s is the same worker (pane %s); name it once", name, targets[i].name, t.pane)
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// gateSubscriptions is what one stream has to carry to cover every target.
//
// workspace.closed is subscribed because closing a workspace emits no
// pane.exited or pane.closed for the panes inside it. pane.updated is what
// makes the metadata channel edge-triggered: attaching tokens emits one.
func gateSubscriptions(targets []gateTarget) []herdrapi.Subscription {
	// pane.updated arrives unfiltered whatever is asked for, so one subscription
	// covers every target and the pane id is checked on the way in instead.
	subs := []herdrapi.Subscription{{Type: herdrapi.SubscriptionPaneUpdated}}
	var workspaces []string
	for _, t := range targets {
		subs = append(subs,
			herdrapi.Subscription{Type: herdrapi.SubscriptionPaneAgentStatusChanged, PaneID: t.pane},
			herdrapi.Subscription{Type: herdrapi.SubscriptionPaneExited, PaneID: t.pane},
			herdrapi.Subscription{Type: herdrapi.SubscriptionPaneClosed, PaneID: t.pane})
		// Workers started from one `worktree create` share a workspace, and
		// subscribing to its close once per worker asks herdr for the same
		// frames several times over.
		if !slices.Contains(workspaces, t.workspace) {
			workspaces = append(workspaces, t.workspace)
			subs = append(subs, herdrapi.Subscription{Type: herdrapi.SubscriptionWorkspaceClosed, WorkspaceID: t.workspace})
		}
	}
	return subs
}

// gateWhere names every worker the gate opened on and where it is, so the line
// a coordinator sees while it waits is the one it can check against `ls`.
func gateWhere(targets []gateTarget) string {
	where := make([]string, 0, len(targets))
	for _, t := range targets {
		where = append(where, fmt.Sprintf("%s (pane %s)", t.name, t.pane))
	}
	return strings.Join(where, ", ")
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

// gateVerdict classifies one frame for one worker, doing the filtering herdr
// does not.
//
// Every pane-scoped frame is matched on its own id rather than trusted to the
// subscription that asked for it. herdr's filter is per subscription and a gate
// on several workers has all of theirs on ONE stream, so "this frame arrived"
// stopped meaning "this frame is about the worker I asked about" — a pane
// exiting would otherwise have ended the wait naming whichever worker was
// checked first. Every one of these payloads carries `pane_id` as a required
// field, so the id is always there to match on.
func gateVerdict(event herdrapi.StreamEvent, pane, workspace string) (gateEventVerdict, string) {
	switch event.Event {
	case herdrapi.StreamEventPaneExited, herdrapi.StreamEventPaneClosed:
		if event.PaneID() != pane {
			return gateIgnore, ""
		}
		return gateGone, "the worker's pane ended"

	case herdrapi.StreamEventPaneUpdated:
		// Unfiltered and backlog-replayed. A frame is only ever a reason to go
		// and look: the payload carries the pane's tokens as they were, so the
		// report is read from herdr, never from the frame.
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
		if data.PaneID != pane {
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

// gateExpired is the diagnosis a caller gets at the deadline. It names each
// worker's baseline, and quotes only the validated slots — a timeout message is
// not a place to print untrusted text.
//
// Every target is accounted for rather than summarised, because a gate that
// covered five workers and released on none of them is five separate questions
// and the answers differ: one pane empty and four holding last task's report is
// a different problem from all five empty.
func gateExpired(opts gateOptions, targets []gateTarget, waited time.Duration) error {
	details := make([]string, 0, len(targets))
	for _, t := range targets {
		detail := fmt.Sprintf("%s (pane %s) held no report when the gate opened", t.name, t.pane)
		if t.hasBaseline {
			detail = fmt.Sprintf("%s (pane %s) already held status=%s pr=%s when the gate opened, which the gate ignored as a previous task's answer",
				t.name, t.pane, t.baseline.status, prSlot(t.baseline))
		}
		details = append(details, detail)
	}
	return fmt.Errorf("gate %s: no new report reached status %s within %s; %s",
		opts.names(), strings.Join(opts.until, "|"), waited.Round(time.Second), strings.Join(details, "; "))
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

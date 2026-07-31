package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steig/muster/internal/gitx"
	"github.com/steig/muster/internal/herdrapi"
	"github.com/steig/muster/internal/reconcile"
	"github.com/steig/muster/internal/repolock"
)

// eventsEnv opts a session in to the event fast path. Unset means events do
// nothing at all.
//
// Off by default, deliberately, for two reasons. The weaker one is that this
// plugin is developed while linked live into a real herdr session, so an
// [[events]] block is armed the moment the manifest is saved. The stronger one
// is that this is the right shipping default: an event hook here starts coding
// agents without being asked, and a plugin that does that on install is handing
// its user an autonomous trigger they never requested. Opting in is one
// exported variable; opting out after a surprise is not.
const eventsEnv = "MUSTER_EVENTS"

// legacyEventsEnv is what eventsEnv was called before the plugin was renamed.
// It enables nothing. It exists so the gate can SAY so.
//
// A silent rename is the bad case here, and it is bad in a quiet direction: a
// set variable becomes an unset one, which fails safe — events go off — but
// gives no one a reason to look. Someone who opted in months ago would find
// their hooks inert with nothing anywhere saying why.
//
// Honouring it as an alias was the alternative and it is worse. This variable
// arms an autonomous trigger that starts coding agents, so keeping it live
// under a name the plugin no longer documents means the loudest thing here
// answers to a spelling that appears in no current README. Detect, refuse, and
// name the replacement.
const legacyEventsEnv = "HERDR_WT_EVENTS"

// eventsEnabled reports whether the fast path is opted in to.
func eventsEnabled() bool {
	switch os.Getenv(eventsEnv) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

// renamedEnvNotice is the line owed to a session still exporting the old name,
// or "" when nothing is owed. An explicit MUSTER_EVENTS of any value silences
// it: at that point the caller knows the current spelling, including when they
// used it to opt out.
func renamedEnvNotice() string {
	if os.Getenv(eventsEnv) != "" || os.Getenv(legacyEventsEnv) == "" {
		return ""
	}
	return fmt.Sprintf("%s is set, but it was renamed to %s and the old name is not honoured; export %s=1 instead\n",
		legacyEventsEnv, eventsEnv, eventsEnv)
}

// onEventCommand is the whole event fast path.
//
// The governing rule: an event is a TRIGGER, never a FACT. Nothing here acts on
// the payload's contents. The payload is read for exactly one thing — which
// repository — and then the same collect/reconcile/execute pipeline `sync` runs
// runs again, over the whole repository, reading live state.
//
// That is the same reasoning execute.prune already applies one level down: a
// snapshot goes stale, so guards are re-read at the moment of acting. An event
// payload is herdr's snapshot from before this process existed, so it is stale
// on arrival by construction.
//
// It buys the consistency story too. Because the event path and the reconciler
// are the same code, "event versus reconciler" is not a category of bug that
// can exist; what remains is two reconcilers running at once, which is a much
// smaller problem.
func onEventCommand(out io.Writer) error {
	// Checked before anything else is even parsed, so a plugin that has not
	// been opted in does nothing whatsoever.
	if !eventsEnabled() {
		fmt.Fprintf(out, "events are off; export %s=1 to enable the worktree fast path\n", eventsEnv)
		fmt.Fprint(out, renamedEnvNotice())
		return nil
	}

	envelope, err := herdrapi.LoadEvent()
	if err != nil {
		return err
	}

	scope, err := envelope.Scope()
	if err != nil {
		// An event we subscribe to but have no behaviour for is a no-op, not a
		// failure — the manifest and the binary can differ across an upgrade.
		if errors.Is(err, herdrapi.ErrUnhandledEvent) {
			fmt.Fprintf(out, "ignoring %s\n", envelope.Event)
			return nil
		}
		return err
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	root := scope.RepoRoot
	if root == "" {
		// herdr omitted the repository root, so derive it from the checkout the
		// event named. Note this never falls back to the process cwd: unlike an
		// action, an event names its own subject, so there is nothing to guess.
		if root, err = gitx.RepoRoot(scope.Checkout); err != nil {
			return fmt.Errorf("%s: %w", envelope.Event, err)
		}
	}

	// CallerDir is empty on purpose: nobody is standing in a directory here, and
	// the guard it feeds only governs removals, which this path never performs.
	s := &session{client: client, root: gitx.Resolve(root)}

	collector := reconcile.NewCollector(s.client, s.root)
	// Prune actions are filtered out below, so the PR lookup that would
	// authorise one decides nothing — and it costs a gh invocation per worktree
	// per event, which is a network round trip on a path that fires whenever the
	// user touches a worktree.
	collector.LookupPR = nil

	// Claim the repository, or leave a mark and stand down. Standing down is
	// not a dropped event: the holder is running the same whole-repository
	// reconcile this would have run, and the mark stops it finishing on a
	// snapshot older than this event. Queueing instead would turn a batch of
	// worktree events into a batch of identical full reconciles.
	lock, err := repolock.AcquireOrMark(stateDir(), s.root)
	if err != nil {
		return err
	}
	if lock == nil {
		fmt.Fprintf(out, "%s: %s is already being reconciled; coalesced into that pass\n", envelope.Event, s.root)
		return nil
	}
	defer lock.Release()

	return lock.Repeat(reconcilePasses, func() error {
		actions, err := s.planWith(collector)
		if err != nil {
			return err
		}
		// Adopt and staff only. Removal stays something a human asks for by
		// name: `prune` and `prune-apply` are separate actions precisely so
		// nothing removes a worktree as a side effect of something else.
		return s.perform(out, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
	})
}

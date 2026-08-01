// Command worktender drives git worktrees as herdr workspaces.
//
// herdr runs it as a plugin: each subcommand below is registered as an action
// in herdr-plugin.toml and invoked by herdr, which supplies HERDR_SOCKET_PATH
// and the launch context in the environment.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/repolock"
	"github.com/steig/worktender/internal/wt"
)

// commands is every subcommand run dispatches, and the source usage is built
// from. One list rather than two, so usage cannot drift from what exists.
var commands = []string{"ls", "doctor", "update", "start", "sync", "dispatch", "prune", "prune-apply", "report", "gate", "on-event", "startup"}

var usage = "usage: worktender <" + strings.Join(commands, "|") + ">"

// releaseLock releases and says so when it fails.
//
// A failed release leaves the lock file on disk, so every other reconcile of
// this repository coalesces into a pass that is not running until
// repolock.MaxHold expires. It reports rather than returns because every caller
// is a defer whose return value is already spoken for.
func releaseLock(lock *repolock.Lock, out io.Writer) {
	if err := lock.Release(); err != nil {
		fmt.Fprintf(out, "warning: %v; this repository stays locked for up to %s\n", err, repolock.MaxHold)
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "worktender:", err)
		os.Exit(1)
	}
}

// run dispatches a subcommand. Every failure returns an error so it reaches the
// process exit code: herdr records a plugin action that exits 0 as "succeeded".
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}

	switch args[0] {
	case "ls", "list":
		return lsCommand(args[1:], out)
	case "doctor":
		// Read-only, takes no lock, works from outside a repository.
		return doctorCommand(out)
	case "update":
		// Moves this plugin's own install forward; touches no repository of the
		// user's.
		return updateCommand(args[1:], out)
	case "start":
		// Issue number in, agent working on it out. Creates a worktree, so it
		// must be told which repository.
		return startCommand(args[1:], out)
	case "sync":
		// Adopt and staff. Finished worktrees are only listed.
		return syncCommand(out)
	case "dispatch":
		// Staffs one named pane; changes no worktree, so it takes no lock.
		return dispatchCommand(args[1:], out)
	case "prune":
		// Lists finished worktrees and removes nothing.
		return pruneCommand(out, false)
	case "prune-apply":
		// The explicit opt-in to actually removing them.
		return pruneCommand(out, true)
	case "report":
		// A worker filling slots for its coordinator; touches neither herdr nor
		// the repository.
		return reportCommand(args[1:], out)
	case "gate":
		// A coordinator waiting on a worker; reads herdr, not the repository.
		return gateCommand(args[1:], out)
	case "on-event":
		// Invoked by herdr, never by hand. Off unless opted in.
		return onEventCommand(out)
	case "startup":
		// Invoked once by herdr after the server is ready. Off unless opted in.
		return startupCommand(out)
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}

// stateDir is where herdr lets this plugin keep state between invocations.
// Empty when we are not running under herdr, which repolock treats as "no lock
// available" and proceeds through.
func stateDir() string { return os.Getenv("HERDR_PLUGIN_STATE_DIR") }

// commandLockWait is how long a human-invoked command waits for a reconcile
// already in progress. An event hook coalesces instead, but a person asked for
// this one, so it queues.
const commandLockWait = 30 * time.Second

// reconcilePasses bounds the coalescing loop: one pass, plus one for whatever
// arrived while it ran, plus slack.
const reconcilePasses = 3

// session is what every command needs: a herdr connection, the repository being
// worked on, and the directory the user invoked from.
type session struct {
	client *herdrapi.Client
	root   string
	dir    string
}

// newSession resolves which repository to work on.
//
// allowFallback decides what happens when herdr supplied no invocation context.
// Read-only commands may fall back to the process working directory; commands
// that change things must not, because herdr runs plugin commands with cwd set
// to the plugin root — itself a git repository.
//
// A malformed context is fatal either way: it is a bug to surface, not a state
// to default around.
func newSession(allowFallback bool) (*session, error) {
	client, err := herdrapi.New()
	if err != nil {
		return nil, err
	}

	ctx, err := herdrapi.LoadContext()
	if err != nil {
		if !errors.Is(err, herdrapi.ErrNoContext) {
			return nil, err
		}
		if !allowFallback {
			return nil, fmt.Errorf("%w; refusing to guess which repository to change", err)
		}
	}

	dir := ctx.LaunchDir()
	if dir == "" {
		if !allowFallback {
			return nil, errors.New("herdr supplied no launch directory; refusing to guess which repository to change")
		}
		if dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}

	// herdr hands us the repository root when it already knows the workspace
	// is a worktree; otherwise ask git.
	root := ctx.RepoRoot()
	if root == "" {
		if root, err = gitx.RepoRoot(dir); err != nil {
			return nil, err
		}
	}
	return &session{client: client, root: gitx.Resolve(root), dir: gitx.Resolve(dir)}, nil
}

// plan collects the current state and decides what the repository needs.
func (s *session) plan() ([]reconcile.Action, error) {
	return s.planWith(reconcile.NewCollector(s.client, s.root))
}

// planWith is plan against a collector the caller has adjusted, which is how
// the event path drops the PR lookup.
func (s *session) planWith(collector *reconcile.Collector) ([]reconcile.Action, error) {
	state, err := collector.Collect()
	if err != nil {
		return nil, err
	}
	return reconcile.Reconcile(state), nil
}

// perform executes actions, writes the report, and fails when any action did.
func (s *session) perform(out io.Writer, actions []reconcile.Action, applyPrune bool) error {
	executor := &execute.Executor{
		Client:     s.client,
		Root:       s.root,
		CallerDir:  s.dir,
		ApplyPrune: applyPrune,
	}
	results := executor.Run(actions)

	fmt.Fprint(out, execute.Render(results))

	counts := execute.Counts(results)
	if counts[execute.StatusPlanned] > 0 {
		fmt.Fprintln(out, "\nrun the `Worktender: prune (apply)` action to remove the worktrees listed above")
	}
	if failed := counts[execute.StatusFailed]; failed > 0 {
		return fmt.Errorf("%d of %d action(s) failed", failed, len(results))
	}
	return nil
}

const lsUsage = "usage: worktender ls [--pr]"

func lsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	withPR := fs.Bool("pr", false, "ask gh for each branch's pull request state")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, lsUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), lsUsage)
	}

	// Read-only: usable from a plain shell.
	s, err := newSession(true)
	if err != nil {
		return err
	}

	// Opt-in, because it is one `gh` invocation per branch and they run in
	// series. A listing people run to see where they stand has to stay fast.
	var lookupPR func(string) string
	if *withPR {
		lookupPR = func(branch string) string {
			return string(reconcile.GhPRState(s.root, branch))
		}
	}
	return wt.Ls(s.client, s.root, s.dir, lookupPR, out)
}

// syncCommand adopts unopened worktrees and staffs agentless workspaces. It
// never prunes: removals are the prune commands' job.
func syncCommand(out io.Writer) error {
	// Opens workspaces and starts agents, so it must be told where.
	s, err := newSession(false)
	if err != nil {
		return err
	}

	// Named for the reason `prune` names it: sync resolves the repository from
	// herdr's invocation context first, so it can act somewhere other than where
	// the caller believes they are standing.
	fmt.Fprintf(out, "repository: %s\n", s.root)

	// Serialise against an event hook reconciling the same repository. The
	// executor re-checks its guards regardless, so this is about not doing the
	// work twice, not about safety.
	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, out)

	collector := reconcile.NewCollector(s.client, s.root)
	// No gh: PR state only ever authorises a prune, and prunes are filtered out
	// below. Every lookup is a network round trip per worktree, deciding nothing.
	collector.LookupPR = nil

	return lock.Repeat(reconcilePasses, func() error {
		actions, err := s.planWith(collector)
		if err != nil {
			return err
		}
		return s.perform(out, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
	})
}

// pruneCommand reports finished worktrees, and removes them only when apply is
// set. It deliberately excludes adoptions and staffing: asking to prune must
// not open workspaces or start agents as a side effect.
func pruneCommand(out io.Writer, apply bool) error {
	// Listing is read-only; applying removes worktrees.
	s, err := newSession(!apply)
	if err != nil {
		return err
	}

	// Both halves name the repository they resolved, because they do not resolve
	// it the same way — listing may fall back to the working directory, applying
	// may not — and must not disagree in silence. Splitting prune from
	// prune-apply is the confirmation step, and that only holds if the second
	// acts on what the first described.
	fmt.Fprintf(out, "repository: %s\n", s.root)

	// Listing changes nothing, so it needs no claim on the repository; only the
	// half that removes worktrees serialises against a concurrent reconcile.
	if !apply {
		actions, err := s.plan()
		if err != nil {
			return err
		}
		return s.perform(out, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), false)
	}

	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, out)

	// A single pass, not Repeat: re-running a removal because more work was
	// marked would act on a trigger someone else observed. The mark is left for
	// the next reconcile.
	actions, err := s.plan()
	if err != nil {
		return err
	}
	return s.perform(out, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), true)
}

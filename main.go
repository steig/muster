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
// from. One list rather than two: a hand-written usage string and a
// hand-written test list had already drifted apart — the test named "every
// command" omitted `gate`, so usage could have dropped it and stayed green.
var commands = []string{"ls", "doctor", "update", "sync", "dispatch", "prune", "prune-apply", "report", "gate", "on-event", "startup"}

var usage = "usage: worktender <" + strings.Join(commands, "|") + ">"

// releaseLock releases and says so when it fails, instead of discarding the
// error the way a bare deferred Release did at four call sites.
//
// A failed release is not cosmetic. The lock file stays on disk, so every other
// reconcile of this repository — event hooks and the startup pass included —
// coalesces into a pass that is not running, until repolock.MaxHold expires. A
// silent defer leaves five minutes of inexplicable no-ops and nothing to read.
//
// It reports rather than returns because every caller is a defer whose return
// value is already spoken for, and because the failure does not invalidate the
// work that just succeeded.
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
// process exit code: herdr records a plugin action that exits 0 as "succeeded",
// so a command that reports a problem and exits 0 is a silent failure.
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}

	switch args[0] {
	case "ls", "list":
		return lsCommand(out)
	case "doctor":
		// Reports the environment the silent failures hide in. Read-only, takes
		// no lock, and works from outside a repository.
		return doctorCommand(out)
	case "update":
		// Moves this plugin's own install forward. It touches no repository of
		// the user's, so it takes no session and no lock.
		return updateCommand(args[1:], out)
	case "sync":
		// Adopt and staff. Both are non-destructive, so they act directly;
		// finished worktrees are only listed.
		return syncCommand(out)
	case "dispatch":
		// Staffs one named pane with configuration `sync` never supplies. Reads
		// herdr and changes no worktree, so it takes no lock.
		return dispatchCommand(args[1:], out)
	case "prune":
		// Lists finished worktrees and removes nothing.
		return pruneCommand(args[1:], out, false)
	case "prune-apply":
		// The explicit opt-in to actually removing them.
		return pruneCommand(args[1:], out, true)
	case "report":
		// A worker filling slots for its coordinator. Touches neither herdr nor
		// the repository, so it takes no session and needs no lock.
		return reportCommand(args[1:], out)
	case "gate":
		// A coordinator waiting on a worker it dispatched. Reads herdr but not
		// the repository, so it takes no lock either.
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
//
// Empty when we are not running under herdr, which repolock treats as "no lock
// available" and proceeds through, rather than as a reason to stop.
func stateDir() string { return os.Getenv("HERDR_PLUGIN_STATE_DIR") }

// commandLockWait is how long a human-invoked command waits for a reconcile
// already in progress. An event hook coalesces into the running pass instead,
// but a person asked for this one, so it queues rather than quietly doing
// nothing.
const commandLockWait = 30 * time.Second

// reconcilePasses bounds the coalescing loop. Two is the natural maximum — one
// pass, plus one for whatever arrived while it ran — and the third is slack.
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
// Read-only commands may fall back to the process working directory, which
// keeps them usable straight from a shell. Commands that change things must
// not: herdr runs plugin commands with cwd set to the plugin root, which is
// itself a git repository, so falling back there would point a removal at this
// plugin's own checkout.
//
// A malformed context is fatal either way. It means herdr sent something we
// cannot parse, which is a bug to surface, not a state to default around.
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

// newSessionIn resolves against a repository the caller named, skipping herdr's
// context entirely.
//
// The context is the right source when herdr is the one invoking — an action
// carries no arguments, so there is nothing else to go on. It is the wrong
// source when a person is: the context names herdr's current workspace, which
// on a machine with several repositories open is routinely not the one they are
// standing in or thinking about. Observed live: a dry run inside a repository
// with four staffed worktrees planned against a different project's checkout.
//
// `dir` is set to the resolved root rather than to what was passed, so a `--repo
// .` from a subdirectory behaves the same as naming the root. Nothing here falls
// back: a path that is not a repository is an error, because the whole point of
// naming one is to stop the resolution from wandering.
func newSessionIn(repo string) (*session, error) {
	client, err := herdrapi.New()
	if err != nil {
		return nil, err
	}

	root, err := gitx.RepoRoot(repo)
	if err != nil {
		return nil, fmt.Errorf("--repo: %w", err)
	}

	resolved := gitx.Resolve(root)
	return &session{client: client, root: resolved, dir: resolved}, nil
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

func lsCommand(out io.Writer) error {
	// Read-only: usable from a plain shell.
	s, err := newSession(true)
	if err != nil {
		return err
	}
	return wt.Ls(s.client, s.root, s.dir, out)
}

// syncCommand adopts unopened worktrees and staffs agentless workspaces. It
// never prunes: removals are the prune commands' job.
func syncCommand(out io.Writer) error {
	// Opens workspaces and starts agents, so it must be told where.
	s, err := newSession(false)
	if err != nil {
		return err
	}

	// Serialise against an event hook reconciling the same repository. The
	// executor re-checks its guards regardless, so this is about not doing the
	// work twice rather than about safety.
	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, out)

	return lock.Repeat(reconcilePasses, func() error {
		actions, err := s.plan()
		if err != nil {
			return err
		}
		return s.perform(out, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
	})
}

// pruneName and pruneUsage keep the two halves' errors saying which half they
// came from. `prune` and `prune-apply` are one function and separate commands,
// and an error naming the wrong one sends you to the wrong place.
func pruneName(apply bool) string {
	if apply {
		return "prune-apply"
	}
	return "prune"
}

func pruneUsage(apply bool) string {
	return "usage: worktender " + pruneName(apply) + " [--repo <path>]"
}

// pruneCommand reports finished worktrees, and removes them only when apply is
// set. It deliberately excludes adoptions and staffing: asking to prune must
// not open workspaces or start agents as a side effect.
func pruneCommand(args []string, out io.Writer, apply bool) error {
	fs := flag.NewFlagSet(pruneName(apply), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "repository to act on, instead of the one herdr is currently in")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, pruneUsage(apply))
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), pruneUsage(apply))
	}

	// Named repository wins outright. Listing is otherwise read-only and may
	// fall back to the working directory; applying removes worktrees and may not.
	var s *session
	var err error
	if *repo != "" {
		s, err = newSessionIn(*repo)
	} else {
		s, err = newSession(!apply)
	}
	if err != nil {
		return err
	}

	// BOTH HALVES NAME THE REPOSITORY THEY RESOLVED, because they do not
	// resolve it the same way and must not disagree in silence.
	//
	// The asymmetry above is deliberate and stays: listing may fall back to the
	// working directory, applying may not, because herdr runs plugin commands
	// with cwd set to the plugin root — itself a git repository — so a removal
	// that fell back there would point at this plugin's own checkout.
	//
	// What that asymmetry cost was legibility. `prune` exists to be the thing
	// you read before running `prune-apply`; splitting them into two actions IS
	// the confirmation step, and that only holds if the second acts on what the
	// first described. Where herdr supplies a context carrying no repository,
	// the two can land on different roots — observed live, as a dry run listing
	// six worktrees followed by an apply reporting "nothing to do".
	//
	// Printing the root does not prevent the divergence. It makes it impossible
	// to have without seeing it, which is the property that actually matters:
	// "nothing to do" is indistinguishable from "nothing to do HERE" until the
	// output says where here is.
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

	// Deliberately a single pass, not Repeat: re-running a REMOVAL because more
	// work was marked would be acting on a trigger someone else observed. The
	// mark is left for the next reconcile, which is the adopt/staff path.
	actions, err := s.plan()
	if err != nil {
		return err
	}
	return s.perform(out, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), true)
}

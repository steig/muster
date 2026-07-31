// Command herdr-wt drives git worktrees as herdr workspaces.
//
// herdr runs it as a plugin: each subcommand below is registered as an action
// in herdr-plugin.toml and invoked by herdr, which supplies HERDR_SOCKET_PATH
// and the launch context in the environment.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steig/herdr-wt/internal/execute"
	"github.com/steig/herdr-wt/internal/gitx"
	"github.com/steig/herdr-wt/internal/herdrapi"
	"github.com/steig/herdr-wt/internal/reconcile"
	"github.com/steig/herdr-wt/internal/repolock"
	"github.com/steig/herdr-wt/internal/wt"
)

const usage = "usage: herdr-wt <ls|sync|prune|prune-apply|report|on-event|startup>"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "wt:", err)
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
	case "sync":
		// Adopt and staff. Both are non-destructive, so they act directly;
		// finished worktrees are only listed.
		return syncCommand(out)
	case "prune":
		// Lists finished worktrees and removes nothing.
		return pruneCommand(out, false)
	case "prune-apply":
		// The explicit opt-in to actually removing them.
		return pruneCommand(out, true)
	case "report":
		// A worker filling slots for its coordinator. Touches neither herdr nor
		// the repository, so it takes no session and needs no lock.
		return reportCommand(args[1:], out)
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
		fmt.Fprintln(out, "\nrun the `wt: prune (apply)` action to remove the worktrees listed above")
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
		return fmt.Errorf("another wt reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer lock.Release()

	return lock.Repeat(reconcilePasses, func() error {
		actions, err := s.plan()
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
		return fmt.Errorf("another wt reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer lock.Release()

	// Deliberately a single pass, not Repeat: re-running a REMOVAL because more
	// work was marked would be acting on a trigger someone else observed. The
	// mark is left for the next reconcile, which is the adopt/staff path.
	actions, err := s.plan()
	if err != nil {
		return err
	}
	return s.perform(out, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), true)
}

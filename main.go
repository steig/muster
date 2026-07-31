// Command herdr-wt drives git worktrees as herdr workspaces.
//
// herdr runs it as a plugin: each subcommand below is registered as an action
// in herdr-plugin.toml and invoked by herdr, which supplies HERDR_SOCKET_PATH
// and the launch context in the environment.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steig/herdr-wt/internal/execute"
	"github.com/steig/herdr-wt/internal/gitx"
	"github.com/steig/herdr-wt/internal/herdrapi"
	"github.com/steig/herdr-wt/internal/reconcile"
	"github.com/steig/herdr-wt/internal/wt"
)

const usage = "usage: herdr-wt <ls|sync|prune|prune-apply>"

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
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}

// session is what every command needs: a herdr connection, the repository being
// worked on, and the directory the user invoked from.
type session struct {
	client *herdrapi.Client
	root   string
	dir    string
}

func newSession() (*session, error) {
	client, err := herdrapi.New()
	if err != nil {
		return nil, err
	}

	ctx := herdrapi.LoadContext()
	dir, err := launchDir(ctx)
	if err != nil {
		return nil, err
	}

	// herdr hands us the repository root when it already knows the workspace
	// is a worktree; otherwise ask git.
	root := ctx.RepoRoot()
	if root == "" {
		if root, err = gitx.RepoRoot(dir); err != nil {
			return nil, err
		}
	}
	return &session{client: client, root: root, dir: dir}, nil
}

// launchDir is the directory the user invoked from.
//
// herdr runs plugin commands with cwd set to the plugin root, so the user's
// directory has to come from the invocation context. Falling back to the
// process cwd keeps the commands usable straight from a shell.
func launchDir(ctx herdrapi.PluginInvocationContext) (string, error) {
	if dir := ctx.LaunchDir(); dir != "" {
		return dir, nil
	}
	return os.Getwd()
}

// plan collects the current state and decides what the repository needs.
func (s *session) plan() ([]reconcile.Action, error) {
	state, err := reconcile.NewCollector(s.client, s.root).Collect()
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
	s, err := newSession()
	if err != nil {
		return err
	}
	return wt.Ls(s.client, s.root, s.dir, out)
}

// syncCommand adopts unopened worktrees and staffs agentless workspaces. It
// never prunes: removals are the prune commands' job.
func syncCommand(out io.Writer) error {
	s, err := newSession()
	if err != nil {
		return err
	}

	actions, err := s.plan()
	if err != nil {
		return err
	}
	return s.perform(out, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
}

// pruneCommand reports finished worktrees, and removes them only when apply is
// set. It deliberately excludes adoptions and staffing: asking to prune must
// not open workspaces or start agents as a side effect.
func pruneCommand(out io.Writer, apply bool) error {
	s, err := newSession()
	if err != nil {
		return err
	}

	actions, err := s.plan()
	if err != nil {
		return err
	}
	return s.perform(out, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), apply)
}

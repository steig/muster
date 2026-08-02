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
	"github.com/steig/worktender/internal/jsonout"
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
		return doctorCommand(args[1:], out)
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
		return syncCommand(args[1:], out)
	case "dispatch":
		// Staffs one named pane; changes no worktree, so it takes no lock.
		return dispatchCommand(args[1:], out)
	case "prune":
		// Lists finished worktrees and removes nothing.
		return pruneCommand(args[1:], out, false)
	case "prune-apply":
		// The explicit opt-in to actually removing them.
		return pruneCommand(args[1:], out, true)
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

// output is where a command's answer goes, and in which of the two shapes.
//
// The JSON is a projection of exactly the results the table renders, written
// once at the end rather than per pass: a consumer parses a single document
// from stdout, and a reconcile runs its body up to reconcilePasses times.
type output struct {
	w    io.Writer
	json bool
	// held is the JSON mode's accumulator across those passes. Text mode holds
	// nothing, because each pass has already printed.
	held []execute.Result
}

func newOutput(w io.Writer, asJSON bool) *output { return &output{w: w, json: asJSON} }

// notes is where a human aside goes — a lock that would not release, the hint
// pointing at prune-apply. Never stdout in JSON mode: an aside printed beside
// the document is exactly what breaks the consumer reading it.
func (o *output) notes() io.Writer {
	if o.json {
		return os.Stderr
	}
	return o.w
}

// record takes one pass's results.
func (o *output) record(results []execute.Result) {
	if o.json {
		o.held = append(o.held, results...)
		return
	}

	fmt.Fprint(o.w, execute.Render(results))
	if execute.Counts(results)[execute.StatusPlanned] > 0 {
		fmt.Fprintln(o.w, "\nrun the `Worktender: prune (apply)` action to remove the worktrees listed above")
	}
}

// reconcileJSON is what `sync`, `prune` and `prune-apply` write for a machine.
//
// The repository is in the document because text mode prints it as a line above
// the table — a line that must not appear on a stdout being parsed, and a fact
// no consumer should have to re-derive to learn what was acted on.
//
// The shape may move before 1.0.
type reconcileJSON struct {
	Repository string               `json:"repository"`
	Results    []execute.ResultJSON `json:"results"`
}

// flush writes the JSON document, and does nothing in text mode.
func (o *output) flush(repository string) error {
	if !o.json {
		return nil
	}
	return jsonout.Write(o.w, reconcileJSON{
		Repository: repository,
		Results:    execute.JSON(o.held),
	})
}

// perform executes actions, records the report, and fails when any action did.
func (s *session) perform(o *output, actions []reconcile.Action, applyPrune bool) error {
	executor := &execute.Executor{
		Client:     s.client,
		Root:       s.root,
		CallerDir:  s.dir,
		ApplyPrune: applyPrune,
	}
	results := executor.Run(actions)

	o.record(results)

	if failed := execute.Counts(results)[execute.StatusFailed]; failed > 0 {
		return fmt.Errorf("%d of %d action(s) failed", failed, len(results))
	}
	return nil
}

// jsonFlag registers --json on a flag set. One description, so the commands
// cannot advertise the switch differently.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "write a machine-readable document instead of the table")
}

const lsUsage = "usage: worktender ls [--pr] [--json]"

func lsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	withPR := fs.Bool("pr", false, "ask gh for each branch's pull request state")
	asJSON := jsonFlag(fs)

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
	var lookupPR func(string) (string, error)
	if *withPR {
		lookupPR = func(branch string) (string, error) {
			state, err := reconcile.GhPRLookup(s.root, branch)
			return string(state), err
		}
	}
	return wt.Ls(s.client, s.root, s.dir, lookupPR, *asJSON, out)
}

const syncUsage = "usage: worktender sync [--json]"

// syncCommand adopts unopened worktrees and staffs agentless workspaces. It
// never prunes: removals are the prune commands' job.
func syncCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, syncUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), syncUsage)
	}

	// Opens workspaces and starts agents, so it must be told where.
	s, err := newSession(false)
	if err != nil {
		return err
	}
	o := newOutput(out, *asJSON)

	// Named for the reason `prune` names it: sync resolves the repository from
	// herdr's invocation context first, so it can act somewhere other than where
	// the caller believes they are standing. The JSON carries it as a field.
	if !o.json {
		fmt.Fprintf(o.w, "repository: %s\n", s.root)
	}

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
	defer releaseLock(lock, o.notes())

	collector := reconcile.NewCollector(s.client, s.root)
	// No gh: PR state only ever authorises a prune, and prunes are filtered out
	// below. Every lookup is a network round trip per worktree, deciding nothing.
	collector.LookupPR = nil

	err = lock.Repeat(reconcilePasses, func() error {
		actions, err := s.planWith(collector)
		if err != nil {
			return err
		}
		return s.perform(o, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
	})
	// Written even when a pass failed: text mode has already printed what the
	// earlier passes did, and the document must say the same.
	return firstError(err, o.flush(s.root))
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
	return "usage: worktender " + pruneName(apply) + " [--repo <path>] [--json]"
}

// pruneCommand reports finished worktrees, and removes them only when apply is
// set. It deliberately excludes adoptions and staffing: asking to prune must
// not open workspaces or start agents as a side effect.
func pruneCommand(args []string, out io.Writer, apply bool) error {
	fs := flag.NewFlagSet(pruneName(apply), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "repository to act on, instead of the one herdr is currently in")
	asJSON := jsonFlag(fs)

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
	o := newOutput(out, *asJSON)

	// Both halves name the repository they resolved, because they do not resolve
	// it the same way — listing may fall back to the working directory, applying
	// may not — and must not disagree in silence. Splitting prune from
	// prune-apply is the confirmation step, and that only holds if the second
	// acts on what the first described. The JSON carries it as a field.
	if !o.json {
		fmt.Fprintf(o.w, "repository: %s\n", s.root)
	}

	// Listing changes nothing, so it needs no claim on the repository; only the
	// half that removes worktrees serialises against a concurrent reconcile.
	if !apply {
		actions, err := s.plan()
		if err != nil {
			return err
		}
		err = s.perform(o, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), false)
		return firstError(err, o.flush(s.root))
	}

	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, o.notes())

	// A single pass, not Repeat: re-running a removal because more work was
	// marked would act on a trigger someone else observed. The mark is left for
	// the next reconcile.
	actions, err := s.plan()
	if err != nil {
		return err
	}
	err = s.perform(o, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep), true)
	return firstError(err, o.flush(s.root))
}

// firstError prefers the failure the command is about over the one writing it
// out. A document that could not be written is worth reporting, but not instead
// of the actions that failed.
func firstError(err, flushErr error) error {
	if err != nil {
		return err
	}
	return flushErr
}

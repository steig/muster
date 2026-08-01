package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/reconcile"
)

// dispatch starts an agent in a named pane with configuration `sync` never
// supplies: which model to spend, and how much autonomy to grant.
//
// WHY THIS IS A SEPARATE COMMAND AND NOT A FLAG ON SYNC. `sync` is the
// janitor's reflex. It runs from a keybinding and from event hooks, where
// nothing knows what the work is, so there is no role to route on. Giving an
// unattended reconciler an opinion about cost or autonomy is how a hook that
// fires on every new worktree starts spending the expensive model, or grants
// bypassPermissions to an agent nobody asked for. `sync` stays dumb; a
// deliberate dispatch routes.
//
// It is also not a herdr action, for the reason `report` and `gate` are not: an
// action is a fixed command array with no argument surface, so a registered
// dispatch could only ever start one hard-coded configuration.
//
// THE PANE RE-CHECK IS NOT DUPLICATED HERE, AND THAT IS THE POINT. #26 asked
// for the rule that "any second staffing path must re-check the pane too" to be
// written down rather than left implied. Writing it down is weaker than not
// having a second path: this builds a KindStaff action and hands it to the same
// executor `sync` uses, so the re-check in execute.staff() covers both by
// construction. agent.start against an occupied pane lands on a live
// conversation and destroys context that exists nowhere else, and a rule in a
// comment does not survive the next person adding a third caller.
func dispatchCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	pane := fs.String("pane", "", "pane to start the agent in")
	name := fs.String("name", "", "herdr agent name")
	model := fs.String("model", "", "model to pass to the agent")
	permissionMode := fs.String("permission-mode", "", "agent permission mode")
	resume := fs.Bool("resume", false, "continue the pane's existing transcript")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, dispatchUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), dispatchUsage)
	}
	if *pane == "" {
		return fmt.Errorf("--pane is required; %s", dispatchUsage)
	}
	if *name == "" {
		return fmt.Errorf("--name is required; %s", dispatchUsage)
	}

	agentArgs := agentArgsFor(*model, *permissionMode, os.Stderr)

	// Dispatch reads herdr and starts an agent; it changes no worktree and
	// removes nothing, so it needs no repository and takes no lock.
	s, err := newSession(true)
	if err != nil {
		return err
	}

	workspace, err := workspaceForPane(s, *pane)
	if err != nil {
		return err
	}

	executor := &execute.Executor{Client: s.client, Root: s.root, CallerDir: s.dir}
	results := executor.Run([]reconcile.Action{{
		Kind:        reconcile.KindStaff,
		WorkspaceID: workspace,
		PaneID:      *pane,
		AgentName:   *name,
		Resume:      *resume,
		AgentArgs:   agentArgs,
	}})

	fmt.Fprint(out, execute.Render(results))
	if execute.Counts(results)[execute.StatusFailed] > 0 {
		return fmt.Errorf("dispatch to %s failed", *pane)
	}
	return nil
}

// agentArgsFor turns dispatch's flags into agent arguments.
//
// WHAT IS DELIBERATELY MISSING: a sandbox profile. #26 called it "the actual
// answer" and it is not reachable from here — `claude` has no sandbox flag.
// Sandboxing is configured in settings.json, and this plugin does not write to
// your agent's configuration, during install or ever. That principle is not
// negotiable for autonomy in particular: a plugin that quietly grants an agent
// a boundary, or removes one, is the same class of surprise as one that starts
// spawning agents on install.
//
// That leaves --permission-mode able to grant autonomy without the boundary
// that was supposed to accompany it, which #26 correctly says is the wrong
// combination. Two things keep that honest, and neither is a refusal. It is
// never defaulted: no permission mode is passed unless the caller names one, so
// nothing here changes what an agent may do until someone asks for it. And a
// mode that disables prompting says so on stderr, naming what was granted and
// what is missing.
//
// A refusal was tried, gated on an environment variable the caller set to
// confirm a sandbox existed. It was removed because it could not tell a
// sandboxed caller from one who had read the variable's name: the confirmation
// was unverifiable, so it bought disclosure at the price of every unattended
// dispatch stalling. Disclosure without the refusal buys the same thing, and a
// worker that cannot start is the failure this command exists to prevent.
func agentArgsFor(model, permissionMode string, warn io.Writer) []string {
	var args []string
	if model != "" {
		args = append(args, "--model", model)
	}
	if permissionMode != "" {
		if unsandboxedModes[permissionMode] {
			fmt.Fprintf(warn, unsandboxedWarning, permissionMode)
		}
		args = append(args, "--permission-mode", permissionMode)
	}
	return args
}

// unsandboxedModes are the permission modes that stop an agent asking before it
// acts. A dispatched worker has no human at its pane, so it stalls on the first
// prompt and stays stalled — and a coordinator structurally cannot clear it,
// which is the real problem #26 set out to solve.
//
// The fix is not to make this easy. An allowlist provably cannot close the gap:
// `$(...)` and `find -exec` are never auto-allowed by a prefix rule because
// they can run anything, so a name-based denylist is decoration wherever the
// action has an API behind it. Demonstrated during this plugin's own
// development: a worker denied `Bash(herdr agent start:*)` reached a live agent
// anyway by calling herdr's socket from Go, logging zero denials.
//
// So the boundary has to be capability-based, and this command cannot install
// one. The most it can do is decline to remove prompting silently, and say what
// is actually being asked for.
var unsandboxedModes = map[string]bool{
	"bypassPermissions": true,
	"acceptEdits":       true,
}

// unsandboxedWarning goes to stderr rather than stdout: an action's stdout is
// read back out of the plugin log and parsed, and a warning that lands there is
// a warning something eventually starts stripping.
const unsandboxedWarning = "worktender: --permission-mode %s stops the agent asking before it acts, " +
	"and worktender cannot sandbox it — `claude` takes no sandbox flag, and this plugin does not " +
	"write your agent's configuration. Give the worker a boundary that does not depend on command " +
	"spelling: a sandbox profile, or a separate uid.\n"

const dispatchUsage = "usage: worktender dispatch --pane <id> --name <agent> " +
	"[--model <model>] [--permission-mode <mode>] [--resume]"

// workspaceForPane asks herdr which workspace holds a pane, so the executor's
// re-check has something to check. A pane herdr does not know is fatal rather
// than dispatched blind.
func workspaceForPane(s *session, pane string) (string, error) {
	info, err := s.client.PaneGet(pane)
	if err != nil {
		return "", fmt.Errorf("ask herdr about pane %s: %w", pane, err)
	}
	if info.Pane.WorkspaceID == "" {
		// Nothing to re-check against means the guard cannot run, and an
		// unverifiable guard is not a satisfied one anywhere else in this
		// plugin either.
		return "", fmt.Errorf("herdr reports no workspace for pane %s; refusing to start an agent there", pane)
	}
	return info.Pane.WorkspaceID, nil
}

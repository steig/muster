// Package execute carries out the actions the reconciler decided on.
//
// reconcile.Reconcile is a pure function over a snapshot, and a snapshot goes
// stale, so every destructive step here re-reads its guards immediately before
// acting. Staffing counts as destructive: starting an agent on a pane that
// already has one lands on a live conversation.
//
// Pruning is a dry run unless ApplyPrune is set. A plugin action has no prompt
// surface, so applying is a separate, explicit action instead.
package execute

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/safetext"
)

// Status is what became of one action.
type Status string

const (
	// StatusDone means the action was carried out.
	StatusDone Status = "done"
	// StatusPlanned means a destructive action was listed but not performed,
	// because pruning is a dry run by default.
	StatusPlanned Status = "planned"
	// StatusSkipped means a guard stopped the action, or it was explanatory.
	StatusSkipped Status = "skipped"
	// StatusFailed means the action was attempted and did not succeed.
	StatusFailed Status = "failed"
)

// Result records the fate of one action. Detail is always populated: a silent
// no-op is the failure mode this package exists to avoid.
type Result struct {
	Action reconcile.Action
	Status Status
	Detail string
}

// AgentStartTimeout bounds herdr's own wait for a pane to become usable — a
// fresh worktree is often still running direnv or nix, and an agent cannot
// start against a busy prompt.
//
// It is exported because it is also the longest a reconcile can legitimately
// hold the repository lock. repolock.MaxHold is sized from it, and a test
// asserts the two cannot drift apart.
const AgentStartTimeout = 60 * time.Second

// agentStartTimeoutMS is the same bound in the units the wire protocol wants.
const agentStartTimeoutMS = int(AgentStartTimeout / time.Millisecond)

// agentKind is the herdr agent to start when staffing a worktree.
const agentKind = "claude"

// Executor performs actions against a live herdr and a real repository.
type Executor struct {
	Client *herdrapi.Client
	// Root is the repository's main checkout.
	Root string
	// CallerDir is the directory the user invoked from, used to refuse
	// removing the ground they are standing on. Empty disables that check.
	CallerDir string
	// ApplyPrune turns prunes from a dry run into actual removals.
	ApplyPrune bool
}

// Run performs every action in order and returns one Result each.
func (e *Executor) Run(actions []reconcile.Action) []Result {
	results := make([]Result, 0, len(actions))
	for _, action := range actions {
		switch action.Kind {
		case reconcile.KindKeep:
			// Explanatory only: it lets the report say why a worktree was
			// spared instead of silently omitting it.
			results = append(results, Result{action, StatusSkipped, action.Reason})
		case reconcile.KindAdopt:
			results = append(results, e.adopt(action))
		case reconcile.KindStaff:
			results = append(results, e.staff(action))
		case reconcile.KindPrune:
			results = append(results, e.prune(action))
		default:
			results = append(results, Result{action, StatusFailed, "unknown action kind"})
		}
	}
	return results
}

// adopt opens a workspace for a worktree. Non-destructive, so it acts directly.
func (e *Executor) adopt(action reconcile.Action) Result {
	label := action.Branch
	if label == "" {
		label = filepath.Base(action.Path)
	}

	// Never focus: adopting a batch must not drag the user through every
	// workspace it opens.
	if err := e.Client.WorktreeOpen(e.Root, action.Path, label, false); err != nil {
		return Result{action, StatusFailed, fmt.Sprintf("open workspace: %v", err)}
	}
	return Result{action, StatusDone, "opened workspace"}
}

// staff starts an agent in a workspace that has none, re-checking first.
//
// The reconciler decided this workspace was empty from a snapshot that has
// since aged, and an agent appearing in the gap is routine — an event hook and
// a human `sync` can plan from the same state moments apart. agent.start
// against an occupied pane lands on a live conversation.
//
// The repository lock makes the collision rare; this re-check is what makes it
// safe. The lock may fail to exclude, so it cannot be the thing standing here.
func (e *Executor) staff(action reconcile.Action) Result {
	if action.WorkspaceID != "" {
		staffed, err := e.workspaceStaffed(action.WorkspaceID)
		switch {
		case err != nil:
			// An unverifiable guard is not a satisfied one.
			return Result{action, StatusSkipped,
				fmt.Sprintf("could not confirm the workspace has no agent: %v", err)}
		case staffed:
			return Result{action, StatusSkipped, "an agent started here since the plan was made"}
		}
	}

	var args []string
	mode := "started"
	if action.Resume {
		// Continue the existing conversation rather than opening a bare one.
		args = []string{"--continue"}
		mode = "resumed"
	}
	// Caller arguments go last so they cannot displace --continue: whether to
	// resume is this executor's decision, not the caller's.
	args = append(args, action.AgentArgs...)

	if err := e.Client.AgentStart(action.AgentName, agentKind, action.PaneID, args, agentStartTimeoutMS); err != nil {
		return Result{action, StatusFailed, fmt.Sprintf("start agent in %s: %v", action.PaneID, err)}
	}
	return Result{action, StatusDone,
		fmt.Sprintf("%s %s as %s in %s", mode, agentKind, action.AgentName, action.PaneID)}
}

// prune removes a finished worktree, re-checking every guard first: uncommitted
// work and a newly started agent are exactly what appears between the plan and
// the removal, and both make it wrong.
func (e *Executor) prune(action reconcile.Action) Result {
	workspaceID, reason, blocked := e.pruneBlocked(action)
	if blocked {
		return Result{action, StatusSkipped, reason}
	}

	if !e.ApplyPrune {
		return Result{action, StatusPlanned,
			fmt.Sprintf("would remove: %s%s", action.Reason, releaseNote(action))}
	}

	if err := e.removeCheckout(action, workspaceID); err != nil {
		return Result{action, StatusFailed, err.Error()}
	}
	return Result{action, StatusDone,
		fmt.Sprintf("removed: %s%s%s", action.Reason, releaseNote(action), e.dropBranch(action.Branch))}
}

// releaseNote says when a removal also takes an agent's pane away. The reason
// covers why the worktree went; this covers what else went with it.
func releaseNote(action reconcile.Action) string {
	if !action.ReleasesAgents {
		return ""
	}
	return ", releasing the finished agent holding it"
}

// pruneBlocked re-reads the guards at execution time and explains any refusal.
// The workspace it returns is the one herdr currently holds the checkout in,
// which the plan may have named wrongly or not at all.
func (e *Executor) pruneBlocked(action reconcile.Action) (workspaceID, reason string, blocked bool) {
	// Guard a, re-checked: worktree.remove is called with force, which bypasses
	// git's own refusal to delete a dirty checkout.
	if gitx.IsDirty(action.Path) {
		return "", "uncommitted changes appeared since the plan was made", true
	}

	// Guard b, re-checked: an agent may have started in the gap.
	//
	// An empty workspace id is not the same fact as "no workspace holds this
	// checkout" — a path join that missed produces exactly this, an action that
	// looks standalone and is really an agent's ground. So herdr is asked again
	// and only its answer decides.
	workspaceID = action.WorkspaceID
	if workspaceID == "" {
		holder, err := e.workspaceHolding(action.Path)
		if err != nil {
			// An unverifiable guard is not a satisfied one.
			return "", fmt.Sprintf("could not confirm no workspace holds %s: %v", action.Path, err), true
		}
		workspaceID = holder
	}
	if workspaceID != "" {
		occupied, busy, err := e.workspaceOccupant(workspaceID)
		switch {
		case err != nil:
			return "", fmt.Sprintf("could not confirm the workspace is idle: %v", err), true
		case occupied && !action.ReleasesAgents:
			return "", "an agent started here since the plan was made", true
		case occupied && busy:
			// The plan was made against an agent that had finished. It has
			// picked work up since, and --release-agents does not reach an
			// agent that is doing something.
			return "", "the agent here started working since the plan was made", true
		}
	}

	// Removing the directory the caller is standing in would leave their shell
	// on a dead cwd, and a plugin subprocess cannot `cd` on their behalf.
	if e.CallerDir != "" && isInside(e.CallerDir, action.Path) {
		return "", fmt.Sprintf("you are in %s — cd out of it first", action.Path), true
	}

	return workspaceID, "", false
}

// workspaceHolding returns the id of the workspace herdr currently has the
// checkout open in, empty when there is none. Paths are compared normalised, or
// re-asking herdr buys nothing.
func (e *Executor) workspaceHolding(checkout string) (string, error) {
	workspaces, err := e.Client.WorkspaceList()
	if err != nil {
		return "", err
	}

	wanted := gitx.Resolve(checkout)
	for _, ws := range workspaces.Workspaces {
		if ws.Worktree != nil && gitx.Resolve(ws.Worktree.CheckoutPath) == wanted {
			return ws.WorkspaceID, nil
		}
	}
	return "", nil
}

// workspaceStaffed reports whether any pane of the workspace hosts an agent,
// whatever it is doing.
func (e *Executor) workspaceStaffed(workspaceID string) (bool, error) {
	staffed, _, err := e.workspaceOccupant(workspaceID)
	return staffed, err
}

// workspaceOccupant reports whether any pane of the workspace hosts an agent
// and whether any of those is busy, reading herdr fresh rather than trusting
// the plan. Busy-ness is decided by the reconciler's own rule, so the guard
// that plans a release and the guard that performs it cannot disagree.
func (e *Executor) workspaceOccupant(workspaceID string) (occupied, busy bool, err error) {
	agents, err := e.Client.AgentList()
	if err != nil {
		return false, false, err
	}
	panes, err := e.Client.PaneList(workspaceID)
	if err != nil {
		return false, false, err
	}

	hosting := make(map[string]reconcile.AgentState, len(agents.Agents))
	for _, a := range agents.Agents {
		hosting[a.PaneID] = reconcile.AgentState(a.AgentStatus)
	}
	for _, p := range panes.Panes {
		status, ok := hosting[p.PaneID]
		if !ok {
			continue
		}
		occupied = true
		if !status.Finished() {
			busy = true
		}
	}
	return occupied, busy, nil
}

// removeCheckout deletes the worktree, preferring herdr so the workspace is
// torn down with it. A worktree herdr has no workspace for is removed with git.
//
// workspaceID comes from the guards rather than the action, because that is the
// one they verified — otherwise herdr is left holding a workspace over a
// checkout that no longer exists.
func (e *Executor) removeCheckout(action reconcile.Action, workspaceID string) error {
	if workspaceID != "" {
		if err := e.Client.WorktreeRemove(workspaceID, true); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
		return nil
	}

	cmd := exec.Command("git", "worktree", "remove", "--force", action.Path)
	cmd.Dir = e.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dropBranch deletes the local branch when git agrees it is fully merged. It
// never forces: an unmerged branch here means the work exists nowhere else.
// The returned string is a suffix for the result detail.
func (e *Executor) dropBranch(branch string) string {
	if branch == "" {
		return ""
	}

	cmd := exec.Command("git", "branch", "-d", branch)
	cmd.Dir = e.Root
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("; kept branch %s (git will not delete it — `git branch -D %s` to force)", branch, branch)
	}

	detail := fmt.Sprintf("; deleted branch %s", branch)
	// The remote counterpart outlives the local one, and deleting a remote ref
	// is not something to do silently — an open PR may still point at it.
	if e.remoteHasBranch(branch) {
		detail += fmt.Sprintf("; origin/%s still exists — `git push origin --delete %s` when the PR is closed", branch, branch)
	}
	return detail
}

func (e *Executor) remoteHasBranch(branch string) bool {
	cmd := exec.Command("git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	cmd.Dir = e.Root
	return cmd.Run() == nil
}

// isInside reports whether dir is root or sits beneath it. Both sides are
// resolved first: an unresolved symlink on either compares unequal and would
// silently disarm the guard.
func isInside(dir, root string) bool {
	rel, err := filepath.Rel(gitx.Resolve(root), gitx.Resolve(dir))
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// Render writes a human-readable report, one aligned line per action. The
// worktree is its own column because several commonly share a reason.
//
// Target and detail are escaped: both carry branch names, git allows a bidi
// override in one, and this report is the confirmation a human reads before
// applying a prune. The detail also carries git's stderr, so escaping keeps the
// promise of one line per action.
func Render(results []Result) string {
	if len(results) == 0 {
		return "nothing to do\n"
	}

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Status, r.Action.Kind,
			safetext.Escape(target(r.Action)), safetext.Escape(r.Detail))
	}
	_ = tw.Flush()
	return b.String()
}

// ResultJSON is Result as a consumer reads it: the action's own fields beside
// the verdict, rather than the one "target" cell a table has room for.
//
// Values are raw, not terminal-escaped, for the reason wt.RowJSON's are — this
// is data, and an escaped path is no longer a path. Detail carries git's stderr
// verbatim and so can be several lines; the table flattens it, this does not.
//
// The shape may move before 1.0.
type ResultJSON struct {
	Status string `json:"status"`
	Kind   string `json:"kind"`
	// Target is the same cell the table prints, kept so a consumer showing a
	// one-line summary does not have to re-derive it.
	Target      string  `json:"target"`
	Detail      string  `json:"detail"`
	Branch      *string `json:"branch"`
	Path        *string `json:"path"`
	WorkspaceID *string `json:"workspace_id"`
	PaneID      *string `json:"pane_id"`
	AgentName   *string `json:"agent_name"`
	// Reason is why the reconciler planned this action, which is not always
	// what became of it — Detail is that.
	Reason *string `json:"reason"`
	// ReleasesAgents is set on a prune that takes a finished agent's pane away
	// with the worktree. A consumer that tracks its own workers wants to know
	// which removals ended one.
	ReleasesAgents bool `json:"releases_agents"`
}

// JSON projects results for a machine, from exactly the []Result the table
// renders.
func JSON(results []Result) []ResultJSON {
	out := make([]ResultJSON, 0, len(results))
	for _, r := range results {
		out = append(out, ResultJSON{
			Status:         string(r.Status),
			Kind:           string(r.Action.Kind),
			Target:         target(r.Action),
			Detail:         r.Detail,
			Branch:         jsonout.String(r.Action.Branch),
			Path:           jsonout.String(r.Action.Path),
			WorkspaceID:    jsonout.String(r.Action.WorkspaceID),
			PaneID:         jsonout.String(r.Action.PaneID),
			AgentName:      jsonout.String(r.Action.AgentName),
			Reason:         jsonout.String(r.Action.Reason),
			ReleasesAgents: r.Action.ReleasesAgents,
		})
	}
	return out
}

// target names the worktree an action is about, preferring the branch.
func target(action reconcile.Action) string {
	if action.Branch != "" {
		return action.Branch
	}
	if action.Path != "" {
		return filepath.Base(action.Path)
	}
	return "-"
}

// Counts summarises results by status, for a one-line tail on the report.
func Counts(results []Result) map[Status]int {
	counts := map[Status]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	return counts
}

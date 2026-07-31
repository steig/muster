// Package reconcile decides what a repository's worktrees need, as a pure
// function over already-collected state.
//
// Nothing here runs git, herdr or gh: Reconcile takes facts and returns
// intentions. Collecting the facts is Collect's job and carrying out the
// intentions is the executor's, which is what makes the interesting logic —
// especially the prune guards — testable without a live herdr.
//
// The one filesystem read is path normalisation. Worktrees and workspaces are
// joined by checkout path, herdr does not promise one spelling of a directory,
// and a guard that misses because two paths name the same place differently is
// a guard that does not run at all.
//
// Reconcile is idempotent and converges over passes. A worktree adopted in this
// pass has no workspace yet, so it cannot also be staffed until the next one.
package reconcile

import (
	"fmt"

	"github.com/steig/worktender/internal/gitx"
)

// PRState is a pull request's state as gh reports it.
type PRState string

const (
	PRNone   PRState = ""
	PROpen   PRState = "OPEN"
	PRMerged PRState = "MERGED"
	PRClosed PRState = "CLOSED"
)

// Worktree is one checkout on disk, with the git facts already resolved.
type Worktree struct {
	Path   string
	Branch string
	// IsLinked distinguishes a linked worktree from the repository's main
	// checkout. Only linked worktrees are staffed or pruned.
	IsLinked bool
	IsBare   bool
	// WorkspaceID is the herdr workspace holding this checkout open, empty
	// when herdr has none.
	WorkspaceID string
	// Dirty reports uncommitted changes, including untracked files.
	Dirty bool
	// OwnCommits is the number of commits base does not already have.
	OwnCommits int
	// MergedIntoBase reports that base absorbed the branch through a merge
	// commit. It is ADVISORY ONLY — see verdict for why topology never
	// removes anything — and is used to explain a keep, not to justify a
	// prune.
	MergedIntoBase bool
	// HasTranscript reports a prior Claude conversation for this checkout,
	// which makes the difference between resuming and starting cold.
	HasTranscript bool
	// PR is the pull request state for the branch, PRNone when gh had no
	// answer or is unavailable.
	PR PRState
}

// Workspace is a herdr workspace belonging to the repository being reconciled.
type Workspace struct {
	ID           string
	CheckoutPath string
	IsLinked     bool
	// PaneIDs is every pane in the workspace, in herdr's order.
	PaneIDs []string
}

// State is the whole input to Reconcile.
type State struct {
	// Base is the ref new work forks from, e.g. "origin/main".
	Base string
	// Worktrees is every worktree herdr knows for this repository.
	Worktrees []Worktree
	// Workspaces is every herdr workspace for this repository.
	Workspaces []Workspace
	// AgentPanes holds the id of every pane currently hosting an agent,
	// across all workspaces.
	AgentPanes map[string]bool
}

// Kind is what an Action asks the executor to do.
type Kind string

const (
	// KindAdopt opens a herdr workspace for a worktree that has none.
	KindAdopt Kind = "adopt"
	// KindStaff starts an agent in a workspace that has none.
	KindStaff Kind = "staff"
	// KindPrune removes a finished worktree.
	KindPrune Kind = "prune"
	// KindKeep is explanatory and is never executed. It records why a
	// worktree that looked prunable was spared, so the UI can say "keep X —
	// agent running" instead of silently omitting it.
	KindKeep Kind = "keep"
)

// Action is one decision. Reason is human-facing and always populated.
type Action struct {
	Kind        Kind
	Path        string
	Branch      string
	WorkspaceID string
	// PaneID is the pane to start an agent in, set on KindStaff.
	PaneID string
	// AgentName is the herdr agent name to use, set on KindStaff.
	AgentName string
	// Resume asks the executor to continue the existing transcript rather
	// than start a fresh conversation. Set on KindStaff.
	Resume bool
	Reason string
}

// Reconcile returns everything the repository needs, in execution order:
// adoptions, then staffing, then prunes.
func Reconcile(state State) []Action {
	var actions []Action
	actions = append(actions, adopt(state)...)
	actions = append(actions, staff(state)...)
	actions = append(actions, prune(state)...)
	return actions
}

// Only keeps the actions of the given kinds, preserving order. Commands use it
// to act on part of a plan: `prune` must not quietly adopt and staff on the way
// past.
func Only(actions []Action, kinds ...Kind) []Action {
	wanted := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		wanted[k] = true
	}

	var out []Action
	for _, a := range actions {
		if wanted[a.Kind] {
			out = append(out, a)
		}
	}
	return out
}

// adopt opens a workspace for every worktree herdr is not already holding open.
// A bare worktree has no working tree to put in a workspace.
func adopt(state State) []Action {
	var actions []Action
	for _, w := range state.Worktrees {
		if w.IsBare || w.WorkspaceID != "" {
			continue
		}
		actions = append(actions, Action{
			Kind:   KindAdopt,
			Path:   w.Path,
			Branch: w.Branch,
			Reason: "no herdr workspace",
		})
	}
	return actions
}

// staff starts an agent in every linked worktree's workspace that has none.
//
// This covers more than freshly adopted worktrees: a workspace can exist and
// still sit there as a bare shell, which is what happens when an agent start
// loses the race with direnv. Those are exactly the ones worth rescuing.
func staff(state State) []Action {
	byPath := worktreesByPath(state)

	var actions []Action
	for _, ws := range state.Workspaces {
		if !ws.IsLinked || len(ws.PaneIDs) == 0 || hasAgent(state, ws) {
			continue
		}

		// Resume onto an existing transcript when there is one; otherwise a
		// cold start, because an unstaffed worktree is doing no work either
		// way.
		resume := byPath[pathKey(ws.CheckoutPath)].HasTranscript
		reason := "no agent, no prior session"
		if resume {
			reason = "no agent, prior session to resume"
		}

		actions = append(actions, Action{
			Kind:        KindStaff,
			Path:        ws.CheckoutPath,
			Branch:      byPath[pathKey(ws.CheckoutPath)].Branch,
			WorkspaceID: ws.ID,
			PaneID:      ws.PaneIDs[0],
			AgentName:   AgentName(Slug(baseName(ws.CheckoutPath))),
			Resume:      resume,
			Reason:      reason,
		})
	}
	return actions
}

// prune decides which finished worktrees to remove.
//
// The guards run before any finished-ness test and in this order, because each
// one describes work that must survive regardless of how the branch looks.
func prune(state State) []Action {
	byPath := workspacesByPath(state)

	var actions []Action
	for _, w := range state.Worktrees {
		// The main checkout is the repository; it is never a candidate.
		if !w.IsLinked {
			continue
		}

		keep := func(reason string) {
			actions = append(actions, Action{
				Kind: KindKeep, Path: w.Path, Branch: w.Branch,
				WorkspaceID: w.WorkspaceID, Reason: reason,
			})
		}

		// Guard a: uncommitted work exists nowhere else.
		if w.Dirty {
			keep("uncommitted changes")
			continue
		}

		// Guard b: an agent is mid-task here. Never delete the ground it
		// stands on, whatever the branch looks like.
		if ws, ok := byPath[pathKey(w.Path)]; ok && hasAgent(state, ws) {
			keep("agent running")
			continue
		}

		if landed, reason := verdict(w, state.Base); landed {
			actions = append(actions, Action{
				Kind: KindPrune, Path: w.Path, Branch: w.Branch,
				WorkspaceID: w.WorkspaceID, Reason: reason,
			})
		} else {
			keep(reason)
		}
	}
	return actions
}

// verdict decides whether a branch's work has landed, and always explains
// itself.
//
// The governing principle: "has this work landed" is NOT decidable from git
// topology. Across fast-forward, squash, rebase and merge-commit workflows the
// graph shapes overlap, and every attempt to separate them with one more
// topological test has produced another case that test gets wrong:
//
//   - A fast-forward merge moves base onto the branch tip, so the tip is a
//     first-parent trunk commit — identical to a branch that never committed.
//   - A branch forked off already-merged work inherits a tip that sits off
//     trunk, so it looks merged while having done nothing. It is literally the
//     same commit as the branch that did land; no graph query separates them.
//   - Squash and rebase rewrite commits entirely, so a fully landed branch is
//     not an ancestor of base at all.
//
// So topology is not used to remove anything. PR state is authoritative
// wherever it exists; where it does not, ambiguity resolves to keeping, and the
// ambiguity is stated rather than dressed up as a verdict. An un-pruned
// worktree costs disk. A wrongly pruned one costs work that exists nowhere
// else. Those are not comparable, so the tie never goes to deletion.
func verdict(w Worktree, base string) (landed bool, reason string) {
	switch w.PR {
	case PRMerged:
		// The only authoritative "yes" available.
		return true, "PR merged"
	case PRClosed:
		// Closed without merging is abandoned work, not finished work: the
		// branch still holds commits that exist nowhere else. Surface it and
		// let a human decide.
		return false, "PR closed without merging — abandoned; remove it by hand if you are sure"
	case PROpen:
		return false, "still open"
	}

	// No PR to appeal to. Everything below is a shape git cannot disambiguate.
	if w.MergedIntoBase {
		return false, fmt.Sprintf(
			"looks merged into %s, but cannot tell that from a branch forked off merged work — keeping", base)
	}
	if w.OwnCommits == 0 {
		return false, "no commits of its own — cannot tell unstarted from fast-forward merged — keeping"
	}
	return false, "still open"
}

// hasAgent reports whether any pane in the workspace hosts an agent.
func hasAgent(state State, ws Workspace) bool {
	for _, pane := range ws.PaneIDs {
		if state.AgentPanes[pane] {
			return true
		}
	}
	return false
}

// pathKey is how a checkout is identified when joining worktrees to
// workspaces.
//
// Raw string equality is not enough, and this is guard b's whole failure mode.
// herdr answers worktree.list and workspace.list from different sources, so one
// checkout can arrive resolved in one and symlinked in the other — on macOS
// that is the ordinary case, since anything under /var is really /private/var.
// A join that misses does not fail loudly: the worktree simply appears to have
// no workspace, the agent check has nothing to look at, and a checkout with a
// live agent in it is planned for removal.
func pathKey(path string) string { return gitx.Resolve(path) }

func worktreesByPath(state State) map[string]Worktree {
	index := make(map[string]Worktree, len(state.Worktrees))
	for _, w := range state.Worktrees {
		index[pathKey(w.Path)] = w
	}
	return index
}

func workspacesByPath(state State) map[string]Workspace {
	index := make(map[string]Workspace, len(state.Workspaces))
	for _, ws := range state.Workspaces {
		index[pathKey(ws.CheckoutPath)] = ws
	}
	return index
}

// Package reconcile decides what a repository's worktrees need, as a pure
// function over already-collected state.
//
// Nothing here runs git, herdr or gh: Reconcile takes facts and returns
// intentions, which is what makes the prune guards testable without a live
// herdr. The one filesystem read is path normalisation, because herdr does not
// promise one spelling of a directory and a join that misses disarms a guard.
//
// Reconcile is idempotent and converges over passes: a worktree adopted in this
// pass has no workspace yet, so it cannot be staffed until the next one.
package reconcile

import (
	"fmt"
	"path/filepath"

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
	// commit. Advisory only: it explains a keep, never justifies a prune.
	MergedIntoBase bool
	// UpstreamGone reports that the branch was published and its remote
	// counterpart has since been deleted. It records a human action rather
	// than a graph shape, which is why verdict may act on it.
	UpstreamGone bool
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
	// KindKeep is explanatory and never executed: it records why a worktree
	// that looked prunable was spared, rather than silently omitting it.
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
	// AgentArgs are extra arguments passed through to the agent binary, after
	// any the executor adds itself. Always empty on an action the reconciler
	// produced: only a deliberate `dispatch` has a role to route on.
	AgentArgs []string
	Reason    string
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

// Only keeps the actions of the given kinds, preserving order, so `prune` does
// not quietly adopt and staff on the way past.
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
// This covers more than freshly adopted worktrees: a workspace can exist and
// still sit there as a bare shell, which is what an agent start losing the race
// with direnv leaves behind.
func staff(state State) []Action {
	byPath := worktreesByPath(state)

	var actions []Action
	for _, ws := range state.Workspaces {
		if !ws.IsLinked || len(ws.PaneIDs) == 0 || hasAgent(state, ws) {
			continue
		}

		// Resume onto an existing transcript when there is one, otherwise a
		// cold start.
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
			AgentName:   AgentName(Slug(filepath.Base(ws.CheckoutPath))),
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
// "Has this work landed" is not decidable from git topology: a fast-forward
// merge leaves a tip identical to a branch that never committed, a branch
// forked off merged work is literally the same commit as one that landed, and
// squash and rebase rewrite commits so a landed branch is not an ancestor at
// all. So topology removes nothing. PR state is authoritative wherever it
// exists; elsewhere ambiguity resolves to keeping, because an un-pruned
// worktree costs disk and a wrongly pruned one costs work that exists nowhere
// else.
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

	// No PR to appeal to. A deleted upstream is admissible here because it is a
	// human action rather than a graph shape: a branch forked off merged work
	// and never pushed has no upstream to delete, while a branch that landed was
	// pushed and had its remote ref removed.
	//
	// Both halves are required. A gone upstream alone is equally what abandoning
	// work looks like; merged-into-base alone is the ambiguity above. Together
	// they are still not proof, but the case is bounded to a branch with no
	// commits base lacks, so the worst case is deleting a checkout whose commits
	// are all reachable from base anyway. Squash and rebase are not covered.
	if w.MergedIntoBase && w.UpstreamGone {
		return true, fmt.Sprintf("merged into %s and its upstream was deleted", base)
	}
	if w.MergedIntoBase {
		return false, fmt.Sprintf(
			"looks merged into %s, but cannot tell that from a branch forked off merged work — keeping", base)
	}
	if w.UpstreamGone {
		// Published, then deleted, but base does not have the commits. That is
		// the abandonment shape, and it is the one this signal must not act on.
		return false, "upstream deleted, but it still holds commits " + base + " does not — keeping"
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

// pathKey is how a checkout is identified when joining worktrees to workspaces.
//
// Raw string equality is not enough: herdr answers worktree.list and
// workspace.list from different sources, so one checkout can arrive resolved in
// one and symlinked in the other. A join that misses does not fail loudly — the
// worktree appears to have no workspace, and a checkout with a live agent in it
// is planned for removal.
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

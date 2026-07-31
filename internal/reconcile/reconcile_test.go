package reconcile_test

import (
	"strings"
	"testing"

	"github.com/steig/muster/internal/reconcile"
)

// linked builds a linked worktree that would otherwise be prunable, so each
// test can flip exactly the one field it is about.
func linked(path, branch string) reconcile.Worktree {
	return reconcile.Worktree{
		Path: path, Branch: branch, IsLinked: true,
		OwnCommits: 3, PR: reconcile.PRMerged,
	}
}

func find(actions []reconcile.Action, kind reconcile.Kind, path string) *reconcile.Action {
	for i := range actions {
		if actions[i].Kind == kind && actions[i].Path == path {
			return &actions[i]
		}
	}
	return nil
}

func TestAdoptsWorktreeWithNoWorkspace(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/a", Branch: "a", IsLinked: true},
		{Path: "/repo/wt/b", Branch: "b", IsLinked: true, WorkspaceID: "w2"},
		{Path: "/repo/bare", IsBare: true},
	}}

	actions := reconcile.Reconcile(state)

	if find(actions, reconcile.KindAdopt, "/repo/wt/a") == nil {
		t.Error("worktree with no workspace should be adopted")
	}
	if find(actions, reconcile.KindAdopt, "/repo/wt/b") != nil {
		t.Error("worktree that already has a workspace should not be adopted")
	}
	if find(actions, reconcile.KindAdopt, "/repo/bare") != nil {
		t.Error("a bare worktree has no working tree to open")
	}
}

func TestStaffsAgentlessWorkspace(t *testing.T) {
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2"}},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1", "w2:p2"}},
		},
		AgentPanes: map[string]bool{},
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a")
	if action == nil {
		t.Fatal("agentless workspace should be staffed")
	}
	if action.PaneID != "w2:p1" {
		t.Errorf("should staff the first pane, got %q", action.PaneID)
	}
	if action.WorkspaceID != "w2" {
		t.Errorf("WorkspaceID = %q, want w2", action.WorkspaceID)
	}
	if action.AgentName != "a" {
		t.Errorf("AgentName = %q, want a", action.AgentName)
	}
}

func TestDoesNotStaffWorkspaceThatAlreadyHasAnAgent(t *testing.T) {
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2"}},
		Workspaces: []reconcile.Workspace{
			// The agent is in the second pane, so a first-pane-only check
			// would wrongly restaff this workspace.
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1", "w2:p2"}},
		},
		AgentPanes: map[string]bool{"w2:p2": true},
	}

	if find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a") != nil {
		t.Error("a workspace with an agent in any pane is already staffed")
	}
}

// The main checkout is the user's own workspace, not something to staff.
func TestDoesNotStaffMainCheckoutWorkspace(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Workspaces: []reconcile.Workspace{
			{ID: "w1", CheckoutPath: "/repo", IsLinked: false, PaneIDs: []string{"w1:p1"}},
		},
		AgentPanes: map[string]bool{},
	}

	if find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo") != nil {
		t.Error("the main checkout should not be auto-staffed")
	}
}

func TestStaffResumesWhenATranscriptExists(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript bool
		wantResume bool
	}{
		{"prior conversation", true, true},
		{"no prior conversation", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := reconcile.State{
				Base: "main",
				Worktrees: []reconcile.Worktree{
					{Path: "/repo/wt/a", Branch: "a", IsLinked: true, WorkspaceID: "w2", HasTranscript: tc.transcript},
				},
				Workspaces: []reconcile.Workspace{
					{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1"}},
				},
				AgentPanes: map[string]bool{},
			}

			action := find(reconcile.Reconcile(state), reconcile.KindStaff, "/repo/wt/a")
			if action == nil {
				t.Fatal("expected a staff action")
			}
			if action.Resume != tc.wantResume {
				t.Errorf("Resume = %v, want %v", action.Resume, tc.wantResume)
			}
		})
	}
}

// A merged PR is the only signal that removes anything.
func TestPrunesFinishedWork(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PRMerged
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a")
	if action == nil {
		t.Fatalf("expected a prune action, got %+v", reconcile.Reconcile(state))
	}
	if action.Reason != "PR merged" {
		t.Errorf("Reason = %q, want \"PR merged\"", action.Reason)
	}
}

// Guard a. This work exists nowhere else.
func TestNeverPrunesDirtyWorktree(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.Dirty = true
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a dirty worktree must never be pruned, merged PR or not")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "uncommitted changes" {
		t.Errorf("expected an explanatory keep, got %+v", keep)
	}
}

// Guard b. In-flight work, whatever the branch looks like.
func TestNeverPrunesWorktreeHostingALiveAgent(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.WorkspaceID = "w2"
	state := reconcile.State{
		Base:      "main",
		Worktrees: []reconcile.Worktree{w},
		Workspaces: []reconcile.Workspace{
			{ID: "w2", CheckoutPath: "/repo/wt/a", IsLinked: true, PaneIDs: []string{"w2:p1"}},
		},
		AgentPanes: map[string]bool{"w2:p1": true},
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a worktree whose pane hosts a live agent must never be pruned")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected an explanatory keep, got %+v", keep)
	}
}

// Guard c. A fresh branch is trivially an ancestor of base; unstarted is not
// finished.
func TestNeverPrunesBranchWithNoCommitsOfItsOwn(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/fresh", Branch: "fresh", IsLinked: true, OwnCommits: 0},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/fresh") != nil {
		t.Fatal("a branch with no commits of its own has not started, let alone finished")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/fresh"); keep == nil {
		t.Error("expected an explanatory keep")
	}
}

// A closed-but-unmerged PR is abandoned work, not finished work: the branch
// still holds commits that exist nowhere else. It must be surfaced for a human
// to decide, never auto-pruned.
func TestNeverPrunesBranchWithAClosedPR(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PRClosed
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("a closed PR is abandoned work, not landed work")
	}

	keep := find(actions, reconcile.KindKeep, "/repo/wt/a")
	if keep == nil {
		t.Fatal("a closed PR must still be surfaced")
	}
	if !strings.Contains(keep.Reason, "closed") {
		t.Errorf("the reason must say the PR was closed so a human can decide, got %q", keep.Reason)
	}
}

// Topology cannot tell a merged branch from a branch forked off merged work:
// both point at the same commit. Where the PR is silent, keep.
func TestNeverPrunesOnTopologyAlone(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/looks-merged", Branch: "looks-merged", IsLinked: true,
			OwnCommits: 0, MergedIntoBase: true, PR: reconcile.PRNone},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/looks-merged") != nil {
		t.Fatal("git topology alone must never be enough to remove a worktree")
	}

	keep := find(actions, reconcile.KindKeep, "/repo/wt/looks-merged")
	if keep == nil {
		t.Fatal("expected a keep")
	}
	// The ambiguity has to be visible, not dressed up as a confident verdict.
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("the reason should admit the ambiguity, got %q", keep.Reason)
	}
}

// A merged PR is authoritative and prunes whatever shape the graph is in.
func TestPrunesOnAMergedPRRegardlessOfTopology(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    reconcile.Worktree
	}{
		{"merge commit", reconcile.Worktree{OwnCommits: 0, MergedIntoBase: true}},
		{"fast-forward", reconcile.Worktree{OwnCommits: 0, MergedIntoBase: false}},
		{"squash, commits still local", reconcile.Worktree{OwnCommits: 3, MergedIntoBase: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.w
			w.Path, w.Branch, w.IsLinked, w.PR = "/repo/wt/a", "a", true, reconcile.PRMerged
			state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

			if find(reconcile.Reconcile(state), reconcile.KindPrune, "/repo/wt/a") == nil {
				t.Fatal("a merged PR is authoritative")
			}
		})
	}
}

// A fast-forward merge leaves no trace distinguishing it from a branch that
// never started, so the reason must not claim the branch is unstarted.
func TestZeroCommitReasonDoesNotClaimUnstarted(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/maybe", Branch: "maybe", IsLinked: true, OwnCommits: 0},
	}}

	keep := find(reconcile.Reconcile(state), reconcile.KindKeep, "/repo/wt/maybe")
	if keep == nil {
		t.Fatal("expected a keep")
	}
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("a fast-forwarded branch looks identical to an unstarted one; "+
			"the reason must not assert one of them, got %q", keep.Reason)
	}
}

// An open PR is active work even when git thinks base contains the commits.
func TestNeverPrunesBranchWithAnOpenPR(t *testing.T) {
	w := linked("/repo/wt/a", "a")
	w.PR = reconcile.PROpen
	w.MergedIntoBase = true
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{w}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, "/repo/wt/a") != nil {
		t.Fatal("an open PR is still active work")
	}
	if keep := find(actions, reconcile.KindKeep, "/repo/wt/a"); keep == nil || keep.Reason != "still open" {
		t.Errorf("expected keep \"still open\", got %+v", keep)
	}
}

// The main checkout is the repository itself.
func TestNeverPrunesMainCheckout(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo", Branch: "main", IsLinked: false, OwnCommits: 5, PR: reconcile.PRMerged},
	}}

	for _, a := range reconcile.Reconcile(state) {
		if a.Kind == reconcile.KindPrune {
			t.Fatalf("the main checkout must never be pruned: %+v", a)
		}
	}
}

// Adoptions have to land before staffing, and prunes last.
func TestActionsAreOrderedForExecution(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Worktrees: []reconcile.Worktree{
			{Path: "/repo/wt/new", Branch: "new", IsLinked: true},
			// Has a workspace id herdr has since closed, so it is neither
			// adopted nor staffed — only pruned.
			{Path: "/repo/wt/old", Branch: "old", IsLinked: true, WorkspaceID: "w4",
				OwnCommits: 2, PR: reconcile.PRMerged},
			{Path: "/repo/wt/bare", Branch: "bare", IsLinked: true, WorkspaceID: "w3"},
		},
		Workspaces: []reconcile.Workspace{
			{ID: "w3", CheckoutPath: "/repo/wt/bare", IsLinked: true, PaneIDs: []string{"w3:p1"}},
		},
		AgentPanes: map[string]bool{},
	}

	var order []reconcile.Kind
	for _, a := range reconcile.Reconcile(state) {
		if a.Kind != reconcile.KindKeep {
			order = append(order, a.Kind)
		}
	}

	want := []reconcile.Kind{reconcile.KindAdopt, reconcile.KindStaff, reconcile.KindPrune}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %v, want %v", order, want)
		}
	}
}

// A worktree adopted in this pass has no workspace yet, so it cannot also be
// staffed until the next one. Reconcile converges rather than guessing ids.
func TestAdoptedWorktreeIsNotAlsoStaffedInTheSamePass(t *testing.T) {
	state := reconcile.State{Base: "main", Worktrees: []reconcile.Worktree{
		{Path: "/repo/wt/new", Branch: "new", IsLinked: true},
	}}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindAdopt, "/repo/wt/new") == nil {
		t.Fatal("expected an adopt")
	}
	if find(actions, reconcile.KindStaff, "/repo/wt/new") != nil {
		t.Error("nothing can be staffed before its workspace exists")
	}
}

func TestReconcileIsPure(t *testing.T) {
	state := reconcile.State{
		Base: "main",
		Worktrees: []reconcile.Worktree{
			{Path: "/repo/wt/a", Branch: "a", IsLinked: true, OwnCommits: 1, PR: reconcile.PRMerged},
		},
		AgentPanes: map[string]bool{},
	}

	first := reconcile.Reconcile(state)
	second := reconcile.Reconcile(state)

	if len(first) != len(second) {
		t.Fatalf("same input gave different output: %d vs %d actions", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("action %d differs:\n%+v\n%+v", i, first[i], second[i])
		}
	}
	if len(state.Worktrees) != 1 || state.Worktrees[0].Path != "/repo/wt/a" {
		t.Error("Reconcile mutated its input")
	}
}

func TestOnlyKeepsRequestedKindsInOrder(t *testing.T) {
	actions := []reconcile.Action{
		{Kind: reconcile.KindAdopt, Path: "a"},
		{Kind: reconcile.KindPrune, Path: "b"},
		{Kind: reconcile.KindStaff, Path: "c"},
		{Kind: reconcile.KindKeep, Path: "d"},
		{Kind: reconcile.KindPrune, Path: "e"},
	}

	got := reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep)
	if len(got) != 3 {
		t.Fatalf("want 3 actions, got %d: %+v", len(got), got)
	}
	for i, wantPath := range []string{"b", "d", "e"} {
		if got[i].Path != wantPath {
			t.Errorf("action %d = %q, want %q (order must be preserved)", i, got[i].Path, wantPath)
		}
	}

	if got := reconcile.Only(actions); len(got) != 0 {
		t.Errorf("Only with no kinds should keep nothing, got %+v", got)
	}
}

func TestSlugAndAgentName(t *testing.T) {
	for _, tc := range []struct{ in, slug, agent string }{
		{"fix-auth", "fix-auth", "fix-auth"},
		{"Feat/249-Segments UI", "feat-249-segments-ui", "feat-249-segments-ui"},
		{"249-segments", "249-segments", "muster-249-segments"},
		{"--weird--", "weird", "weird"},
	} {
		if got := reconcile.Slug(tc.in); got != tc.slug {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.slug)
		}
		if got := reconcile.AgentName(reconcile.Slug(tc.in)); got != tc.agent {
			t.Errorf("AgentName(Slug(%q)) = %q, want %q", tc.in, got, tc.agent)
		}
	}
}

// herdr rejects names longer than 32 characters.
func TestAgentNameFitsHerdrsLimit(t *testing.T) {
	long := reconcile.AgentName(reconcile.Slug("a-really-very-extremely-long-branch-name-that-keeps-going"))
	if len(long) > 32 {
		t.Errorf("agent name %q is %d chars, want <= 32", long, len(long))
	}

	prefixed := reconcile.AgentName(reconcile.Slug("1234567890123456789012345678901234567890"))
	if len(prefixed) > 32 {
		t.Errorf("prefixed agent name %q is %d chars, want <= 32", prefixed, len(prefixed))
	}
}

// Claude Code's directory name: every "/" and "." becomes "-".
func TestTranscriptSlug(t *testing.T) {
	got := reconcile.TranscriptSlug("/Users/t/code/repo/.claude/worktrees/fix-auth")
	want := "-Users-t-code-repo--claude-worktrees-fix-auth"
	if got != want {
		t.Errorf("TranscriptSlug = %q, want %q", got, want)
	}
}

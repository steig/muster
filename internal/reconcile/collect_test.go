package reconcile_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/muster/internal/herdrapi"
	"github.com/steig/muster/internal/herdrtest"
	"github.com/steig/muster/internal/reconcile"
)

// worktreeJSON builds one entry of a worktree.list reply.
func worktreeJSON(path, branch string, linked bool, workspaceID string) map[string]any {
	entry := map[string]any{
		"path": path, "label": filepath.Base(path), "branch": branch,
		"is_bare": false, "is_detached": false, "is_prunable": false,
		"is_linked_worktree": linked,
	}
	if workspaceID != "" {
		entry["open_workspace_id"] = workspaceID
	}
	return entry
}

// collectFixture wires a real repo to a fake herdr and returns the collector.
func collectFixture(t *testing.T, repo *herdrtest.Repo, worktrees []map[string]any,
	workspaces []map[string]any, agents []map[string]any, panes map[string][]string,
) *reconcile.Collector {
	t.Helper()

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": worktrees,
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list", "workspaces": workspaces,
	})
	server.HandleResult("agent.list", map[string]any{
		"type": "agent_list", "agents": agents,
	})
	server.Handle("pane.list", func(params map[string]any) (any, error) {
		id, _ := params["workspace_id"].(string)
		out := []map[string]any{}
		for _, pane := range panes[id] {
			out = append(out, map[string]any{"pane_id": pane})
		}
		return map[string]any{"type": "pane_list", "panes": out}, nil
	})

	collector := reconcile.NewCollector(herdrapi.NewWithSocket(server.SocketPath), repo.Root)
	collector.ProjectsDir = t.TempDir()
	// Default to "no PR"; individual tests install a fake gh instead.
	collector.LookupPR = func(string) reconcile.PRState { return reconcile.PRNone }
	return collector
}

// The full loop: real worktrees on disk, herdr's view of them, and the git
// facts that decide their fate.
func TestCollectAndReconcileAgainstRealGit(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fresh := repo.AddWorktree("fresh", "fresh")
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{
			worktreeJSON(repo.Root, "main", false, "w1"),
			worktreeJSON(fresh, "fresh", true, ""),
			worktreeJSON(done, "done", true, ""),
		}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	actions := reconcile.Reconcile(state)

	// With no PR to appeal to, neither is removed: git alone cannot tell a
	// merged branch from a branch forked off merged work, and cannot tell an
	// unstarted branch from a fast-forwarded one.
	for _, checkout := range []string{done, fresh} {
		if find(actions, reconcile.KindPrune, checkout) != nil {
			t.Errorf("nothing should be pruned on topology alone, got %+v", actions)
		}
		keep := find(actions, reconcile.KindKeep, checkout)
		if keep == nil {
			t.Fatalf("every candidate must be explained, none for %s", checkout)
		}
		if !strings.Contains(keep.Reason, "cannot tell") {
			t.Errorf("%s: reason should admit the ambiguity, got %q", filepath.Base(checkout), keep.Reason)
		}
	}
	// Both lack a workspace, so both get adopted.
	if find(actions, reconcile.KindAdopt, fresh) == nil {
		t.Error("the fresh worktree should be adopted")
	}
}

// The same repository with a merged PR: the authoritative signal removes what
// topology alone could not.
func TestCollectPrunesOnlyWithAPRVerdict(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	herdrtest.FakeGh(t, `echo '{"state":"MERGED"}'`)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(done, "done", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, done) == nil {
		t.Error("a merged PR should prune the worktree")
	}
}

// A branch forked off already-merged work points at the same commit as the
// branch that landed, so topology calls it merged. It has done nothing, and
// removing it would bin a worktree someone just set up.
func TestCollectDoesNotPruneABranchForkedOffMergedWork(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Brand-new branch off the merged tip: no work of its own.
	later := repo.AddWorktreeFrom("later", "later", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(later, "later", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, later) != nil {
		t.Fatal("a branch forked off merged work has done nothing and must not be pruned")
	}
}

// A fast-forward merged branch and an unstarted one reach the reconciler with
// identical facts, so neither may be removed and neither may be described as
// definitely unstarted.
func TestCollectKeepsFastForwardMergedWorktree(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	ff := repo.AddWorktree("ff", "ff")
	repo.CommitIn(ff, "ff.txt", "work")
	repo.Git("merge", "--ff-only", "ff")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(ff, "ff", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, ff) != nil {
		t.Fatal("a fast-forward merge is indistinguishable from unstarted work; keep it")
	}
	keep := find(actions, reconcile.KindKeep, ff)
	if keep == nil {
		t.Fatal("expected a keep")
	}
	if !strings.Contains(keep.Reason, "cannot tell") {
		t.Errorf("the reason must not claim the branch is unstarted, got %q", keep.Reason)
	}
}

// Guard a against real git: a real uncommitted file must stop a prune that
// every other signal says should happen.
func TestCollectSeesUncommittedChanges(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")
	herdrtest.WriteFile(t, filepath.Join(checkout, "scratch.txt"), "unsaved\n")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "done", true, "")}, nil, nil, nil)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, checkout) != nil {
		t.Fatal("uncommitted changes must stop a prune even for a merged branch")
	}
	if keep := find(actions, reconcile.KindKeep, checkout); keep == nil || keep.Reason != "uncommitted changes" {
		t.Errorf("expected keep for uncommitted changes, got %+v", keep)
	}
}

// Guard b against real state: herdr reports an agent in the workspace's pane.
func TestCollectSeesLiveAgent(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "shipped.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "done", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "done", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}},
		[]map[string]any{{
			"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1",
			"terminal_id": "term1", "agent_status": "working", "focused": false, "revision": 1,
		}},
		map[string][]string{"w2": {"w2:p1"}},
	)

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	actions := reconcile.Reconcile(state)
	if find(actions, reconcile.KindPrune, checkout) != nil {
		t.Fatal("a worktree hosting a live agent must never be pruned")
	}
	if keep := find(actions, reconcile.KindKeep, checkout); keep == nil || keep.Reason != "agent running" {
		t.Errorf("expected keep for a running agent, got %+v", keep)
	}
	if find(actions, reconcile.KindStaff, checkout) != nil {
		t.Error("a staffed workspace should not be staffed again")
	}
}

// A squash-merged branch is not an ancestor of base, so only gh knows it is
// done. This is the case the git fallback cannot catch.
func TestCollectUsesGhForSquashMergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	herdrtest.FakeGh(t, `echo '{"state":"MERGED"}'`)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "squashed", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindPrune, checkout)
	if action == nil {
		t.Fatal("a squash-merged branch should be pruned on the PR verdict")
	}
	if action.Reason != "PR merged" {
		t.Errorf("Reason = %q, want \"PR merged\"", action.Reason)
	}
}

// gh missing or failing must not invent a verdict.
func TestGhPRStateToleratesFailure(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	herdrtest.FakeGh(t, `exit 1`)

	if got := reconcile.GhPRState(repo.Root, "anything"); got != reconcile.PRNone {
		t.Errorf("a failing gh should yield PRNone, got %q", got)
	}
}

// An open PR keeps the worktree even when everything else looks finished.
func TestCollectKeepsWorktreeWithOpenPR(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")
	repo.CommitIn(checkout, "wip.txt", "work")

	herdrtest.FakeGh(t, `echo '{"state":"OPEN"}'`)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "wip", true, "")}, nil, nil, nil)
	collector.LookupPR = func(branch string) reconcile.PRState {
		return reconcile.GhPRState(repo.Root, branch)
	}

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if find(reconcile.Reconcile(state), reconcile.KindPrune, checkout) != nil {
		t.Error("an open PR is active work")
	}
}

// The resume decision reads a real transcript directory.
func TestCollectDetectsPriorTranscript(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("resume-me", "resume-me")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "resume-me", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "resume-me", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})

	// Claude Code stores conversations under a slug of the absolute path.
	herdrtest.WriteFile(t, filepath.Join(collector.ProjectsDir,
		reconcile.TranscriptSlug(checkout), "session.jsonl"), "{}\n")

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, checkout)
	if action == nil {
		t.Fatal("an agentless workspace should be staffed")
	}
	if !action.Resume {
		t.Error("a worktree with a prior transcript should resume, not start cold")
	}
}

func TestCollectStartsColdWithoutTranscript(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("brand-new", "brand-new")

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "brand-new", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "brand-new", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	action := find(reconcile.Reconcile(state), reconcile.KindStaff, checkout)
	if action == nil {
		t.Fatal("an agentless workspace should be staffed")
	}
	if action.Resume {
		t.Error("no transcript means a cold start")
	}
}

// herdr reports resolved paths; the collector may have been handed the repo
// root with its symlinks intact. Comparing them unresolved drops every
// workspace, which turns sync into a silent no-op — nothing to staff, because
// nothing matched.
func TestCollectMatchesWorkspacesAcrossSymlinkedRoots(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	if repo.RealRoot == repo.Root {
		t.Skip("temp dir is not behind a symlink on this machine")
	}
	resolvedCheckout := strings.Replace(checkout, repo.Root, repo.RealRoot, 1)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(checkout, "wip", true, "w2")},
		[]map[string]any{{
			"workspace_id": "w2", "number": 2, "label": "wip", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			// As herdr reports it: fully resolved.
			"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
				"repo_root": repo.RealRoot, "checkout_path": resolvedCheckout,
				"is_linked_worktree": true},
		}}, nil, map[string][]string{"w2": {"w2:p1"}})
	// As the invocation context supplies it: symlinks intact.
	collector.Root = repo.Root

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(state.Workspaces) != 1 {
		t.Fatalf("the workspace was dropped over a symlink difference: %+v", state.Workspaces)
	}
	if find(reconcile.Reconcile(state), reconcile.KindStaff, resolvedCheckout) == nil {
		t.Error("an agentless workspace should still be staffed")
	}
}

// Workspaces belonging to other repositories must not be touched.
func TestCollectIgnoresOtherRepositoriesWorkspaces(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	collector := collectFixture(t, repo,
		[]map[string]any{worktreeJSON(repo.Root, "main", false, "w1")},
		[]map[string]any{{
			"workspace_id": "w9", "number": 9, "label": "elsewhere", "focused": false,
			"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
			"worktree": map[string]any{"repo_key": "other", "repo_name": "other",
				"repo_root": "/some/other/repo", "checkout_path": "/some/other/repo/wt/x",
				"is_linked_worktree": true},
		}}, nil, map[string][]string{"w9": {"w9:p1"}})

	state, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(state.Workspaces) != 0 {
		t.Errorf("another repository's workspace leaked in: %+v", state.Workspaces)
	}
}

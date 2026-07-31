package execute_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steig/muster/internal/execute"
	"github.com/steig/muster/internal/herdrapi"
	"github.com/steig/muster/internal/herdrtest"
	"github.com/steig/muster/internal/reconcile"
)

// fixture wires an executor to a fake herdr with no agents and no panes.
func fixture(t *testing.T, repo *herdrtest.Repo) (*execute.Executor, *herdrtest.Server) {
	t.Helper()

	server := herdrtest.NewServer(t)
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})
	server.HandleResult("worktree.remove", map[string]any{"type": "worktree_removed"})
	server.HandleResult("agent.start", map[string]any{"type": "agent_started"})

	return &execute.Executor{
		Client: herdrapi.NewWithSocket(server.SocketPath),
		Root:   repo.Root,
	}, server
}

// callTo finds the request a test is actually about.
//
// Indexing into Calls() couples a test to how many guard reads happen to
// precede the call, so adding a re-check breaks assertions about behaviour that
// did not change.
func callTo(t *testing.T, server *herdrtest.Server, method string) herdrtest.Call {
	t.Helper()

	for _, call := range server.Calls() {
		if call.Method == method {
			return call
		}
	}
	t.Fatalf("no %s call was made", method)
	return herdrtest.Call{}
}

func only(t *testing.T, results []execute.Result) execute.Result {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(results), results)
	}
	return results[0]
}

// mergedWorktree builds a checkout that genuinely is finished.
func mergedWorktree(t *testing.T, repo *herdrtest.Repo, name string) string {
	t.Helper()

	checkout := repo.AddWorktree(name, name)
	repo.CommitIn(checkout, name+".txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge "+name, name)
	return checkout
}

// THE staleness case: the plan said prune, then the worktree picked up
// uncommitted work before the executor got to it.
func TestPruneRefusesWorktreeThatWentDirtyAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	// The gap between reconcile and execute: real, uncommitted work appears.
	herdrtest.WriteFile(t, filepath.Join(checkout, "rescue-me.txt"), "unsaved\n")

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "uncommitted changes") {
		t.Errorf("detail should explain the refusal, got %q", result.Detail)
	}
	// The point of the whole exercise: the checkout is still on disk.
	if !repo.Exists(checkout) {
		t.Fatal("the worktree was removed despite having uncommitted work")
	}
}

// The same gap, but an agent starts in it.
func TestPruneRefusesWorktreeThatGainedAnAgentAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	// herdr now reports an agent in this workspace's pane.
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{{
		"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "w2:t1", "terminal_id": "t",
		"agent_status": "working", "focused": false, "revision": 1,
	}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "agent started here") {
		t.Errorf("detail should explain the refusal, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree was removed while an agent was working in it")
	}
}

// Pruning is a dry run unless explicitly applied.
func TestPruneIsDryRunByDefault(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusPlanned {
		t.Fatalf("status = %q, want planned: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "would remove") {
		t.Errorf("a dry run should say what it would do, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("a dry run removed the worktree")
	}
}

func TestPruneAppliesWhenAsked(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the worktree should be gone")
	}
	// A merged branch is safe to delete, and the report has to say so.
	if !strings.Contains(result.Detail, "deleted branch done") {
		t.Errorf("detail should report the branch deletion, got %q", result.Detail)
	}
}

// Removing the directory the caller is standing in would leave their shell on
// a dead cwd, and a plugin subprocess cannot cd on their behalf.
func TestPruneRefusesTheDirectoryTheCallerIsStandingIn(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	exec.CallerDir = filepath.Join(checkout, "src")

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "cd out of it") {
		t.Errorf("detail should tell the user what to do, got %q", result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree the caller is standing in was removed")
	}
}

// The asymmetric case, which is the one that happens in production: herdr
// reports resolved paths while the invocation context carries the path the user
// saw, symlinks intact. Comparing them unresolved silently disarms the guard.
func TestPruneRefusesTheCallerDirEvenWhenOnlyOneSideIsResolved(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	// repo.Root keeps its symlinks; RealRoot is what git and herdr report.
	resolved := strings.Replace(checkout, repo.Root, repo.RealRoot, 1)
	if resolved == checkout {
		t.Skip("temp dir is not behind a symlink on this machine")
	}

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	// The action names the checkout as herdr does; the caller sits in the same
	// directory by its unresolved name.
	exec.CallerDir = checkout

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: resolved,
		Branch: "done", Reason: "merged into main"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Fatalf("the guard must see through the symlink; got %q: %s", result.Status, result.Detail)
	}
	if !repo.Exists(checkout) {
		t.Fatal("the worktree the caller is standing in was removed")
	}
}

// A sibling directory sharing a name prefix is not inside the checkout.
func TestPruneAllowsCallerInASimilarlyNamedSibling(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true
	exec.CallerDir = checkout + "-other"

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "done", Reason: "merged into main"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
}

// An unmerged branch is never force-deleted: the work exists nowhere else.
func TestPruneKeepsABranchGitWillNotDelete(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	exec, _ := fixture(t, repo)
	exec.ApplyPrune = true

	// The PR said merged, so the reconciler planned a prune, but git still
	// sees the branch as unmerged because the squash rewrote the commit.
	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout,
		Branch: "squashed", Reason: "PR merged"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}
	if repo.Exists(checkout) {
		t.Error("the checkout should be removed")
	}
	if !strings.Contains(result.Detail, "kept branch squashed") {
		t.Errorf("an unmerged branch must be kept and reported, got %q", result.Detail)
	}
	if repo.BranchMissing("squashed") {
		t.Error("the branch was deleted despite git refusing")
	}
}

// With a workspace, removal goes through herdr so the workspace is torn down
// with the checkout.
func TestPruneWithWorkspaceGoesThroughHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")

	exec, server := fixture(t, repo)
	exec.ApplyPrune = true

	action := reconcile.Action{Kind: reconcile.KindPrune, Path: checkout, Branch: "done",
		WorkspaceID: "w2", Reason: "merged into main"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q, want done: %s", result.Status, result.Detail)
	}

	var removed *herdrtest.Call
	for _, c := range server.Calls() {
		if c.Method == "worktree.remove" {
			call := c
			removed = &call
		}
	}
	if removed == nil {
		t.Fatal("expected herdr to be asked to remove the worktree")
	}
	if removed.Params["workspace_id"] != "w2" {
		t.Errorf("workspace_id = %v, want w2", removed.Params["workspace_id"])
	}
}

func TestAdoptOpensWorkspaceWithoutFocus(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}

	call := server.Calls()[0]
	if call.Method != "worktree.open" {
		t.Fatalf("method = %q, want worktree.open", call.Method)
	}
	// Adopting a batch must not drag the user through every workspace.
	if call.Params["focus"] != false {
		t.Errorf("focus = %v, want false", call.Params["focus"])
	}
	if call.Params["path"] != "/repo/wt/a" {
		t.Errorf("path = %v", call.Params["path"])
	}
}

func TestStaffResumeUsesContinue(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a", Resume: true}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "resumed") {
		t.Errorf("detail should say it resumed, got %q", result.Detail)
	}

	call := callTo(t, server, "agent.start")
	args, _ := call.Params["args"].([]any)
	if len(args) != 1 || args[0] != "--continue" {
		t.Errorf("args = %v, want [--continue]", call.Params["args"])
	}
	if call.Params["pane_id"] != "w2:p1" {
		t.Errorf("pane_id = %v", call.Params["pane_id"])
	}
}

func TestStaffColdStartSendsNoArgs(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	if _, ok := callTo(t, server, "agent.start").Params["args"]; ok {
		t.Error("a cold start should not pass --continue")
	}
}

// A pane still running direnv rejects the agent. That has to surface as a
// reported failure, not a silent no-op.
// The staffing twin of the prune staleness cases, and the reason staffing
// re-reads its guard at all.
//
// The reconciler decides to staff a workspace that has no agent, and that
// snapshot ages. An agent starting in the gap is the common case, not a corner:
// an event hook and a human `sync` can plan from the same state moments apart.
// Starting a second agent on a pane that already has one does not fail
// harmlessly — it lands on a live conversation.
func TestStaffRefusesAPaneThatGainedAnAgentAfterThePlan(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	// An agent appeared in the workspace between the plan and this moment.
	server.HandleResult("agent.list", map[string]any{"type": "agent_list",
		"agents": []map[string]any{{"name": "already-here", "pane_id": "w2:p1"}}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			t.Fatal("started a second agent on a pane that already had one")
		}
	}
	if !strings.Contains(result.Detail, "agent") {
		t.Errorf("the refusal should explain itself, got %q", result.Detail)
	}
}

// Staffing must still happen when the workspace really is empty; a guard that
// refuses everything is not a guard.
func TestStaffProceedsWhenTheWorkspaceIsStillIdle(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a", Branch: "a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	if result := only(t, exec.Run([]reconcile.Action{action})); result.Status != execute.StatusDone {
		t.Fatalf("status = %q: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			return
		}
	}
	t.Error("an idle workspace was never staffed")
}

// A workspace herdr cannot be asked about must not be staffed on the assumption
// that it is idle. The same call already blocks a prune for the same reason:
// an unverifiable guard is not a satisfied one.
func TestStaffRefusesWhenTheWorkspaceCannotBeChecked(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.Handle("agent.list", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		WorkspaceID: "w2", PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped: %s", result.Status, result.Detail)
	}
	for _, call := range server.Calls() {
		if call.Method == "agent.start" {
			t.Fatal("staffed a workspace whose state could not be confirmed")
		}
	}
}

func TestStaffReportsAFailureToStart(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.Handle("agent.start", func(map[string]any) (any, error) {
		return nil, errStartFailed{}
	})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Detail, "w2:p1") {
		t.Errorf("detail should name the pane, got %q", result.Detail)
	}
}

type errStartFailed struct{}

func (errStartFailed) Error() string { return "pane is busy" }

// The slow-direnv case, end to end. A freshly created worktree can still be
// running direnv or nix when staffing is attempted, which is why agent.start
// asks herdr to wait. A client deadline shorter than that wait would abort the
// call first and report a hard failure on exactly the case the wait exists for.
func TestStaffSurvivesAServerSlowerThanTheOrdinaryDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~6s of real time by design")
	}

	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	// Longer than the 5s an ordinary call allows, well under the 60s that
	// staffing asks herdr to wait.
	server.HandleSlow("agent.start", 6*time.Second, map[string]any{"type": "agent_started"})

	action := reconcile.Action{Kind: reconcile.KindStaff, Path: "/repo/wt/a",
		PaneID: "w2:p1", AgentName: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusDone {
		t.Fatalf("staffing must outlast the wait it asked herdr for; got %q: %s",
			result.Status, result.Detail)
	}
}

// The other half: an ordinary call keeps its short deadline, so a wedged herdr
// still fails rather than hanging a plugin action forever.
func TestOrdinaryCallStillTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s of real time by design")
	}

	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	server.HandleSlow("worktree.open", 8*time.Second, map[string]any{"type": "workspace_created"})

	action := reconcile.Action{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusFailed {
		t.Fatalf("a wedged server should fail the call, got %q: %s", result.Status, result.Detail)
	}
}

// Keep actions are explanatory and must reach the report.
func TestKeepIsReportedNotExecuted(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	exec, server := fixture(t, repo)

	action := reconcile.Action{Kind: reconcile.KindKeep, Path: "/repo/wt/a",
		Branch: "a", Reason: "agent running"}

	result := only(t, exec.Run([]reconcile.Action{action}))
	if result.Status != execute.StatusSkipped {
		t.Errorf("status = %q, want skipped", result.Status)
	}
	if result.Detail != "agent running" {
		t.Errorf("detail = %q, want the keep reason", result.Detail)
	}
	if len(server.Calls()) != 0 {
		t.Errorf("a keep must not talk to herdr: %+v", server.Calls())
	}
}

// Every result carries a detail — a silent no-op is the failure mode here.
func TestEveryResultExplainsItself(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := mergedWorktree(t, repo, "done")
	exec, _ := fixture(t, repo)

	results := exec.Run([]reconcile.Action{
		{Kind: reconcile.KindAdopt, Path: "/repo/wt/a", Branch: "a"},
		{Kind: reconcile.KindStaff, Path: "/repo/wt/b", PaneID: "w3:p1", AgentName: "b"},
		{Kind: reconcile.KindPrune, Path: checkout, Branch: "done", Reason: "merged into main"},
		{Kind: reconcile.KindKeep, Path: "/repo/wt/c", Reason: "still open"},
	})

	if len(results) != 4 {
		t.Fatalf("want 4 results, got %d", len(results))
	}
	for _, r := range results {
		if strings.TrimSpace(r.Detail) == "" {
			t.Errorf("%s action produced no explanation: %+v", r.Action.Kind, r)
		}
	}

	report := execute.Render(results)
	if strings.Count(strings.TrimRight(report, "\n"), "\n") != 3 {
		t.Errorf("report should have one line per action:\n%s", report)
	}
	if execute.Counts(results)[execute.StatusPlanned] != 1 {
		t.Errorf("expected one planned prune, got %v", execute.Counts(results))
	}
}

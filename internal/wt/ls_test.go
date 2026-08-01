package wt_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/wt"
)

func ptr(s string) *string { return &s }

func TestRowsJoinsWorkspaceAgentStatus(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo", Branch: ptr("main"), IsLinkedWorktree: false},
		{Path: "/repo/.claude/worktrees/fix-auth", Branch: ptr("fix-auth"),
			IsLinkedWorktree: true, OpenWorkspaceID: ptr("w2")},
	}}
	workspaces := &herdrapi.WorkspaceListResponse{Workspaces: []herdrapi.WorkspaceInfo{
		{WorkspaceID: "w2", AgentStatus: herdrapi.AgentStatusWorking},
	}}

	rows := wt.Rows(worktrees, workspaces)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}

	if !rows[0].Main {
		t.Error("first worktree is the main checkout, want Main=true")
	}
	if rows[0].WorkspaceID != "-" || rows[0].AgentStatus != "-" {
		t.Errorf("unopened worktree should have no workspace/status, got %+v", rows[0])
	}

	// Rows joins two herdr calls and no more: the pane and pull request columns
	// each cost their own lookup, so they are left as "-" for WithPanes and
	// WithPRs to fill.
	want := wt.Row{Main: false, Branch: "fix-auth", WorkspaceID: "w2", PaneID: "-",
		AgentStatus: "working", PR: "-", Dir: "fix-auth"}
	if rows[1] != want {
		t.Errorf("row 1:\n got %+v\nwant %+v", rows[1], want)
	}
}

// A worktree can point at a workspace herdr has already closed. The status must
// degrade to "-" rather than reporting a stale or zero-valued agent state.
func TestRowsHandlesStaleWorkspaceReference(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo/wt/gone", Branch: ptr("gone"), IsLinkedWorktree: true,
			OpenWorkspaceID: ptr("w-closed")},
	}}
	workspaces := &herdrapi.WorkspaceListResponse{}

	rows := wt.Rows(worktrees, workspaces)
	if rows[0].AgentStatus != "-" {
		t.Errorf("stale workspace ref should yield %q, got %q", "-", rows[0].AgentStatus)
	}
	if rows[0].WorkspaceID != "w-closed" {
		t.Errorf("workspace id should still be shown, got %q", rows[0].WorkspaceID)
	}
}

func TestRowsDetachedHeadHasNoBranch(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo/wt/detached", Branch: nil, IsDetached: true, IsLinkedWorktree: true},
	}}

	rows := wt.Rows(worktrees, nil)
	if rows[0].Branch != "-" {
		t.Errorf("detached worktree should render branch as %q, got %q", "-", rows[0].Branch)
	}
}

func TestRenderAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	err := wt.Render(&buf, []wt.Row{
		{Main: true, Branch: "main", WorkspaceID: "-", PaneID: "-", AgentStatus: "-", Dir: "repo"},
		{Branch: "a-much-longer-branch", WorkspaceID: "w2", PaneID: "p1", AgentStatus: "idle", Dir: "wt"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "*") {
		t.Errorf("main checkout should be marked with *, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], " ") {
		t.Errorf("linked worktree should not be marked, got %q", lines[1])
	}
	// Alignment is the reason tabwriter is here at all.
	if strings.Index(lines[0], "-") != strings.Index(lines[1], "w2") {
		t.Errorf("columns not aligned:\n%q\n%q", lines[0], lines[1])
	}
}

// git refuses ASCII control characters in a ref name but accepts a bidi
// override, so `evil<U+202E>hctap` is a branch anyone who can open a pull
// request can create — and a terminal draws it as `evilpatch`. The row is
// escaped rather than dropped: this listing is what a human reads before
// pruning, so a worktree missing from it is the same hiding by another route.
func TestRenderEscapesABranchNameThatDrawsAsAnother(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.Git("branch", "--", "evil‮hctap")

	branch := repo.Git("branch", "--list", "--format=%(refname:short)", "evil‮hctap")
	if !strings.ContainsRune(branch, '‮') {
		t.Fatalf("git dropped the override, so there is nothing to defend against: %q", branch)
	}

	var buf bytes.Buffer
	if err := wt.Render(&buf, []wt.Row{
		{Branch: branch, WorkspaceID: "-", PaneID: "-", AgentStatus: "-", Dir: "wt"},
	}, false); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.ContainsRune(out, '‮') {
		t.Errorf("the override reached the terminal: %q", out)
	}
	if !strings.Contains(out, `evil\u{202E}hctap`) {
		t.Errorf("the row must still say which branch it is, got %q", out)
	}
}

// End to end over the wire: real git worktrees on disk, a fake herdr answering
// the same NDJSON protocol as the real one.
func TestLsAgainstFakeHerdr(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("fix-auth", "fix-auth")

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
			{"path": checkout, "label": "fix-auth", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "fix-auth",
				"open_workspace_id": "w2"},
		},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "fix-auth", "focused": true,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
		},
	})

	server.HandleResult("pane.list", map[string]any{
		"type": "pane_list",
		"panes": []map[string]any{
			{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0},
		},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	// Called from inside the linked worktree: the listing must still cover the
	// whole repository, which is what RepoRoot's --git-common-dir is for.
	if err := wt.Ls(client, "", checkout, nil, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	out := buf.String()
	// The pane is here because it is what `dispatch --pane` takes; a listing
	// that stops at the workspace leaves that step with nowhere to get it.
	for _, want := range []string{"main", "fix-auth", "w2", "w2:p1", "working"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// No lookup was passed, so gh was not consulted and the column is absent
	// rather than dashed — a "-" there reads as "no pull request".
	if strings.Contains(out, "OPEN") || strings.Contains(out, "MERGED") {
		t.Errorf("pull request state must not appear unless asked for:\n%s", out)
	}

	calls := server.Calls()
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Method != "worktree.list" || calls[1].Method != "workspace.list" ||
		calls[2].Method != "pane.list" {
		t.Errorf("unexpected methods: %+v", calls)
	}
	// The cwd must be the MAIN checkout, not the linked worktree we ran from.
	if got := calls[0].Params["cwd"]; got != repo.RealRoot {
		t.Errorf("worktree.list cwd = %v, want repo root %s", got, repo.RealRoot)
	}
}

// A workspace list failure must fail the command. Degrading the status column
// to "-" would render a herdr that did not answer as a session with no
// workspaces at all, which reads as an instruction to run sync.
func TestLsFailsWhenWorkspaceListFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
		},
	})
	// workspace.list deliberately unhandled -> server returns an error.

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, "", repo.Root, nil, &buf); err == nil {
		t.Fatal("Ls should fail when the workspace list fails")
	}
	if buf.Len() != 0 {
		t.Errorf("no listing should be printed when the status is unknown:\n%s", buf.String())
	}
}

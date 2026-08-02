package wt_test

import (
	"bytes"
	"errors"
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
	if rows[0].WorkspaceID != "" || rows[0].AgentStatus != "" {
		t.Errorf("unopened worktree should have no workspace/status, got %+v", rows[0])
	}

	// Rows joins two herdr calls and no more: the pane and pull request columns
	// each cost their own lookup, so they are left empty for WithPanes and
	// WithPRs to fill.
	want := wt.Row{Main: false, Branch: "fix-auth", WorkspaceID: "w2",
		AgentStatus: "working", Dir: "fix-auth"}
	if rows[1] != want {
		t.Errorf("row 1:\n got %+v\nwant %+v", rows[1], want)
	}
}

// A worktree can point at a workspace herdr has already closed. The status must
// degrade to absent rather than reporting a stale or zero-valued agent state.
func TestRowsHandlesStaleWorkspaceReference(t *testing.T) {
	worktrees := &herdrapi.WorktreeListResponse{Worktrees: []herdrapi.WorktreeInfo{
		{Path: "/repo/wt/gone", Branch: ptr("gone"), IsLinkedWorktree: true,
			OpenWorkspaceID: ptr("w-closed")},
	}}
	workspaces := &herdrapi.WorkspaceListResponse{}

	rows := wt.Rows(worktrees, workspaces)
	if rows[0].AgentStatus != "" {
		t.Errorf("stale workspace ref should yield no status, got %q", rows[0].AgentStatus)
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
	if rows[0].Branch != "" {
		t.Errorf("detached worktree has no branch, got %q", rows[0].Branch)
	}
}

func TestRenderAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	err := wt.Render(&buf, []wt.Row{
		{Main: true, Branch: "main", Dir: "repo"},
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
		{Branch: branch, Dir: "wt"},
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
	if err := wt.Ls(client, "", checkout, nil, wt.Options{}, &buf); err != nil {
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

// fleet is two repositories herdr has worktree workspaces for, one of which it
// cannot read. It is the shape `--all-repos` exists for, and the shape that
// makes the failure rule matter.
func fleet(t *testing.T) (*herdrtest.Server, *herdrtest.Repo, string) {
	t.Helper()

	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("77-cross-repo", "77-cross-repo")
	const unreadable = "/nowhere/lighthouse"

	server := herdrtest.NewServer(t)
	server.Handle("worktree.list", func(params map[string]any) (any, error) {
		if params["cwd"] == unreadable {
			return nil, errors.New("not a git repository")
		}
		return map[string]any{
			"type":   "worktree_list",
			"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
			"worktrees": []map[string]any{
				{"path": repo.Root, "label": "repo", "is_bare": false, "is_detached": false,
					"is_prunable": false, "is_linked_worktree": false, "branch": "main"},
				{"path": checkout, "label": "77-cross-repo", "is_bare": false, "is_detached": false,
					"is_prunable": false, "is_linked_worktree": true, "branch": "77-cross-repo",
					"open_workspace_id": "w2"},
			},
		}, nil
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "77-cross-repo", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "blocked"},
		},
	})
	server.HandleResult("pane.list", map[string]any{
		"type":  "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0}},
	})
	return server, repo, unreadable
}

// The whole complaint #77 makes: someone with agents in six repositories has to
// visit six directories to see them. One listing covers all of them — and one
// repository failing must not cost the others their rows, which is the rule
// doctor already follows.
func TestLsAllGroupsEveryRepositoryAndKeepsGoingPastAFailure(t *testing.T) {
	server, repo, unreadable := fleet(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot, unreadable}, wt.Options{}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	out := buf.String()
	for _, want := range []string{repo.RealRoot, "77-cross-repo", "w2:p1", "blocked", unreadable, "not a git repository"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The full root rather than the basename: two checkouts on one machine
	// routinely share one, and this listing is read across all of them.
	if !strings.Contains(out, repo.RealRoot+"\n") {
		t.Errorf("the repository heading must be the root on a line of its own:\n%s", out)
	}
	// Rows belong to the repository above them, so they are indented under it.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "77-cross-repo") && !strings.HasPrefix(line, "  ") {
			t.Errorf("row is not indented under its repository: %q", line)
		}
	}
}

// `--blocked` across six repositories is a question with a short answer, and
// five headings with nothing under them bury it. A repository that could not be
// read is never skipped: that is the row the reader most needs.
func TestLsAllBlockedSkipsQuietRepositoriesButNeverAFailedOne(t *testing.T) {
	server, repo, unreadable := fleet(t)
	// Nothing is blocked now, so the readable repository has nothing to show.
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "77-cross-repo", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
		},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot, unreadable}, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, repo.RealRoot) {
		t.Errorf("a repository with nothing blocked should not be drawn as an empty heading:\n%s", out)
	}
	if !strings.Contains(out, unreadable) || !strings.Contains(out, "not a git repository") {
		t.Errorf("a repository that could not be read must still be named:\n%s", out)
	}
}

// Nothing blocked anywhere has to say so. Silence is the same output as a
// listing that reached no repositories at all.
func TestLsAllBlockedSaysNothingIsBlockedRatherThanPrintingNothing(t *testing.T) {
	server, repo, _ := fleet(t)
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list", "workspaces": []map[string]any{},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot}, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	if got, want := buf.String(), "no blocked agents in 1 repository\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// herdr having nothing open is a different answer from every repository being
// quiet, and both are different from a listing that failed.
func TestLsAllSaysWhenHerdrHasNothingOpen(t *testing.T) {
	server := herdrtest.NewServer(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, nil, wt.Options{}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	if !strings.Contains(buf.String(), "no worktree workspaces open") {
		t.Errorf("got %q", buf.String())
	}
	// Not one call: with no repositories to read there is nothing to ask about.
	if calls := server.Calls(); len(calls) != 0 {
		t.Errorf("herdr was asked %d question(s) about no repositories: %+v", len(calls), calls)
	}
}

// blockedRows is the fixture both filter tests read: one of each status herdr
// can report, so a filter that keeps too much is caught by what it kept.
var blockedRows = []wt.Row{
	{Branch: "working", AgentStatus: "working"},
	{Branch: "stuck", AgentStatus: "blocked"},
	{Branch: "idle", AgentStatus: "idle"},
	{Branch: "finished", AgentStatus: "done"},
	{Branch: "no-agent"},
}

func TestOnlyBlockedKeepsHerdrsBlockedAndNothingElse(t *testing.T) {
	kept := wt.OnlyBlocked(blockedRows)

	if len(kept) != 1 || kept[0].Branch != "stuck" {
		t.Fatalf("want only the blocked row, got %+v", kept)
	}
}

// The filter has to run before the lookups, not after. A pane is a round trip
// per workspace, and the whole point of `--blocked` is that it is the question
// you ask across everything you have open.
func TestLsBlockedAsksForNoPaneItWillNotPrint(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	working := repo.AddWorktree("working", "working")
	stuck := repo.AddWorktree("stuck", "stuck")

	server := herdrtest.NewServer(t)
	server.HandleResult("worktree.list", map[string]any{
		"type":   "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo", "repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{
			{"path": working, "label": "working", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "working",
				"open_workspace_id": "w2"},
			{"path": stuck, "label": "stuck", "is_bare": false, "is_detached": false,
				"is_prunable": false, "is_linked_worktree": true, "branch": "stuck",
				"open_workspace_id": "w3"},
		},
	})
	server.HandleResult("workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []map[string]any{
			{"workspace_id": "w2", "number": 2, "label": "working", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "working"},
			{"workspace_id": "w3", "number": 3, "label": "stuck", "focused": false,
				"pane_count": 1, "tab_count": 1, "active_tab_id": "t2", "agent_status": "blocked"},
		},
	})
	server.HandleResult("pane.list", map[string]any{
		"type":  "pane_list",
		"panes": []map[string]any{{"pane_id": "w3:p1", "workspace_id": "w3", "tab_id": "t2", "index": 0}},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "stuck") || !strings.Contains(out, "w3:p1") {
		t.Errorf("the blocked worktree and the pane that can restart it must both be there:\n%s", out)
	}
	if strings.Contains(out, "working") {
		t.Errorf("a working agent is not blocked:\n%s", out)
	}

	var panes []herdrtest.Call
	for _, call := range server.Calls() {
		if call.Method == "pane.list" {
			panes = append(panes, call)
		}
	}
	if len(panes) != 1 {
		t.Fatalf("want one pane lookup, got %d: %+v", len(panes), panes)
	}
	if got := panes[0].Params["workspace_id"]; got != "w3" {
		t.Errorf("pane looked up for workspace %v, want the blocked one w3", got)
	}
}

// Nothing blocked is an answer, and an empty table is not how to give it: it is
// the same output as a listing that reached nothing at all.
func TestLsBlockedSaysSoWhenNothingIsBlocked(t *testing.T) {
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
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{Blocked: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	if !strings.Contains(buf.String(), "no blocked agents") {
		t.Errorf("a filter that matched nothing must say so, got %q", buf.String())
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
	if err := wt.Ls(client, "", repo.Root, nil, wt.Options{}, &buf); err == nil {
		t.Fatal("Ls should fail when the workspace list fails")
	}
	if buf.Len() != 0 {
		t.Errorf("no listing should be printed when the status is unknown:\n%s", buf.String())
	}
}

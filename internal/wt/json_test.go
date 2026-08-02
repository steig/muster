package wt_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/wt"
)

// The whole complaint the JSON answers: the table prints "-" for no workspace,
// no pane and no agent alike, and a consumer cannot tell those from a workspace
// literally named "-". Each has to arrive as null.
func TestJSONRendersAbsenceAsNull(t *testing.T) {
	rows := wt.JSON([]wt.Row{{Main: true, Branch: "main", Dir: "repo"}})

	row := rows[0]
	for name, got := range map[string]*string{
		"workspace_id": row.WorkspaceID,
		"pane_id":      row.PaneID,
		"agent_status": row.AgentStatus,
	} {
		if got != nil {
			t.Errorf("%s = %q, want null", name, *got)
		}
	}
	if row.Branch == nil || *row.Branch != "main" {
		t.Errorf("branch = %v, want main", row.Branch)
	}
	// Not asked about at all, which is neither "no pull request" nor a failure.
	if row.PR != nil {
		t.Errorf("pr = %+v, want null when no lookup ran", row.PR)
	}
}

// The distinction the table has no room for, and the one that costs the most:
// an unauthenticated gh reads there as "no pull request", which is the reading
// that makes prune keep everything.
func TestJSONTellsNoPullRequestFromNoAnswer(t *testing.T) {
	rows := []wt.Row{
		{Branch: "none", Dir: "none"},
		{Branch: "open", Dir: "open"},
		{Branch: "unknown", Dir: "unknown"},
	}
	wt.WithPRs(rows, func(branch string) (string, error) {
		switch branch {
		case "open":
			return "OPEN", nil
		case "unknown":
			return "", errors.New("gh: not logged in")
		}
		return "", nil
	})

	got := wt.JSON(rows)

	if pr := got[0].PR; pr == nil || pr.State != nil || pr.Error != nil {
		t.Errorf("a branch with no pull request should be asked and answered null, got %+v", pr)
	}
	if pr := got[1].PR; pr == nil || pr.State == nil || *pr.State != "OPEN" {
		t.Errorf("pr state = %+v, want OPEN", pr)
	}
	if pr := got[2].PR; pr == nil || pr.Error == nil || !strings.Contains(*pr.Error, "not logged in") {
		t.Errorf("a lookup gh could not answer must say so, got %+v", pr)
	} else if pr.State != nil {
		t.Errorf("state is meaningless when the lookup failed, got %q", *pr.State)
	}
}

// Trunk is never asked about — it has no pull request and every lookup is a
// network round trip — so its column must read as "nobody asked" rather than
// as an answer.
func TestWithPRsLeavesTheMainCheckoutUnasked(t *testing.T) {
	rows := []wt.Row{{Main: true, Branch: "main", Dir: "repo"}}

	asked := 0
	wt.WithPRs(rows, func(string) (string, error) {
		asked++
		return "OPEN", nil
	})

	if asked != 0 {
		t.Errorf("gh was asked about the main checkout %d time(s)", asked)
	}
	if rows[0].PR != nil {
		t.Errorf("main checkout PR = %+v, want nil", rows[0].PR)
	}
}

// The JSON carries raw values where the table carries escaped ones. Escaping is
// a terminal's problem: a branch name with \u{202E} spelled out is no longer a
// branch name that can be handed back to git, and the consumer has to do its own
// escaping anyway at the point it draws.
func TestJSONCarriesRawValuesTheTableEscapes(t *testing.T) {
	const branch = "evil‮hctap"

	rows := []wt.Row{{Branch: branch, Dir: "wt"}}

	if got := wt.JSON(rows)[0].Branch; got == nil || *got != branch {
		t.Errorf("branch = %v, want the raw name back", got)
	}

	var table bytes.Buffer
	if err := wt.Render(&table, rows, false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(table.String(), '‮') {
		t.Errorf("the table must still escape it: %q", table.String())
	}
}

// The schema change #77 needs, and the reason it is a group rather than a
// repository string on every row: a repository that could not be read has no
// rows to carry its name, and a repository whose rows were all filtered away
// still has to be in the document as an empty group. A flat array says neither.
func TestLsAllJSONGroupsByRepositoryRatherThanTaggingEveryRow(t *testing.T) {
	server, repo, unreadable := fleet(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.LsAll(client, []string{repo.RealRoot, unreadable}, wt.Options{JSON: true}, &buf); err != nil {
		t.Fatalf("LsAll: %v", err)
	}

	var document wt.ListJSON
	if err := json.Unmarshal(buf.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, buf.String())
	}

	// Exactly one of the two fields is ever non-null, so a consumer can tell
	// which question was asked from the document alone.
	if document.Worktrees != nil {
		t.Errorf("worktrees must be null when the answer was grouped, got %+v", document.Worktrees)
	}
	if len(document.Repositories) != 2 {
		t.Fatalf("want both repositories, got %+v", document.Repositories)
	}

	read, failed := document.Repositories[0], document.Repositories[1]
	if read.Root != repo.RealRoot || read.Name != filepath.Base(repo.RealRoot) {
		t.Errorf("repository = %+v, want root %s", read, repo.RealRoot)
	}
	if read.Error != nil {
		t.Fatalf("the readable repository carries an error: %v", *read.Error)
	}
	if len(read.Worktrees) != 2 {
		t.Fatalf("want 2 worktrees, got %+v", read.Worktrees)
	}
	if pane := read.Worktrees[1].PaneID; pane == nil || *pane != "w2:p1" {
		t.Errorf("pane_id = %v, want w2:p1 — the pane is what makes this actable", pane)
	}

	// One unreadable repository costs the others nothing, and says why on its
	// own group. Its worktrees are null, not empty: "none" and "unreadable" are
	// the distinction this format exists to keep.
	if failed.Error == nil || !strings.Contains(*failed.Error, "not a git repository") {
		t.Errorf("error = %v, want the reason it could not be read", failed.Error)
	}
	if failed.Worktrees != nil {
		t.Errorf("worktrees = %+v, want null when the repository could not be read", failed.Worktrees)
	}
}

// The inverse, which is what keeps the two modes apart: a single-repository
// listing leaves the grouped field null rather than wrapping one group around
// itself.
func TestLsJSONLeavesTheGroupedFieldNull(t *testing.T) {
	server, repo, _ := fleet(t)

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, repo.RealRoot, "", nil, wt.Options{JSON: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	var document wt.ListJSON
	if err := json.Unmarshal(buf.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, buf.String())
	}
	if document.Repositories != nil {
		t.Errorf("repositories = %+v, want null when one repository was listed", document.Repositories)
	}
	if len(document.Worktrees) != 2 {
		t.Errorf("want the flat listing, got %+v", document.Worktrees)
	}
}

// One document on stdout, and the same rows the table would have printed.
func TestLsJSONWritesOneDocumentInsteadOfTheTable(t *testing.T) {
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
		"type":  "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "tab_id": "t1", "index": 0}},
	})

	var buf bytes.Buffer
	client := herdrapi.NewWithSocket(server.SocketPath)
	if err := wt.Ls(client, "", checkout, nil, wt.Options{JSON: true}, &buf); err != nil {
		t.Fatalf("Ls: %v", err)
	}

	var document wt.ListJSON
	if err := json.Unmarshal(buf.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, buf.String())
	}
	if len(document.Worktrees) != 2 {
		t.Fatalf("want 2 worktrees, got %d: %s", len(document.Worktrees), buf.String())
	}

	main, linked := document.Worktrees[0], document.Worktrees[1]
	if !main.Main || main.WorkspaceID != nil {
		t.Errorf("main checkout: %+v", main)
	}
	if linked.WorkspaceID == nil || *linked.WorkspaceID != "w2" {
		t.Errorf("workspace_id = %v, want w2", linked.WorkspaceID)
	}
	// The pane is what `dispatch --pane` takes, and the reason a consumer of
	// this listing can act on it rather than only display it.
	if linked.PaneID == nil || *linked.PaneID != "w2:p1" {
		t.Errorf("pane_id = %v, want w2:p1", linked.PaneID)
	}
	if linked.AgentStatus == nil || *linked.AgentStatus != "working" {
		t.Errorf("agent_status = %v, want working", linked.AgentStatus)
	}

	// One or the other, never both: an action's stdout is read back out of the
	// plugin log and parsed.
	if strings.Contains(buf.String(), "* main") {
		t.Errorf("the table was printed alongside the document:\n%s", buf.String())
	}
}

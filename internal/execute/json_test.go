package execute_test

import (
	"strings"
	"testing"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/reconcile"
)

// The table has one target column and picks the branch or the basename for it.
// A consumer needs the fields that cell was chosen from, or it has to guess a
// path back from a directory name.
func TestJSONCarriesTheActionTheTableCollapses(t *testing.T) {
	results := []execute.Result{{
		Status: execute.StatusPlanned,
		Action: reconcile.Action{
			Kind: reconcile.KindPrune, Path: "/repo/wt/done", Branch: "done",
			WorkspaceID: "w4", Reason: "merged into main",
		},
		Detail: "would remove: merged into main",
	}}

	got := execute.JSON(results)[0]

	if got.Status != "planned" || got.Kind != "prune" {
		t.Errorf("status/kind = %q/%q", got.Status, got.Kind)
	}
	if got.Target != "done" {
		t.Errorf("target = %q, want the same cell the table prints", got.Target)
	}
	if got.Path == nil || *got.Path != "/repo/wt/done" {
		t.Errorf("path = %v, want the checkout the table only shows the basename of", got.Path)
	}
	if got.WorkspaceID == nil || *got.WorkspaceID != "w4" {
		t.Errorf("workspace_id = %v, want w4", got.WorkspaceID)
	}
	// Reason is why it was planned; Detail is what became of it. The table has
	// room for one of them.
	if got.Reason == nil || *got.Reason != "merged into main" {
		t.Errorf("reason = %v", got.Reason)
	}
	if got.Detail != "would remove: merged into main" {
		t.Errorf("detail = %q", got.Detail)
	}
	if got.PaneID != nil || got.AgentName != nil {
		t.Errorf("a prune has no pane or agent; both must be null, got %+v", got)
	}
}

// The table escapes because it is drawn in a terminal. The document is data:
// an escaped path cannot be opened and an escaped branch cannot be checked out,
// so it carries what herdr and git actually said.
func TestJSONCarriesRawValuesTheTableEscapes(t *testing.T) {
	const branch = "evil‮hctap"

	results := []execute.Result{{
		Status: execute.StatusPlanned,
		Action: reconcile.Action{Kind: reconcile.KindPrune, Path: "/repo/wt/evil", Branch: branch},
		Detail: "merged into main; would delete branch " + branch,
	}}

	got := execute.JSON(results)[0]
	if got.Branch == nil || *got.Branch != branch {
		t.Errorf("branch = %v, want the raw name back", got.Branch)
	}
	if !strings.ContainsRune(got.Detail, '‮') {
		t.Errorf("detail = %q, want it unaltered", got.Detail)
	}

	if strings.ContainsRune(execute.Render(results), '‮') {
		t.Error("the table must still escape it")
	}
}

// "nothing to do" is a sentence. An empty list is the same fact for a consumer,
// and must be a list rather than null so `.results | length` works either way.
func TestJSONOfNothingIsAnEmptyList(t *testing.T) {
	got := execute.JSON(nil)
	if got == nil {
		t.Fatal("want an empty list, got null")
	}
	if len(got) != 0 {
		t.Errorf("want no entries, got %d", len(got))
	}
}

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/muster/internal/herdrtest"
)

// An unrecognised subcommand must fail. herdr records a plugin action that
// exits 0 as "succeeded", so returning nil here would report a command that did
// nothing as a success.
func TestUnknownCommandFails(t *testing.T) {
	for _, args := range [][]string{
		{"prune-typo"},
		{"--help"},
		{""},
		nil,
	} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}

func TestUsageNamesEveryCommand(t *testing.T) {
	for _, command := range []string{"ls", "sync", "prune", "prune-apply", "report", "on-event", "startup"} {
		if !strings.Contains(usage, command) {
			t.Errorf("usage does not mention %q: %s", command, usage)
		}
	}
}

// herdr runs plugin commands with cwd set to the plugin root, which is itself a
// git repository. A destructive command that falls back to the process cwd
// would therefore target this plugin's own checkout, so it must refuse instead.
func TestDestructiveCommandsRefuseWithoutContext(t *testing.T) {
	server := herdrtest.NewServer(t)

	for _, tc := range []struct {
		name string
		run  func(io.Writer) error
	}{
		{"sync starts agents", syncCommand},
		{"prune-apply removes worktrees", func(w io.Writer) error { return pruneCommand(w, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
			t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")

			err := tc.run(io.Discard)
			if err == nil {
				t.Fatal("expected a refusal when herdr supplied no context")
			}
			if !strings.Contains(err.Error(), "refusing to guess") {
				t.Errorf("error should explain the refusal, got %v", err)
			}
		})
	}
}

// A malformed context is a bug signal, and fatal even for a read-only command:
// treating it as absent would silently retarget the command.
func TestEveryCommandRejectsAMalformedContext(t *testing.T) {
	server := herdrtest.NewServer(t)

	for _, tc := range []struct {
		name string
		run  func(io.Writer) error
	}{
		{"ls", lsCommand},
		{"sync", syncCommand},
		{"prune", func(w io.Writer) error { return pruneCommand(w, false) }},
		{"prune-apply", func(w io.Writer) error { return pruneCommand(w, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
			t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"workspace_cwd":`)

			if err := tc.run(io.Discard); err == nil {
				t.Fatal("a malformed context must not be treated as absent")
			}
		})
	}
}

// fakeSession points the commands at a fake herdr and a real repository, the
// way herdr would when it invokes the plugin.
func fakeSession(t *testing.T, repo *herdrtest.Repo) *herdrtest.Server {
	t.Helper()

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"workspace_cwd":"`+repo.Root+`"}`)
	// Point plugin state somewhere disposable. Inherited, it would be the real
	// ~/.local/state/herdr/plugins/steig.muster, and the suite would leave repository
	// locks in a developer's live plugin state. It happens to be unset in a
	// normal shell, which is luck rather than isolation.
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	return server
}

// worktreeListReply builds a worktree.list reply for one linked checkout.
func worktreeListReply(repo *herdrtest.Repo, checkout, branch, workspaceID string) map[string]any {
	entry := map[string]any{
		"path": checkout, "label": filepath.Base(checkout), "branch": branch,
		"is_bare": false, "is_detached": false, "is_prunable": false,
		"is_linked_worktree": true,
	}
	if workspaceID != "" {
		entry["open_workspace_id"] = workspaceID
	}
	return map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{entry},
	}
}

func workspaceListReply(repo *herdrtest.Repo, checkout, workspaceID string) map[string]any {
	return map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{
		"workspace_id": workspaceID, "number": 2, "label": "wt", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "t1", "agent_status": "idle",
		"worktree": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "checkout_path": checkout, "is_linked_worktree": true},
	}}}
}

// The exit-code class: a command whose actions all failed must not report
// success just because it printed the failures.
func TestSyncFailsWhenAnActionFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "wip", "w2"))
	server.HandleResult("workspace.list", workspaceListReply(repo, checkout, "w2"))
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list",
		"panes": []map[string]any{{"pane_id": "w2:p1"}}})
	// The pane is still running direnv, so the agent cannot start.
	server.Handle("agent.start", func(map[string]any) (any, error) {
		return nil, errBusyPane{}
	})

	var out strings.Builder
	err := syncCommand(&out)
	if err == nil {
		t.Fatalf("sync should fail when staffing failed; output was:\n%s", out.String())
	}
	// The report still has to be printed, not swallowed by the error.
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("the failure should be reported, got:\n%s", out.String())
	}
}

type errBusyPane struct{}

func (errBusyPane) Error() string { return "pane is busy" }

func TestSyncSucceedsWhenNothingFails(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "repo",
			"repo_root": repo.Root, "source_checkout_path": repo.Root},
		"worktrees": []map[string]any{},
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := syncCommand(&out); err != nil {
		t.Fatalf("sync with nothing to do should succeed: %v", err)
	}
}

// `prune` is the safe half: it must list candidates and remove nothing.
func TestPruneListsWithoutRemoving(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Topology alone never prunes; an authoritative PR verdict does.
	herdrtest.FakeGh(t, `echo '{"state":"MERGED"}'`)

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(&out, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if !strings.Contains(out.String(), "would remove") {
		t.Errorf("prune should list candidates, got:\n%s", out.String())
	}
	if !repo.Exists(checkout) {
		t.Fatal("prune removed a worktree; it must only list")
	}
	for _, call := range server.Calls() {
		if call.Method == "worktree.remove" {
			t.Error("prune asked herdr to remove a worktree")
		}
	}
}

// Asking to prune must not open workspaces or start agents on the way past.
func TestPruneDoesNotAdoptOrStaff(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("fresh", "fresh")

	server := fakeSession(t, repo)
	// The worktree has no workspace, so a full plan would adopt it.
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "fresh", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(&out, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, call := range server.Calls() {
		switch call.Method {
		case "worktree.open", "agent.start":
			t.Errorf("prune performed %s as a side effect", call.Method)
		}
	}
}

// And the reverse: sync must not remove anything.
func TestSyncDoesNotPrune(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(&out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !repo.Exists(checkout) {
		t.Fatal("sync removed a worktree")
	}
}

// herdr's own worktree creation puts checkouts under ~/.herdr/worktrees/<repo>/,
// outside the repository entirely, so the .claude/worktrees convention the rest
// of these tests use is only one of the shapes that reaches the reconciler. The
// claim under test is that path layout is irrelevant: workspaces are matched by
// repo_root equality, never by containment.
func TestSyncAdoptsAWorktreeOutsideTheRepoRoot(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	outside := filepath.Join(t.TempDir(), "worktree-brave-valley-66f8")
	checkout := repo.AddWorktreeAt(outside, "worktree/brave-valley-66f8")

	if strings.HasPrefix(checkout, repo.Root) {
		t.Fatalf("checkout %s is inside the repository root %s; this test proves nothing", checkout, repo.Root)
	}

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "worktree/brave-valley-66f8", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("worktree.open", map[string]any{"type": "workspace_created"})

	var out strings.Builder
	if err := syncCommand(&out); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, call := range server.Calls() {
		if call.Method != "worktree.open" {
			continue
		}
		if path, _ := call.Params["path"].(string); path != checkout {
			t.Errorf("adopted %q, want the out-of-root checkout %q", path, checkout)
		}
		return
	}
	t.Errorf("an out-of-root worktree was never adopted; output:\n%s", out.String())
}

func TestPruneApplyRemoves(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Topology alone never prunes; an authoritative PR verdict does.
	herdrtest.FakeGh(t, `echo '{"state":"MERGED"}'`)

	server := fakeSession(t, repo)
	server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

	var out strings.Builder
	if err := pruneCommand(&out, true); err != nil {
		t.Fatalf("prune-apply: %v", err)
	}
	if repo.Exists(checkout) {
		t.Error("prune-apply should have removed the worktree")
	}
}

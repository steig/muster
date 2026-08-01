package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
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
	for _, command := range commands {
		if !strings.Contains(usage, command) {
			t.Errorf("usage does not mention %q: %s", command, usage)
		}
	}
}

// The half the list above cannot prove on its own: that every name usage
// advertises actually dispatches. These run for real and are expected to fail —
// there is no herdr and no flags — so the assertion is only that they failed for
// some reason OTHER than not existing.
//
// `list` is checked separately because it is an alias, reachable from the switch
// but deliberately absent from usage.
func TestEveryAdvertisedCommandDispatches(t *testing.T) {
	for _, command := range append(append([]string{}, commands...), "list") {
		t.Run(command, func(t *testing.T) {
			err := run([]string{command}, io.Discard)
			if err != nil && strings.Contains(err.Error(), "unknown command") {
				t.Errorf("usage advertises %q but run does not dispatch it: %v", command, err)
			}
		})
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
		{"prune-apply removes worktrees", func(w io.Writer) error { return pruneCommand(nil, w, true) }},
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
		{"prune", func(w io.Writer) error { return pruneCommand(nil, w, false) }},
		{"prune-apply", func(w io.Writer) error { return pruneCommand(nil, w, true) }},
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
	// ~/.local/state/herdr/plugins/steig.worktender, and the suite would leave repository
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
	if err := pruneCommand(nil, &out, false); err != nil {
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
	if err := pruneCommand(nil, &out, false); err != nil {
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
	if err := pruneCommand(nil, &out, true); err != nil {
		t.Fatalf("prune-apply: %v", err)
	}
	if repo.Exists(checkout) {
		t.Error("prune-apply should have removed the worktree")
	}
}

// The dry run exists to be read before the apply, so both must say which
// repository they resolved. They do not resolve it the same way — listing may
// fall back to the working directory and applying may not — and that asymmetry
// once let a `prune` listing six worktrees be followed by a `prune-apply`
// reporting "nothing to do" about a different root, with nothing in either
// output to show they had disagreed.
func TestBothPruneHalvesNameTheRepositoryTheyResolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
	}{
		{"prune", false},
		{"prune-apply", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := herdrtest.NewRepo(t)
			checkout := repo.AddWorktree("done", "done")
			repo.CommitIn(checkout, "done.txt", "work")
			repo.Git("merge", "--no-ff", "-m", "merge done", "done")
			herdrtest.FakeGh(t, `echo '{"state":"MERGED"}'`)

			server := fakeSession(t, repo)
			server.HandleResult("worktree.list", worktreeListReply(repo, checkout, "done", ""))
			server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
			server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})

			var out strings.Builder
			if err := pruneCommand(nil, &out, tc.apply); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if !strings.Contains(out.String(), "repository: ") {
				t.Fatalf("%s must name the repository it acted on, got:\n%s", tc.name, out.String())
			}
			if !strings.Contains(out.String(), repo.RealRoot) {
				t.Errorf("%s named a root other than %s:\n%s", tc.name, repo.RealRoot, out.String())
			}
		})
	}
}

// --- --repo ------------------------------------------------------------------------------
// An action carries no arguments, so herdr's context is the only thing prune can resolve
// from — and that context names herdr's current workspace, not the repository the operator
// is standing in. Observed live: a dry run inside a repository with four staffed worktrees
// planned against a different project entirely.

func TestPruneActsOnTheNamedRepositoryRatherThanHerdrsContext(t *testing.T) {
	current := herdrtest.NewRepo(t) // what herdr thinks is current
	named := herdrtest.NewRepo(t)   // what the operator asked for

	server := fakeSession(t, current)
	server.HandleResult("worktree.list", map[string]any{
		"type": "worktree_list",
		"source": map[string]any{"repo_key": "k", "repo_name": "named",
			"repo_root": named.Root, "source_checkout_path": named.Root},
		"worktrees": []map[string]any{},
	})
	server.HandleResult("workspace.list", map[string]any{"type": "workspace_list", "workspaces": []map[string]any{}})
	server.HandleResult("agent.list", map[string]any{"type": "agent_list", "agents": []map[string]any{}})
	server.HandleResult("pane.list", map[string]any{"type": "pane_list", "panes": []map[string]any{}})

	var out bytes.Buffer
	if err := run([]string{"prune", "--repo", named.Root}, &out); err != nil {
		t.Fatalf("prune --repo: %v", err)
	}

	want := "repository: " + gitx.Resolve(named.Root)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected %q in output, got:\n%s", want, out.String())
	}
	// The header is the whole safety property: it must not name the one it did not act on.
	if strings.Contains(out.String(), gitx.Resolve(current.Root)) {
		t.Fatalf("named repository was ignored in favour of herdr's context:\n%s", out.String())
	}
}

func TestPruneRefusesARepoThatIsNotOne(t *testing.T) {
	// Never a fallback. The point of naming a repository is to stop the resolution from
	// wandering, so a bad path has to stop rather than quietly land somewhere plausible.
	current := herdrtest.NewRepo(t)
	fakeSession(t, current)

	var out bytes.Buffer
	err := run([]string{"prune", "--repo", t.TempDir()}, &out)
	if err == nil {
		t.Fatalf("expected an error for a non-repository, got output:\n%s", out.String())
	}
	if strings.Contains(out.String(), gitx.Resolve(current.Root)) {
		t.Fatalf("fell back to herdr's context instead of failing:\n%s", out.String())
	}
}

func TestPruneRejectsAStrayArgument(t *testing.T) {
	// `prune /some/path` is the shape someone reaches for before finding the flag. Taking
	// it silently would act on the context while looking like it acted on the path.
	fakeSession(t, herdrtest.NewRepo(t))

	var out bytes.Buffer
	if err := run([]string{"prune-apply", "/tmp/somewhere"}, &out); err == nil {
		t.Fatal("expected a stray positional argument to be refused")
	} else if !strings.Contains(err.Error(), "prune-apply") {
		t.Fatalf("the error should name the half it came from, got: %v", err)
	}
}

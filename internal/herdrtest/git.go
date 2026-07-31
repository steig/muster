package herdrtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a real git repository in a temp directory.
type Repo struct {
	Root string
	t    *testing.T
}

// NewRepo creates a git repository with one commit on `main`. It is real git,
// not a fake: worktree layout is exactly what `wt` has to read in production.
func NewRepo(t *testing.T) *Repo {
	t.Helper()

	root := t.TempDir()
	// macOS /var is a symlink to /private/var; git reports resolved paths, so
	// resolve up front or every path comparison in a test is off by a prefix.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	r := &Repo{Root: root, t: t}
	r.Git("init", "-b", "main")
	r.Git("config", "user.email", "test@example.com")
	r.Git("config", "user.name", "Test")
	r.Write("README.md", "# test\n")
	r.Git("add", ".")
	r.Git("commit", "-m", "initial")
	return r
}

// Git runs a git command in the repository root and fails the test on error.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return r.GitIn(r.Root, args...)
}

// GitIn runs a git command in dir.
func (r *Repo) GitIn(dir string, args ...string) string {
	r.t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A developer's own git config (signing keys, hooks, templates) must not
	// reach into the test repository.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write creates a file relative to the repository root.
func (r *Repo) Write(rel, content string) {
	r.t.Helper()

	path := filepath.Join(r.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

// AddWorktree creates a linked worktree on a new branch at the path `wt` uses:
// <root>/.claude/worktrees/<slug>. It returns the checkout path.
func (r *Repo) AddWorktree(slug, branch string) string {
	r.t.Helper()

	path := filepath.Join(r.Root, ".claude", "worktrees", slug)
	r.Git("worktree", "add", "-b", branch, path)
	return path
}

// FakeGh puts a stub `gh` on PATH for the duration of the test. The script is a
// shell body; write to stdout to fake a response.
func FakeGh(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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
	// Root is the path as a caller would hand it in, symlinks and all. On
	// macOS t.TempDir() lives under /var, which is a symlink to /private/var,
	// so this is deliberately NOT the resolved path: production code is given
	// unresolved paths all the time and has to cope.
	Root string
	// RealRoot is Root with symlinks expanded — what git itself reports.
	RealRoot string
	t        *testing.T
}

// NewRepo creates a git repository with one commit on `main`. It is real git,
// not a fake: worktree layout is exactly what `wt` has to read in production.
//
// Root is handed out unresolved on purpose. Resolving it here would paper over
// exactly the class of bug where code compares an unresolved caller path
// against a resolved git path and silently concludes they are different.
func NewRepo(t *testing.T) *Repo {
	t.Helper()

	root := t.TempDir()
	realRoot := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = resolved
	}

	r := &Repo{Root: root, RealRoot: realRoot, t: t}
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

// AddWorktreeAt creates a linked worktree at an arbitrary path, which may sit
// outside the repository root.
//
// The other constructors both build under <root>/.claude/worktrees/, which is
// only one of the layouts in use: herdr's own worktree creation puts checkouts
// under ~/.herdr/worktrees/<repo>/, entirely outside the repository. Nothing in
// the reconciler is supposed to care — workspaces are matched by repo_root
// equality, never by path containment — but until this existed, no test had ever
// executed that shape.
func (r *Repo) AddWorktreeAt(path, branch string) string {
	r.t.Helper()

	r.Git("worktree", "add", "-b", branch, path)
	return path
}

// CommitIn writes a file inside a checkout and commits it there.
func (r *Repo) CommitIn(dir, rel, content string) {
	r.t.Helper()

	WriteFile(r.t, filepath.Join(dir, rel), content)
	r.GitIn(dir, "add", rel)
	r.GitIn(dir, "commit", "-m", "add "+rel)
}

// SetOriginHead fakes an origin whose HEAD points at branch, so BaseRef has
// something to read. No network is involved: the refs are created directly.
func (r *Repo) SetOriginHead(branch string) {
	r.t.Helper()

	r.Git("update-ref", "refs/remotes/origin/"+branch, "HEAD")
	r.Git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

// Exists reports whether a path is still on disk.
func (r *Repo) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// BranchMissing reports whether a local branch is gone.
func (r *Repo) BranchMissing(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	cmd.Dir = r.Root
	return cmd.Run() != nil
}

// WriteFile writes a file, creating parent directories.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// AddWorktreeFrom creates a linked worktree on a new branch forked from start,
// which need not be the base branch.
func (r *Repo) AddWorktreeFrom(slug, branch, start string) string {
	r.t.Helper()

	path := filepath.Join(r.Root, ".claude", "worktrees", slug)
	r.Git("worktree", "add", "-b", branch, path, start)
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

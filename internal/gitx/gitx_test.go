package gitx_test

import (
	"path/filepath"
	"testing"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrtest"
)

func TestRepoRootFromLinkedWorktree(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	got, err := gitx.RepoRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	// git reports resolved paths, and so must we: repo.Root is handed out
	// with its symlinks intact.
	if got != repo.RealRoot {
		t.Errorf("RepoRoot(%s) = %s, want %s", checkout, got, repo.RealRoot)
	}
}

func TestRepoRootOutsideGitFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := gitx.RepoRoot(filepath.Clean(dir)); err == nil {
		t.Skip("temp dir is inside a git repository on this machine")
	}
}

// Without an origin there is nothing to ask, and main is the only sane guess.
func TestBaseRefFallsBackToMain(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	if got := gitx.BaseRef(repo.Root); got != "main" {
		t.Errorf("BaseRef = %q, want main", got)
	}
}

// The base ref is develop on some repositories. It must be read, never assumed.
func TestBaseRefFollowsOriginHead(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.SetOriginHead("develop")

	if got := gitx.BaseRef(repo.Root); got != "origin/develop" {
		t.Errorf("BaseRef = %q, want origin/develop", got)
	}
}

func TestIsDirty(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	if gitx.IsDirty(checkout) {
		t.Error("a fresh worktree should be clean")
	}

	// An untracked file is uncommitted work too: it exists nowhere else.
	herdrtest.WriteFile(t, filepath.Join(checkout, "scratch.txt"), "wip\n")
	if !gitx.IsDirty(checkout) {
		t.Error("untracked file should count as dirty")
	}
}

func TestOwnCommitsCountsOnlyNewWork(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("feature", "feature")

	if got := gitx.OwnCommits(checkout, "main"); got != 0 {
		t.Errorf("a branch with no commits should have 0 own commits, got %d", got)
	}

	repo.CommitIn(checkout, "a.txt", "one")
	repo.CommitIn(checkout, "b.txt", "two")
	if got := gitx.OwnCommits(checkout, "main"); got != 2 {
		t.Errorf("OwnCommits = %d, want 2", got)
	}
}

// Guard c's reason for existing, against real git: a worktree created seconds
// ago is an ancestor of base, and must NOT be reported as merged.
func TestIsMergedIntoRejectsUnstartedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	repo.AddWorktree("fresh", "fresh")

	if gitx.IsMergedInto(repo.Root, "fresh", "main") {
		t.Error("a branch with no commits of its own must not count as merged")
	}
}

// The other half of the same distinction: a branch that really was merged into
// base has to be detected, even though it also has zero commits base lacks.
func TestIsMergedIntoDetectsMergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("done", "done")
	repo.CommitIn(checkout, "feature.txt", "shipped")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	if !gitx.IsMergedInto(repo.Root, "done", "main") {
		t.Error("a branch merged into base must count as merged")
	}
	// And the naive commit count cannot tell the two apart, which is the
	// whole reason IsMergedInto looks at the first-parent trunk.
	if got := gitx.OwnCommits(checkout, "main"); got != 0 {
		t.Errorf("after a merge the branch has 0 commits base lacks, got %d", got)
	}
}

func TestIsMergedIntoRejectsUnmergedBranch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("wip", "wip")
	repo.CommitIn(checkout, "wip.txt", "in progress")

	if gitx.IsMergedInto(repo.Root, "wip", "main") {
		t.Error("an unmerged branch must not count as merged")
	}
}

// A fast-forward merge moves base's pointer to the branch tip, so the tip
// becomes a first-parent trunk commit — exactly what a branch that never
// committed also looks like. This test pins that indistinguishability: both
// states produce the same two answers, so no caller may claim to tell them
// apart.
func TestFastForwardMergeIsIndistinguishableFromUnstarted(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	merged := repo.AddWorktree("ff", "ff")
	repo.CommitIn(merged, "ff.txt", "work")
	repo.Git("merge", "--ff-only", "ff")

	unstarted := repo.AddWorktree("fresh", "fresh")

	for _, tc := range []struct {
		name     string
		branch   string
		checkout string
	}{
		{"fast-forward merged", "ff", merged},
		{"never started", "fresh", unstarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gitx.IsMergedInto(repo.Root, tc.branch, "main") {
				t.Error("a tip on base's trunk cannot be reported as merged")
			}
			if got := gitx.OwnCommits(tc.checkout, "main"); got != 0 {
				t.Errorf("OwnCommits = %d, want 0", got)
			}
		})
	}
}

// A branch forked off already-merged work inherits a tip that sits off base's
// trunk, so the topological test calls it merged even though the branch has
// done nothing. Same commit, two branches: graph shape cannot separate them.
func TestBranchForkedOffMergedWorkLooksMerged(t *testing.T) {
	repo := herdrtest.NewRepo(t)

	done := repo.AddWorktree("done", "done")
	repo.CommitIn(done, "done.txt", "work")
	repo.Git("merge", "--no-ff", "-m", "merge done", "done")

	// Fork a brand-new branch from the merged branch's tip.
	fresh := repo.AddWorktreeFrom("later", "later", "done")

	if !gitx.IsMergedInto(repo.Root, "later", "main") {
		t.Skip("topology no longer reports this as merged; the guard below is moot")
	}
	// The branch has no work of its own, which is what makes acting on the
	// topological verdict alone unsafe.
	if got := gitx.OwnCommits(fresh, "main"); got != 0 {
		t.Errorf("OwnCommits = %d, want 0", got)
	}
}

// A squash merge rewrites the commit, so the branch is not an ancestor of base
// at all. git cannot see it as merged; only the PR state can.
func TestIsMergedIntoMissesSquashMerge(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("squashed", "squashed")
	repo.CommitIn(checkout, "sq.txt", "work")
	repo.Git("merge", "--squash", "squashed")
	repo.Git("commit", "-m", "squashed work")

	if gitx.IsMergedInto(repo.Root, "squashed", "main") {
		t.Error("a squash merge leaves the branch unmerged as far as git is concerned")
	}
}

// IsDirty returns true when git cannot be run at all. The fail-safe direction:
// a checkout that cannot be read must never be reported as clean, because clean
// is what authorises removing it.
func TestIsDirtyTreatsAnUnreadableCheckoutAsDirty(t *testing.T) {
	if !gitx.IsDirty(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("an unreadable checkout must read as dirty; clean is what lets it be deleted")
	}
}

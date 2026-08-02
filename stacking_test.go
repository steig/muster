package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// The stacking guidance — docs/dispatch.md, skills/coordinator/SKILL.md,
// skills/worktrees/SKILL.md — tells a coordinator which rebase to run on a
// branch stacked on another branch's pull request. It shipped once saying that
// while the parent is unmerged "an ordinary `git rebase origin/main` is enough
// — the child's own commits are the only ones it has to replay". Two people
// read that line and agreed with it. It is false, and what caught it was
// somebody building a scratch repository and running it (#109).
//
// So the guidance is executed here rather than reviewed. These tests are that
// scratch repository: they run the commands the documents recommend and count
// what git actually replays.

// stack builds the shape the guidance is about: a trunk, a parent branch with
// two commits, a child forked from the parent with one, and then a trunk that
// has moved on — which is the only condition under which any of this bites.
// The trunk's new commit writes trunkFile, so a caller can decide whether the
// parent's rebase will conflict.
//
// It returns the fork point, the parent tip the child was forked from. That is
// the sha `start` prints and the argument `--onto` needs.
func stack(t *testing.T, r *herdrtest.Repo, trunkFile, trunkContent string) string {
	t.Helper()

	r.Git("checkout", "-b", "parent")
	r.Write("shared.txt", "parent edit\n")
	r.Git("add", ".")
	r.Git("commit", "-m", "parent commit 1")
	r.Write("parent2.txt", "p2\n")
	r.Git("add", ".")
	r.Git("commit", "-m", "parent commit 2")
	forkPoint := r.Git("rev-parse", "HEAD")

	r.Git("checkout", "-b", "child")
	r.Write("child1.txt", "c1\n")
	r.Git("add", ".")
	r.Git("commit", "-m", "child commit 1")

	r.Git("checkout", "main")
	r.Write(trunkFile, trunkContent)
	r.Git("add", ".")
	r.Git("commit", "-m", "trunk moved on")

	r.Git("checkout", "child")
	return forkPoint
}

// gitFails runs git where the test expects it not to succeed, which Repo.Git
// cannot express: it fails the test on a non-zero exit.
func gitFails(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git %s succeeded, and the test needs it not to:\n%s",
			strings.Join(args, " "), out)
	}
	return string(out)
}

// The claim that shipped. `git rebase <trunk>` replays everything the child has
// that the trunk does not, and while the parent is unmerged that is the
// parent's commits as well as the child's — rewritten, under the child's name.
func TestPlainRebaseOnAStackedChildReplaysTheParentsCommits(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	stack(t, repo, "trunk2.txt", "t2\n")

	repo.Git("rebase", "main")

	if replayed := repo.Git("rev-list", "--count", "main..child"); replayed != "3" {
		t.Errorf("commits on the child after the rebase = %s, want 3 (the child's one and the parent's two)", replayed)
	}
	// The parent branch still holds the originals, so what the child now has
	// are copies of commits it does not own, and it is no longer stacked on
	// anything. A conflict in either of them belongs to whoever ran this.
	gitFails(t, repo.Root, "merge-base", "--is-ancestor", "parent", "child")
}

// What the guidance says to do instead, before the parent merges: rebase the
// parent onto the trunk, then move the child onto the parent's new tip, naming
// the fork point. One commit is replayed, and it is the child's.
func TestRestackingOntoTheParentReplaysOnlyTheChildsCommits(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	forkPoint := stack(t, repo, "trunk2.txt", "t2\n")

	repo.Git("checkout", "parent")
	repo.Git("rebase", "main")
	repo.Git("checkout", "child")
	repo.Git("rebase", "--onto", "parent", forkPoint)

	if replayed := repo.Git("rev-list", "--count", "parent..child"); replayed != "1" {
		t.Errorf("commits the child holds beyond the parent = %s, want 1", replayed)
	}
	if subject := repo.Git("log", "-1", "--format=%s"); subject != "child commit 1" {
		t.Errorf("the replayed commit is %q, want the child's own", subject)
	}
	// The commits under the child's own are the parent branch's, not copies of
	// them: the child is stacked on the parent rather than carrying it.
	if base, tip := repo.Git("rev-parse", "child~1"), repo.Git("rev-parse", "parent"); base != tip {
		t.Errorf("the child sits on %s, want the parent's tip %s", base, tip)
	}
}

// Why the guidance names `--onto` and the fork point rather than saying "rebase
// onto the parent". A bare `git rebase parent` replays the child's commits and
// the parent's old ones, and gets away with it only while git can match the
// rewritten patches. Let the parent's own rebase resolve a conflict and the
// match is gone: the child's rebase then stops inside a commit the parent
// wrote. `--onto` with the fork point never looks at those commits at all.
func TestRestackingWithoutTheForkPointReplaysWhatTheParentRewrote(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	forkPoint := stack(t, repo, "shared.txt", "trunk edit\n")

	repo.Git("checkout", "parent")
	gitFails(t, repo.Root, "rebase", "main")
	repo.Write("shared.txt", "resolved\n")
	repo.Git("add", "shared.txt")
	repo.Git("-c", "core.editor=true", "rebase", "--continue")

	repo.Git("checkout", "child")
	out := gitFails(t, repo.Root, "rebase", "parent")
	if !strings.Contains(out, "parent commit 1") {
		t.Errorf("the child's rebase was expected to stop in a commit the parent wrote:\n%s", out)
	}
	repo.Git("rebase", "--abort")

	repo.Git("rebase", "--onto", "parent", forkPoint)
	if replayed := repo.Git("rev-list", "--count", "parent..child"); replayed != "1" {
		t.Errorf("commits replayed with --onto = %s, want 1", replayed)
	}
}

// The other half of the guidance, which the fork point exists for: after the
// parent is squash-merged, `--onto` the trunk leaves the child holding its own
// commit and none of the parent's, so its pull request stops showing the
// parent's diff as its own.
func TestOntoTheTrunkAfterTheParentIsSquashMerged(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	forkPoint := stack(t, repo, "trunk2.txt", "t2\n")

	repo.Git("checkout", "main")
	repo.Git("merge", "--squash", "parent")
	repo.Git("commit", "-m", "parent slice (#42)")
	repo.Git("branch", "-D", "parent")

	repo.Git("checkout", "child")
	repo.Git("rebase", "--onto", "main", forkPoint)

	if replayed := repo.Git("rev-list", "--count", "main..child"); replayed != "1" {
		t.Errorf("commits the child holds beyond the trunk = %s, want 1", replayed)
	}
	if diff := repo.Git("diff", "--name-only", "main", "child"); diff != "child1.txt" {
		t.Errorf("the child's diff against the trunk is %q, want child1.txt alone", diff)
	}
}

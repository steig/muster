// Package gitx runs the git queries the worktree driver depends on.
//
// It is the only place that shells out to git, so the reconciler above it can
// stay a pure function over already-collected facts.
package gitx

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// run executes git in dir and returns trimmed stdout.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RepoRoot returns the absolute path of the MAIN worktree, even when called
// from a linked one. --git-common-dir points at the real .git for every linked
// checkout, so its parent is the main checkout.
//
// This matters because herdr keys worktrees by repository: asking from inside a
// linked worktree must return the same list as asking from the main one.
func RepoRoot(dir string) (string, error) {
	common, err := run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %s", dir)
	}
	if common == "" {
		return "", fmt.Errorf("git reported no common dir for %s", dir)
	}
	return Resolve(filepath.Dir(common)), nil
}

// Resolve normalises a path for comparison: absolute, cleaned, and with every
// symlink expanded.
//
// This is not cosmetic. git reports resolved paths, so a caller directory that
// still contains a symlink compares unequal to the same directory as git names
// it — and on macOS that is the normal case, since TMPDIR lives under /var,
// which is a symlink to /private/var. Comparing unresolved paths silently
// disarms the guard that refuses to delete the directory you are standing in.
//
// Paths that do not exist still resolve. EvalSymlinks fails outright on a
// missing path, and missing paths are routine here — a caller's directory may
// have been deleted, and a prune target is about to be — so the longest
// existing ancestor is resolved and the remainder re-appended. Without that,
// a path whose leaf is absent stays unresolved and compares unequal to the very
// directory it names.
func Resolve(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)

	remainder := ""
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path // reached the root without resolving anything
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// DefaultBase is used when a repository has no origin to ask.
const DefaultBase = "main"

// BaseRef is the branch new work forks from: whatever origin/HEAD points at,
// which is main on some repositories and develop on others. Never assume.
func BaseRef(root string) string {
	base, err := run(root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil || base == "" {
		return DefaultBase
	}
	return base
}

// IsDirty reports whether a checkout has uncommitted changes, including
// untracked files. Work that exists nowhere else must never be pruned.
func IsDirty(checkout string) bool {
	out, err := run(checkout, "status", "--porcelain")
	if err != nil {
		// An unreadable checkout is treated as dirty: refusing to delete is
		// always the safe direction.
		return true
	}
	return out != ""
}

// OwnCommits counts commits on the checkout's HEAD that base does not have.
//
// A branch with none of its own is trivially an ancestor of base, so callers
// use this to tell "not started" apart from "finished". An unresolvable base
// counts as zero, which keeps the worktree.
func OwnCommits(checkout, base string) int {
	out, err := run(checkout, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0
	}
	return n
}

// UpstreamGone reports that a branch was published and its remote counterpart
// has since been deleted.
//
// This is the one fact in this file that is not a graph shape. Every other
// signal here describes where commits sit; this one records that a person
// deleted a remote branch, which is what almost every branch-cleanup script
// actually keys on and what a merge button does by default.
//
// The distinction that makes it worth having is NEVER-PUBLISHED versus
// PUBLISHED-THEN-DELETED, and git keeps them apart. A branch with no configured
// upstream was never pushed anywhere; a branch whose `branch.<name>.merge` is
// still configured but whose remote-tracking ref has gone was pushed and then
// had that ref removed. Only the second returns true here. `git branch -vv`
// renders the same state as "[origin/foo: gone]".
//
// It establishes nothing on its own — a remote branch can be deleted without
// merging, which is abandonment rather than completion — so verdict pairs it
// with ancestry rather than acting on it alone.
//
// Requires the remote-tracking refs to be current. A stale ref that fetch has
// not pruned reads as still present, which keeps the worktree, so being out of
// date fails in the safe direction.
func UpstreamGone(root, branch string) bool {
	if branch == "" {
		return false
	}

	remote, err := run(root, "config", "--get", "branch."+branch+".remote")
	if err != nil || remote == "" {
		// Never published. Nothing was deleted, so nothing is gone.
		return false
	}
	merge, err := run(root, "config", "--get", "branch."+branch+".merge")
	if err != nil || merge == "" {
		return false
	}

	// branch.<name>.merge is a full ref on the remote (refs/heads/foo); the
	// tracking ref is that name under refs/remotes/<remote>/.
	tracking := remote + "/" + strings.TrimPrefix(merge, "refs/heads/")
	if _, err := run(root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+tracking); err != nil {
		return true
	}
	return false
}

// IsMergedInto reports that base absorbed the branch through a merge.
//
// "Ancestor of base" alone is not enough, and this is the subtle one. A branch
// created moments ago and never committed to is also an ancestor of base — it
// points straight at a commit on base's trunk. Both cases have zero commits
// base lacks, so counting commits cannot separate them either.
//
// What does separate them: a merged branch's tip is pulled in as the second
// parent of a merge commit, so it sits OFF base's first-parent trunk. An
// unstarted branch's tip is a trunk commit itself. Squash and rebase merges
// rewrite the commits, so the branch is not an ancestor at all and the PR state
// is the only signal — which is why callers check the PR first.
func IsMergedInto(root, branch, base string) bool {
	if !isAncestor(root, branch, base) {
		return false
	}
	tip, err := run(root, "rev-parse", branch)
	if err != nil {
		return false
	}

	onTrunk, ok := onFirstParentTrunk(root, base, tip)
	if !ok {
		// The walk failed, so nothing was established. Reporting "merged"
		// here would be a confident answer built on a broken read.
		return false
	}
	return !onTrunk
}

func isAncestor(root, branch, base string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", branch, base)
	cmd.Dir = root
	return cmd.Run() == nil
}

// onFirstParentTrunk reports whether sha is on base's first-parent history.
// The walk streams and stops at the first match, so a deep history costs no
// more than reaching the commit.
//
// ok is false when the walk could not be completed. A truncated read looks
// exactly like "not found", and callers must not mistake one for the other.
func onFirstParentTrunk(root, base, sha string) (found, ok bool) {
	cmd := exec.Command("git", "rev-list", "--first-parent", base)
	cmd.Dir = root

	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, false
	}
	if err := cmd.Start(); err != nil {
		return false, false
	}
	defer func() {
		_ = out.Close()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		if scanner.Text() == sha {
			return true, true
		}
	}
	if err := scanner.Err(); err != nil {
		return false, false
	}
	return false, true
}

// RemoteURL is origin's URL, or empty when there is no origin. `gh` needs it to
// target the right repository from inside a worktree.
func RemoteURL(root string) string {
	out, err := run(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return out
}

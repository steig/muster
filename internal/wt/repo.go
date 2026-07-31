package wt

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoRoot returns the absolute path of the MAIN worktree, even when called
// from a linked one. --git-common-dir points at the real .git for every linked
// checkout, so its parent is the main checkout.
//
// This matters because herdr keys worktrees by repository: asking from inside a
// linked worktree must return the same list as asking from the main one.
func RepoRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %s", dir)
	}

	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("git reported no common dir for %s", dir)
	}
	return filepath.Dir(common), nil
}

package gitx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steig/herdr-wt/internal/gitx"
)

// The macOS case that hides itself: /var is a symlink to /private/var, so a
// path handed in by a caller and the same path as git reports it are different
// strings until both are resolved.
func TestResolveExpandsSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if gitx.Resolve(link) != gitx.Resolve(real) {
		t.Errorf("Resolve(%s) = %s, want it to equal Resolve(%s) = %s",
			link, gitx.Resolve(link), real, gitx.Resolve(real))
	}
}

// A path whose leaf does not exist still has to resolve. Caller directories get
// deleted and prune targets are about to be, so refusing to resolve a missing
// path would leave exactly the comparisons that matter unresolved.
func TestResolveHandlesMissingPaths(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	missingViaLink := filepath.Join(link, "does", "not", "exist")
	missingViaReal := filepath.Join(real, "does", "not", "exist")

	if gitx.Resolve(missingViaLink) != gitx.Resolve(missingViaReal) {
		t.Errorf("Resolve did not resolve through a symlink to a missing path:\n %s\n %s",
			gitx.Resolve(missingViaLink), gitx.Resolve(missingViaReal))
	}
}

func TestResolveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	once := gitx.Resolve(dir)
	if twice := gitx.Resolve(once); twice != once {
		t.Errorf("Resolve is not idempotent: %s then %s", once, twice)
	}
}

func TestResolveEmptyStaysEmpty(t *testing.T) {
	if got := gitx.Resolve(""); got != "" {
		t.Errorf("Resolve(\"\") = %q, want empty", got)
	}
}

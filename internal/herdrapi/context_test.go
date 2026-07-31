package herdrapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/steig/muster/internal/herdrapi"
)

// An absent context is a state a read-only command may tolerate, so it has to
// be distinguishable from a broken one.
func TestLoadContextReportsAnAbsentContext(t *testing.T) {
	t.Setenv(herdrapi.ContextEnv, "")

	_, err := herdrapi.LoadContext()
	if !errors.Is(err, herdrapi.ErrNoContext) {
		t.Errorf("err = %v, want ErrNoContext", err)
	}
}

// A malformed context is a bug in what herdr sent. Returning an empty context
// would send callers to os.Getwd(), which for a plugin command is this plugin's
// own checkout — so a destructive command would target the wrong repository.
func TestLoadContextRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{
		`{"workspace_cwd":`,
		`not json at all`,
		`[]`,
		`{"workspace_cwd": 42}`,
	} {
		t.Setenv(herdrapi.ContextEnv, raw)

		ctx, err := herdrapi.LoadContext()
		if err == nil {
			t.Errorf("LoadContext(%q) returned no error", raw)
			continue
		}
		if errors.Is(err, herdrapi.ErrNoContext) {
			t.Errorf("LoadContext(%q) reported the context as absent, not malformed", raw)
		}
		if !strings.Contains(err.Error(), herdrapi.ContextEnv) {
			t.Errorf("error should name the variable, got %v", err)
		}
		if ctx.LaunchDir() != "" {
			t.Errorf("a malformed context must not yield a usable launch dir, got %q", ctx.LaunchDir())
		}
	}
}

func TestLoadContextParsesAValidContext(t *testing.T) {
	t.Setenv(herdrapi.ContextEnv,
		`{"workspace_cwd":"/repo","focused_pane_cwd":"/repo/wt/a",
		  "worktree":{"repo_key":"k","repo_name":"r","repo_root":"/repo",
		              "checkout_path":"/repo/wt/a","is_linked_worktree":true}}`)

	ctx, err := herdrapi.LoadContext()
	if err != nil {
		t.Fatal(err)
	}
	// The focused pane's directory wins over the workspace's.
	if got := ctx.LaunchDir(); got != "/repo/wt/a" {
		t.Errorf("LaunchDir = %q, want /repo/wt/a", got)
	}
	if got := ctx.RepoRoot(); got != "/repo" {
		t.Errorf("RepoRoot = %q, want /repo", got)
	}
}

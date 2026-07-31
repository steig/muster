package herdrapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ContextEnv is the environment variable herdr sets when it invokes a plugin
// command, describing what was focused at the time.
const ContextEnv = "HERDR_PLUGIN_CONTEXT_JSON"

// ErrNoContext reports that herdr injected no invocation context, which means
// the process was not started by herdr as a plugin command.
var ErrNoContext = errors.New(ContextEnv + " is not set; not running as a herdr plugin action")

// LoadContext reads the invocation context herdr injected.
//
// The two failure modes are deliberately distinct. An absent context means the
// command was run by hand, which read-only commands may reasonably tolerate. A
// malformed context means herdr sent something we cannot parse — a bug, never a
// state to paper over with an empty struct, because callers would then fall
// back to the process cwd, which for a plugin command is this plugin's own
// checkout.
func LoadContext() (PluginInvocationContext, error) {
	var ctx PluginInvocationContext

	raw := os.Getenv(ContextEnv)
	if raw == "" {
		return ctx, ErrNoContext
	}
	if err := json.Unmarshal([]byte(raw), &ctx); err != nil {
		return PluginInvocationContext{}, fmt.Errorf("malformed %s: %w", ContextEnv, err)
	}
	return ctx, nil
}

// LaunchDir is the directory the user invoked from: the focused pane's cwd,
// falling back to the workspace cwd.
//
// It is NOT the process working directory — herdr runs plugin commands with cwd
// set to the plugin root, so os.Getwd() would report this repository rather than
// the user's.
func (c PluginInvocationContext) LaunchDir() string {
	for _, candidate := range []*string{c.FocusedPaneCWD, c.WorkspaceCWD} {
		if candidate != nil && *candidate != "" {
			return *candidate
		}
	}
	return ""
}

// RepoRoot is the main checkout of the workspace's repository, when herdr
// already knows the workspace is a worktree. Empty when it does not, leaving
// the caller to derive it from LaunchDir with git.
func (c PluginInvocationContext) RepoRoot() string {
	if c.Worktree != nil {
		return c.Worktree.RepoRoot
	}
	return ""
}

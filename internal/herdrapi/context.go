package herdrapi

import (
	"encoding/json"
	"os"
)

// ContextEnv is the environment variable herdr sets when it invokes a plugin
// command, describing what was focused at the time.
const ContextEnv = "HERDR_PLUGIN_CONTEXT_JSON"

// LoadContext reads the invocation context herdr injected. A missing or
// malformed context yields an empty one: callers fall back to their own
// defaults rather than refusing to run.
func LoadContext() PluginInvocationContext {
	var ctx PluginInvocationContext
	if raw := os.Getenv(ContextEnv); raw != "" {
		_ = json.Unmarshal([]byte(raw), &ctx)
	}
	return ctx
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

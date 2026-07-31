package reconcile

import (
	"path/filepath"
	"strings"
)

// Slug lowercases a label and reduces it to [a-z0-9-], collapsing runs of
// separators. Used for branch and agent naming.
func Slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// agentNameMax is herdr's limit: names must match [a-z][a-z0-9_-]{0,31}.
const agentNameMax = 32

// AgentName coerces a slug into a name herdr will accept. A name that does not
// start with a letter gets a "wt-" prefix rather than being rejected.
func AgentName(slug string) string {
	if slug == "" {
		return "wt"
	}
	if c := slug[0]; c < 'a' || c > 'z' {
		return truncate("wt-"+slug, agentNameMax)
	}
	return truncate(slug, agentNameMax)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// TranscriptSlug is the directory name Claude Code stores a conversation under
// in ~/.claude/projects: the absolute checkout path with every "/" and "."
// replaced by "-". A worktree with one of these has a conversation worth
// resuming instead of starting cold.
func TranscriptSlug(checkoutPath string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(checkoutPath)
}

func baseName(path string) string {
	return filepath.Base(path)
}

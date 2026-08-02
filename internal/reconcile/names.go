package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/steig/worktender/internal/gitx"
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

// agentNameDigest is how many hex characters of the identity digest a name
// carries. Six is git's short-sha shape and sixteen million values, against a
// namespace holding the worktrees one person has open at once.
const agentNameDigest = 6

// agentNameLead goes in front of a name that would not start with a letter.
// Short on purpose: every character it spends is a character of branch the
// 32-character limit takes away, and an issue branch — the shape `start`
// produces — always needs it.
const agentNameLead = "wt-"

// AgentName derives the herdr agent name for one worktree of one repository.
//
// herdr's agent namespace is global, and it enforces uniqueness rather than
// resolving ambiguously: agent.start answers agent_name_taken and names the pane
// already holding the name. Measured against herdr protocol 18, whose schema
// says nothing about any of it. A name from a checkout basename or an issue
// number alone is therefore not merely ambiguous, it fails — two repositories
// with a worktree called `api`, or an issue #12 each, and whichever is staffed
// second gets no agent, under an error naming the other repository's pane.
//
// So the name carries a digest of the repository root and the whole label. It
// goes last because a truncated head is the second half of the same defect —
// two long branches sharing 25 characters converge otherwise — and a
// disambiguator at the front is the first thing the limit cuts.
//
// Changing the derivation changes the name `sync` re-staffs an existing worktree
// under. That is survivable because herdr frees a name when its pane goes away,
// so the only names that exist are the ones live agents hold.
func AgentName(repoRoot, label string) string {
	slug := Slug(label)

	head := slug
	if head == "" || head[0] < 'a' || head[0] > 'z' {
		head = agentNameLead + head
	}
	head = strings.Trim(truncate(head, agentNameMax-agentNameDigest-1), "-")

	return head + "-" + identity(repoRoot, slug)
}

// identity separates two worktrees the readable half of a name cannot. The
// repository root is resolved first because herdr does not promise one spelling
// of a directory, and two spellings of one repository must not be two agents.
func identity(repoRoot, slug string) string {
	sum := sha256.Sum256([]byte(gitx.Resolve(repoRoot) + "\x00" + slug))
	return hex.EncodeToString(sum[:])[:agentNameDigest]
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

// Package safetext holds the one definition of which runes are not allowed to
// reach a human's terminal as themselves.
//
// The class is the same wherever it appears — control, format, line and
// paragraph separators — but the policy is not. A note is rejected, because the
// worker can report again; a branch name is escaped, because nobody can retry a
// name that already exists and dropping the row would hide the worktree.
//
// One predicate, two policies.
package safetext

import (
	"fmt"
	"strings"
	"unicode"
)

// IsUnsafe reports whether r is a rune that can make text lie about itself.
// Cc and Zl/Zp end the line the text was framed on; Cf — U+202E among them —
// reorders what is drawn without changing what is stored, so the reader and the
// program disagree about which string they are looking at.
func IsUnsafe(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// Escape replaces every unsafe rune with a visible `\u{...}` so the row still
// appears and still says which string it is. The escape is deliberately not a
// stripping: a name with the runes removed renders as a legitimate name, which
// is the same spoof by a shorter route.
func Escape(s string) string {
	if !strings.ContainsFunc(s, IsUnsafe) {
		return s
	}

	var b strings.Builder
	for _, r := range s {
		if IsUnsafe(r) {
			fmt.Fprintf(&b, "\\u{%04X}", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

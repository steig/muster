// Package jsonout writes this plugin's machine-readable output.
//
// One encoder for every command, so a consumer that can read `ls --json` can
// read `doctor --json` without discovering that one of them escapes differently.
package jsonout

import (
	"encoding/json"
	"fmt"
	"io"
)

// Write encodes v as the whole of a command's stdout.
//
// HTML escaping is off. It is on by default, and it turns a branch name
// containing & or < into & or < — still valid JSON, still a surprise
// to everyone who greps the output or hands the value back to git.
//
// Indented, because this is a document someone will read with their eyes at
// least once while writing the consumer, and every parser reads it identically
// either way.
func Write(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// String is a *string for a field whose absence must arrive as null, nil when
// there is nothing to say. Absence is the whole reason this output exists: the
// table's "-" means no workspace, no pane, no agent and no pull request, and a
// consumer cannot tell those apart.
func String(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

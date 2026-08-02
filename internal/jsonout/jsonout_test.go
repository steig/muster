package jsonout_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/jsonout"
)

// encoding/json escapes &, < and > by default, on the assumption the document
// is going into a web page. A branch name is not, and `feat/a&b` arriving as
// `feat/a&b` is a name that no longer matches anything the consumer greps
// for or hands back to git.
func TestWriteDoesNotEscapeForHTML(t *testing.T) {
	var buf bytes.Buffer
	if err := jsonout.Write(&buf, map[string]string{"branch": "feat/a&b<c>"}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), `feat/a&b<c>`) {
		t.Errorf("want the branch name intact, got %s", buf.String())
	}
}

// A document is one line-terminated value on stdout. Without the newline a
// consumer reading line by line blocks on a document that is already complete.
func TestWriteEndsTheDocument(t *testing.T) {
	var buf bytes.Buffer
	if err := jsonout.Write(&buf, map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("want a trailing newline, got %q", buf.String())
	}
}

func TestStringIsNilForAbsence(t *testing.T) {
	if got := jsonout.String(""); got != nil {
		t.Errorf("empty must become null, got %q", *got)
	}
	if got := jsonout.String("w2"); got == nil || *got != "w2" {
		t.Errorf("String(%q) = %v", "w2", got)
	}
}

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The taxonomy is a contract a coordinator branches on, so the numbers
// themselves are the assertion. Renaming a constant is free; renumbering one
// silently reroutes every caller's decision, and this is what refuses to let
// that happen quietly.
func TestExitCodeValues(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"exitOK", exitOK, 0},
		{"exitUsage", exitUsage, 1},
		{"exitEnvironment", exitEnvironment, 2},
		{"exitNeedsHuman", exitNeedsHuman, 3},
		{"exitNoAnswer", exitNoAnswer, 4},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d; renumbering breaks every caller that branches on it", c.name, c.got, c.want)
		}
	}
}

func TestExitCodeOfNilIsOK(t *testing.T) {
	if got := exitCode(nil); got != exitOK {
		t.Errorf("exitCode(nil) = %d, want %d", got, exitOK)
	}
}

// An untagged error is exitEnvironment, not exitUsage. This is the decision the
// constant's comment argues for and the one most likely to be casually
// "corrected" later: defaulting to usage would tell a coordinator to rewrite a
// correct invocation every time an unforeseen failure appeared.
func TestUntaggedErrorIsEnvironment(t *testing.T) {
	if got := exitCode(errors.New("something nobody classified")); got != exitEnvironment {
		t.Errorf("untagged error = %d, want exitEnvironment (%d)", got, exitEnvironment)
	}
}

// The tag has to survive wrapping, because almost every site that raises one is
// several frames below the site that returns it.
func TestCodeSurvivesWrapping(t *testing.T) {
	err := codef(exitNeedsHuman, "worker is blocked")
	wrapped := fmt.Errorf("gate alpha: %w", fmt.Errorf("while waiting: %w", err))

	if got := exitCode(wrapped); got != exitNeedsHuman {
		t.Errorf("wrapped twice = %d, want exitNeedsHuman (%d)", got, exitNeedsHuman)
	}
	if !strings.Contains(wrapped.Error(), "worker is blocked") {
		t.Errorf("wrapping lost the message: %q", wrapped.Error())
	}
}

// The outermost tag wins. A caller re-tagging a lower layer's error is saying
// the failure means something different to *its* caller, and that judgement is
// better informed than the raising site's.
func TestOutermostTagWins(t *testing.T) {
	inner := codef(exitEnvironment, "socket refused")
	outer := withCode(exitUsage, fmt.Errorf("--target: %w", inner))

	if got := exitCode(outer); got != exitUsage {
		t.Errorf("re-tagged error = %d, want exitUsage (%d)", got, exitUsage)
	}
}

func TestWithCodeLeavesNilAlone(t *testing.T) {
	if err := withCode(exitNeedsHuman, nil); err != nil {
		t.Errorf("withCode(_, nil) = %v, want nil", err)
	}
}

// usagef is the shorthand the flag-parse sites use; it must not drift from the
// constant it stands for.
func TestUsagefIsExitUsage(t *testing.T) {
	if got := exitCode(usagef("bad flag")); got != exitUsage {
		t.Errorf("usagef = %d, want exitUsage (%d)", got, exitUsage)
	}
}

// Every command's argument handling answers exitUsage. Run through run() rather
// than against each parser, because run is what main hands to os.Exit and the
// wiring in between is exactly what could drop the tag.
func TestUnparseableArgumentsExitUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nonesuch"},
		{"ls", "--nonesuch"},
		{"ls", "stray"},
		{"sync", "--nonesuch"},
		{"prune", "stray"},
		{"doctor", "stray"},
		{"gate", "--nonesuch"},
		{"gate"},
		{"gate", "--target", "x", "--until", "nonesuch"},
		{"gate", "--target", "x", "--timeout", "0"},
		{"report", "--nonesuch"},
		{"dispatch", "--nonesuch"},
		{"dispatch", "--name", "x"},
		{"start", "--nonesuch"},
		{"start", "not-a-number"},
		{"start"},
	} {
		err := run(args, &strings.Builder{})
		if err == nil {
			t.Errorf("run(%q) succeeded; expected a usage error", args)
			continue
		}
		if got := exitCode(err); got != exitUsage {
			t.Errorf("run(%q) = exit %d (%v), want exitUsage (%d)", args, got, err, exitUsage)
		}
	}
}

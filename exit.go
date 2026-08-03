package main

import (
	"errors"
	"fmt"
)

// The exit codes, keyed to what the caller does next rather than to what went
// wrong. A coordinating agent is the caller this distinction exists for: it has
// four responses available, and one code per response is what lets it pick.
//
// Before this there were two, and `gate` returning 1 could mean the worker is
// blocked and needs a person, or that herdr's socket had died. Those demand
// opposite actions and were separable only by matching on stderr.
const (
	// exitOK is success, and the only code that means the work happened.
	exitOK = 0

	// exitUsage is the caller's mistake: an unknown flag, a missing argument, a
	// repository it declined to name. Retrying is pointless — it fails the same
	// way until the invocation changes.
	exitUsage = 1

	// exitEnvironment is the machine, not the call: herdr's socket unreachable,
	// git failing, gh unauthenticated.
	//
	// It is also the catch-all for anything unclassified, which is deliberate.
	// The alternative default is exitUsage, which would tell a coordinator to
	// rewrite a correct invocation, or exitNeedsHuman, which would wake somebody
	// for a bug. Sending an unrecognised failure here costs at worst a retry.
	exitEnvironment = 2

	// exitNeedsHuman is a stop that no amount of retrying clears, because the
	// answer is not the caller's to give: a worker reporting blocked, most of
	// all. Escalate; do not redispatch.
	exitNeedsHuman = 3

	// exitNoAnswer is the wait that ended with nothing: a gate timing out, a
	// pane dying before it reported. The work may simply be unfinished, so
	// redispatching is reasonable in a way it is not for exitNeedsHuman.
	exitNoAnswer = 4
)

// codedError carries the exit code its error should leave the process with.
//
// The code travels with the error rather than being decided at the top, because
// only the site that failed knows which of the four it was. main unwraps.
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// withCode tags an existing error. A nil error stays nil, so a caller may wrap
// unconditionally.
func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// codef builds a tagged error, for the sites that are constructing one anyway.
func codef(code int, format string, a ...any) error {
	return &codedError{code: code, err: fmt.Errorf(format, a...)}
}

// usagef is codef(exitUsage, ...). Its own name because it is much the most
// common tag — every flag-parse failure and every argument the parser did not
// want is one — and because a bare exitUsage at thirty call sites reads as a
// number rather than as the claim it is making.
func usagef(format string, a ...any) error {
	return &codedError{code: exitUsage, err: fmt.Errorf(format, a...)}
}

// exitCode is the code an error should leave the process with.
//
// The outermost tag wins: a caller that wraps a lower layer's error has more
// context about what the failure means to its own caller than the layer that
// raised it. Untagged errors are exitEnvironment, per the constant's comment.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return exitEnvironment
}

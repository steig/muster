package herdrtest

import (
	"encoding/json"
	"testing"
)

// The helpers below own the shape of a faked `gh pr list --json state` answer,
// so a test names the states it means and never a JSON literal. When the
// question changes shape again — as it did in #96, from an object answer to an
// array one — every fake moves with it, because there is only one of them.
//
// They are built on FakeGh, which stays for the call sites that need something
// other than an answer: an exit code, a message on stderr, a record of argv.

// ghPR is one entry of what `gh pr list --json state` answers with.
type ghPR struct {
	State string `json:"state"`
}

// FakeGhPRState fakes a branch carrying one pull request in the given state.
func FakeGhPRState(t *testing.T, state string) {
	t.Helper()

	FakeGhPRStates(t, state)
}

// FakeGhPRStates fakes a branch carrying several pull requests, listed in the
// order gh would hand them back.
func FakeGhPRStates(t *testing.T, states ...string) {
	t.Helper()

	FakeGh(t, GhPRScript(states...))
}

// FakeGhNoPR fakes the answer for a branch that was never opened as a pull
// request: an empty list, and a success. That is not the same as gh failing,
// and the two mean opposite things to whoever reads the verdict.
func FakeGhNoPR(t *testing.T) {
	t.Helper()

	FakeGhPRStates(t)
}

// FakeGhUnavailable fakes a gh that cannot answer at all — not installed, not
// logged in, no such repository. Use FakeGh directly when the test cares which
// of those it was, or what gh said about it.
func FakeGhUnavailable(t *testing.T) {
	t.Helper()

	FakeGh(t, "exit 1")
}

// GhPRScript is the shell body the FakeGhPR* helpers install, for the few call
// sites that need to fake a side effect alongside the answer.
func GhPRScript(states ...string) string {
	prs := make([]ghPR, len(states))
	for i, state := range states {
		prs[i].State = state
	}

	answer, err := json.Marshal(prs)
	if err != nil {
		panic("marshal fake gh answer: " + err.Error())
	}
	return "echo '" + string(answer) + "'"
}

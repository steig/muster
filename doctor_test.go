package main

import (
	"strings"
	"testing"

	"github.com/steig/worktender/internal/wt"
)

// The whole point of the events line is that it reports what the GATE decided,
// not what the variable says. A value nobody wrote a rule for leaves events off,
// and the notice that would normally say so is printed by a hook that will not
// fire — so this is the only place a mistyped opt-in becomes visible.
func TestDoctorReportsEventsAsTheGateParsesThem(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		state state
		says  string
	}{
		{"", stateOff, "unset"},
		{"1", stateOK, "on"},
		{"off", stateOff, "off"},
		{"ture", stateWarn, "recognise"},
		{"  ", stateOff, "unset"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(eventsEnv, tc.raw)

			got := eventsCheck()

			if got.state != tc.state {
				t.Errorf("state = %q, want %q", got.state, tc.state)
			}
			if line := got.value + " " + got.note; !strings.Contains(line, tc.says) {
				t.Errorf("%q should mention %q, got %q", tc.raw, tc.says, line)
			}
		})
	}
}

// A repository that does not use pull requests is entitled to no `gh` at all,
// so a missing or unauthenticated one must never read as a failure. It must not
// read as fine either: it silently costs prune everything a merged pull request
// would have authorised.
func TestDoctorTreatsMissingGhAsAWarningNotAFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	got := ghCheck()

	if got.state != stateWarn {
		t.Errorf("state = %q, want %q", got.state, stateWarn)
	}
	if !strings.Contains(got.note, "prune") {
		t.Errorf("the note must say what it costs, got %q", got.note)
	}
}

func TestDoctorSummarisesAgentsBusiestFirst(t *testing.T) {
	rows := []wt.Row{
		{AgentStatus: "idle"},
		{AgentStatus: "working"},
		{AgentStatus: "working"},
		{AgentStatus: "-"},
		{AgentStatus: ""},
	}

	if got, want := summariseAgents(rows), "2 working, 1 idle"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

// A worktree with no agent must not be counted as one with an unnamed status:
// "-" is what the listing prints for a workspace herdr has nothing for.
func TestDoctorCountsNoAgentsRatherThanBlankOnes(t *testing.T) {
	rows := []wt.Row{{AgentStatus: "-"}, {AgentStatus: ""}}

	if got, want := summariseAgents(rows), "no agents"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

func TestDoctorPluralisesWorktrees(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "0 worktrees"}, {1, "1 worktree"}, {3, "3 worktrees"}} {
		if got := plural(tc.n, "worktree"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

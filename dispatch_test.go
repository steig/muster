package main

import (
	"io"
	"strings"
	"testing"
)

// The reconciler must never route models or autonomy. `sync` runs from a
// keybinding and from event hooks where no role exists to route on, so an
// unattended pass is given no opinion about cost or permission.
func TestReconcilerNeverSetsAgentArgs(t *testing.T) {
	args, err := agentArgsFor("", "")
	if err != nil {
		t.Fatalf("agentArgsFor: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("unconfigured dispatch produced %q; staffing must stay bare unless asked", args)
	}
}

func TestModelAndPermissionModeBecomeAgentArgs(t *testing.T) {
	t.Setenv(permissionModeAck, "1")

	args, err := agentArgsFor("sonnet", "bypassPermissions")
	if err != nil {
		t.Fatalf("agentArgsFor: %v", err)
	}
	got := strings.Join(args, " ")
	if want := "--model sonnet --permission-mode bypassPermissions"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// A mode that stops the agent asking before it acts must not be grantable in
// passing. worktender cannot install a sandbox — `claude` takes no such flag,
// and this plugin does not write agent configuration — so the most it can do is
// refuse to remove prompting silently.
func TestUnsandboxedPermissionModeIsRefusedWithoutAcknowledgement(t *testing.T) {
	for mode := range unsandboxedModes {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(permissionModeAck, "")

			_, err := agentArgsFor("", mode)
			if err == nil {
				t.Fatalf("%s was granted with no sandbox and no acknowledgement", mode)
			}
			if !strings.Contains(err.Error(), permissionModeAck) {
				t.Errorf("the refusal must name the way through, got %q", err)
			}
		})
	}
}

// The acknowledgement is read the same way the events opt-in is, so nobody has
// to learn a second spelling — and, like that one, an unrecognised value is not
// a yes.
func TestAcknowledgementFailsClosedOnAnUnrecognisedValue(t *testing.T) {
	t.Setenv(permissionModeAck, "ture")

	if _, err := agentArgsFor("", "bypassPermissions"); err == nil {
		t.Error("a value no rule covers must not read as confirmation")
	}
}

// A mode that leaves prompting intact needs no acknowledgement: it grants
// nothing that was not already the default.
func TestOrdinaryPermissionModeNeedsNoAcknowledgement(t *testing.T) {
	t.Setenv(permissionModeAck, "")

	args, err := agentArgsFor("", "manual")
	if err != nil {
		t.Fatalf("a prompting mode must pass through freely: %v", err)
	}
	if strings.Join(args, " ") != "--permission-mode manual" {
		t.Errorf("args = %q", args)
	}
}

func TestDispatchRequiresPaneAndName(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--pane", "w2:p1"},
		{"--name", "worker"},
	} {
		if err := dispatchCommand(args, io.Discard); err == nil {
			t.Errorf("dispatch(%q) returned nil; both --pane and --name are required", args)
		}
	}
}

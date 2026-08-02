# Dispatching a worker, and waiting for it

This is what the plugin is for once more than one agent is involved: a
coordinating agent hands a slice of work to another and needs to know when it is
done — without reading everything that agent did to get there.

```sh
# Resolve the binary once. It is not on PATH; herdr owns the install.
# `worktender doctor` prints this line, so you need the jq only once.
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# COORDINATOR — when the slice is a GitHub issue, one command does all of it.
# --repo because start creates a checkout and is not a herdr action, so nothing
# tells it which repository unless you do. Flags go on either side of the number.
"$worktender" start 42 --repo . --model sonnet
# The agent name is not the branch: it carries a digest of the repository,
# because herdr's agent namespace is global. `start` prints this exact line.
"$worktender" gate --target wt-42-fix-the-thing-016aab --until done --require-pr --timeout 20m

# ...and when it is not, the pieces are still separate. Dispatch, then wait.
"$worktender" dispatch --pane w22:p1 --name reconcile-split --model sonnet
"$worktender" gate --target reconcile-split --until done --require-pr --timeout 20m

# WORKER — from inside its own pane:
"$worktender" report --status done --pr 42 --note "landed the reconcile split"
```

`start` is `worktree.create` + `dispatch` + a briefing **it confirms**, in that
order, against one issue. The briefing is typed and then submitted as a separate
Enter key event, and `start` waits for herdr to report the agent working before
it says "briefed": herdr answering ok means it delivered keystrokes, not that an
agent received a prompt, and a brief left sitting in a composer is a worker with
nothing to do that a listing reports as `idle`. It exists because the pane id the middle step needs came from
nowhere: `ls` did not print one, so the documented loop had a `<pane>` in it and
no command that produced it. `ls` prints panes now, and `start` does not need
you to look.

**`start` does not wait.** Starting five issues and then waiting on them one at
a time is the point; a start that gated would serialise the fleet.

The gate prints the report and exits 0 when the predicate holds. It exits
non-zero when the worker reports `blocked`, when the worker dies before
reporting, and when it times out.

```sh
worktender start <issue> [--model <model>] [--permission-mode <mode>] [--base <ref>] [--repo <path>] [--focus]
worktender dispatch --pane <id> --name <agent> [--model <model>] [--permission-mode <mode>] [--resume]
worktender report --status planned|blocked|done [--pr N] --note <text>
worktender gate --target <agent|pane> [--until done] [--require-pr] [--timeout 15m]
```

## Why dispatch is separate from `sync`

`sync` staffs a bare agent with no arguments, and that is deliberate. It runs
from a keybinding and from event hooks, where nothing knows what the work is —
so there is no role to route on, and giving an unattended reconciler an opinion
about which model to spend or how much autonomy to grant is how a hook that
fires on every new worktree quietly starts doing both. **`sync` stays dumb;
dispatch routes.**

Dispatch goes through the same executor `sync` does, which is what guarantees it
re-checks the pane before starting. `agent.start` against a pane that already
hosts an agent does not bounce off — it lands on a live conversation and
destroys context that exists nowhere else.

## The permission-mode problem, stated plainly

A dispatched worker has no human at its pane, so it stalls on the first
permission prompt and stays stalled — and a coordinating agent structurally
cannot clear it. That stall is the failure this command exists to prevent, so
`--permission-mode` passes through to the agent, including the modes that stop
it asking at all.

It comes with a real caveat rather than a reassurance:

**worktender cannot sandbox the agent it starts.** `claude` takes no sandbox
flag; sandboxing lives in settings.json, and this plugin does not write your
agent's configuration. So `bypassPermissions` and `acceptEdits` grant autonomy
*without* the boundary that should accompany it, and worktender **says so on
stderr** every time one is used rather than refusing it.

Give the worker a boundary that does not depend on command spelling — a sandbox
profile, or a separate uid. An allowlist does not substitute. A guard on
spelling only holds where the action has exactly one spelling: `$(...)` and
`find -exec` are never auto-allowed by a prefix rule because they can run
anything, and during this plugin's own development a worker denied
`Bash(herdr agent start:*)` reached a live agent anyway by calling herdr's
socket from Go, logging zero denials. Blocking the CLI blocked the convenient
path, not the capability.

An earlier version **refused** these modes unless `WORKTENDER_UNSANDBOXED_OK=1`
was exported. That is gone. It could not tell a caller who had built a sandbox
from one who had read the variable's name, so the confirmation proved nothing
while every unattended dispatch stalled on it. The warning survives; the gate
does not.

Nothing is defaulted. Without `--permission-mode`, dispatch changes nothing about
what an agent may do.

Details that bite:

- **Dispatch, then gate.** The gate ignores whatever the pane already held when it
  opened — that was a previous task's answer, and releasing on it is the stale
  hand-off the gate exists to prevent.
- **The worker reads `HERDR_PANE_ID` from its own environment and never asks.**
  Running `report` from another pane's shell files the report against *that* pane.
- **Outside herdr, `report` prints the envelope, warns on stderr, and exits 0.** A
  caller checking only the exit code sees success where nothing was delivered.
- **`--until` is repeatable** — pass it more than once to release on any of
  several statuses.
- **`--timeout` defaults to 15 minutes, and there is no wait-forever option.** A
  gate that cannot expire wedges a coordinator with no diagnosis, which is worse
  than no gate.
- A `--pr` that is not a positive integer is **fatal, not dropped**. A note over
  200 characters is **refused, not truncated** — shorten it and report again.

Report `blocked` when you are actually blocked: that releases the coordinator's
gate with a failure instead of making it wait out its clock, and the only party
who can unblock you is the one sitting in that wait.

## Why the report has no free text

**A report is three fixed slots**: a status from a closed set, an optional pull
request number, and a note capped at 200 characters. The shape is the feature,
not a limitation of it.

A worker's task usually arrived as a GitHub issue, whose body is written by anyone
who can file one, so a worker may be relaying a stranger's words — and a report is
therefore not a message the worker composes but slots the *coordinator* renders
into its own prompt. The note reaches the coordinator quoted and announced as
untrusted data, and the cap bounds how much of it can ever reach the context most
worth protecting.

The same reasoning bounds the gate. `--until` matches the status, `--require-pr`
matches the presence of the pull request number, and there is no way to write a
predicate over the note. A `--note-contains` would hand whoever wrote that issue
the decision of when the coordinator's next agent starts.

**What a gate does not establish is authorship.** It proves a well-formed report
appeared in the worker's pane after the gate started, and nothing about who
composed it. Any process already holding the herdr socket could write those slots
onto another pane. That is a limit to state rather than a hole to engineer around:
it needs code already running as you, so it crosses no privilege boundary — and a
shared secret would not close it either, because the dispatch prompt sits in the
worker's context beside the untrusted text, so anything that can talk the worker
into faking a report can read the secret out of the same context and include it.

Treat a `done` as a claim. Check the pull request it names.

## How a report actually reaches a gate

**Two channels, read on every look, and neither wins.**

`report` attaches the envelope to the worker's own pane as herdr metadata, and
also prints it. The gate reads the metadata *and* the pane's terminal buffer
each time it wakes.

Metadata exists because the pane alone never worked for the agent kind this is
used with: Claude Code collapses a finished tool call to `Ran 1 shell command`,
so a worker that *runs* `report` leaves the envelope in its transcript and
nothing on screen.

But metadata does not supersede the terminal, and that asymmetry was tried and
removed. A reader that stopped at metadata the moment it held anything left a
worker that reported `planned` over the tool call and finished by echoing `done`
permanently unreleased.

Three consequences worth knowing before you build on this:

- **A worker that merely prints a well-formed envelope has reported.** It need
  never run the command. The parser tolerates Claude Code's `⏺ ` decoration and
  arbitrary indentation, because one demanding the bare header at column zero
  read nothing at all from the pane of the very agent this exists to gate.
- **Identity is a per-channel counter, never content.** Two byte-identical
  reports are two reports, so a coordinator dispatching the same slice twice is
  heard twice — which content comparison would have made inaudible. The
  terminal's counter legitimately goes *down* as the buffer scrolls, and the
  gate follows it down on purpose.
- **Neither channel authenticates authorship**, only shape and position. See
  above, and [SECURITY.md](../SECURITY.md).

---

[← README](../README.md)

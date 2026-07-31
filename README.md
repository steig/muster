# muster

Drive git worktrees as [herdr](https://github.com/herdr/herdr) workspaces — list them,
adopt them, staff them with agents, and tear them down again.

Plenty of tools create worktrees. This one is mostly about the other end: knowing which
checkouts are finished, and removing them without ever removing one that wasn't.

```
$ herdr plugin action invoke ls --plugin steig.muster
* main                      w21  idle     muster
  feat/1-reconcile-execute  w22  working  1-reconcile-execute
  fix/257-erasure-comments  w1K  idle     257-erasure-comments
  worktree/brave-valley     -    -        brave-valley-66f8
```

Columns are branch, herdr workspace, agent status, and directory. `*` marks the
repository's main checkout; `-` means herdr has nothing for that worktree — the last row is
a checkout with no workspace and no agent, which is exactly what `sync` picks up.

## Requirements

- **herdr 0.7.0+** — this is a plugin; it talks to herdr over its local socket.
- **git**
- **gh** *(optional)* — only used to read pull request state. Without it, nothing is ever
  pruned, because a merged PR is the sole authority this plugin accepts for "finished".

## Install

```sh
herdr plugin install steig/muster
```

**A herdr plugin is not sandboxed.** This one runs as you, with your files, your shell and
your credentials, and what it does with them is start coding agents and delete git
worktrees. That is what it is *for* rather than a side effect — most of this README is
about the guards on the deleting half — but installing it is a decision to let code from
someone else's repository do those things on your machine, and it is worth making on
purpose. The two capabilities most worth knowing before you do: removal accepts a merged
pull request as its only authority and keeps anything ambiguous, and the hooks that would
start agents without being asked are off until you turn them on.

Installing runs `scripts/build.sh`, which prefers a local Go toolchain and falls back to a
prebuilt release binary, so it works with or without Go. On Windows the build needs Go on
`PATH`.

With Go present you get the stronger of the two paths by a distance: the binary is compiled
from the source that was just cloned, so what you can read is what you run.

Without Go, the script downloads the release matching the version in the manifest it
cloned — pinned to that tag rather than to `latest`, so reading `v0.1.0` and installing
cannot hand you something newer — and checks it against the `checksums.txt` published
alongside. A missing or mismatched checksum aborts the install rather than warning about
it, and no unverified download is left behind on any failure path.

That check proves the download arrived intact, and nothing beyond it. The binary and its
checksum come from the same release, so both are published by whoever can publish releases
here, and there is no signature and no attestation to say who that was. On the no-Go path
you are trusting this GitHub account rather than a proof of authorship. That is the same
trust nearly all software installed from GitHub asks for — which is a reason to say so
plainly, not a reason to imply the checksum is doing more work than it is.

For local development, from a checkout:

```sh
herdr plugin link .
```

Note that `link` points herdr at the working tree, so manifest edits take effect
immediately.

## Actions

| Action | What it does |
| --- | --- |
| `Muster: list worktrees` | Every worktree in the current repository, with its herdr workspace and agent status. |
| `Muster: sync worktrees` | Opens a workspace for any worktree that lacks one, and starts an agent in any workspace sitting idle as a bare shell. Never removes anything. |
| `Muster: prune (list)` | Reports which worktrees look finished and which were spared, and why. Changes nothing. |
| `Muster: prune (apply)` | Actually removes them. |

Output lands in `herdr plugin log list --plugin steig.muster`.

That is the whole of the herdr *action* surface, and not the whole of the plugin: `report`
and `gate` are commands rather than actions, for reasons covered below.

`prune` and `prune-apply` are two actions rather than one with a confirmation, because a
plugin action has no prompt surface — there is nowhere to ask "are you sure?". Splitting
them is the confirmation. It also means no stray keybinding can reach a removal.

## Reporting back, and waiting for it

Two more commands ship, and they are deliberately not in the table above, because they are
not herdr actions:

```sh
muster report --status planned|blocked|done [--pr N] --note <text>
muster gate --target <agent|pane> [--until done] [--require-pr] [--timeout 15m]
```

`report` is how a dispatched worker tells the coordinator that dispatched it where it got
to. `gate` is how that coordinator waits for one: it blocks until the worker's report
satisfies the predicate, prints the report, and releases.

Neither is registered as an action, for two reasons. An action is a fixed command array
with no argument surface, so a registered `gate` could only ever wait on one hard-coded
target. And an action's output lands in `herdr plugin log list` after it finishes, which is
exactly where a caller blocked on the answer is not looking. They are run directly instead,
from wherever herdr put the plugin:

```sh
muster=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.muster") | .plugin_root')/bin/muster
```

**A report is three fixed slots and no free text**: a status from a closed set, an optional
pull request number, and a note capped at 200 characters. The shape is the feature, not a
limitation of it. A worker's task usually arrived as a GitHub issue, whose body is written
by anyone who can file one, so a worker may be relaying a stranger's words — and a report
is therefore not a message the worker composes but slots the *coordinator* renders into its
own prompt. The note reaches the coordinator quoted and announced as untrusted data, and
the cap bounds how much of it can ever reach the context most worth protecting.

The same reasoning bounds the gate. `--until` matches the status, `--require-pr` matches
the presence of the pull request number, and there is no way to write a predicate over the
note. A `--note-contains` would hand whoever wrote that issue the decision of when the
coordinator's next agent starts.

The report travels as herdr pane metadata attached to the reporting worker's own pane, and
never into the coordinator's; a worker that could write into the coordinator's context at
will would have the injection surface back with this plugin doing the delivery. Outside
herdr there is no pane to attach to, and `report` says so on stderr rather than claiming a
delivery it did not make.

`gate` ignores whatever the pane already held when it opened — that was a previous task's
answer, and releasing on it is the stale hand-off the gate exists to prevent. It fails
rather than waits out its clock on a worker that reports `blocked`, since the only party
who can unblock it is the one sitting in the wait, and on a worker that dies before
reporting. And it always expires: there is no wait-forever option, because a gate that
cannot expire wedges a coordinator with no diagnosis, which is worse than no gate.

What a gate does not establish is authorship. A pane is a buffer the worker controls, so
the gate proves that a well-formed report appeared there after the gate started and nothing
about who composed it. A shared secret would not close that either — the dispatch prompt
sits in the worker's context beside the untrusted text, so anything that can talk the
worker into faking a report can read the secret out of the same context and include it.

## Events

herdr can invoke this plugin when worktrees appear, so a new checkout is adopted and
staffed the moment it exists instead of the next time you remember to run `sync`.

**Events are off by default. They do nothing until you opt in:**

```sh
export MUSTER_EVENTS=1
```

That is deliberate. These hooks start coding agents, and a plugin that begins spawning
agents the moment it is installed has handed you an autonomous trigger you never asked
for. Opting in is one exported variable; opting out after a surprise is not.

The variable is read from herdr's own environment, so export it before starting herdr (or
in your shell profile) rather than in a single pane.

When enabled, the event path **adopts and staffs only — it never prunes.** Removal stays
something you ask for by name.

## Startup

Events cover the session. They cannot cover the time herdr was not running — a worktree
you added from a plain shell, a workspace restored without the agent that used to live in
it. That gap opens exactly once, so it is closed exactly once: herdr runs a single
adopt-and-staff pass per open repository after the server is ready, and the command exits.

There is no watcher and no poll loop. The zsh original had one — `wt watch`, waking every
90 seconds to ask the GitHub API about every worktree — and this is what replaced it. The
startup pass makes no network calls at all: pull request state only ever authorises a
removal, and startup never removes anything.

It shares the `MUSTER_EVENTS` opt-in above, and is off without it. Same reason, more so:
it starts agents across every open repository at once, on every launch.

## How it decides things

Two ideas do most of the work here.

**An event is a trigger, never a fact.** The event handler does not act on what the event
says. It reads it for one thing — which repository — and then runs the same whole-repository
pass that `sync` runs, against live state. An event payload describes the world as it was
before this process started, so it is out of date on arrival. This also means the event
path and the reconciler cannot disagree, because they are the same code.

**Whether work has landed is not decidable from git topology.** Across fast-forward,
squash, rebase and merge-commit workflows the graph shapes overlap: a branch merged by
fast-forward looks exactly like a branch that never committed, and a branch forked off
already-merged work looks exactly like one that landed. So topology never removes anything
here. A merged pull request is the only authority accepted, ambiguity always resolves to
keeping the worktree, and the reason is printed rather than dressed up as a verdict.

An un-pruned worktree costs disk. A wrongly pruned one costs work that exists nowhere else.
Those are not comparable, so the tie never goes to deletion.

That principle shows up as a set of guards, each re-checked immediately before anything is
removed rather than trusted from the plan:

- uncommitted changes, including untracked files
- an agent currently running in the worktree
- the directory you are standing in
- a pull request that is closed but not merged — abandoned work still holds commits

## For coding agents

An agent driving this plugin gets a few things wrong without being told: that
`plugin action invoke` returns an invocation record rather than the action's output,
that `prune` and `prune-apply` are different in kind, and that enabling events is not
its call to make. A skill covering that ships in this repository:

```sh
npx skills add steig/muster --skill worktrees -g
```

Or vendor `skills/worktrees/SKILL.md` into your own agent configuration, which pins it
rather than tracking this repository.

Nothing here writes to your agent's configuration during install, and nothing should.
Running unsandboxed as it does, a plugin that quietly edits how your coding agent behaves
is the same kind of surprise as one that starts spawning agents on install.

## Development

```sh
go test ./...
```

Tests run **real git in a temp directory** against a fake herdr speaking the same protocol,
with a fake `gh`. Nothing touches a live session and nothing reaches the network.

Types in `internal/herdrapi/types_gen.go` are generated from herdr's own API schema, so a
field herdr renames becomes a compile error rather than a silently-nil lookup:

```sh
herdr api schema --json > internal/herdrapi/schema.json
go generate ./...
```

## License

MIT — see [LICENSE](LICENSE).

# herdr-wt

Drive git worktrees as [herdr](https://github.com/herdr/herdr) workspaces — list them,
adopt them, staff them with agents, and tear them down again.

Plenty of tools create worktrees. This one is mostly about the other end: knowing which
checkouts are finished, and removing them without ever removing one that wasn't.

```
$ herdr plugin action invoke ls --plugin steig.wt
* main                      w21  idle     herdr-wt
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
herdr plugin install steig/herdr-wt
```

Installing runs `scripts/build.sh`, which prefers a local Go toolchain and falls back to a
prebuilt release binary, so it works with or without Go. On Windows the build needs Go on
`PATH`.

Downloaded binaries are checked against the `checksums.txt` published with the release, and
a missing or mismatched checksum aborts the install rather than warning about it — the
fallback path fetches an executable over the network and hands it to herdr to run, so it is
the one step worth being strict about.

For local development, from a checkout:

```sh
herdr plugin link .
```

Note that `link` points herdr at the working tree, so manifest edits take effect
immediately.

## Actions

| Action | What it does |
| --- | --- |
| `wt: list worktrees` | Every worktree in the current repository, with its herdr workspace and agent status. |
| `wt: sync worktrees` | Opens a workspace for any worktree that lacks one, and starts an agent in any workspace sitting idle as a bare shell. Never removes anything. |
| `wt: prune (list)` | Reports which worktrees look finished and which were spared, and why. Changes nothing. |
| `wt: prune (apply)` | Actually removes them. |

Output lands in `herdr plugin log list --plugin steig.wt`.

`prune` and `prune-apply` are two actions rather than one with a confirmation, because a
plugin action has no prompt surface — there is nowhere to ask "are you sure?". Splitting
them is the confirmation. It also means no stray keybinding can reach a removal.

## Events

herdr can invoke this plugin when worktrees appear, so a new checkout is adopted and
staffed the moment it exists instead of the next time you remember to run `sync`.

**Events are off by default. They do nothing until you opt in:**

```sh
export HERDR_WT_EVENTS=1
```

That is deliberate. These hooks start coding agents, and a plugin that begins spawning
agents the moment it is installed has handed you an autonomous trigger you never asked
for. Opting in is one exported variable; opting out after a surprise is not.

The variable is read from herdr's own environment, so export it before starting herdr (or
in your shell profile) rather than in a single pane.

When enabled, the event path **adopts and staffs only — it never prunes.** Removal stays
something you ask for by name.

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
npx skills add steig/herdr-wt --skill worktrees -g
```

Or vendor `skills/worktrees/SKILL.md` into your own agent configuration, which pins it
rather than tracking this repository.

Nothing here writes to your agent's configuration during install, and nothing should.
A herdr plugin runs unsandboxed as you; one that quietly edits how your coding agent
behaves is the same kind of surprise as one that starts spawning agents on install.

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

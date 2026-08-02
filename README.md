# worktender

**Git worktrees for parallel coding agents — adopted, staffed, and removed once
the work has landed.**

A [herdr](https://github.com/herdrdev/herdr) plugin. herdr is the terminal
multiplexer coding agents run in; this plugin keeps its workspaces and your git
worktrees pointing at the same reality.

## The problem

Running several coding agents at once means running several worktrees. Making
the directory is the easy half, and every tool does it — what nothing does is
the round trip: an issue, a checkout named for it, an agent briefed on it, and a
way to know when it is finished. `start` and `gate` are that half.

The other end is where it goes wrong. A few weeks in there are eleven checkouts
on disk, some have a herdr workspace and some do not, some still have an agent
sitting in them and some are ghosts, and the one you are least certain about is
the one you least want to delete. So nobody deletes anything — and the honest
reason is that no cleanup script has ever been trustworthy enough to run without
reading its output line by line first.

worktender covers both ends. It starts an agent on an issue in a worktree of its
own, and it reconciles `git worktree list` against herdr's workspaces and agents
— adopting what herdr does not know about, staffing empty workspaces, and
removing finished checkouts **on the rule that ambiguity always keeps the
worktree.**

```sh
$ worktender ls
* main                      w21  w21:p1  idle     worktender
  feat/1-reconcile-execute  w22  w22:p1  working  1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     257-erasure-comments
  worktree/brave-valley     -    -       -        brave-valley-66f8
```

Columns are branch, herdr workspace, pane, agent status, and directory. `*`
marks the repository's main checkout; `-` means herdr has nothing for that
worktree — the last row is a checkout with no workspace and no agent, which is
exactly what `sync` picks up.

The pane is the one `dispatch --pane` takes. Add `--pr` for a pull request
column, which is off by default because it costs one `gh` call per branch:

```sh
$ worktender ls --pr
* main                      w21  w21:p1  idle     -       worktender
  feat/1-reconcile-execute  w22  w22:p1  working  OPEN    1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     MERGED  257-erasure-comments
```

`ls`, `doctor`, `sync`, `prune` and `prune-apply` take `--json` if you are
building on this rather than reading it — see
[Machine-readable output](docs/json.md).

Everything is a subcommand of one binary, which herdr installs rather than
putting on `PATH`. Resolve it once:

```sh
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender
```

The same four reconcile commands are *also* registered as herdr actions, so a
keybinding or menu can reach them — but that path is worse to script against,
and this is what newcomers trip on first: **`invoke` returns an invocation
record, not the action's output.** What the action printed is in the plugin log.
Call the binary and the output is just on stdout.

## Starting work on an issue

```sh
$ worktender start 42 --repo .
repository: /Users/you/code/thing
worktree: 42-fix-the-thing on origin/main (workspace w9, pane w9:p1)
done  staff  42-fix-the-thing  started claude as wt-42-fix-the-thing-016aab in w9:p1

briefed wt-42-fix-the-thing-016aab on #42; wait for it with:
  worktender gate --target wt-42-fix-the-thing-016aab --until done --require-pr
```

The agent name is not the branch name: herdr's agent namespace spans every
repository at once, so the name carries a digest of the repository. Copy the
line `start` prints rather than retyping it.

With several running, wait on all of them at once — the first to report releases
the gate and it says which one:

```sh
$ worktender gate --any wt-42-fix-the-thing-016aab,wt-43-other-9c21f4 --until done
gate: waiting on wt-42-fix-the-thing-016aab (pane w9:p1), wt-43-other-9c21f4 (pane w10:p1) for status done, up to 15m
gate: wt-43-other-9c21f4 released after 4m12s
```

One command from an issue number to an agent working on it: it reads the issue
with `gh`, creates a worktree named for it, starts an agent in the new pane, and
types a brief covering the whole round — read the issue, explore, change, test,
self-review, open a PR, then `report`.

`--repo` because `start` creates a checkout, so it refuses to guess which
repository — and unlike the reconcile commands it has no herdr action to be
invoked through, because an action carries no arguments and `start` is nothing
without its issue number. Flags may be written on either side of the number.

**The brief is confirmed, not claimed.** It is submitted with a separate Enter
key event and `start` then waits for herdr to report the agent working, because
herdr answering ok means it delivered keystrokes and not that an agent received
a prompt. An agent still `idle` when that wait runs out fails the command.

Start several, then wait on the lot of them. `start` deliberately does not wait;
`gate` is the other half.

**The issue body reaches the agent as framed, untrusted data.** Anyone who can
file an issue writes it, so it is announced as data and delimited before it
arrives, flattened onto one line, and never presented as an instruction. Nothing
about the agent's autonomy is defaulted: without `--permission-mode`, `start`
changes nothing about what it may do.

## Quickstart

```sh
# 1. install — read Trust below first; this runs unsandboxed
herdr plugin install steig/worktender

# 2. resolve the binary; herdr owns the install, so it is not on PATH
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# 3. see where you stand — and, if anything looks wrong, why.
#    doctor also prints the line above, so you only need the jq once.
"$worktender" ls
"$worktender" doctor

# 4. adopt every orphan checkout, staff every idle workspace
"$worktender" sync

# 5. ask what looks finished — a DRY RUN, it removes nothing
"$worktender" prune                     # or: prune --repo /path/to/repo

# 6. only once you have read step 5's reasons
"$worktender" prune-apply

# 7. when doctor's version line says origin has moved past you
"$worktender" update
```

Each of those four is also a herdr action — `Worktender: list worktrees` and
friends — for reaching them from a keybinding or the plugin menu.

Four things worth knowing before step 5 surprises you:

- **`prune-apply` deletes the local branch too**, not just the checkout. It uses
  `git branch -d` and never `-D`, so a branch git considers unmerged survives and
  the output says how to force it. When `origin/<branch>` still exists that is
  reported rather than quietly left behind.
- **`gh` must be authenticated, not merely installed.** A merged pull request is
  the strongest authority this plugin accepts for "finished", and an
  unauthenticated `gh` is indistinguishable from "this branch has no PR" — so
  almost nothing is pruned, and the reasons look entirely ordinary while that
  happens. If prune keeps everything on a repository where you expect otherwise,
  check `gh auth status` first.
- **The repository comes from herdr, not from where you are standing.** Run as an
  action it resolves herdr's current workspace, which on a machine with several
  repositories open is routinely not the one you meant — a dry run inside a repository
  with four staffed worktrees, planning against a different project. Both halves print
  the root they resolved, so read the `repository:` line before acting on a plan. Pass
  `--repo <path>` to settle it: a path anywhere inside a repository resolves to its
  root, and a path that is not one is an error rather than a fallback.
- **Prune reads remote-tracking refs, so run `git fetch --prune` first** if you
  want a deleted upstream to count. A stale tracking ref reads as still present,
  which keeps the worktree — being out of date fails in the safe direction.
- **`sync` converges over two passes, not one.** A checkout adopted this pass has
  no workspace yet, so it cannot be staffed until the next. Running `sync` twice
  against a brand-new orphan is expected, not a bug.
- **An install stays where it was installed.** herdr has no `plugin update`, so
  nothing moves it forward on its own — one install sat four releases behind
  without a word. `doctor`'s `version` line says when origin has moved past you
  and `update` fetches and rebuilds; the one thing neither can fix is that
  `herdr plugin list` keeps reporting the commit it recorded at install time.
  **Step 7 cannot be how you first reach step 7**, either — an install older than
  0.6.0 has no `update` to run, so `herdr plugin install steig/worktender` is the
  way onto it, and the only way to correct that recorded commit.
  See [Staying current](docs/reference.md#staying-current).

## Requirements

- **herdr 0.7.0+** — this is a plugin; it talks to herdr over its local socket.
  Every measured behaviour behind `report` and `gate` was tested against **0.7.5**
  and nothing checks the running version, so prefer 0.7.5+ if you intend to use
  the hand-off pair.
- **git**
- **jq** — for reading action output out of the plugin log, as above.
- **gh**, *authenticated* *(optional)* — only used to read pull request state.
  Without it, the only removals left are the ones a deleted upstream authorises
  (see [How removal is decided](docs/pruning.md)), and a
  repository that uses pull requests will prune almost nothing.

## Actions

| Action | What it does |
| --- | --- |
| `Worktender: list worktrees` | Every worktree in the current repository, with its herdr workspace and agent status. |
| `Worktender: sync worktrees` | Opens a workspace for any worktree that lacks one, and starts an agent in any workspace sitting idle as a bare shell. Never removes anything. |
| `Worktender: prune (list)` | Reports which worktrees look finished and which were spared, and why. Changes nothing. |
| `Worktender: prune (apply)` | Actually removes them, and their local branches. |

Output lands in `herdr plugin log list --plugin steig.worktender`.

`prune` and `prune-apply` are two actions rather than one with a confirmation,
because a plugin action has no prompt surface — there is nowhere to ask "are you
sure?". Splitting them is the confirmation. It also means no stray keybinding can
reach a removal.

Staffing starts `claude`, and **resumes rather than restarts**: a checkout that
already has a Claude Code transcript in `~/.claude/projects` is picked up with
`--continue`, so re-staffing does not throw away the conversation.

## Trust

**A herdr plugin is not sandboxed.** This one runs as you, with your files, your
shell and your credentials, and what it does with them is start coding agents and
delete git worktrees and branches. That is what it is *for* rather than a side
effect, but installing it is a decision to let code from someone else's
repository do those things on your machine, and it is worth making on purpose.

The two capabilities most worth knowing before you install:

- **Removal needs either a merged pull request, or a deleted upstream over
  commits base already has.** Anything ambiguous is kept, and the reason is
  printed.
- **The hooks that would start agents without being asked are off** until you
  turn them on.

With a Go toolchain the binary is compiled from the source that was just cloned,
so what you can read is what you run. Without Go, a prebuilt release binary is
downloaded, pinned to the manifest version and checksummed — which proves the
download arrived intact and **nothing about who published it**.

The full argument, including what the checksum does not establish, is in
[docs/trust.md](docs/trust.md). How to report something privately is in
[SECURITY.md](SECURITY.md).

For local development, from a checkout:

```sh
herdr plugin link .
```

`link` points herdr at the working tree, so manifest edits take effect
immediately.

## Documentation

Rendered at **<https://steig.github.io/worktender/>**, which carries two pages
that exist nowhere else: an overview, and *Patterns* — delegating to agents
without losing the thread, with five worked examples. The markdown below stays
canonical, so an agent that clones this repository reads the same words.

| | |
| --- | --- |
| [Dispatching a worker](docs/dispatch.md) | `dispatch`, `report` and `gate` — handing a slice to another agent and waiting for it, and why the report has fixed slots. |
| [How removal is decided](docs/pruning.md) | What authorises a removal, why git topology never does it alone, and the guards. |
| [Events and startup](docs/events.md) | The hooks that adopt and staff automatically, and the one-shot pass that covers what they cannot. |
| [Reference](docs/reference.md) | Exit codes, the errors you are likely to meet, keybindings, and the smaller behaviours. |
| [Trust](docs/trust.md) | What running unsandboxed means here, and what the install path does and does not prove. |

## For coding agents

An agent driving this plugin gets a few things wrong without being told: that
`plugin action invoke` returns an invocation record rather than the action's
output, that `prune` and `prune-apply` are different in kind, and that enabling
events is not its call to make. A skill covering that ships in this repository:

```sh
npx skills add steig/worktender --skill worktrees -g
```

A second skill covers the other end — an agent that *dispatches* worktender's
agents rather than being one:

```sh
npx skills add steig/worktender --skill coordinator -g
```

It encodes what a session running as a router needs and keeps getting wrong:
never read a worker's diff, verify with targeted commands instead of relaying
claims, ask whether something was run or merely reasoned, pass a brief inline so
no worker stalls on a file-read prompt, and keep anything touching live or shared
state out of a dispatch entirely.

Or vendor `skills/worktrees/SKILL.md` and `skills/coordinator/SKILL.md` into your
own agent configuration, which pins them rather than tracking this repository.

Nothing here writes to your agent's configuration during install, and nothing
should. Running unsandboxed as it does, a plugin that quietly edits how your
coding agent behaves is the same kind of surprise as one that starts spawning
agents on install. The same rule covers autonomy: worktender does not set
permission modes or sandbox profiles on your behalf.

## Development

```sh
go test ./...
```

Tests run **real git in a temp directory** against a fake herdr speaking the same
protocol, with a fake `gh`. Nothing touches a live session and nothing reaches the
network.

Types in `internal/herdrapi/types_gen.go` are generated from herdr's own API
schema, so a field herdr renames becomes a compile error rather than a
silently-nil lookup:

```sh
herdr api schema --json > internal/herdrapi/schema.json
go generate ./...
```

The docs site builds from `docs/*.md` plus the hand-written pages in
`site/pages/`, into `_site/`:

```sh
python3 -m venv .venv && .venv/bin/pip install -r site/requirements.txt
.venv/bin/python site/build.py && python3 -m http.server -d _site 8765
```

## License

MIT — see [LICENSE](LICENSE).

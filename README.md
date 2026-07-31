# worktender

**Git worktrees for parallel coding agents — adopted, staffed, and removed once
the work has landed.**

A [herdr](https://github.com/herdrdev/herdr) plugin. herdr is the terminal
multiplexer coding agents run in; this plugin keeps its workspaces and your git
worktrees pointing at the same reality.

## The problem

Running several coding agents at once means running several worktrees. Creating
them is the easy half, and every tool does it.

The other end is where it goes wrong. A few weeks in there are eleven checkouts
on disk, some have a herdr workspace and some do not, some still have an agent
sitting in them and some are ghosts, and the one you are least certain about is
the one you least want to delete. So nobody deletes anything — and the honest
reason is that no cleanup script has ever been trustworthy enough to run without
reading its output line by line first.

worktender is built for that end of the job. It reconciles `git worktree list`
against herdr's workspaces and agents, adopts what herdr does not know about,
staffs empty workspaces with an agent, and removes finished checkouts — **on the
rule that ambiguity always keeps the worktree.**

```sh
$ herdr plugin action invoke ls --plugin steig.worktender
$ herdr plugin log list --plugin steig.worktender | jq -r '.result.logs[-1].stdout'
* main                      w21  idle     worktender
  feat/1-reconcile-execute  w22  working  1-reconcile-execute
  fix/257-erasure-comments  w1K  idle     257-erasure-comments
  worktree/brave-valley     -    -        brave-valley-66f8
```

Columns are branch, herdr workspace, agent status, and directory. `*` marks the
repository's main checkout; `-` means herdr has nothing for that worktree — the
last row is a checkout with no workspace and no agent, which is exactly what
`sync` picks up.

Those two commands are not a typo, and this is what newcomers trip on first:
**`invoke` returns an invocation record, not the action's output.** What the
action printed is in the plugin log.

## Quickstart

```sh
# 1. install — read Trust below first; this runs unsandboxed
herdr plugin install steig/worktender

# 2. see where you stand
herdr plugin action invoke ls --plugin steig.worktender
herdr plugin log list --plugin steig.worktender | jq -r '.result.logs[-1].stdout'

# 3. adopt every orphan checkout, staff every idle workspace
herdr plugin action invoke sync --plugin steig.worktender

# 4. ask what looks finished — a DRY RUN, it removes nothing
herdr plugin action invoke prune --plugin steig.worktender
herdr plugin log list --plugin steig.worktender | jq -r '.result.logs[-1].stdout'

# 5. only once you have read step 4's reasons
herdr plugin action invoke prune-apply --plugin steig.worktender
```

Three things worth knowing before step 5 surprises you:

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
- **Prune reads remote-tracking refs, so run `git fetch --prune` first** if you
  want a deleted upstream to count. A stale tracking ref reads as still present,
  which keeps the worktree — being out of date fails in the safe direction.
- **`sync` converges over two passes, not one.** A checkout adopted this pass has
  no workspace yet, so it cannot be staffed until the next. Running `sync` twice
  against a brand-new orphan is expected, not a bug.

## Requirements

- **herdr 0.7.0+** — this is a plugin; it talks to herdr over its local socket.
  Every measured behaviour behind `report` and `gate` was tested against **0.7.5**
  and nothing checks the running version, so prefer 0.7.5+ if you intend to use
  the hand-off pair.
- **git**
- **jq** — for reading action output out of the plugin log, as above.
- **gh**, *authenticated* *(optional)* — only used to read pull request state.
  Without it, the only removals left are the ones a deleted upstream authorises
  (see [How it decides what to remove](#how-it-decides-what-to-remove)), and a
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

That is the whole of the herdr *action* surface, and not the whole of the plugin:
`report` and `gate` are commands rather than actions, for reasons covered below.

## Dispatching a worker, and waiting for it

This is what the plugin is for once more than one agent is involved: a
coordinating agent hands a slice of work to another and needs to know when it is
done — without reading everything that agent did to get there.

```sh
# Resolve the binary once. It is not on PATH; herdr owns the install.
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# COORDINATOR — dispatch first, then wait. Order matters.
"$worktender" dispatch --pane w22:p1 --name reconcile-split --model sonnet
"$worktender" gate --target reconcile-split --until done --require-pr --timeout 20m

# WORKER — from inside its own pane:
"$worktender" report --status done --pr 42 --note "landed the reconcile split"
```

The gate prints the report and exits 0 when the predicate holds. It exits
non-zero when the worker reports `blocked`, when the worker dies before
reporting, and when it times out.

```sh
worktender dispatch --pane <id> --name <agent> [--model <model>] [--permission-mode <mode>] [--resume]
worktender report --status planned|blocked|done [--pr N] --note <text>
worktender gate --target <agent|pane> [--until done] [--require-pr] [--timeout 15m]
```

### Why dispatch is separate from `sync`

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

### The permission-mode problem, stated plainly

A dispatched worker has no human at its pane, so it stalls on the first
permission prompt and stays stalled — and a coordinating agent structurally
cannot clear it. `--permission-mode` is the way out, and it comes with a real
caveat rather than a reassurance:

**worktender cannot sandbox the agent it starts.** `claude` takes no sandbox
flag; sandboxing lives in settings.json, and this plugin does not write your
agent's configuration. So a mode that stops the agent asking before it acts
grants autonomy *without* the boundary that should accompany it.

An allowlist does not substitute. A guard on command spelling only holds where
the action has exactly one spelling: `$(...)` and `find -exec` are never
auto-allowed by a prefix rule because they can run anything, and during this
plugin's own development a worker denied `Bash(herdr agent start:*)` reached a
live agent anyway by calling herdr's socket from Go, logging zero denials.
Blocking the CLI blocked the convenient path, not the capability.

So `--permission-mode bypassPermissions` and `acceptEdits` are **refused** unless
you confirm the worker already has a boundary that does not depend on spelling —
a sandbox profile, or a separate uid:

```sh
export WORKTENDER_UNSANDBOXED_OK=1
```

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

### Why the report has no free text

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

## Events

herdr can invoke this plugin when worktrees appear, so a new checkout is adopted
and staffed the moment it exists instead of the next time you remember to run
`sync`.

**Events are off by default. They do nothing until you opt in:**

```sh
export WORKTENDER_EVENTS=1
```

That is deliberate. These hooks start coding agents, and a plugin that begins
spawning agents the moment it is installed has handed you an autonomous trigger
you never asked for. Opting in is one exported variable; opting out after a
surprise is not.

Turning it off works the way you would expect: `0`, `false`, `no`, `off` and
`disabled` all disable events, in any capitalisation. So does unsetting it.
Anything the variable does not recognise leaves events **off** and says so on the
next hook — a value nobody wrote a rule for is not a request to start agents, and
a typo in an opt-in is cheaper to notice than a typo in an opt-out is to survive.

The variable is read from herdr's own environment, so export it before starting
herdr (or in your shell profile) rather than in a single pane.

If you opted in under an older name — `MUSTER_EVENTS`, or `HERDR_WT_EVENTS`
before that — **it enables nothing**, and the next hook says so rather than
failing quietly.

When enabled, the event path **adopts and staffs only — it never prunes.** Removal
stays something you ask for by name.

## Startup

Events cover the session. They cannot cover the time herdr was not running — a
worktree you added from a plain shell, a workspace restored without the agent that
used to live in it. That gap opens exactly once, so it is closed exactly once:
herdr runs a single adopt-and-staff pass per open repository after the server is
ready, and the command exits.

There is no watcher and no poll loop. The zsh original had one — `wt watch`,
waking every 90 seconds to ask the GitHub API about every worktree — and this is
what replaced it. The startup pass makes no network calls at all: pull request
state only ever authorises a removal, and startup never removes anything.

It shares the `WORKTENDER_EVENTS` opt-in above, and is off without it. Same
reason, more so: it starts agents across every open repository at once, on every
launch.

## How it decides what to remove

Two ideas do most of the work here.

**An event is a trigger, never a fact.** The event handler does not act on what
the event says. It reads it for one thing — which repository — and then runs the
same whole-repository pass that `sync` runs, against live state. An event payload
describes the world as it was before this process started, so it is out of date on
arrival. This also means the event path and the reconciler cannot disagree,
because they are the same code.

**Whether work has landed is not decidable from git topology.** Across
fast-forward, squash, rebase and merge-commit workflows the graph shapes overlap:
a branch merged by fast-forward looks exactly like a branch that never committed,
and a branch forked off already-merged work looks exactly like one that landed. So
topology never removes anything **on its own** here, ambiguity always resolves to
keeping the worktree, and the reason is printed rather than dressed up as a
verdict.

An un-pruned worktree costs disk. A wrongly pruned one costs work that exists
nowhere else. Those are not comparable, so the tie never goes to deletion.

Two things can authorise a removal:

1. **A merged pull request.** The only unambiguous "yes" available, and the only
   one that covers squash and rebase workflows — those rewrite commits, so the
   branch is not an ancestor of base at all and no amount of topology will say so.
2. **A deleted upstream, together with the branch's commits already being in
   base.** Both halves are required.

The second exists because the first goes inert in a repository that does not use
pull requests. It works because **a deleted remote branch is a human action rather
than a graph shape**, and that is exactly the fact topology is missing. The
ambiguous case is "did this branch land, or was it forked off work that had
already landed?" — indistinguishable by shape, since they can be the same commit,
but not by publication history: a branch forked off merged work and never pushed
has no upstream to delete, while a branch that landed was pushed and had its
remote ref removed, which is what a merge button does by default.

Neither half is enough alone. A deleted upstream by itself is equally what
abandoning work looks like, so it is reported and the worktree kept. Being an
ancestor of base by itself is the original ambiguity.

That principle shows up as a set of guards, each re-checked immediately before
anything is removed rather than trusted from the plan:

- uncommitted changes, including untracked files
- an agent currently running in the worktree
- the directory you are standing in
- a pull request that is closed but not merged — abandoned work still holds commits

A guard that cannot be checked counts as unsatisfied. If herdr cannot be asked
whether an agent is running, the worktree is kept rather than removed.

## Trust

**A herdr plugin is not sandboxed.** This one runs as you, with your files, your
shell and your credentials, and what it does with them is start coding agents and
delete git worktrees and branches. That is what it is *for* rather than a side
effect — most of this README is about the guards on the deleting half — but
installing it is a decision to let code from someone else's repository do those
things on your machine, and it is worth making on purpose. The two capabilities
most worth knowing: removal needs either a merged pull request or a deleted
upstream over commits base already has, and keeps anything ambiguous; and the
hooks that would start agents without being asked are off until you turn them on.

Installing runs `scripts/build.sh`, which prefers a local Go toolchain and falls
back to a prebuilt release binary, so it works with or without Go. On Windows the
build needs Go on `PATH`.

With Go present you get the stronger of the two paths by a distance: the binary is
compiled from the source that was just cloned, so what you can read is what you
run.

Without Go, the script downloads the release matching the version in the manifest
it cloned — pinned to that tag rather than to `latest`, so reading `v0.3.0` and
installing cannot hand you something newer — and checks it against the
`checksums.txt` published alongside. A missing or mismatched checksum aborts the
install rather than warning about it, and no unverified download is left behind on
any failure path.

That check proves the download arrived intact, and nothing beyond it. The binary
and its checksum come from the same release, so both are published by whoever can
publish releases here, and there is no signature and no attestation to say who
that was. On the no-Go path you are trusting this GitHub account rather than a
proof of authorship. That is the same trust nearly all software installed from
GitHub asks for — which is a reason to say so plainly, not a reason to imply the
checksum is doing more work than it is.

See [SECURITY.md](SECURITY.md) for the trust boundary in full, and for how to
report something privately.

For local development, from a checkout:

```sh
herdr plugin link .
```

Note that `link` points herdr at the working tree, so manifest edits take effect
immediately.

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

## License

MIT — see [LICENSE](LICENSE).

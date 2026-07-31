---
name: worktrees
description: Drive git worktrees as herdr workspaces through the muster plugin — list them, adopt orphans, staff empty workspaces with agents, and remove worktrees whose work has landed. Use when starting work in isolation or in parallel, when a task would mean checking out a branch over work already in progress, and when cleaning up finished worktrees.
---

# Worktrees via the `steig.muster` herdr plugin

This plugin reconciles `git worktree list` against herdr's workspaces and agents:
adopting checkouts herdr does not know about, staffing empty workspaces, and removing
worktrees whose work has landed.

**There is no `muster` command.** This is a herdr plugin, not a CLI. Everything goes
through `herdr plugin action invoke`.

## Invoking it

```bash
herdr plugin action invoke ls    --plugin steig.muster   # worktrees + workspace + agent state
herdr plugin action invoke sync  --plugin steig.muster   # adopt orphans, staff empty workspaces
herdr plugin action invoke prune --plugin steig.muster   # DRY RUN — lists candidates, removes nothing
```

**The invoke call does not return the action's output.** It returns an invocation
record with `status: "running"`. Read what the action actually printed from the log:

```bash
herdr plugin log list --plugin steig.muster \
  | jq -r '.result.logs[-1] | "exit=\(.exit_code)\n\(.stdout)"'
```

This is the most common mistake. An invoke that "returned nothing useful" almost
always ran fine and wrote its output somewhere else.

## Creating a worktree

Use herdr's own worktree creation (`herdr worktree create`, or the bound key in the
herdr UI). **This plugin does not create worktrees.**

It also does not care where a checkout lives — it matches workspaces by repository
root, not by path containment, so checkouts under `~/.herdr/worktrees/` and inside a
repository both work. There is no directory convention to honour.

## Removing worktrees

`prune` is a **dry run**: it lists candidates and reasons and performs nothing.
`prune-apply` is the destructive one, and it is a separate action precisely so that
nothing reaches a removal by accident. Never invoke `prune-apply` to see what would
happen — that is what `prune` is for.

**Pruning requires a merged pull request.** This is not a limitation to route around:

- `merge-base --is-ancestor branch base` is true *exactly when* the branch has zero
  commits of its own, and that shape is identical for merged work, unstarted work,
  fast-forward merges, and a branch forked off already-merged work.
- Squash and rebase merges rewrite commits, so a fully landed branch is not an
  ancestor of base at all.

Git topology therefore cannot decide whether work has landed, and the plugin says so
rather than guessing. Expect reasons like `cannot tell unstarted from fast-forward
merged — keeping`. Do not "fix" this by adding another topological test; that path
has already produced a series of cases each new test got wrong.

A closed-but-unmerged PR is reported as abandoned and never auto-pruned — the branch
still holds commits that exist nowhere else.

Removal also refuses a worktree with uncommitted changes, one whose pane hosts a live
agent, and the directory the caller is standing in. All are re-checked immediately
before removal rather than trusted from the plan.

## Events

The plugin declares hooks (`worktree.created`, `worktree.opened`,
`pane.agent_detected`) so adoption and staffing can happen when something changes
rather than when someone remembers to run `sync`.

**They are off by default and you must not turn them on.** Handlers no-op unless
`MUSTER_EVENTS=1` is set, and they log that they declined. Enabling it means the
plugin can autonomously start coding agents — that is the user's decision, not yours.
If you think it should be on, ask.

When enabled the event path adopts and staffs only. It never prunes, and it makes no
`gh` calls.

## Startup

The plugin also declares a `[[startup]]` one-shot: after herdr's server is ready it
runs one adopt-and-staff pass per open repository, then exits. It exists to cover what
events cannot — anything that changed while herdr was not running. It is not a watcher
and does not poll; if you are ever tempted to add a loop here, that is the thing this
replaced.

It is gated by the same opt-in as the events above, and adopts and staffs only.

## Rules

- **Never `git worktree add` by hand.** It produces checkouts herdr never learns
  about. Create through herdr and let `sync` adopt anything created another way.
- **Never enable `MUSTER_EVENTS` yourself.** Ask.
- **Read the plugin log, not the invoke response**, for an action's output.
- **Do not run `sync` or `prune-apply` casually against a live session.** `sync` can
  start real agents in whatever repository is in scope. Use a scratch repository when
  testing.

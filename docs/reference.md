# Reference

Exit codes, errors, keybindings, and the behaviours that do not belong anywhere else.

## Finding out what is wrong

```sh
$ worktender doctor
herdr   0.7.5          ok
gh      authenticated  ok
events  unset          off

repos
  worktender    3 worktrees  1 working
  house         7 worktrees  1 idle, 1 working
```

Three of this plugin's failures are environmental, silent, and shaped exactly
like ordinary operation, so `doctor` exists to name them without being asked the
right question first:

- **`gh` missing or unauthenticated** collapses to "this branch has no pull
  request", so prune keeps almost everything while every reason it prints reads
  as reasonable. Reported as `warn` rather than `fail` — a repository that does
  not use pull requests is entitled to no `gh` at all.
- **An unrecognised `WORKTENDER_EVENTS`** leaves events off, and the notice
  saying so is printed by a hook that will not fire. `doctor` reports the value
  as the gate *parses* it, not as it is spelled.
- **A herdr that cannot be reached**, which makes every other answer here
  meaningless — so it is said once and the command stops.

It is read-only, takes no lock, and works from outside a repository: someone who
cannot tell what is wrong often cannot tell where they are either. The
repository list is herdr's open worktree workspaces rather than wherever the
caller is standing.

`doctor` is a command rather than an action, and deliberately: an action's
output lands in the plugin log, which is exactly the indirection a diagnostic
should not have.

## Exit codes and errors

There are exactly two exit codes: **0**, or **1** with `worktender: <error>` on
stderr.

Everything fails loudly on purpose. herdr records a plugin action that exits 0
as "succeeded", so a command that reports a problem and exits 0 is a silent
failure — which is why `sync` and `prune` exit 1 with `%d of %d action(s)
failed` rather than printing a warning and returning success.

Errors you are most likely to meet:

| Message | Means |
| --- | --- |
| `refusing to guess which repository to change` | You ran a changing command outside herdr. `ls` and `prune` allow it; `sync`, `prune-apply` and the event paths do not. |
| `another worktender reconcile has held X for more than 30s` | A concurrent pass. Retry. |
| `WORKTENDER_EVENTS="ture" is not a value this gate recognises` | Events stay **off**. Fix the value yourself; nothing here rewrites it. |
| `MUSTER_EVENTS is set, but it was renamed` | A superseded opt-in enabling nothing. So is `HERDR_WT_EVENTS`. |
| `--note is N characters; the limit is 200` | Refused, never truncated. Shorten and report again. |
| `the worker reported blocked after Ns` | The gate failed fast rather than waiting out its clock. |
| `no new report reached status done within Ns` | Timed out. The message quotes what the pane already held when the gate opened, which it ignored as a previous task's answer. |

## Smaller things worth knowing

- **`base` is `origin/HEAD`, not `main`.** It falls back to `main` only when
  origin cannot be asked, so a repository defaulting to `master` or `develop` is
  handled without configuration.
- **`list` is an alias for `ls`.**
- **Agent names** come from the checkout's directory basename, lowercased to
  `[a-z0-9-]`, truncated to 32 characters, and prefixed `worktender-` if the
  result does not start with a letter.
- **Adoption does not focus the workspace**, so adopting a batch does not drag
  you through every one of them.
- **The repository lock is fail-open.** It lives under
  `HERDR_PLUGIN_STATE_DIR`, and if that is absent or unwritable it degrades to a
  lock that excludes nothing, silently. Any lock held longer than five minutes is
  taken. It stops two reconciles duplicating work; **it is not a safety
  control** — the guards that are re-checked immediately before removal are.

## Binding a key to an action

herdr has no `plugin_action` keybinding type — its key commands are `command`,
`pane`, `popup` and `split`. So a binding runs the invoke CLI like any other
command, in your own `config.toml`:

```toml
[[keys.command]]
key = "prefix+alt+s"
type = "popup"
command = "herdr plugin action invoke sync --plugin steig.worktender"
width = "70%"
height = "50%"
```

**Bind `sync` and `prune`, never `prune-apply`.** A key that can reach a removal
is exactly what splitting those two actions was meant to prevent.

This plugin deliberately ships **no** `[[keys.command]]` entries of its own. An
install that silently claims `prefix+alt+s` is the same class of surprise as one
that edits your agent's configuration — and you may already have that key.

That is the whole of the herdr *action* surface, and it is a subset of the
binary's: every action runs `./bin/worktender <id>`, so `ls`, `sync`, `prune`
and `prune-apply` are subcommands you can call directly — and should, since the
action path returns an invocation record rather than the output. `report` and
`gate` have no action equivalent at all: they take arguments and `gate` blocks,
neither of which an action can do.


---

[← README](../README.md)

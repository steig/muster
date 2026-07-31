# Reference

Exit codes, errors, keybindings, and the behaviours that do not belong anywhere else.

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

That is the whole of the herdr *action* surface, and not the whole of the plugin:
`report` and `gate` are commands rather than actions, for reasons covered below.


---

[← README](../README.md)

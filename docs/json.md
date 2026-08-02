# Machine-readable output

`ls`, `doctor`, `sync`, `prune` and `prune-apply` take `--json`. It replaces the
table rather than joining it: **one shape or the other on stdout, never both.**
An action's output is read back out of the plugin log and parsed, so a document
with a stray line above it is a document nobody can parse.

> **The shape may move before 1.0.** It is a supported way to consume this
> plugin, not a stability promise. Pin the version you built against.

## Why the table is not enough

The alignment is computed from the data, so a column moves the moment a branch
name gets longer. And the table has one word for four different absences:

```sh
$ worktender ls --pr
* main                      w21  w21:p1  idle     -       worktender
  worktree/brave-valley     -    -       -        -       brave-valley-66f8
```

Every `-` there means something different — no workspace, no pane, no agent, no
pull request — and the last one means *two* things the table cannot separate:
this branch has no pull request, and `gh` could not be asked. That second
reading is the expensive one. An unauthenticated `gh` fails exactly like a
branch nobody opened a pull request for, and the verdict that follows is *keep*,
so prune keeps everything while every reason it prints reads as ordinary.
`doctor` exists to explain that in prose. The JSON says it in a field.

## `ls --json`

```sh
$ worktender ls --pr --json
{
  "worktrees": [
    {
      "main": true,
      "branch": "main",
      "workspace_id": "w21",
      "pane_id": "w21:p1",
      "agent_status": "idle",
      "pr": null,
      "dir": "worktender"
    },
    {
      "main": false,
      "branch": "worktree/brave-valley",
      "workspace_id": null,
      "pane_id": null,
      "agent_status": null,
      "pr": { "state": null, "error": "gh pr view worktree/brave-valley: gh: To get started with GitHub CLI, please run: gh auth login" },
      "dir": "brave-valley-66f8"
    }
  ]
}
```

Absence is `null`, always. `pr` has three states and they are the reason this
object exists rather than a string:

| `pr` | Means |
| --- | --- |
| `null` | Nobody asked. `--pr` was not passed, or this is the main checkout, which is never asked about — trunk has no pull request and every lookup is a network round trip. |
| `{"state": null, "error": null}` | Asked, and this branch has no pull request. |
| `{"state": "OPEN", "error": null}` | Asked and answered. Also `MERGED` and `CLOSED`. |
| `{"state": null, "error": "…"}` | **`gh` could not be asked.** Not the same as no pull request, and the one the table cannot show you. |

`pane_id` is the pane `dispatch --pane` takes, which is what makes this listing
something to act on rather than only display.

## `ls --all-repos --json`

Across repositories the answer is **grouped**, and `worktrees` is null:

```sh
$ worktender ls --all-repos --blocked --json
{
  "worktrees": null,
  "repositories": [
    {
      "root": "/Users/you/code/worktender",
      "name": "worktender",
      "error": null,
      "worktrees": [
        {
          "main": false,
          "branch": "77-cross-repo",
          "workspace_id": "w30",
          "pane_id": "w30:p1",
          "agent_status": "blocked",
          "pr": null,
          "dir": "77-cross-repo"
        }
      ]
    },
    { "root": "/Users/you/code/lighthouse", "name": "lighthouse", "error": null, "worktrees": [] },
    { "root": "/Users/you/code/gone", "name": "gone", "error": "worktree.list: not a git repository", "worktrees": null }
  ]
}
```

Exactly one of `worktrees` and `repositories` is ever non-null, so a consumer
can tell which question was asked from the document alone.

Grouped rather than a `repository` field on every row, which was the other
candidate and the cheaper one. Three things decided it:

- **A per-repository failure needs somewhere to live.** `--all-repos` keeps
  going when one repository cannot be read, and a flat array of worktrees has
  nowhere to record that except by inventing a row that is not a worktree.
- **A repository whose rows were all filtered out has to survive as an empty
  group.** *Asked, and none* versus *not asked* is the distinction this whole
  format exists to keep, and a flat array erases it by simply having fewer rows.
  That is `lighthouse` above: read, and nothing blocked.
- **`doctor --json` already reports per repository**, so the two
  cross-repository views nest the same way rather than each inventing one.

Within a group, `worktrees` is `null` when the repository could not be read and
`[]` when it was read and nothing matched. `--pr` is refused with `--all-repos`:
the lookup runs one `gh` call per branch in series and is scoped to a single
repository, so across several it would be slow and asking the wrong repository.

## `sync --json`, `prune --json`, `prune-apply --json`

```sh
$ worktender prune --repo . --json
{
  "repository": "/Users/you/code/thing",
  "results": [
    {
      "status": "planned",
      "kind": "prune",
      "target": "done",
      "detail": "would remove: merged into main",
      "branch": "done",
      "path": "/Users/you/code/thing/.claude/worktrees/done",
      "workspace_id": "w4",
      "pane_id": null,
      "agent_name": null,
      "reason": "merged into main"
    }
  ]
}
```

- **`repository`** is the `repository:` line the table prints above itself. It is
  a field rather than a line because those commands resolve the repository from
  herdr's context, which on a machine with several open is routinely not the one
  you meant — it is the fact worth reading, so a consumer must not have to
  re-derive it.
- **`status`** is `done`, `planned`, `skipped` or `failed`. `planned` is a dry
  run: `prune` never removes anything, and `prune-apply` is the deliberate
  second step.
- **`reason`** is why the reconciler planned the action; **`detail`** is what
  became of it. The table has room for one of them.
- **`results`** is one document for the whole command, even though `sync`
  reconciles in up to three passes. The exit code is unchanged — a failed action
  still exits 1, and the report is written either way, because a consumer that
  learns only "non-zero" learns nothing about which action failed.

## `doctor --json`

```sh
$ worktender doctor --json
{
  "checks": [
    { "name": "version", "value": "0.7.0 @f074c65", "state": "warn", "note": "origin/main is at @a1b2c3d; run `worktender update`" },
    { "name": "herdr",   "value": "0.7.5",          "state": "ok",   "note": null },
    { "name": "gh",      "value": "not authenticated", "state": "warn", "note": "reads as \"no pull request\", so prune will keep almost everything" },
    { "name": "events",  "value": "unset",          "state": "off",  "note": null }
  ],
  "binary": "/Users/you/.config/herdr/plugins/github/steig.worktender-3ebd/bin/worktender",
  "repositories": [
    {
      "root": "/Users/you/code/thing",
      "name": "thing",
      "error": null,
      "worktrees": 3,
      "agents": { "working": 1, "blocked": 1, "idle": 1 },
      "blocked": [
        { "main": false, "branch": "79-stuck", "workspace_id": "w7", "pane_id": null, "agent_status": "blocked", "pr": null, "dir": "79-stuck" }
      ]
    }
  ],
  "error": null
}
```

- **`state`** is `ok`, `off`, `warn` or `fail`. `off` is the documented default
  for events and is not a problem; `warn` is a capability lost without being
  told.
- **`repositories`** is `null`, not `[]`, when herdr could not be reached — an
  empty list is the answer for a herdr with nothing open, which is a different
  fact. The reason is in **`error`**, and the command still exits 1.
- A repository that could not be read carries its own `error` and null counts,
  and costs the others nothing.
- **`blocked`** names the worktrees rather than leaving them as one number among
  `agents`. It is the one status where the session has stopped and only a person
  can restart it, so which worktree it was is the entire question — a count
  answers nobody. Empty when none are, null when the repository could not be
  read. `pane_id` is null throughout: `doctor` does not ask for panes, and
  `ls --all-repos --blocked --json` is the view that carries the pane a dispatch
  needs.
- The scope is herdr's open worktree workspaces rather than wherever you are
  standing, so `doctor` answers from outside any repository at all — the same
  scope, and now the same read, that `ls --all-repos` lists in full.

## Call the binary, not the action

A herdr action is a fixed command array with no argument surface, so
`Worktender: list worktrees` cannot be asked for `--json` — and its output lands
in the plugin log rather than on your stdout anyway. Resolve the binary once and
run it directly:

```sh
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

"$worktender" ls --pr --json | jq '.worktrees[] | select(.agent_status == "working")'

# every blocked agent anywhere, as repository + pane, which is what restarts it
"$worktender" ls --all-repos --blocked --json \
  | jq -r '.repositories[] | .root as $r | .worktrees[]? | "\($r)\t\(.branch)\t\(.pane_id)"'
```

## Two things it deliberately does not do

**It does not escape.** The table replaces a bidi override in a branch name with
`\u{202E}`, because git accepts one in a ref and a terminal will draw
`evil‮hctap` as `evilpatch` — and that table is what a human reads before
approving a removal. The JSON carries the raw name, because it is data: an
escaped branch name cannot be handed back to git, and a consumer that draws it
has to do its own escaping at the point it draws, exactly as `Render` does.

**It is not a second collection path.** The document is a projection of the same
`[]Row` and `[]Result` the table renders, built after the same lookups. Two
paths would drift, and then the table would be the liar.

---

[← README](../README.md)

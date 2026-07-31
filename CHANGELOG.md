# Changelog

Notable changes to worktender. Format follows [Keep a
Changelog](https://keepachangelog.com/en/1.1.0/); versioning is
[semver](https://semver.org/spec/v2.0.0.html), with the caveat that `herdr plugin
install` tracks branch HEAD rather than a tag — the version in
`herdr-plugin.toml` is what the no-Go install path pins its download to.

## [Unreleased]

### Fixed

- **The README explains how a report reaches a gate again** (#45). The #28
  restructure dropped the paragraph saying a report travels as pane metadata and
  never replaced it, so the largest section in the document described `report`
  and `gate` without once saying how one reaches the other.

  What replaces it is also more accurate than what was lost: the gate reads
  **both** the metadata and the pane's terminal buffer, every look, and neither
  wins. So a worker that merely prints a well-formed envelope has reported, and
  identity is a per-channel counter rather than content — two identical reports
  are two reports.

### Changed

- **Split the README into `docs/`** (#47). At 542 lines it carried the problem
  statement, quickstart, action surface, the whole dispatch design, exit codes,
  event hooks, the removal model and the supply-chain argument in one scroll —
  reference material a first-time reader does not need and a returning reader
  could not find. README is now 216 lines; the rest moved to `docs/dispatch.md`,
  `docs/pruning.md`, `docs/events.md`, `docs/reference.md` and `docs/trust.md`.

  **The unsandboxed warning stayed in README, above the install decision.**
  Moving it into `docs/trust.md` would have re-created #18 in a new form. So did
  the two quickstart surprises — that `prune-apply` deletes local branches, and
  that `gh` must be authenticated. No prose was lost; this was a re-container,
  not an edit.

### Added

- Exit codes and the errors a user is most likely to meet.
- That `base` is `origin/HEAD` rather than an assumed `main`, that `list` is an
  alias for `ls`, how agent names are derived, that adoption never focuses the
  workspace, and that the repository lock is fail-open and is not a safety
  control.

## [0.4.1] — 2026-07-31

### Fixed

- **`prune` and `prune-apply` now both name the repository they resolved.** (#41)
  They do not resolve it the same way, and must not disagree in silence: listing
  may fall back to the working directory, applying may not — because herdr runs
  plugin commands with cwd set to the plugin root, itself a git repository, so a
  removal that fell back there would point at this plugin's own checkout.

  That asymmetry is deliberate and stays. What it cost was legibility: observed
  live as a dry run listing six worktrees followed by an apply reporting
  "nothing to do" about a different root, with nothing in either output showing
  they had disagreed. Printing the root does not prevent the divergence; it
  makes it impossible to have without seeing it.

### Added

- Documented how to bind a key to an action (#9). herdr has no `plugin_action`
  key type — its key commands are `command`, `pane`, `popup` and `split` — so a
  binding runs `herdr plugin action invoke` like any other command. This plugin
  ships no `[[keys.command]]` entries of its own on purpose.

## [0.4.0] — 2026-07-31

### Added

- **A deleted upstream can now authorise a removal, paired with ancestry.** Prune
  previously required a merged pull request, so it went inert in any repository
  that does not use them — the differentiated half of this plugin doing nothing
  for a whole class of users. (#8)

  A branch whose remote counterpart has been deleted **and** whose commits base
  already has is now pruned. Both halves are required and neither is sufficient: a
  gone upstream alone is abandonment as often as completion, and being an ancestor
  of base alone is the ambiguity that has always resolved to keeping.

  It is admissible because a deleted upstream is a *human action* rather than a
  graph shape, which is precisely the fact topology lacks. A branch forked off
  merged work and never pushed has no upstream to delete; a branch that landed was
  pushed and had its remote ref removed. Squash and rebase workflows remain
  uncovered — they rewrite commits, so the branch is not an ancestor at all — and
  continue to need a pull request.

  Prune reads remote-tracking refs, so `git fetch --prune` must have run for this
  to fire. A stale ref reads as still present and keeps the worktree, which fails
  in the safe direction.

- **`dispatch` — staff one named pane with a model and a permission mode.** (#26)
  `sync` staffs a bare `claude` with no arguments, so there was no way to
  influence how a staffed agent runs. `sync` keeps doing exactly that, on
  purpose: it fires from keybindings and event hooks where no role exists to
  route on, and an unattended reconciler should hold no opinion about cost or
  autonomy. A deliberate dispatch is where those belong.

  Dispatch runs through the same executor `sync` uses, so the pane re-check
  before `agent.start` covers it by construction rather than by a rule someone
  has to remember. #26 asked for that rule to be written down; not having a
  second staffing path is stronger than writing it down.

  **`--permission-mode bypassPermissions` and `acceptEdits` are refused** unless
  `WORKTENDER_UNSANDBOXED_OK` confirms the worker is sandboxed by something
  else. worktender cannot sandbox it — `claude` takes no sandbox flag, and this
  plugin does not write your agent's configuration — so granting autonomy
  without a boundary is exactly the combination #26 warned against, and it is
  stated rather than smoothed over. Nothing is defaulted: without the flag,
  dispatch changes nothing about what an agent may do.

### Changed

- `reconcile.Action` gained `AgentArgs` and is therefore no longer a comparable
  struct. It is always empty on anything the reconciler plans.

## [0.3.0] — 2026-07-31

Renamed. Everything user-facing moved; nothing about how removal decides changed.

### Changed — BREAKING

- **Renamed `muster` to `worktender`.** `muster` collided with an existing herdr
  plugin (`kichel.muster`, published four weeks earlier and listed in the same
  `herdr-plugin` marketplace topic). Plugin ids are namespaced so nothing
  technically clashed, but two plugins called Muster in one index is a
  discoverability problem, and it was cheaper to fix at zero installs than ever
  again.
  - plugin id `steig.muster` → `steig.worktender`
  - binary `bin/muster` → `bin/worktender`
  - actions retitled `Muster: …` → `Worktender: …`
  - module path `github.com/steig/muster` → `github.com/steig/worktender`
- **`MUSTER_EVENTS` → `WORKTENDER_EVENTS`.** The old name enables nothing. Both it
  and `HERDR_WT_EVENTS` before it are detected and refused with a line naming the
  current spelling, so a stale opt-in fails loudly instead of going quietly inert.
  **If you had events enabled, you must re-export under the new name.**
- **Report metadata keys `muster_*` → `worktender_*`.** `report` and `gate` ship
  together, so this only matters if you mix versions: a 0.2.0 worker's report is
  invisible to a 0.3.0 gate, and vice versa.

### Added

- `SECURITY.md` — private reporting route, supported versions, and the trust
  boundary in full, including the accepted limits on what a report authenticates.
- This changelog.

### Fixed

- **README rewritten around what the tool is for.** It opened on mechanics and
  reached install before ever stating the problem, and spent 386 words on
  supply-chain trust before the table of what the tool does. Now: problem
  statement, quickstart, and a worked dispatch example, with the trust and
  rationale material kept in full and moved below them.
- **Documented that `prune-apply` deletes the local branch**, not just the
  checkout. Behaviour is unchanged and was always `git branch -d`, never `-D` —
  it was simply never written down.
- **Documented that `gh` must be authenticated, not merely installed.** An
  unauthenticated `gh` reads as "no pull request", so nothing is ever pruned while
  the printed reasons look entirely ordinary.
- Documented `sync`'s two-pass convergence, transcript resume via `--continue`,
  and that `--until` is repeatable.
- README linked `github.com/herdr/herdr`, which does not exist. Corrected to
  `github.com/herdrdev/herdr`.
- The manifest description advertised worktree *creation*, which this plugin has
  never done and explicitly declines. That string is the marketplace listing copy.
- The README's opening example showed `plugin action invoke` printing a table
  inline. It returns an invocation record; the output is in the plugin log. The
  first code block taught the mistake the shipped skill exists to prevent.
- `skills/worktrees/SKILL.md` still described a `pane.agent_detected` hook, removed
  in #17.

## [0.2.0] — 2026-07-31

### Added

- **`report` and `gate`** — a fixed-slot hand-off pair for agent-to-agent
  dispatch. A worker reports `planned|blocked|done` with an optional PR number and
  a 200-character note; a coordinator blocks on `gate` until the predicate holds.
  Neither is registered as a herdr action, because an action has no argument
  surface and writes its output to a log the blocked caller is not reading.
- **`[[startup]]` one-shot** — a single adopt-and-staff pass per open repository
  after herdr's server is ready, replacing the zsh original's 90-second
  `wt watch` poll loop. Makes no network calls.
- Event hooks for `worktree.created` and `worktree.opened`, off by default.

### Changed — BREAKING

- **Renamed `herdr-wt` to `muster`**, including `HERDR_WT_EVENTS` →
  `MUSTER_EVENTS`. The old variable enables nothing and is refused loudly.

### Fixed

- Events gate armed on values meaning *off*; `MUSTER_EVENTS=off` could start
  agents. Unrecognised values now fail closed and say so. (#14)
- Metadata reports were forgeable while a comment claimed otherwise. The claim was
  wrong, not the code; the limit is now documented rather than overstated. (#16)
- Install path left an unverified binary on disk when the checksum fetch failed,
  at exactly the path the manifest execs. A trap now covers every exit path, and a
  missing or mismatched checksum aborts. (#19)
- Bidi characters in a branch name could spoof the prune confirmation. Every
  rendered cell is escaped to `\u{XXXX}` rather than stripped — a stripped name
  renders as a legitimate one. (#22)
- `ls` swallowed a `WorkspaceList` failure and reported it as an empty session,
  which is indistinguishable from a real one. (#21)
- A repolock could be stolen through a zero-byte lock file. (#20)
- `prune` could force-remove a checkout hosting a live agent when the workspace id
  was empty, silently skipping the agent guard. (#13)
- `gate` identified a report by its content, so two identical reports read as one
  and a coordinator dispatching the same slice twice was heard once. Now counted
  per channel. (#15)
- A tool-call envelope never reached the pane, so `report` and `gate` could not
  meet. (#12)

## [0.1.0]

First tagged release, so the no-Go install path had something to download. (#2)

Worktree lifecycle reconcile and execute, the four herdr actions
(`ls`/`sync`/`prune`/`prune-apply`), the merged-PR removal rule, and the
`skills/worktrees` agent skill.

[0.4.1]: https://github.com/steig/worktender/releases/tag/v0.4.1
[0.4.0]: https://github.com/steig/worktender/releases/tag/v0.4.0
[0.3.0]: https://github.com/steig/worktender/releases/tag/v0.3.0
[0.2.0]: https://github.com/steig/worktender/releases/tag/v0.2.0
[0.1.0]: https://github.com/steig/worktender/releases/tag/v0.1.0

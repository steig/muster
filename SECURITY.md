# Security

## Reporting

Report privately through [GitHub's advisory
form](https://github.com/steig/worktender/security/advisories/new) rather than in
a public issue.

If you would rather not use that, open a public issue saying only that you have
something to report and asking for a contact route. Say nothing else in it.

There is no bounty and no guaranteed response time. This is a personal project.

## Supported versions

The most recent release, and nothing else. There are no maintenance branches, and
`herdr plugin install` tracks branch HEAD rather than a tag, so "upgrade" and "fix"
are the same operation here.

## What this software can do to a machine

A herdr plugin is **not sandboxed**. worktender runs as you, with your files, your
shell and your credentials. What it does with them, by design:

- starts coding agents, which are themselves unsandboxed programs that write code
- removes git worktrees, including a `git worktree remove --force`
- deletes local git branches with `git branch -d`
- shells out to `git` and `gh`
- opens herdr's local control socket and drives the session through it

None of that is incidental. It is the feature set. The security posture is
therefore not "this cannot touch anything" but "the destructive half is
conservative, legible, and hard to trigger by accident."

## Known and accepted limits

These are stated determinations rather than open findings. If you rediscover one,
it is documented — please do not file it as a vulnerability.

**A report proves shape and position, never authorship.** `report` and `gate`
carry status between agents over herdr pane metadata and the pane's own terminal
buffer. Both channels authenticate that a well-formed report appeared in a given
pane after the gate opened, and nothing about who wrote it. `pane.report_metadata`
accepts an arbitrary pane id, `source` is provenance rather than a namespace, and
what comes back is a flat map with no attribution on it — so any process already
holding the herdr socket can write those slots onto another worker's pane and
release a coordinator's gate.

This is accepted because it crosses no privilege boundary: it requires code
already running as you, which could simply start the agent itself. A shared secret
would not close it either. The dispatch prompt sits in the worker's context beside
untrusted task text, so anything that can talk a worker into faking a report can
read the secret out of that same context and include it. Writing only to
`HERDR_PANE_ID` is this plugin's own discipline, not a guarantee the channel
enforces.

The practical consequence for a coordinator: **a `done` report is a claim.** Check
the pull request it names before acting as though work landed. `--require-pr`
exists for this.

**The report note is untrusted data and is never a predicate.** A worker's task
usually arrives as a GitHub issue whose body anyone could have written, so the
note reaches a coordinator quoted, announced as untrusted, and capped at 200
characters. The gate can match only on status and the presence of a PR number.
There is deliberately no `--note-contains`: it would hand whoever filed that issue
the decision of when the next agent starts. This is not an oversight to fix.

**The no-Go install path proves integrity, not authorship.** Without a Go
toolchain, `scripts/build.sh` downloads a release binary pinned to the manifest
version and verifies it against the `checksums.txt` from the same release. A
missing or mismatched checksum aborts, and no unverified download survives any
exit path. But both halves are published by whoever can publish releases on this
repository, and there is no signature or attestation. On that path you are
trusting this GitHub account. **With Go installed, the binary is compiled from the
source that was just cloned** — prefer it.

**The repository lock is fail-open.** An absent, unwritable or corrupt plugin
state directory degrades to a lock that excludes nothing, silently, and any lock
held longer than five minutes is taken. It serialises reconciles against races; it
is not a security control.

**Event hooks and the startup pass start coding agents autonomously.** They are
off unless `WORKTENDER_EVENTS` is explicitly set to a recognised truthy value.
Anything unrecognised leaves them off and says so. Superseded names
(`MUSTER_EVENTS`, `HERDR_WT_EVENTS`) enable nothing and are detected only so the
failure is loud. Enabling this is the user's decision — the shipped skill instructs
agents never to set it themselves.

## What is in scope

Worth reporting:

- a path by which `prune-apply` removes a worktree that one of its guards should
  have kept — uncommitted changes, a live agent, the caller's own directory, or a
  closed-but-unmerged pull request
- anything that removes a worktree without a merged pull request authorising it
- a way to make the install path run an unverified binary
- terminal escape or bidi content reaching a rendered cell unescaped
- privilege or capability gained beyond what the invoking user already had

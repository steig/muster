# Trust

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
it cloned — pinned to that tag rather than to `latest`, so reading `v0.4.1` and
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

See [SECURITY.md](../SECURITY.md) for the trust boundary in full, and for how to
report something privately.


---

[← README](../README.md)

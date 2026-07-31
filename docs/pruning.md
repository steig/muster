# How it decides what to remove

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

---

[← README](../README.md)

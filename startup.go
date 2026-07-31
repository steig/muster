package main

import (
	"fmt"
	"io"

	"github.com/steig/herdr-wt/internal/gitx"
	"github.com/steig/herdr-wt/internal/herdrapi"
	"github.com/steig/herdr-wt/internal/reconcile"
	"github.com/steig/herdr-wt/internal/repolock"
)

// startupCommand is what herdr's [[startup]] entry runs once, after the server
// is ready.
//
// It replaces the zsh original's `wt watch`: a 90s loop that woke up forever and
// asked the GitHub API about every worktree on every cycle. Nothing here loops,
// sleeps, or stays resident. It is ONE reconcile pass per repository and then
// the process exits.
//
// That is a complete replacement rather than a reduced one because the poll loop
// was answering the wrong question. Worktrees, workspaces and agents only change
// when something happens, and herdr already reports those things — the
// [[events]] hooks cover the whole session. What events cannot cover is the
// interval when herdr was NOT RUNNING: a worktree added from a plain shell, a
// workspace restored without the agent that used to live in it. That gap opens
// exactly once, at startup, which is exactly when this runs. A resident watcher
// re-derives at 90s intervals what an event already said the instant it was true.
//
// The alternative shape — a one-shot that launches a long-lived events.subscribe
// watcher — was rejected. It reintroduces the supervised resident process this
// slice exists to delete, and inherits the questions that come with one: who
// reaps it when herdr restarts, what happens when it dies quietly, and why a
// second copy is not now running. The [[events]] hooks already deliver the same
// stream with herdr doing the supervising.
func startupCommand(out io.Writer) error {
	// Startup shares the [[events]] opt-in rather than adding a switch of its
	// own. Both do the same autonomous thing — open workspaces and start coding
	// agents without being asked — and this one fires on every launch across
	// every repository, so it is the louder of the two. Someone who unset
	// HERDR_WT_EVENTS after a surprise would reasonably expect that to have
	// covered all of it; a second variable would let them opt out of one trigger
	// while the other kept firing.
	if !eventsEnabled() {
		fmt.Fprintf(out, "events are off; export %s=1 to reconcile worktrees at startup\n", eventsEnv)
		return nil
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	roots, err := openRepositories(client)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		fmt.Fprintln(out, "startup: no worktree workspaces open; nothing to reconcile")
		return nil
	}

	// One repository failing must not cost the others their only pass — there is
	// no second startup. Failures are collected and reported at the end so the
	// exit code still carries them: herdr records a command that exits 0 as
	// succeeded.
	var failed int
	for _, root := range roots {
		fmt.Fprintf(out, "\nstartup: %s\n", root)
		if err := reconcileAtStartup(out, client, root); err != nil {
			failed++
			fmt.Fprintf(out, "startup: %s: %v\n", root, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("startup reconcile failed for %d of %d repositor(ies)", failed, len(roots))
	}
	return nil
}

// openRepositories is every distinct repository herdr has a worktree workspace
// for, in herdr's own order.
//
// This is the scope, rather than the invocation context, because startup is not
// about where anyone is standing — at server start nobody is standing anywhere
// yet. It is about everything herdr just restored.
//
// Workspaces herdr does NOT report as worktrees are skipped, and that is the
// conservative direction on purpose. Such a workspace is a directory someone
// opened, which may happen to sit in a git repository this plugin was never
// pointed at; deriving a root from it would have startup adopting and staffing
// repositories nobody asked it to manage. `sync` remains the way to reach one.
func openRepositories(client *herdrapi.Client) ([]string, error) {
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var roots []string
	for _, ws := range workspaces.Workspaces {
		if ws.Worktree == nil || ws.Worktree.RepoRoot == "" {
			continue
		}
		// Normalised, because Collect compares workspace roots against this one
		// by equality and herdr's paths are resolved while ours may not be.
		root := gitx.Resolve(ws.Worktree.RepoRoot)
		if seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots, nil
}

// reconcileAtStartup runs the adopt-and-staff pass for one repository.
//
// It is deliberately the same collect/reconcile/execute pipeline `sync` and the
// event hook run, filtered the same way. A startup path with reconciling logic
// of its own would be a second answer to "what does this repository need", and
// the two would drift.
func reconcileAtStartup(out io.Writer, client *herdrapi.Client, root string) error {
	// CallerDir is empty for the same reason it is on the event path: nobody is
	// standing in a directory, and the guard it feeds governs removals, which
	// this path never performs.
	s := &session{client: client, root: root}

	collector := reconcile.NewCollector(client, root)
	// No gh, and this is the point of the slice. `wt watch` cost one GitHub API
	// call per worktree per cycle; a startup pass that asked once per worktree
	// would be the same call at a lower rate, which is a smaller version of the
	// thing being deleted rather than the end of it. It buys nothing here
	// regardless: PR state only ever authorises a prune, and prunes are filtered
	// out below.
	collector.LookupPR = nil

	// Claim the repository, or leave a mark and stand down. herdr emits
	// worktree.opened as it restores workspaces, so an event hook may already be
	// reconciling this very repository — and it is running this same pass.
	// Queueing behind it would only duplicate it.
	lock, err := repolock.AcquireOrMark(stateDir(), root)
	if err != nil {
		return err
	}
	if lock == nil {
		fmt.Fprintf(out, "startup: %s is already being reconciled; coalesced into that pass\n", root)
		return nil
	}
	defer lock.Release()

	return lock.Repeat(reconcilePasses, func() error {
		actions, err := s.planWith(collector)
		if err != nil {
			return err
		}
		// Adopt and staff only. Startup is the one moment with the least
		// information about what a human intends, so it is the last place that
		// should be removing checkouts.
		return s.perform(out, reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
	})
}

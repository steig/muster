// Package repolock serialises reconciles of one repository across processes.
//
// Every entry point — an event hook, `sync`, `prune-apply` — runs the same
// collect/reconcile/execute pipeline, so the only concurrency question left is
// two of them running over one repository at once. This is what answers it.
//
// The lock is best-effort by design. Reconcile is idempotent and monotone, so a
// lock that fails to exclude costs duplicated work, while one that fails closed
// costs a silent outage — a handler that never runs looks exactly like an event
// that never fired. So every unreadable, corrupt, expired or abandoned state
// resolves to taking the lock.
//
// Staleness has two independent stories. A crashed holder leaves a lock file
// behind, so a dead PID must not wedge the path; a holder can also be alive and
// wedged, which no liveness check catches, so a lock older than MaxHold is taken
// regardless. Liveness is the optimisation, the timestamp is the guarantee.
package repolock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaxHold is how long a lock is honoured before it is treated as abandoned.
//
// It must exceed the longest legitimate hold: staffing blocks for up to
// execute.AgentStartTimeout while herdr waits for a pane still running direnv,
// and a handler sitting there is healthy. Size this below that and a live lock
// is stolen rather than a stale one cleared — the second holder sees a workspace
// with no agent because the first is mid-start, and starts a second one.
//
// Five times that bound leaves room for several sequential staffings in one
// pass. It is not imported from execute; a test pins the relationship instead
// and fails if either side moves.
const MaxHold = 5 * time.Minute

// holder is what a lock file contains. The PID and time are the staleness
// evidence; the repository is there so a human reading the file can tell what
// it belongs to.
type holder struct {
	PID        int    `json:"pid"`
	StartedAt  int64  `json:"started_unix_ms"`
	Repository string `json:"repository"`
}

// Lock is a held claim on one repository. A Lock with no path is the degraded
// case — the state directory was unusable — and behaves as a claim that
// excludes nothing, which is the correct way to fail.
type Lock struct {
	path     string
	dirty    string
	acquired bool
}

// Acquire claims the repository. It returns nil when another live, recent
// holder has it; that is a normal outcome, not an error.
//
// A caller that comes back nil should mark the repository dirty and exit: the
// holder is running the same whole-repository reconcile it wanted to run, and
// the mark makes sure the holder does not finish on a snapshot that predates it.
func Acquire(stateDir, repository string) (*Lock, error) {
	if stateDir == "" {
		// Nowhere to record a claim. Proceeding unserialised is strictly better
		// than not proceeding.
		return &Lock{}, nil
	}

	path := lockPath(stateDir, repository)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return &Lock{}, nil
	}

	lock := &Lock{path: path, dirty: dirtyPath(stateDir, repository)}
	acquired, usable := lock.claim(repository)
	switch {
	case acquired:
		return lock, nil
	case !usable:
		// The state directory cannot carry a claim. Same answer as having
		// nowhere to record one: proceed unserialised rather than mistake a
		// broken lock path for a busy repository and never run again.
		return &Lock{}, nil
	}

	// Someone holds it. Take it anyway if the evidence says they are gone.
	if !heldByALiveHolder(path) {
		// Removing another process's lock file is safe here precisely because
		// the reconcile it guards is idempotent.
		_ = os.Remove(path)
		switch acquired, usable = lock.claim(repository); {
		case acquired:
			return lock, nil
		case !usable:
			return &Lock{}, nil
		}
	}
	return nil, nil
}

// AcquireOrMark claims the repository, or marks it dirty for the current holder
// and returns nil. This is what an event hook wants: the holder is running the
// same whole-repository pass, so queueing behind it would only duplicate it.
func AcquireOrMark(stateDir, repository string) (*Lock, error) {
	lock, err := Acquire(stateDir, repository)
	if err != nil || lock != nil {
		return lock, err
	}
	if err := MarkDirty(stateDir, repository); err != nil {
		return nil, err
	}

	// The holder may have released between the failed claim and the mark, which
	// would leave the mark with nobody to act on it. Retrying closes most of
	// that window; what remains is a delay, repaired by the next event or the
	// next reconcile — the backstop this design already leans on.
	return Acquire(stateDir, repository)
}

// AcquireWithin waits up to timeout for a busy repository before giving up. A
// human is attached to the commands that use it, so a brief wait is better than
// an immediate refusal.
func AcquireWithin(stateDir, repository string, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := Acquire(stateDir, repository)
		if err != nil || lock != nil {
			return lock, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Repeat runs body, then runs it again while work was marked during a pass.
//
// The mark is cleared BEFORE the first pass and checked AFTER each one, which is
// what makes the coalescing sound: a mark predating the pass is satisfied by it,
// and a mark arriving mid-pass survives to trigger the next. Clearing after the
// work instead would drop exactly the events that arrived while it ran.
//
// maxPasses bounds it. Two is the natural maximum — one pass, plus one for
// whatever arrived during it — so the cap only matters if events are arriving
// faster than the repository can be reconciled, and dropping to the next trigger
// is the right answer there.
func (l *Lock) Repeat(maxPasses int, body func() error) error {
	l.TakeDirty()
	for pass := 0; pass < maxPasses; pass++ {
		if err := body(); err != nil {
			return err
		}
		if !l.TakeDirty() {
			return nil
		}
	}
	return nil
}

// claim writes the holder record into a temporary file and links it into place,
// so the lock file carries its evidence from the instant it exists. A claim made
// first and filled in afterwards is readable in between, and an unreadable lock
// file is treated as abandoned by every reader — a live lock stolen rather than
// a stale one cleared.
//
// link rather than rename: rename replaces an existing target, which would take
// a lock somebody else holds. link refuses, which is the mutual exclusion.
//
// acquired reports the claim; usable reports whether the state directory can
// carry one at all. A directory that cannot is degraded to unserialised rather
// than treated as busy, because this lock fails open by design.
func (l *Lock) claim(repository string) (acquired, usable bool) {
	record, err := json.Marshal(holder{
		PID:        os.Getpid(),
		StartedAt:  time.Now().UnixMilli(),
		Repository: repository,
	})
	if err != nil {
		return false, false
	}

	file, err := os.CreateTemp(filepath.Dir(l.path), "claim-*")
	if err != nil {
		return false, false
	}
	// The temporary name is never the lock; it is unlinked either way, and on
	// the success path the link at l.path keeps the content alive.
	defer os.Remove(file.Name())

	if _, err := file.Write(record); err != nil {
		file.Close()
		return false, false
	}
	if err := file.Close(); err != nil {
		return false, false
	}

	if err := os.Link(file.Name(), l.path); err != nil {
		// Someone got there first: an ordinary busy outcome.
		if errors.Is(err, os.ErrExist) {
			return false, true
		}
		return false, false
	}

	l.acquired = true
	return true, true
}

// heldByALiveHolder reports whether the existing lock file is evidence of a
// holder still worth waiting for. Anything it cannot read or parse counts as
// abandoned.
func heldByALiveHolder(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var h holder
	if err := json.Unmarshal(raw, &h); err != nil {
		return false
	}
	if time.Since(time.UnixMilli(h.StartedAt)) > MaxHold {
		return false
	}
	return processAlive(h.PID)
}

// Release drops the claim. Releasing a degraded lock is a no-op.
func (l *Lock) Release() error {
	if !l.acquired {
		return nil
	}
	l.acquired = false
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release repository lock: %w", err)
	}
	return nil
}

// MarkDirty records that the repository needs reconciling again, for whoever
// currently holds the lock to pick up.
func MarkDirty(stateDir, repository string) error {
	if stateDir == "" {
		return nil
	}

	path := dirtyPath(stateDir, repository)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	return os.WriteFile(path, nil, 0o600)
}

// TakeDirty reports whether more work was marked while this lock was held, and
// clears the mark so the holder loops once rather than forever.
//
// It is deliberately checked and cleared under the lock: clearing first and
// reconciling second would drop a mark that arrived mid-pass.
func (l *Lock) TakeDirty() bool {
	if l.dirty == "" {
		return false
	}
	if err := os.Remove(l.dirty); err != nil {
		return false
	}
	return true
}

// lockPath names the lock for a repository. The path is hashed because a
// repository path contains separators and is longer than some filesystems allow
// as a single component.
func lockPath(stateDir, repository string) string {
	return filepath.Join(stateDir, "locks", digest(repository)+".lock")
}

func dirtyPath(stateDir, repository string) string {
	return filepath.Join(stateDir, "locks", digest(repository)+".dirty")
}

func digest(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	return hex.EncodeToString(sum[:16])
}

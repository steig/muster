package repolock

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/steig/worktender/internal/execute"
)

const repo = "/tmp/example-repo"

// deadPID returns a process id that is definitely not running: a real child is
// started and reaped, so the number was valid a moment ago rather than being a
// guess that might collide with something live.
func deadPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// writeHolder plants a lock file as another process would have left it, so the
// staleness rules can be tested without arranging a real crashed holder.
func writeHolder(t *testing.T, stateDir, repository string, pid int, at time.Time) {
	t.Helper()

	path := lockPath(stateDir, repository)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record, err := json.Marshal(holder{
		PID: pid, StartedAt: at.UnixMilli(), Repository: repository,
	})
	if err != nil {
		t.Fatalf("marshal holder: %v", err)
	}
	if err := os.WriteFile(path, record, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func TestAcquireExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if first == nil {
		t.Fatal("first acquire came back busy on a fresh directory")
	}

	second, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("second acquire errored instead of reporting busy: %v", err)
	}
	if second != nil {
		t.Error("two holders acquired the same repository at once")
	}
}

func TestReleaseAllowsReacquisition(t *testing.T) {
	dir := t.TempDir()

	held, err := Acquire(dir, repo)
	if err != nil || held == nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, err := Acquire(dir, repo)
	if err != nil || again == nil {
		t.Fatalf("could not reacquire after release: %v", err)
	}
}

// Different repositories must not contend. The whole point of the lock is to
// serialise reconciles of ONE repository.
func TestDifferentRepositoriesDoNotContend(t *testing.T) {
	dir := t.TempDir()

	a, err := Acquire(dir, "/tmp/repo-a")
	if err != nil || a == nil {
		t.Fatalf("acquire a: %v", err)
	}
	b, err := Acquire(dir, "/tmp/repo-b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if b == nil {
		t.Error("a lock on one repository blocked a different repository")
	}
}

// The failure this exists to prevent: unlinking a plugin does not remove its
// state directory, so a lock file outlives the process that wrote it — and a
// crashed handler must not wedge the fast path permanently. A silently
// never-running handler is indistinguishable from "no events fired".
func TestLockFromADeadHolderIsTaken(t *testing.T) {
	dir := t.TempDir()
	writeHolder(t, dir, repo, deadPID(t), time.Now())

	took, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if took == nil {
		t.Fatal("a lock held by a dead process was not taken; the fast path is wedged forever")
	}
}

// The other side of it: a live holder must actually be respected, or the lock
// excludes nothing and the coalescing is theatre.
func TestLockFromALiveHolderIsRespected(t *testing.T) {
	dir := t.TempDir()
	writeHolder(t, dir, repo, os.Getpid(), time.Now())

	got, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got != nil {
		t.Error("acquired a lock held by a live, recent holder")
	}
}

// A process can be alive and wedged. Liveness alone would block forever, so the
// timestamp — not the PID check — is what makes "never fail closed" true.
func TestExpiredLockIsTakenEvenFromALiveHolder(t *testing.T) {
	dir := t.TempDir()
	writeHolder(t, dir, repo, os.Getpid(), time.Now().Add(-2*MaxHold))

	took, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if took == nil {
		t.Fatal("an expired lock was not taken from a live holder")
	}
}

// The bound must clear the longest LEGITIMATE hold, not the typical one.
//
// Staffing blocks for up to execute.AgentStartTimeout while herdr waits for a
// pane that is still running direnv, and a handler sitting there is healthy.
// A bound below that does not clear stale locks, it steals live ones — and the
// second holder then issues a duplicate agent start against a pane the first is
// still mid-start on, which idempotence cannot save because the state has not
// been created yet.
//
// The two constants live in different packages on purpose, so this is what stops
// them drifting apart.
func TestMaxHoldExceedsTheLongestLegitimateHold(t *testing.T) {
	if MaxHold <= execute.AgentStartTimeout {
		t.Fatalf("MaxHold %s does not exceed a single agent start (%s); a healthy holder would be evicted mid-staff",
			MaxHold, execute.AgentStartTimeout)
	}
	// One staffing is the floor, not the target: a pass can staff several
	// worktrees in sequence, each waiting the full timeout.
	if margin := MaxHold / execute.AgentStartTimeout; margin < 3 {
		t.Errorf("MaxHold is only %dx a single agent start; a pass staffing several worktrees would be evicted", margin)
	}
}

// The concrete version of the same trap: a live holder that has legitimately
// held the lock for longer than a naive bound must still be respected.
func TestALongButLegitimateHoldIsRespected(t *testing.T) {
	dir := t.TempDir()

	// Longer than a naively-chosen 30s bound, and longer than one agent start,
	// but well inside MaxHold — exactly the shape of a healthy staffing pass.
	held := execute.AgentStartTimeout + 30*time.Second
	if held >= MaxHold {
		t.Fatalf("fixture age %s is not inside MaxHold %s; this test proves nothing", held, MaxHold)
	}
	writeHolder(t, dir, repo, os.Getpid(), time.Now().Add(-held))

	got, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got != nil {
		t.Errorf("stole a lock from a live holder %s in; it was staffing, not wedged", held)
	}
}

// Corruption must not be fatal either. Unreadable state is a reason to proceed,
// not a reason to stop: the reconciler is idempotent, so the worst case of
// taking the lock wrongly is duplicated work, while refusing forever is a
// silent outage.
func TestCorruptLockFileIsTaken(t *testing.T) {
	dir := t.TempDir()

	path := lockPath(dir, repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	took, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("a corrupt lock must not be an error: %v", err)
	}
	if took == nil {
		t.Fatal("a corrupt lock file wedged the fast path")
	}
}

// Coalescing: an event that cannot get the lock leaves a mark, and the holder
// picks it up rather than finishing on a snapshot that predates the event.
func TestDirtyMarkIsSeenByTheHolder(t *testing.T) {
	dir := t.TempDir()

	held, err := Acquire(dir, repo)
	if err != nil || held == nil {
		t.Fatalf("acquire: %v", err)
	}
	if held.TakeDirty() {
		t.Error("a fresh lock reported pending work")
	}

	if err := MarkDirty(dir, repo); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}

	if !held.TakeDirty() {
		t.Fatal("the holder did not see work marked while it was running")
	}
	// Taking it clears it, so the holder loops once and not forever.
	if held.TakeDirty() {
		t.Error("the dirty mark was not cleared when taken")
	}
}

func TestDirtyMarkIsPerRepository(t *testing.T) {
	dir := t.TempDir()

	held, err := Acquire(dir, "/tmp/repo-a")
	if err != nil || held == nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := MarkDirty(dir, "/tmp/repo-b"); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}

	if held.TakeDirty() {
		t.Error("a mark against one repository woke a holder of another")
	}
}

// An unwritable state directory must not stop the reconcile. The lock is an
// optimisation against duplicate work; it is never a precondition for working.
func TestAnUnusableStateDirDoesNotBlock(t *testing.T) {
	took, err := Acquire("", repo)
	if err != nil {
		t.Fatalf("an unusable state dir must not error: %v", err)
	}
	if took == nil {
		t.Fatal("an unusable state dir blocked the reconcile; the lock must never fail closed")
	}
	if err := took.Release(); err != nil {
		t.Errorf("release of an unlocked holder should be a no-op, got %v", err)
	}
}

// A lock file that does not parse is treated as ABANDONED by every reader, so a
// claim that becomes readable before it is complete is a live lock that can be
// stolen while it is held — and the file's own MaxHold note says what a stolen
// live lock costs: a second agent.start against a pane mid-start, landing on a
// conversation that exists nowhere else.
//
// The invariant is therefore not "the record is written eventually" but "the
// lock file never exists in a state a reader would discard". This asserts it
// under contention; a create-then-write claim leaves exactly that window.
func TestAClaimIsNeverObservableBeforeItIsComplete(t *testing.T) {
	dir := t.TempDir()
	path := lockPath(dir, repo)

	stop := make(chan struct{})
	unreadable := make(chan []byte, 1)

	var watching sync.WaitGroup
	watching.Add(1)
	go func() {
		defer watching.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue // no lock held at this instant, which is fine
			}
			var h holder
			if json.Unmarshal(raw, &h) != nil || h.PID == 0 {
				select {
				case unreadable <- raw:
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 3000; i++ {
		lock, err := Acquire(dir, repo)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if lock == nil {
			t.Fatal("nothing else holds this lock, so the claim should have succeeded")
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	close(stop)
	watching.Wait()

	select {
	case raw := <-unreadable:
		t.Fatalf("a held lock was observable as %q, which any other process would treat as abandoned and take", raw)
	default:
	}
}

// A state directory that cannot carry a claim must not read as a busy
// repository. The lock fails OPEN by design: a claim that can never be made
// would stop every reconcile, and a handler that never runs looks exactly like
// an event that never fired.
func TestAStateDirThatCannotBeWrittenDegradesRatherThanBlocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory anyway")
	}

	dir := t.TempDir()
	locks := filepath.Dir(lockPath(dir, repo))
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(locks, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locks, 0o700) })

	lock, err := Acquire(dir, repo)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lock == nil {
		t.Fatal("an unwritable lock directory was reported as another process holding the repository")
	}
	if err := lock.Release(); err != nil {
		t.Errorf("releasing a degraded lock should be a no-op, got %v", err)
	}
}

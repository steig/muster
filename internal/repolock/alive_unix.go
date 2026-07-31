//go:build !windows

package repolock

import "syscall"

// processAlive reports whether a process id still names a running process.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists but belongs to someone else, which
// is still alive; only ESRCH means gone.
//
// This is an optimisation, not the correctness guarantee. A PID can be recycled
// onto an unrelated process, which would make a dead holder look alive — the
// maxHold timestamp is what bounds that, and what makes the lock behave on
// platforms where this check is weaker.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

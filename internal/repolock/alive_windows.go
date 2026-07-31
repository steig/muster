package repolock

import "os"

// processAlive reports whether a process id still names a running process.
//
// Windows has no signal 0, but os.FindProcess opens a real handle there and
// fails when the process is gone, unlike the Unix implementation where it
// always succeeds. The maxHold timestamp remains the actual guarantee; see the
// package comment.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = process.Release()
	return true
}

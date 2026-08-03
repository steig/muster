//go:build windows

package herdrapi

import (
	"io"
	"os"
)

// dialHerdr opens herdr's named pipe. Go has no native named-pipe dialer, but a
// pipe opened by path behaves as a read/write file handle, which is all the
// NDJSON protocol needs. The handle has no deadline support, so call() skips it.
func dialHerdr(socketPath string) (io.ReadWriteCloser, error) {
	return os.OpenFile(socketPath, os.O_RDWR, 0)
}

// herdrHome is empty on windows, and that is a claim about knowledge rather
// than about herdr.
//
// herdr is addressed here by a named pipe, not by a socket under a config
// directory, and worktender does not know how that pipe is named. The unix
// layout would be worse than useless: a ~/.config-shaped path essentially never
// exists on windows, so dialling it returns ENOENT — which Probe reads as
// *proof* that herdr is not running. That is the permissive classification, and
// it would fire unconditionally on a platform release.yml ships binaries for,
// unlocking the degraded path without ever addressing herdr at all.
//
// Empty makes Probe answer ErrHerdrUnknown instead, so windows refuses rather
// than assumes. herdr still exports HERDR_SOCKET_PATH into the commands it
// runs, so the plugin path is unaffected — it names the pipe and Probe dials it.
// Discovering the pipe for a bare windows shell is a separate piece of work.
func herdrHome() string { return "" }

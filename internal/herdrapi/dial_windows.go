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

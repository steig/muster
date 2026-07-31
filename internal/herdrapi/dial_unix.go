//go:build !windows

package herdrapi

import (
	"io"
	"net"
	"time"
)

// dialHerdr opens herdr's unix domain socket. net.Conn already satisfies
// io.ReadWriteCloser and carries deadlines.
func dialHerdr(socketPath string) (io.ReadWriteCloser, error) {
	return net.DialTimeout("unix", socketPath, 2*time.Second)
}

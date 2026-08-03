//go:build !windows

package herdrapi

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// herdrHome is the directory herdr keeps its sessions under:
// $XDG_CONFIG_HOME/herdr, falling back to ~/.config/herdr.
//
// It is ~/.config even on darwin, where os.UserConfigDir would say
// ~/Library/Application Support. herdr uses the XDG layout on every unix, so
// following the Go helper would look where herdr never puts anything. Verified
// against herdr 0.7.5: `herdr status` reports a socket under exactly this
// directory and relocates it wholesale when XDG_CONFIG_HOME moves.
//
// Empty when there is no home directory to derive it from, which callers must
// treat as "cannot tell" rather than as an empty search.
func herdrHome() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "herdr")
}

// dialHerdr opens herdr's unix domain socket. net.Conn already satisfies
// io.ReadWriteCloser and carries deadlines.
func dialHerdr(socketPath string) (io.ReadWriteCloser, error) {
	return net.DialTimeout("unix", socketPath, 2*time.Second)
}

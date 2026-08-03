package herdrapi_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/steig/worktender/internal/herdrapi"
)

// herdrHome points DefaultSocketPath at a directory this test owns and returns
// the socket path it now resolves to. Short, because macOS caps unix socket
// paths near 104 bytes and t.TempDir() names are long.
func herdrHome(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "herdr", "herdr.sock")
}

func listen(t *testing.T, path string) net.Listener {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// The variable unset is the ordinary state of a user's terminal and says
// nothing about whether herdr is running. Probe has to answer the second
// question, because everything that degrades on its answer is deciding whether
// a checkout may be removed.
func TestProbeFindsHerdrWhenNothingNamedItsSocket(t *testing.T) {
	socket := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	listen(t, socket)

	if _, err := herdrapi.Probe(); err != nil {
		t.Fatalf("herdr is listening on its default socket; Probe said %v", err)
	}
}

// The same environment variable, the opposite fact. Nothing listening anywhere
// a probe would look is what herdr being absent actually means.
func TestProbeReportsHerdrDownWhenNothingIsListening(t *testing.T) {
	herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")

	_, err := herdrapi.Probe()
	if err == nil {
		t.Fatal("nothing is listening; Probe found a herdr")
	}
	if !errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("error should be ErrNoHerdr so callers can branch on it, got %v", err)
	}
}

// A shell that outlived the herdr that exported the variable. New is satisfied
// by the string; only a dial is not.
func TestProbeReportsHerdrDownOnAStaleSocketPath(t *testing.T) {
	// Not t.TempDir: its names run past the ~104-byte cap darwin puts on a
	// socket address, and the dial then fails with EINVAL — which is correctly
	// classed as "could not tell" and would test the wrong branch.
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, filepath.Join(dir, "gone.sock"))

	if _, err := herdrapi.Probe(); !errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("a named socket with nothing behind it is herdr down, got %v", err)
	}
	// The contrast that makes the point: New is happy with the same env.
	if _, err := herdrapi.New(); err != nil {
		t.Errorf("New only reads the variable, so it should be satisfied: %v", err)
	}
}

// A named socket that is live wins over the default, because it is the session
// that started this process.
func TestProbePrefersTheSocketHerdrNamed(t *testing.T) {
	herdrHome(t)

	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	named := filepath.Join(dir, "n.sock")
	listen(t, named)
	t.Setenv(herdrapi.SocketEnv, named)

	if got := herdrapi.SocketPath(); got != named {
		t.Errorf("SocketPath = %q, want the named socket %q", got, named)
	}
	if _, err := herdrapi.Probe(); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// herdr relocates its whole config directory with XDG_CONFIG_HOME, and uses
// that layout on darwin too — where os.UserConfigDir would say ~/Library.
func TestDefaultSocketPathFollowsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/somewhere/else")

	want := filepath.Join("/somewhere/else", "herdr", "herdr.sock")
	if got := herdrapi.DefaultSocketPath(); got != want {
		t.Errorf("DefaultSocketPath = %q, want %q", got, want)
	}
}

// A dial that did not finish is not an answer. Only ENOENT and ECONNREFUSED
// prove nothing is there; a timeout, a permission denial or an unaddressable
// endpoint leave the question open, and a caller that degrades on the open
// question removes a checkout an agent may be standing in.
func TestProbeSeparatesProvenAbsenceFromAFailedDial(t *testing.T) {
	// A directory in place of a socket: the dial cannot succeed and cannot
	// prove absence either.
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(herdrapi.SocketEnv, dir)

	_, err = herdrapi.Probe()
	if errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("a dial that established nothing must not read as proven absence: %v", err)
	}
	if !errors.Is(err, herdrapi.ErrHerdrUnknown) {
		t.Errorf("want ErrHerdrUnknown so callers refuse to degrade, got %v", err)
	}
}

// The stale socket file a herdr that exited leaves behind: the path is there,
// nobody is accepting. That is proof, and it is the case worth degrading on.
func TestProbeReadsARefusedConnectionAsProvenAbsence(t *testing.T) {
	socket := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	ln := listen(t, socket)

	if _, err := herdrapi.Probe(); err != nil {
		t.Fatalf("setup: herdr should be reachable first: %v", err)
	}

	// Stop accepting but leave the socket inode on disk, which is what a herdr
	// killed without cleaning up leaves behind. Go unlinks it on Close by
	// default, and a plain file in its place would fail with ENOTSOCK — a
	// different error, and not the one this is about.
	unix, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("want a unix listener, got %T", ln)
	}
	unix.SetUnlinkOnClose(false)
	if err := unix.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the socket file should still be there: %v", err)
	}

	if _, err := herdrapi.Probe(); !errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("a socket path with nobody accepting is proven absence, got %v", err)
	}
}

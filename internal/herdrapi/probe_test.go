package herdrapi_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A shell that outlived the herdr that exported the variable, and whose socket
// went with it, on a machine running no herdr at all. New is satisfied by the
// string; only a dial is not.
//
// Absence here rests on every endpoint being missing, not on the named one
// being missing — see TestProbeFallsThroughAStaleVariableToARunningSession for
// the case that separates those.
func TestProbeReportsHerdrDownWhenTheNamedSocketIsGone(t *testing.T) {
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
		t.Errorf("nothing is listening anywhere, so this is herdr down, got %v", err)
	}
	// The contrast that makes the point: New is happy with the same env.
	if _, err := herdrapi.New(); err != nil {
		t.Errorf("New only reads the variable, so it should be satisfied: %v", err)
	}
}

// The residual the fixed blocker left behind, one level down: a herdr running
// as a named session, and nothing at the default path.
//
// A plain shell has no HERDR_SOCKET_PATH, so the search starts at the default
// socket and finds nothing there. Reading that as proof would be the original
// bug in a narrower doorway — degrade, and prune-apply force-removes a checkout
// that session's agent is standing in. Absence has to mean no endpoint anywhere.
func TestProbeFindsAHerdrRunningOnlyAsANamedSession(t *testing.T) {
	home := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	// The default session is not running: only `work` is.
	listen(t, filepath.Join(filepath.Dir(home), "sessions", "work", "herdr.sock"))

	client, err := herdrapi.Probe()
	if err != nil {
		t.Fatalf("a named session is running; Probe must find it, got %v", err)
	}
	if client == nil {
		t.Fatal("want a live client for the named session")
	}
}

// The mirror: a stale HERDR_SOCKET_PATH from an exited session, while another
// herdr is running. The variable names a path that is gone, which is evidence
// about the variable and none about the machine, so the search falls through.
func TestProbeFallsThroughAStaleVariableToARunningSession(t *testing.T) {
	home := herdrHome(t)
	listen(t, home)

	gone, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(gone) })
	t.Setenv(herdrapi.SocketEnv, filepath.Join(gone, "exited.sock"))

	if _, err := herdrapi.Probe(); err != nil {
		t.Fatalf("herdr is running; a stale variable must not read as absence, got %v", err)
	}
}

// Two sessions running and nothing saying which. Guessing is not the smaller
// error: the wrong session lists the wrong workspaces, so a checkout with a
// live agent in it reads as held by nobody and prune-apply removes it.
func TestProbeRefusesToGuessBetweenTwoRunningSessions(t *testing.T) {
	home := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	listen(t, home)
	listen(t, filepath.Join(filepath.Dir(home), "sessions", "work", "herdr.sock"))

	_, err := herdrapi.Probe()
	if !errors.Is(err, herdrapi.ErrHerdrUnknown) {
		t.Fatalf("want ErrHerdrUnknown rather than a guess, got %v", err)
	}
	if !strings.Contains(err.Error(), herdrapi.SocketEnv) {
		t.Errorf("the error should say how to disambiguate, got %v", err)
	}
}

// A socket beside herdr's that is not a session endpoint must not count as one.
// herdr keeps a herdr-client.sock next to the server's, and a plugin checkout
// can hold unrelated sockets — git's fsmonitor daemon leaves one. Counting
// those as a running herdr would make worktender refuse for ever on a machine
// that has none.
func TestProbeIgnoresSocketsThatAreNotSessionEndpoints(t *testing.T) {
	home := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	listen(t, filepath.Join(filepath.Dir(home), "herdr-client.sock"))
	listen(t, filepath.Join(filepath.Dir(home), "plugins", "fsmonitor.ipc"))

	if _, err := herdrapi.Probe(); !errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("no session endpoint is listening, so herdr is down, got %v", err)
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

// A dial that did not finish is not an answer. Only a missing socket proves
// nothing is there; everything else leaves the question open, and a caller that
// degrades on the open question removes a checkout an agent may be standing in.
//
// A directory in place of a socket is the case that pins the platforms
// together: dialling one fails with ENOTSOCK on darwin and ECONNREFUSED on
// linux, so a classifier reading errnos other than ENOENT as proof gives two
// different verdicts for one on-disk state — and gives the permissive one on
// linux only.
func TestProbeSeparatesProvenAbsenceFromAFailedDial(t *testing.T) {
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

// A live herdr that is merely too busy to accept must never read as absent.
//
// On darwin a listening socket whose accept queue is full refuses the
// connection outright — the same ECONNREFUSED a socket with nobody behind it
// gives. That is why ECONNREFUSED is not proof: this herdr is up, holding
// workspaces, with agents in its panes, and degrading here would plan to remove
// the ground out from under them.
func TestProbeDoesNotCallABackedUpHerdrAbsent(t *testing.T) {
	socket := herdrHome(t)
	t.Setenv(herdrapi.SocketEnv, "")
	listen(t, socket)

	// Fill the accept queue: the listener above never calls Accept.
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for i := 0; i < 512; i++ {
		c, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if err != nil {
			break
		}
		held = append(held, c)
	}

	// Whether the queue actually filled is a kernel tuning detail, so this
	// asserts the only thing that must hold either way: a listener that is
	// there is never reported as proven absence.
	if _, err := herdrapi.Probe(); errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Fatalf("herdr is listening; a refused or slow dial must not read as absent: %v", err)
	}
}

// The stale socket file a herdr killed without cleaning up leaves behind: the
// path is there and nobody is accepting.
//
// It reads as absent to a person and is not proof to this code, because darwin
// gives the identical ECONNREFUSED for a live listener with a full queue — see
// TestProbeDoesNotCallABackedUpHerdrAbsent. So worktender refuses instead of
// degrading, and says how to clear it. Refusing costs an error message;
// degrading wrongly costs an agent's checkout.
func TestProbeWillNotDegradeOnAStaleSocketItCannotTellFromABusyOne(t *testing.T) {
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

	_, err := herdrapi.Probe()
	if errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("indistinguishable from a busy listener on darwin, so it is not proof: %v", err)
	}
	if !errors.Is(err, herdrapi.ErrHerdrUnknown) {
		t.Fatalf("want ErrHerdrUnknown, got %v", err)
	}
	// The state persists until somebody clears it, so the error has to say how.
	if !strings.Contains(err.Error(), socket) {
		t.Errorf("the error should name the socket to remove, got %v", err)
	}
}

// Not knowing where to look is not an empty search.
//
// This is the rule windows runs on: herdr is a named pipe there, worktender
// does not know its name, so herdrHome is empty and there is no endpoint to
// examine. Absence is proven by having looked and found nothing, so zero places
// to look must refuse — otherwise the degraded path unlocks unconditionally on
// a platform release.yml ships and CI does not run.
//
// Reached here through the same code by leaving no home to derive the layout
// from, because a windows runner is not available to assert it directly.
func TestProbeRefusesWhenThereIsNowhereToLook(t *testing.T) {
	t.Setenv(herdrapi.SocketEnv, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if got := herdrapi.SessionSocketPaths(); len(got) != 0 {
		t.Fatalf("no layout is derivable, so there is nothing to enumerate, got %v", got)
	}
	_, err := herdrapi.Probe()
	if errors.Is(err, herdrapi.ErrNoHerdr) {
		t.Errorf("nowhere to look is not proof that nothing is there: %v", err)
	}
	if !errors.Is(err, herdrapi.ErrHerdrUnknown) {
		t.Fatalf("want ErrHerdrUnknown, got %v", err)
	}
}

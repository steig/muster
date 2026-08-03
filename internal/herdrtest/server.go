// Package herdrtest provides a fake herdr for tests: a unix socket that speaks
// the same NDJSON protocol as the real server, plus a fake `gh` on PATH.
//
// Tests drive real git in a temp directory and this fake herdr, so nothing
// touches the developer's live session.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// socketEnv is herdrapi.SocketEnv, spelled out rather than imported: herdrapi's
// own tests live in package herdrapi, and a dependency from here to there would
// stop them ever using this fake.
const socketEnv = "HERDR_SOCKET_PATH"

// CodedError makes a handler reply with a chosen herdr error code. Callers
// branch on the code and not the message, so a test that cares which failure it
// is cannot use a bare error — every one of those arrives as `handler_error`.
type CodedError struct {
	Code    string
	Message string
}

func (e *CodedError) Error() string { return e.Message }

// Handler answers one method call. Returning an error makes the server reply
// with a herdr-shaped error object.
type Handler func(params map[string]any) (any, error)

// Server is a fake herdr listening on a unix socket.
type Server struct {
	SocketPath string

	mu       sync.Mutex
	handlers map[string]Handler
	streams  map[string]streamHandler
	calls    []Call
	listener net.Listener
}

// Call records one request the server received.
type Call struct {
	Method string
	Params map[string]any
}

// shortTempDir is a temp directory with a short name, removed when the test
// ends. macOS caps unix socket paths near 104 bytes and t.TempDir() names are
// long, so anything with a socket under it needs this instead.
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "hwt")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// NewServer starts a fake herdr and stops it when the test ends.
func NewServer(t *testing.T) *Server {
	t.Helper()
	return NewServerAt(t, filepath.Join(shortTempDir(t), "h.sock"))
}

// NewServerAt starts a fake herdr on a socket path the caller chose.
//
// It exists so a test can put herdr where worktender will look for it without
// being told — at herdrapi.DefaultSocketPath — which is the only way to build
// the state that separates "herdr is not running" from "nobody named its
// socket". Those two are the same environment as far as HERDR_SOCKET_PATH is
// concerned, and opposite as far as removing a checkout is concerned.
func NewServerAt(t *testing.T, path string) *Server {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	// Fail loudly rather than let the listen error read as a bug in the code
	// under test. Only darwin enforces this, so a suite that passes on Linux
	// would otherwise fail mysteriously on the maintainer's machine.
	if len(path) > 100 {
		t.Fatalf("socket path is %d bytes, past the platform limit: %s", len(path), path)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}

	s := &Server{
		SocketPath: path,
		handlers:   map[string]Handler{},
		streams:    map[string]streamHandler{},
		listener:   ln,
	}
	t.Cleanup(func() { _ = ln.Close() })

	go s.serve()
	return s
}

// isolateHerdrHome points herdrapi.DefaultSocketPath at a directory belonging to
// this test and returns the socket path it now resolves to.
//
// Without this, a test that clears HERDR_SOCKET_PATH would fall through to
// ~/.config/herdr on the machine running it — and on a maintainer's machine
// that is a live herdr holding real workspaces. The suite would then reconcile
// the developer's own worktrees, which is the failure mode this whole helper
// exists to make impossible.
func isolateHerdrHome(t *testing.T) string {
	t.Helper()

	home := shortTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", home)
	return filepath.Join(home, "herdr", "herdr.sock")
}

// HerdrDown is the environment of a machine with no herdr running: no socket
// named, and nothing listening where one would be looked for by default.
//
// This is the state the degraded path is for, and establishing it takes both
// halves. Clearing HERDR_SOCKET_PATH alone proves only that clearing a variable
// takes the nil-client branch — see HerdrUnnamed for the state that is the same
// on that reading and the opposite in effect.
func HerdrDown(t *testing.T) {
	t.Helper()

	t.Setenv(socketEnv, "")
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	isolateHerdrHome(t)
}

// StaleHerdrSocket leaves a socket at herdr's default endpoint with nothing
// accepting on it, which is what a herdr killed without cleaning up leaves
// behind. Call it after HerdrDown.
//
// It builds the third state, the one that is neither: dialling gives
// ECONNREFUSED rather than ENOENT, so herdr is not proven absent — and on
// darwin that is the same answer a herdr too busy to accept gives. Callers must
// refuse rather than degrade.
func StaleHerdrSocket(t *testing.T) {
	t.Helper()

	socket := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "herdr", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen %s: %v", socket, err)
	}
	// Stop accepting but leave the inode: Go unlinks it on Close by default,
	// and a plain file in its place fails with ENOTSOCK, which is a different
	// error from the one this is building.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
}

// HerdrUnnamed is a running herdr that did not start this process: the state of
// the user's own terminal, where HERDR_SOCKET_PATH is unset and herdr is
// nonetheless up, holding workspaces with agents in their panes.
//
// It is indistinguishable from HerdrDown by the environment variable and the
// opposite of it in consequence — degrading here disarms the guard that spares
// a checkout an agent is standing in. The returned server is listening exactly
// where a probe will look.
func HerdrUnnamed(t *testing.T) *Server {
	t.Helper()

	t.Setenv(socketEnv, "")
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	return NewServerAt(t, isolateHerdrHome(t))
}

// Handle registers the reply for a method.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// HandleResult registers a static reply for a method.
func (s *Server) HandleResult(method string, result any) {
	s.Handle(method, func(map[string]any) (any, error) { return result, nil })
}

// HandleSlow registers a reply that arrives only after delay, so a test can
// prove the client's deadline outlasts a wait it asked herdr to perform.
func (s *Server) HandleSlow(method string, delay time.Duration, result any) {
	s.Handle(method, func(map[string]any) (any, error) {
		time.Sleep(delay)
		return result, nil
	})
}

// Pump pushes frames down a subscription after the initial response. It returns
// when the test has no more to send, which ends the connection.
type Pump func(params map[string]any, push func(any) error)

// HandleStream registers a subscription method: the server answers with result,
// then hands the connection to pump, which drives the stream. This is the one
// method shape where herdr keeps talking after it has replied.
func (s *Server) HandleStream(method string, result any, pump Pump) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[method] = streamHandler{result: result, pump: pump}
}

type streamHandler struct {
	result any
	pump   Pump
}

// Calls returns the requests received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed at test cleanup
		}
		go s.handleConn(conn)
	}
}

type wireRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireResponse struct {
	ID     string     `json:"id"`
	Result any        `json:"result,omitempty"`
	Error  *wireError `json:"error,omitempty"`
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var req wireRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return
		}

		s.mu.Lock()
		s.calls = append(s.calls, Call{Method: req.Method, Params: req.Params})
		handler, ok := s.handlers[req.Method]
		stream, streaming := s.streams[req.Method]
		s.mu.Unlock()

		encoder := json.NewEncoder(conn)

		if streaming {
			if err := encoder.Encode(wireResponse{ID: req.ID, Result: stream.result}); err != nil {
				return
			}
			stream.pump(req.Params, func(frame any) error { return encoder.Encode(frame) })
			return
		}

		resp := wireResponse{ID: req.ID}
		switch {
		case !ok:
			resp.Error = &wireError{Code: "unknown_method", Message: "no handler for " + req.Method}
		default:
			result, err := handler(req.Params)
			if err != nil {
				var coded *CodedError
				if errors.As(err, &coded) {
					resp.Error = &wireError{Code: coded.Code, Message: coded.Message}
				} else {
					resp.Error = &wireError{Code: "handler_error", Message: err.Error()}
				}
			} else {
				resp.Result = result
			}
		}
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

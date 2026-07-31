// Package herdrtest provides a fake herdr for tests: a unix socket that speaks
// the same NDJSON protocol as the real server, plus a fake `gh` on PATH.
//
// Tests drive real git in a temp directory and this fake herdr, so nothing
// touches the developer's live session.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Handler answers one method call. Returning an error makes the server reply
// with a herdr-shaped error object.
type Handler func(params map[string]any) (any, error)

// Server is a fake herdr listening on a unix socket.
type Server struct {
	SocketPath string

	mu       sync.Mutex
	handlers map[string]Handler
	calls    []Call
	listener net.Listener
}

// Call records one request the server received.
type Call struct {
	Method string
	Params map[string]any
}

// NewServer starts a fake herdr and stops it when the test ends.
func NewServer(t *testing.T) *Server {
	t.Helper()

	// macOS caps unix socket paths near 104 bytes and t.TempDir() names are
	// long, so keep the leaf short.
	dir, err := os.MkdirTemp("", "hwt")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}

	s := &Server{SocketPath: path, handlers: map[string]Handler{}, listener: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go s.serve()
	return s
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
		s.mu.Unlock()

		resp := wireResponse{ID: req.ID}
		switch {
		case !ok:
			resp.Error = &wireError{Code: "unknown_method", Message: "no handler for " + req.Method}
		default:
			result, err := handler(req.Params)
			if err != nil {
				resp.Error = &wireError{Code: "handler_error", Message: err.Error()}
			} else {
				resp.Result = result
			}
		}
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			return
		}
	}
}

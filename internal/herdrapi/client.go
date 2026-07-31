// Package herdrapi talks to a running herdr over its local IPC endpoint.
//
// The wire protocol is newline-delimited JSON: one request object per line, one
// response object per line. Each call opens a short-lived connection, writes a
// request, and reads a single response. herdr injects HERDR_SOCKET_PATH into
// every plugin command, so this works whenever herdr is the one running us.
//
// Reads go over the socket. Where herdr's CLI already does real work we shell
// out to it instead — see BinPath.
package herdrapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

//go:generate go run ../gen schema.json types_gen.go

// callTimeout bounds a single request/response exchange. A plugin action runs
// unattended, so a wedged server must fail rather than hang forever.
const callTimeout = 5 * time.Second

// Client is a connection factory for one herdr IPC endpoint. It holds no
// connection of its own; every call dials afresh.
type Client struct {
	socketPath string
}

// New builds a Client from HERDR_SOCKET_PATH. It fails when the process is not
// running inside herdr.
func New() (*Client, error) {
	path := os.Getenv("HERDR_SOCKET_PATH")
	if path == "" {
		return nil, errors.New("HERDR_SOCKET_PATH is not set; are you running inside herdr?")
	}
	return &Client{socketPath: path}, nil
}

// NewWithSocket points a Client at an explicit endpoint. Tests use it to talk
// to a fake server.
func NewWithSocket(path string) *Client {
	return &Client{socketPath: path}
}

// BinPath is the herdr executable to shell out to. herdr sets HERDR_BIN_PATH
// for plugin commands; outside herdr we fall back to PATH.
func BinPath() string {
	if p := os.Getenv("HERDR_BIN_PATH"); p != "" {
		return p
	}
	return "herdr"
}

// request is one JSON-RPC-style message sent to herdr.
type request struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// Error is the code and human message herdr returns on failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("herdr: %s (%s)", e.Message, e.Code) }

// response is one JSON line returned by herdr. Exactly one of Result or Error
// is populated.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// deadliner is implemented by connections that support timeouts. The Windows
// endpoint is a file handle and may not, so the deadline is best-effort.
type deadliner interface{ SetDeadline(time.Time) error }

// call sends one request over a fresh connection and decodes the result into
// out, which may be nil when the payload does not matter.
func (c *Client) call(method string, params map[string]any, out any) error {
	conn, err := dialHerdr(c.socketPath)
	if err != nil {
		return fmt.Errorf("connect herdr IPC endpoint: %w", err)
	}
	defer conn.Close()

	if d, ok := conn.(deadliner); ok {
		_ = d.SetDeadline(time.Now().Add(callTimeout))
	}

	if params == nil {
		params = map[string]any{}
	}
	// Encode appends a trailing newline, which is exactly herdr's framing.
	if err := json.NewEncoder(conn).Encode(request{ID: "herdr-wt", Method: method, Params: params}); err != nil {
		return fmt.Errorf("write %s request: %w", method, err)
	}

	var resp response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// WorktreeList returns every git worktree herdr knows about for the repository
// containing cwd. An empty cwd lets herdr pick the focused workspace's repo.
func (c *Client) WorktreeList(cwd string) (*WorktreeListResponse, error) {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var out WorktreeListResponse
	if err := c.call("worktree.list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkspaceList returns every open workspace, including its agent status.
func (c *Client) WorkspaceList() (*WorkspaceListResponse, error) {
	var out WorkspaceListResponse
	if err := c.call("workspace.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ensure the dial split keeps returning something we can use.
var _ func(string) (io.ReadWriteCloser, error) = dialHerdr

// Package herdrapi talks to a running herdr over its local IPC endpoint.
//
// The wire protocol is newline-delimited JSON: one request object per line, one
// response object per line. Each call opens a short-lived connection, writes a
// request, and reads a single response. herdr injects HERDR_SOCKET_PATH into
// every plugin command, so this works whenever herdr is the one running us.
//
// Everything goes over the socket; nothing shells out to the herdr CLI.
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

// defaultCallTimeout bounds an ordinary request/response exchange. A plugin
// action runs unattended, so a wedged server must fail rather than hang.
const defaultCallTimeout = 5 * time.Second

// callMargin is how much longer than herdr's own wait the client will hold on.
//
// Some requests ask herdr to wait — agent.start blocks until the pane is usable
// — and a client deadline shorter than the wait it just requested would abort
// the call before herdr could possibly answer, turning "still starting" into a
// hard failure on exactly the slow case the wait exists for.
const callMargin = 10 * time.Second

// deadlineFor is how long to wait for a reply to a request that asked herdr to
// wait serverTimeoutMS milliseconds. Zero means the request has no server-side
// wait and gets the ordinary timeout.
func deadlineFor(serverTimeoutMS int) time.Duration {
	if serverTimeoutMS <= 0 {
		return defaultCallTimeout
	}
	return time.Duration(serverTimeoutMS)*time.Millisecond + callMargin
}

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
	return c.callWithin(method, params, out, defaultCallTimeout)
}

// callWithin is call with an explicit deadline, for requests that ask herdr to
// wait longer than an ordinary exchange.
func (c *Client) callWithin(method string, params map[string]any, out any, timeout time.Duration) error {
	conn, err := dialHerdr(c.socketPath)
	if err != nil {
		return fmt.Errorf("connect herdr IPC endpoint: %w", err)
	}
	defer conn.Close()

	if d, ok := conn.(deadliner); ok {
		_ = d.SetDeadline(time.Now().Add(timeout))
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

// AgentList returns every agent herdr is tracking, across all workspaces. The
// pane ids are what identify a workspace as staffed.
func (c *Client) AgentList() (*AgentListResponse, error) {
	var out AgentListResponse
	if err := c.call("agent.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneList returns the panes of one workspace, in herdr's order.
func (c *Client) PaneList(workspaceID string) (*PaneListResponse, error) {
	params := map[string]any{}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	}
	var out PaneListResponse
	if err := c.call("pane.list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorktreeOpen opens an existing checkout into a herdr workspace. focus is
// deliberately a parameter: adopting a batch of worktrees must not yank the
// user between workspaces.
func (c *Client) WorktreeOpen(cwd, path, label string, focus bool) error {
	return c.call("worktree.open", map[string]any{
		"cwd": cwd, "path": path, "label": label, "focus": focus,
	}, nil)
}

// WorktreeRemove removes the worktree held open by a workspace, closing the
// workspace with it.
//
// force is what lets herdr tear down a workspace that still has panes, but it
// also bypasses git's own refusal to delete a dirty checkout — so callers must
// re-check for uncommitted changes immediately before calling this.
func (c *Client) WorktreeRemove(workspaceID string, force bool) error {
	return c.call("worktree.remove", map[string]any{
		"workspace_id": workspaceID, "force": force,
	}, nil)
}

// AgentStart starts an agent in a pane. timeoutMS bounds herdr's own wait for
// the pane to become usable; zero leaves herdr's default in place.
func (c *Client) AgentStart(name, kind, paneID string, args []string, timeoutMS int) error {
	params := map[string]any{"name": name, "kind": kind, "pane_id": paneID}
	if len(args) > 0 {
		params["args"] = args
	}
	if timeoutMS > 0 {
		params["timeout_ms"] = timeoutMS
	}
	// herdr may hold this request open for timeoutMS while it waits for the
	// pane, so the client has to outlast the wait it just asked for.
	return c.callWithin("agent.start", params, nil, deadlineFor(timeoutMS))
}

// ensure the dial split keeps returning something we can use.
var _ func(string) (io.ReadWriteCloser, error) = dialHerdr

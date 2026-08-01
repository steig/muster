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

// A subscription is the only edge-triggered signal herdr offers.
//
// Measured against herdr 0.7.5: `agent.wait --until idle` and `events.wait` are
// both level-triggered and return immediately when the agent is already in the
// requested state, so a "wait, then re-read" loop on either busy-waits.
// events.subscribe says nothing until something happens.
//
// One subscription also carries several event types at once, where events.wait
// takes a single match and would need one blocked connection per outcome.

// Subscription is one server-side filter. Pane-scoped kinds require PaneID;
// herdr rejects the subscription outright without it, which is worth having —
// an unfiltered stream would deliver every pane in the session.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
	// WorkspaceID is sent because herdr's schema declares it, not because it
	// narrows anything — see the note below on workspace.closed.
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Subscription kinds this plugin uses.
//
// These names are DOTTED, and so are the `event` fields of the frames that come
// back. The plugin-hook envelope in event.go carries the UNDERSCORED spelling of
// the same events. Both namespaces are live at once, and a stream reader that
// matches on the underscored name compiles and never fires.
const (
	SubscriptionPaneAgentStatusChanged = "pane.agent_status_changed"
	SubscriptionPaneExited             = "pane.exited"
	SubscriptionPaneClosed             = "pane.closed"
	SubscriptionPaneUpdated            = "pane.updated"
	SubscriptionWorkspaceClosed        = "workspace.closed"
)

// Stream event kinds, in the same dotted namespace as the subscriptions —
// except where they are not. workspace.closed and pane.updated both arrive
// UNDERSCORED, which was found by reading frames off a live socket rather than
// off the schema, and a reader matching the dotted spelling for either compiles
// and never fires.
const (
	StreamEventPaneAgentStatusChanged = "pane.agent_status_changed"
	StreamEventPaneExited             = "pane.exited"
	StreamEventPaneClosed             = "pane.closed"
	StreamEventPaneUpdated            = "pane_updated"
	StreamEventWorkspaceClosed        = "workspace_closed"
)

// pane.updated and workspace.closed are neither server-filtered nor
// edge-triggered, measured against herdr 0.7.5 rather than read off the schema.
//
// pane.updated takes no pane_id at all, and workspace.closed accepts a
// workspace_id and delivers every workspace's close regardless. Both hand a
// fresh subscriber the session's backlog first, carrying state as it was at the
// time — tokens included — so a reader taking a frame's payload as current would
// act on metadata replaced a quarter of an hour ago.
//
// So a subscriber filters on the id itself and treats a frame as nothing more
// than "go and look".

// StreamEvent is one frame pushed down a subscription. Data is left raw because
// each kind carries a different payload and a reader wants one of them.
type StreamEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// PaneAgentStatusData is the payload of a pane.agent_status_changed frame.
type PaneAgentStatusData struct {
	PaneID      string      `json:"pane_id"`
	AgentStatus AgentStatus `json:"agent_status"`
	Agent       *string     `json:"agent,omitempty"`
}

// AgentStatus reads the status out of a pane.agent_status_changed frame.
func (e StreamEvent) AgentStatus() (PaneAgentStatusData, error) {
	var data PaneAgentStatusData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return PaneAgentStatusData{}, fmt.Errorf("decode %s payload: %w", e.Event, err)
	}
	return data, nil
}

// PaneID reads the pane out of a frame that names one, so a caller can do the
// filtering herdr does not.
//
// The two shapes are both live: pane.agent_status_changed carries the id at the
// top level, while pane_updated carries a whole PaneInfo under `pane` and the id
// with it.
func (e StreamEvent) PaneID() string {
	var data struct {
		PaneID string `json:"pane_id"`
		Pane   struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	if json.Unmarshal(e.Data, &data) != nil {
		return ""
	}
	if data.PaneID != "" {
		return data.PaneID
	}
	return data.Pane.PaneID
}

// WorkspaceID reads the workspace out of a frame that names one, so a caller
// can do the filtering herdr does not.
func (e StreamEvent) WorkspaceID() string {
	var data struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if json.Unmarshal(e.Data, &data) != nil {
		return ""
	}
	return data.WorkspaceID
}

// Stream is a live subscription. Unlike every other call in this package it
// holds its connection open, because the connection IS the subscription.
type Stream struct {
	conn io.ReadWriteCloser
	dec  *json.Decoder
}

// ErrStreamExpired reports that the stream's deadline passed with no further
// frame. It is the caller's timeout, surfaced as a value rather than as a
// transport error, because for a gate it is a verdict rather than a fault.
var ErrStreamExpired = errors.New("subscription deadline expired")

// Subscribe opens an event stream that stops delivering at deadline.
//
// The deadline is mandatory and is enforced on the connection itself. A stream
// that cannot expire would let a caller block for ever on a worker that died
// without saying so, so an endpoint that will not take a deadline is refused
// rather than used without one.
func (c *Client) Subscribe(subs []Subscription, deadline time.Time) (*Stream, error) {
	if len(subs) == 0 {
		return nil, errors.New("events.subscribe needs at least one subscription")
	}

	conn, err := dialHerdr(c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect herdr IPC endpoint: %w", err)
	}

	d, ok := conn.(deadliner)
	if !ok {
		conn.Close()
		return nil, errors.New("this herdr endpoint does not support deadlines; refusing to open a subscription that cannot expire")
	}
	if err := d.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set subscription deadline: %w", err)
	}

	if err := json.NewEncoder(conn).Encode(request{
		ID:     "worktender",
		Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs},
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write events.subscribe request: %w", err)
	}

	// The first line back is an ordinary response; every line after it is a
	// pushed frame. Reading it here means a rejected subscription fails at
	// Subscribe rather than as a confusing frame mid-stream.
	stream := &Stream{conn: conn, dec: json.NewDecoder(bufio.NewReader(conn))}
	var resp response
	if err := stream.dec.Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read events.subscribe response: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, resp.Error
	}
	return stream, nil
}

// Next blocks until the next frame arrives, returning ErrStreamExpired once the
// deadline has passed.
func (s *Stream) Next() (StreamEvent, error) {
	var event StreamEvent
	if err := s.dec.Decode(&event); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return StreamEvent{}, ErrStreamExpired
		}
		return StreamEvent{}, fmt.Errorf("read subscription frame: %w", err)
	}
	return event, nil
}

// Close ends the subscription.
func (s *Stream) Close() error { return s.conn.Close() }

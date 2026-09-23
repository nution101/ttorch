package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// EventFilter is one entry of events.subscribe's subscriptions list. Type is
// a Herdr event name such as "pane.agent_status_changed" or "pane.exited".
// PaneID and AgentStatus narrow the pane-scoped events that accept them.
type EventFilter struct {
	Type        string      `json:"type"`
	PaneID      string      `json:"pane_id,omitempty"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`
}

// AgentStatusChanges subscribes to a pane's agent status changes, only to
// status when it is non-empty.
func AgentStatusChanges(paneID string, status AgentStatus) EventFilter {
	return EventFilter{Type: "pane.agent_status_changed", PaneID: paneID, AgentStatus: status}
}

// Event is one pushed event line: {"event": kind, "data": {...}}.
type Event struct {
	Kind string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

// StatusChange is the data of a pane.agent_status_changed event.
type StatusChange struct {
	PaneID      string      `json:"pane_id"`
	WorkspaceID string      `json:"workspace_id"`
	Agent       string      `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status"`
}

// StatusChange decodes the event as a pane.agent_status_changed payload.
func (e Event) StatusChange() (StatusChange, error) {
	var sc StatusChange
	if e.Kind != "pane.agent_status_changed" {
		return sc, fmt.Errorf("herdr: event %q is not pane.agent_status_changed", e.Kind)
	}
	if err := json.Unmarshal(e.Data, &sc); err != nil {
		return sc, fmt.Errorf("%w: %s data: %v", ErrMalformedResponse, e.Kind, err)
	}
	if sc.PaneID == "" || sc.AgentStatus == "" {
		return sc, fmt.Errorf("%w: %s data lacks pane_id or agent_status", ErrMalformedResponse, e.Kind)
	}
	return sc, nil
}

// Subscription is an open events.subscribe stream on its own connection.
// Next is not safe for concurrent use; Close may be called from any
// goroutine and unblocks a pending Next.
type Subscription struct {
	conn    net.Conn
	r       *bufio.Reader
	maxLine int
	once    sync.Once
}

// Subscribe opens an event stream (events.subscribe). The handshake, up to
// the server's subscription_started acknowledgement, is bounded like any
// other call; once acknowledged the stream stays open until Close, a
// cancelled Next, or the server hanging up. Herdr does not replay events
// from before the acknowledgement.
func (c *Client) Subscribe(ctx context.Context, filters ...EventFilter) (*Subscription, error) {
	if len(filters) == 0 {
		return nil, fmt.Errorf("herdr: events.subscribe: at least one filter is required")
	}
	hctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	conn, err := c.dial(hctx)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	stop := bindDeadline(hctx, conn)
	params := struct {
		Subscriptions []EventFilter `json:"subscriptions"`
	}{filters}
	err = c.exchange(hctx, conn, r, "events.subscribe", params, "subscription_started", nil)
	if !stop() || err != nil {
		// stop reports false when the cancel hook already ran, in which case
		// the connection's deadline is in the past and the stream is unusable.
		conn.Close()
		if err == nil {
			err = fmt.Errorf("herdr: events.subscribe: %w", hctx.Err())
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &Subscription{conn: conn, r: r, maxLine: c.maxLine()}, nil
}

// Next blocks until the next event arrives. It returns ErrConnectionClosed
// when the server ends the stream. If ctx ends first Next returns ctx's
// error and closes the subscription, since a read cut off mid-line leaves
// the stream at an unknown offset.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	stop := bindDeadline(ctx, s.conn)
	line, err := readLine(s.r, s.maxLine)
	if stop() {
		_ = s.conn.SetDeadline(time.Time{})
	} else {
		// The cancel hook ran, or is running, and has pushed the deadline
		// into the past; the connection cannot be read again.
		s.Close()
	}
	if err != nil {
		s.Close()
		return Event{}, ioError(ctx, "events.subscribe", err)
	}
	return decodeEvent(line)
}

// Close ends the stream. It is safe to call more than once.
func (s *Subscription) Close() error {
	var err error
	s.once.Do(func() { err = s.conn.Close() })
	return err
}

func decodeEvent(line []byte) (Event, error) {
	var raw struct {
		Event
		ID    *string `json:"id"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, fmt.Errorf("%w: events.subscribe: %v", ErrMalformedResponse, err)
	}
	if raw.Error != nil {
		return Event{}, &APIError{Method: "events.subscribe", Code: raw.Error.Code, Message: raw.Error.Message}
	}
	if raw.Kind == "" || len(raw.Data) == 0 {
		return Event{}, fmt.Errorf("%w: events.subscribe: line is not an event: %.200s", ErrMalformedResponse, line)
	}
	return raw.Event, nil
}

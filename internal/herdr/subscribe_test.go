package herdr

import (
	"context"
	"errors"
	"testing"
	"time"
)

// streamHandler acks the subscription, then writes lines, then either keeps
// the connection open (hold) or hangs up.
func streamHandler(hold bool, lines ...string) fakeHandler {
	return func(c *fakeConn, req fakeRequest) {
		c.result(req.ID, map[string]any{"type": "subscription_started"})
		for _, l := range lines {
			c.line(l)
		}
		if !hold {
			c.Close()
		}
	}
}

func subscribe(t *testing.T, s *fakeServer, filters ...EventFilter) *Subscription {
	t.Helper()
	sub, err := s.client().Subscribe(context.Background(), filters...)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Close() })
	return sub
}

func next(t *testing.T, sub *Subscription) (Event, error) {
	t.Helper()
	var ev Event
	err := within(t, 5*time.Second, func() error {
		var err error
		ev, err = sub.Next(context.Background())
		return err
	})
	return ev, err
}

func TestSubscribe_StatusChangesArePushed(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true,
		`{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent":"claude","agent_status":"working"}}`,
		`{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent":"claude","agent_status":"blocked","state_labels":{}}}`,
		`{"event":"pane.exited","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`,
	))
	sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""), EventFilter{Type: "pane.exited"})
	wantParams(t, s, "events.subscribe",
		`{"subscriptions":[{"type":"pane.agent_status_changed","pane_id":"w1:p1"},{"type":"pane.exited"}]}`)

	for _, want := range []AgentStatus{StatusWorking, StatusBlocked} {
		ev, err := next(t, sub)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		sc, err := ev.StatusChange()
		if err != nil {
			t.Fatalf("StatusChange: %v", err)
		}
		if sc.PaneID != "w1:p1" || sc.WorkspaceID != "w1" || sc.Agent != "claude" || sc.AgentStatus != want {
			t.Fatalf("status change = %+v, want %s", sc, want)
		}
	}
	ev, err := next(t, sub)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != "pane.exited" {
		t.Fatalf("kind = %q, want pane.exited", ev.Kind)
	}
	if _, err := ev.StatusChange(); err == nil {
		t.Fatal("StatusChange accepted a pane.exited event")
	}
}

func TestSubscribe_StatusFilterIsSent(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true))
	subscribe(t, s, AgentStatusChanges("w1:p1", StatusBlocked))
	wantParams(t, s, "events.subscribe",
		`{"subscriptions":[{"type":"pane.agent_status_changed","pane_id":"w1:p1","agent_status":"blocked"}]}`)
}

func TestSubscribe_ServerEndingStreamIsConnectionClosed(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(false,
		`{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"done"}}`,
	))
	sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""))
	if _, err := next(t, sub); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := next(t, sub); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("err = %v, want ErrConnectionClosed", err)
	}
}

// The stream is long-lived: Client.Timeout bounds only the handshake, and
// the context given to Subscribe does not own the stream after it returns.
func TestSubscribe_StreamOutlivesHandshakeBounds(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", func(c *fakeConn, req fakeRequest) {
		c.result(req.ID, map[string]any{"type": "subscription_started"})
		time.Sleep(300 * time.Millisecond)
		c.line(`{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"done"}}`)
	})
	c := s.client()
	c.Timeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := c.Subscribe(ctx, AgentStatusChanges("w1:p1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	cancel()
	if _, err := next(t, sub); err != nil {
		t.Fatalf("Next: %v", err)
	}
}

func TestSubscribe_HandshakeDeadlineWhenNeverAcked(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", func(*fakeConn, fakeRequest) {})
	c := s.client()
	c.Timeout = 100 * time.Millisecond
	err := within(t, 5*time.Second, func() error {
		_, err := c.Subscribe(context.Background(), AgentStatusChanges("w1:p1", ""))
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestSubscribe_UnknownMethod(t *testing.T) {
	s := newFakeServer(t) // no handler: answered as an unknown variant
	_, err := s.client().Subscribe(context.Background(), AgentStatusChanges("w1:p1", ""))
	if !errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("err = %v, want ErrUnknownMethod", err)
	}
}

func TestSubscribe_WrongAckTypeIsMalformed(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", replyOK)
	_, err := s.client().Subscribe(context.Background(), AgentStatusChanges("w1:p1", ""))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestSubscribe_RequiresAFilter(t *testing.T) {
	s := newFakeServer(t)
	if _, err := s.client().Subscribe(context.Background()); err == nil {
		t.Fatal("empty filter list accepted")
	}
	if n := len(s.received()); n != 0 {
		t.Fatalf("server got %d requests, want none", n)
	}
}

func TestNext_CancelReturnsContextErrorAndCloses(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true))
	sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := within(t, 5*time.Second, func() error { _, err := sub.Next(ctx); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if _, err := next(t, sub); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Next after a cancelled Next = %v, want ErrConnectionClosed", err)
	}
}

func TestClose_UnblocksPendingNext(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true))
	sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""))
	go func() {
		time.Sleep(50 * time.Millisecond)
		sub.Close()
	}()
	if _, err := next(t, sub); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("err = %v, want ErrConnectionClosed", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNext_BadLinesAreErrorsNotPanics(t *testing.T) {
	cases := map[string]string{
		"not json":      `{"event":"pane.agent_status_changed","data":`,
		"json null":     `null`,
		"no event name": `{"data":{"pane_id":"w1:p1"}}`,
		"no data":       `{"event":"pane.exited"}`,
		"event number":  `{"event":5,"data":{}}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle("events.subscribe", streamHandler(true, line))
			sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""))
			if _, err := next(t, sub); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

func TestNext_ErrorLineIsAPIError(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true, `{"id":"","error":{"code":"internal_error","message":"event hub stopped"}}`))
	sub := subscribe(t, s, AgentStatusChanges("w1:p1", ""))
	_, err := next(t, sub)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "internal_error" {
		t.Fatalf("err = %v, want *APIError internal_error", err)
	}
}

func TestStatusChange_MissingFieldsAreMalformed(t *testing.T) {
	for name, data := range map[string]string{
		"no status":    `{"pane_id":"w1:p1","workspace_id":"w1"}`,
		"no pane":      `{"workspace_id":"w1","agent_status":"done"}`,
		"wrong shape":  `{"pane_id":7}`,
		"not a object": `"done"`,
	} {
		t.Run(name, func(t *testing.T) {
			ev := Event{Kind: "pane.agent_status_changed", Data: []byte(data)}
			if _, err := ev.StatusChange(); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

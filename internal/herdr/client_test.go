package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPing_DecodesVersionAndProtocol(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", reply(map[string]any{"type": "pong", "version": "0.9.1", "protocol": 22}))

	p, err := s.client().Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if p.Version != "0.9.1" || p.Protocol != 22 {
		t.Fatalf("Ping = %+v, want version 0.9.1 protocol 22", p)
	}
}

// Herdr's request enum is adjacently tagged: a request without params does
// not parse, so a no-argument method must still send an empty object.
func TestCall_SendsOneLineWithIDMethodAndObjectParams(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", reply(map[string]any{"type": "pong", "version": "x", "protocol": 1}))
	c := s.client()
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := s.received()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(reqs[0].Line), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["params"]) != "{}" {
		t.Errorf("params = %s, want {}", raw["params"])
	}
	if reqs[0].ID == "" || reqs[0].ID == reqs[1].ID {
		t.Errorf("request ids %q and %q must be non-empty and distinct", reqs[0].ID, reqs[1].ID)
	}
}

func TestCall_MissingSocketIsErrNoSocket(t *testing.T) {
	c := New(filepath.Join(shortSocketDir(t), "absent.sock"))
	_, err := c.Ping(context.Background())
	if !errors.Is(err, ErrNoSocket) {
		t.Fatalf("err = %v, want ErrNoSocket", err)
	}
}

// A server that exits without unlinking leaves a socket file nobody accepts
// on. That is the same condition as no socket: there is no server.
func TestCall_StaleSocketFileIsErrNoSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file should remain: %v", err)
	}
	_, err = New(path).Ping(context.Background())
	if !errors.Is(err, ErrNoSocket) {
		t.Fatalf("err = %v, want ErrNoSocket", err)
	}
}

func TestCall_UnknownMethodIsTypedError(t *testing.T) {
	s := newFakeServer(t) // no ping handler: the fake answers as Herdr does
	_, err := s.client().Ping(context.Background())
	if !errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("err = %v, want ErrUnknownMethod", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" || apiErr.Method != "ping" {
		t.Fatalf("err = %#v, want *APIError{Method: ping, Code: invalid_request}", err)
	}
}

// Herdr reports an unknown enum value inside params with the same code and
// wording. Only an unknown variant naming the method itself is ErrUnknownMethod.
func TestCall_UnknownParamVariantIsNotUnknownMethod(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) {
		c.line(`{"id":"","error":{"code":"invalid_request","message":"invalid request: unknown variant ` + "`sideways`" + `, expected one of ` + "`right`, `down`" + `"}}`)
	})
	_, err := s.client().Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("err = %v matched ErrUnknownMethod", err)
	}
}

func TestCall_ServerErrorKeepsCodeAndMessage(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) { c.fail(req.ID, "not_found", "pane not found") })
	_, err := s.client().Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" || apiErr.Message != "pane not found" {
		t.Fatalf("err = %#v, want not_found / pane not found", err)
	}
	if errors.Is(err, ErrUnknownMethod) || errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("not_found must not match a sentinel: %v", err)
	}
}

func TestCall_DeadlineHonouredWhenServerNeverAnswers(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(*fakeConn, fakeRequest) {}) // read, never answer
	c := s.client()
	c.Timeout = 150 * time.Millisecond

	start := time.Now()
	err := within(t, 5*time.Second, func() error { _, err := c.Ping(context.Background()); return err })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("returned after %v, want about 150ms", elapsed)
	}
}

// The caller's own deadline wins when it is earlier than Client.Timeout.
func TestCall_ContextDeadlineShorterThanTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(*fakeConn, fakeRequest) {})
	c := s.client()
	c.Timeout = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := within(t, 5*time.Second, func() error { _, err := c.Ping(ctx); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("ignored the context deadline")
	}
}

func TestCall_CancelUnblocksAWaitingCall(t *testing.T) {
	s := newFakeServer(t)
	got := make(chan struct{})
	s.handle("ping", func(*fakeConn, fakeRequest) { close(got) })
	c := s.client()
	c.Timeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-got; cancel() }()
	err := within(t, 5*time.Second, func() error { _, err := c.Ping(ctx); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCall_WrongIDIsRejected(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) {
		c.result(req.ID+"_other", map[string]any{"type": "pong", "version": "x", "protocol": 1})
	})
	_, err := s.client().Ping(context.Background())
	if !errors.Is(err, ErrMismatchedID) {
		t.Fatalf("err = %v, want ErrMismatchedID", err)
	}
}

// An error carrying some other request's id is not this call's error either.
func TestCall_ErrorWithWrongIDIsRejected(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) { c.fail("req_elsewhere", "not_found", "nope") })
	_, err := s.client().Ping(context.Background())
	if !errors.Is(err, ErrMismatchedID) {
		t.Fatalf("err = %v, want ErrMismatchedID", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("a mismatched error must not surface as *APIError: %v", err)
	}
}

// A success response with an empty id is not attributable: only errors for
// unparseable requests legitimately come back without the id.
func TestCall_ResultWithEmptyIDIsRejected(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) {
		c.result("", map[string]any{"type": "pong", "version": "x", "protocol": 1})
	})
	_, err := s.client().Ping(context.Background())
	if !errors.Is(err, ErrMismatchedID) {
		t.Fatalf("err = %v, want ErrMismatchedID", err)
	}
}

func TestCall_MalformedResponsesAreErrorsNotPanics(t *testing.T) {
	cases := map[string]string{
		"not json":            `this is not json`,
		"truncated object":    `{"id":"ttorch_1","result":{"type":"po`,
		"json null":           `null`,
		"json array":          `[1,2,3]`,
		"empty object":        `{}`,
		"id only":             `{"id":"ttorch_1"}`,
		"null result":         `{"id":"ttorch_1","result":null}`,
		"id not a string":     `{"id":7,"result":{"type":"pong"}}`,
		"result not object":   `{"id":"ttorch_1","result":"pong"}`,
		"wrong result type":   `{"id":"ttorch_1","result":{"type":"pane_info"}}`,
		"field of wrong type": `{"id":"ttorch_1","result":{"type":"pong","protocol":"twenty-two","version":"x"}}`,
		"blank line":          ``,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle("ping", func(c *fakeConn, req fakeRequest) { c.line(line) })
			c := s.client()
			c.nextID.Store(0) // the first request id is ttorch_1
			_, err := c.Ping(context.Background())
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

func TestCall_ServerClosesMidCall(t *testing.T) {
	t.Run("before writing", func(t *testing.T) {
		s := newFakeServer(t)
		s.handle("ping", func(c *fakeConn, req fakeRequest) { c.Close() })
		_, err := s.client().Ping(context.Background())
		if !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("err = %v, want ErrConnectionClosed", err)
		}
	})
	t.Run("partway through the line", func(t *testing.T) {
		s := newFakeServer(t)
		s.handle("ping", func(c *fakeConn, req fakeRequest) {
			_, _ = c.Write([]byte(`{"id":"` + req.ID + `","result":{"type":"pong"`))
			c.Close()
		})
		_, err := s.client().Ping(context.Background())
		if !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("err = %v, want ErrConnectionClosed", err)
		}
	})
}

func TestCall_OversizedLineIsRejected(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(c *fakeConn, req fakeRequest) {
		c.result(req.ID, map[string]any{"type": "pong", "version": strings.Repeat("v", 8192), "protocol": 1})
	})
	c := s.client()
	c.maxResponseBytes = 1024
	_, err := c.Ping(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
}

// A line longer than bufio's default buffer must still be read whole.
func TestCall_LongLineWithinLimitIsRead(t *testing.T) {
	s := newFakeServer(t)
	long := strings.Repeat("v", 100<<10)
	s.handle("ping", reply(map[string]any{"type": "pong", "version": long, "protocol": 1}))
	p, err := s.client().Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != long {
		t.Fatalf("version truncated to %d bytes", len(p.Version))
	}
}

// within runs fn and fails the test if it has not returned after d, so a
// regression that stops honouring a deadline fails here instead of hanging
// the package until go test's own timeout.
func within(t *testing.T, d time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("call still blocked after %v", d)
		return nil
	}
}

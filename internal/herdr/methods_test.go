package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// wantParams fails unless the only request the server received was method
// with params equal, as JSON, to want.
func wantParams(t *testing.T, s *fakeServer, method, want string) {
	t.Helper()
	reqs := s.received()
	if len(reqs) != 1 {
		t.Fatalf("server got %d requests, want 1", len(reqs))
	}
	if reqs[0].Method != method {
		t.Fatalf("method = %q, want %q", reqs[0].Method, method)
	}
	var got, exp any
	if err := json.Unmarshal(reqs[0].Params, &got); err != nil {
		t.Fatalf("params %s: %v", reqs[0].Params, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatalf("bad want %s: %v", want, err)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("params = %s\nwant     %s", reqs[0].Params, want)
	}
}

var (
	fakeWorkspace = map[string]any{
		"workspace_id": "w1", "number": 1, "label": "TASK-1", "focused": false,
		"pane_count": 1, "tab_count": 1, "active_tab_id": "w1:t1", "agent_status": "unknown",
	}
	fakeTab = map[string]any{
		"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "main",
		"focused": false, "pane_count": 1, "agent_status": "unknown",
	}
	fakePane = map[string]any{
		"pane_id": "w1:p1", "terminal_id": "term_1", "workspace_id": "w1", "tab_id": "w1:t1",
		"focused": false, "agent_status": "working", "agent": "claude", "cwd": "/wt/1",
		"revision": 7, "tokens": map[string]any{}, "state_labels": map[string]any{},
		"some_future_field": true,
	}
	fakeAgent = map[string]any{
		"pane_id": "w1:p1", "terminal_id": "term_1", "workspace_id": "w1", "tab_id": "w1:t1",
		"focused": false, "agent_status": "done", "agent": "claude", "name": nil, "revision": 9,
	}
)

func TestCreateWorkspace(t *testing.T) {
	s := newFakeServer(t)
	s.handle("workspace.create", reply(map[string]any{
		"type": "workspace_created", "workspace": fakeWorkspace, "tab": fakeTab, "root_pane": fakePane,
	}))
	got, err := s.client().CreateWorkspace(context.Background(), WorkspaceCreate{
		Cwd: "/wt/1", Label: "TASK-1", Env: map[string]string{"TTORCH_TASK": "TASK-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "workspace.create", `{"cwd":"/wt/1","label":"TASK-1","env":{"TTORCH_TASK":"TASK-1"}}`)
	if got.Workspace.WorkspaceID != "w1" || got.Workspace.ActiveTabID != "w1:t1" ||
		got.Tab.TabID != "w1:t1" || got.RootPane.PaneID != "w1:p1" || got.RootPane.Cwd != "/wt/1" {
		t.Fatalf("decoded %+v", got)
	}
}

func TestCreateTab(t *testing.T) {
	s := newFakeServer(t)
	s.handle("tab.create", reply(map[string]any{"type": "tab_created", "tab": fakeTab, "root_pane": fakePane}))
	got, err := s.client().CreateTab(context.Background(), TabCreate{WorkspaceID: "w1", Label: "TASK-2", Focus: true})
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "tab.create", `{"workspace_id":"w1","label":"TASK-2","focus":true}`)
	if got.Tab.WorkspaceID != "w1" || got.RootPane.TerminalID != "term_1" {
		t.Fatalf("decoded %+v", got)
	}
}

func TestSplitPane(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.split", reply(map[string]any{"type": "pane_info", "pane": fakePane}))
	got, err := s.client().SplitPane(context.Background(), PaneSplit{
		TargetPaneID: "w1:p1", Direction: SplitRight, Ratio: 0.5, Cwd: "/wt/3",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.split", `{"target_pane_id":"w1:p1","direction":"right","ratio":0.5,"cwd":"/wt/3"}`)
	if got.PaneID != "w1:p1" || got.AgentStatus != StatusWorking || got.Revision != 7 {
		t.Fatalf("decoded %+v", got)
	}
}

func TestSendText(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_text", replyOK)
	if err := s.client().SendText(context.Background(), "w1:p1", "hello\n"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.send_text", `{"pane_id":"w1:p1","text":"hello\n"}`)
}

func TestSendKeys(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_keys", replyOK)
	if err := s.client().SendKeys(context.Background(), "w1:p1", "ctrl+c", "enter"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.send_keys", `{"pane_id":"w1:p1","keys":["ctrl+c","enter"]}`)
}

// keys is required by Herdr's schema, so no keys is an empty array, not null.
func TestSendKeys_NoKeysSendsEmptyArray(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_keys", replyOK)
	if err := s.client().SendKeys(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.send_keys", `{"pane_id":"w1:p1","keys":[]}`)
}

func TestSendInput(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_input", replyOK)
	if err := s.client().SendInput(context.Background(), "w1:p1", "claude --resume", "enter"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.send_input", `{"pane_id":"w1:p1","text":"claude --resume","keys":["enter"]}`)
}

func TestSendInput_ServerErrorSurfaces(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_input", func(c *fakeConn, req fakeRequest) { c.fail(req.ID, "not_found", "pane not found") })
	err := s.client().SendInput(context.Background(), "w9:p9", "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" || apiErr.Method != "pane.send_input" {
		t.Fatalf("err = %v, want not_found from pane.send_input", err)
	}
}

func TestReadPane(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.read", reply(map[string]any{"type": "pane_read", "read": map[string]any{
		"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "source": "recent",
		"format": "text", "text": "line 1\nline 2", "revision": 12, "truncated": true,
	}}))
	got, err := s.client().ReadPane(context.Background(), PaneReadRequest{PaneID: "w1:p1", Source: ReadRecent, Lines: 50})
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.read", `{"pane_id":"w1:p1","source":"recent","lines":50}`)
	if got.Text != "line 1\nline 2" || got.Source != ReadRecent || got.Revision != 12 || !got.Truncated {
		t.Fatalf("decoded %+v", got)
	}
}

func TestClosePane(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.close", replyOK)
	if err := s.client().ClosePane(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.close", `{"pane_id":"w1:p1"}`)
}

// An "ok" method must still reject a result of some other type.
func TestClosePane_WrongResultTypeIsMalformed(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.close", reply(map[string]any{"type": "pane_info", "pane": fakePane}))
	err := s.client().ClosePane(context.Background(), "w1:p1")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestAgent(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.get", reply(map[string]any{"type": "agent_info", "agent": fakeAgent}))
	got, err := s.client().Agent(context.Background(), "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "agent.get", `{"target":"w1:p1"}`)
	if got.AgentStatus != StatusDone || got.Agent != "claude" || got.Name != "" || got.PaneID != "w1:p1" {
		t.Fatalf("decoded %+v", got)
	}
}

func TestWaitAgent(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.wait", func(c *fakeConn, req fakeRequest) {
		time.Sleep(50 * time.Millisecond) // the server blocks until the state is reached
		c.result(req.ID, map[string]any{"type": "agent_info", "agent": fakeAgent})
	})
	got, err := s.client().WaitAgent(context.Background(), "w1:p1", 90*time.Second, StatusDone, StatusBlocked)
	if err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "agent.wait", `{"target":"w1:p1","until":["done","blocked"],"timeout_ms":90000}`)
	if got.AgentStatus != StatusDone {
		t.Fatalf("decoded %+v", got)
	}
}

// With no statuses the field is omitted and Herdr applies its own default.
func TestWaitAgent_EmptyUntilIsOmitted(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.wait", reply(map[string]any{"type": "agent_info", "agent": fakeAgent}))
	if _, err := s.client().WaitAgent(context.Background(), "w1:p1", 500*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "agent.wait", `{"target":"w1:p1","timeout_ms":1}`)
}

// The wait outlives Client.Timeout: its bound is its own timeout plus grace.
func TestWaitAgent_NotCutOffByClientTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.wait", func(c *fakeConn, req fakeRequest) {
		time.Sleep(300 * time.Millisecond)
		c.result(req.ID, map[string]any{"type": "agent_info", "agent": fakeAgent})
	})
	c := s.client()
	c.Timeout = 50 * time.Millisecond
	if _, err := c.WaitAgent(context.Background(), "w1:p1", time.Second, StatusDone); err != nil {
		t.Fatalf("WaitAgent: %v", err)
	}
}

func TestWaitAgent_ServerTimeoutIsErrWaitTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.wait", func(c *fakeConn, req fakeRequest) {
		c.fail(req.ID, "timeout", "timed out waiting for agent status")
	})
	_, err := s.client().WaitAgent(context.Background(), "w1:p1", 100*time.Millisecond, StatusDone)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a server timeout is not a local deadline: %v", err)
	}
}

// A server that never answers a wait is cut off at timeout plus WaitGrace.
func TestWaitAgent_LocalDeadlineWhenServerNeverAnswers(t *testing.T) {
	s := newFakeServer(t)
	s.handle("agent.wait", func(*fakeConn, fakeRequest) {})
	start := time.Now()
	err := within(t, 10*time.Second, func() error {
		_, err := s.client().WaitAgent(context.Background(), "w1:p1", 100*time.Millisecond, StatusDone)
		return err
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed < 100*time.Millisecond+WaitGrace {
		t.Fatalf("cut off after %v, before timeout plus grace", elapsed)
	}
}

func TestWaitAgent_RequiresPositiveTimeout(t *testing.T) {
	s := newFakeServer(t)
	if _, err := s.client().WaitAgent(context.Background(), "w1:p1", 0); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if n := len(s.received()); n != 0 {
		t.Fatalf("server got %d requests, want none", n)
	}
}

// SendLiteral refuses, without sending anything, text a terminal would act
// on: CR or LF would submit it, ESC and C1 bytes start control sequences,
// and invalid UTF-8 can decode as C1 in some terminals.
func TestSendLiteral_RefusesControlCharacters(t *testing.T) {
	for name, text := range map[string]string{
		"newline":      "fix the bug\nrm -rf ~",
		"carriage ret": "done\r",
		"escape":       "hi\x1b[2J",
		"tab":          "a\tb",
		"nul":          "a\x00b",
		"delete":       "a\x7fb",
		"c1 csi":       "a\u009b2J",
		"bad utf-8":    "a\x9bb",
	} {
		t.Run(name, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle("pane.send_input", replyOK)
			err := s.client().SendLiteral(context.Background(), "w1:p1", text, "enter")
			if !errors.Is(err, ErrControlCharacter) {
				t.Fatalf("err = %v, want ErrControlCharacter", err)
			}
			if n := len(s.received()); n != 0 {
				t.Fatalf("server received %d requests, want none", n)
			}
		})
	}
}

func TestSendLiteral_SendsPlainTextAndKeysTogether(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.send_input", replyOK)
	if err := s.client().SendLiteral(context.Background(), "w1:p1", "continue with the plan, café ✓", "enter"); err != nil {
		t.Fatal(err)
	}
	wantParams(t, s, "pane.send_input", `{"pane_id":"w1:p1","text":"continue with the plan, café ✓","keys":["enter"]}`)
}

// Only agent.wait's timeout is a wait timeout; the same code from another
// method is a plain server error.
func TestErrWaitTimeout_OnlyForAgentWait(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.read", func(c *fakeConn, req fakeRequest) { c.fail(req.ID, "timeout", "read timed out") })
	_, err := s.client().ReadPane(context.Background(), PaneReadRequest{PaneID: "w1:p1", Source: ReadRecent})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "timeout" {
		t.Fatalf("err = %v, want *APIError with code timeout", err)
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("pane.read's timeout matched ErrWaitTimeout: %v", err)
	}
}

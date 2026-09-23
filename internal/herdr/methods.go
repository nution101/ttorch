package herdr

import (
	"context"
	"errors"
	"time"
)

// AgentStatus is Herdr's semantic agent state. Done means idle and not yet
// seen by a user.
type AgentStatus string

const (
	StatusIdle    AgentStatus = "idle"
	StatusWorking AgentStatus = "working"
	StatusBlocked AgentStatus = "blocked"
	StatusDone    AgentStatus = "done"
	StatusUnknown AgentStatus = "unknown"
)

// SplitDirection is where a new pane opens relative to its target.
type SplitDirection string

const (
	SplitRight SplitDirection = "right"
	SplitDown  SplitDirection = "down"
)

// ReadSource selects which buffer pane.read returns.
type ReadSource string

const (
	ReadVisible         ReadSource = "visible"
	ReadRecent          ReadSource = "recent"
	ReadRecentUnwrapped ReadSource = "recent_unwrapped"
	ReadDetection       ReadSource = "detection"
)

// WorkspaceInfo is the subset of Herdr's workspace record ttorch reads.
type WorkspaceInfo struct {
	WorkspaceID string      `json:"workspace_id"`
	Label       string      `json:"label"`
	ActiveTabID string      `json:"active_tab_id"`
	AgentStatus AgentStatus `json:"agent_status"`
}

// TabInfo is the subset of Herdr's tab record ttorch reads.
type TabInfo struct {
	TabID       string      `json:"tab_id"`
	WorkspaceID string      `json:"workspace_id"`
	Label       string      `json:"label"`
	AgentStatus AgentStatus `json:"agent_status"`
}

// PaneInfo is the subset of Herdr's pane record ttorch reads. PaneID is the
// public id (such as "w1:p2") every pane method takes.
type PaneInfo struct {
	PaneID      string      `json:"pane_id"`
	TerminalID  string      `json:"terminal_id"`
	WorkspaceID string      `json:"workspace_id"`
	TabID       string      `json:"tab_id"`
	Agent       string      `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status"`
	Cwd         string      `json:"cwd"`
	Revision    uint64      `json:"revision"`
}

// AgentInfo is the subset of Herdr's agent record ttorch reads.
type AgentInfo struct {
	PaneID      string      `json:"pane_id"`
	TerminalID  string      `json:"terminal_id"`
	WorkspaceID string      `json:"workspace_id"`
	TabID       string      `json:"tab_id"`
	Name        string      `json:"name"`
	Agent       string      `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status"`
	Revision    uint64      `json:"revision"`
}

// PaneRead is the text pane.read returned.
type PaneRead struct {
	PaneID    string     `json:"pane_id"`
	Source    ReadSource `json:"source"`
	Text      string     `json:"text"`
	Revision  uint64     `json:"revision"`
	Truncated bool       `json:"truncated"`
}

// WorkspaceCreate holds workspace.create params. Env applies to the root
// pane's process only.
type WorkspaceCreate struct {
	Cwd   string            `json:"cwd,omitempty"`
	Label string            `json:"label,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	Focus bool              `json:"focus,omitempty"`
}

// WorkspaceCreated is a new workspace with its first tab and root pane.
type WorkspaceCreated struct {
	Workspace WorkspaceInfo `json:"workspace"`
	Tab       TabInfo       `json:"tab"`
	RootPane  PaneInfo      `json:"root_pane"`
}

// TabCreate holds tab.create params. An empty WorkspaceID targets the
// server's active workspace.
type TabCreate struct {
	WorkspaceID string            `json:"workspace_id,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Label       string            `json:"label,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Focus       bool              `json:"focus,omitempty"`
}

// TabCreated is a new tab with its root pane.
type TabCreated struct {
	Tab      TabInfo  `json:"tab"`
	RootPane PaneInfo `json:"root_pane"`
}

// PaneSplit holds pane.split params. An empty TargetPaneID splits the
// server's focused pane; Ratio zero leaves the split at Herdr's default.
type PaneSplit struct {
	TargetPaneID string            `json:"target_pane_id,omitempty"`
	Direction    SplitDirection    `json:"direction"`
	Ratio        float64           `json:"ratio,omitempty"`
	Cwd          string            `json:"cwd,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Focus        bool              `json:"focus,omitempty"`
}

// PaneReadRequest holds pane.read params. Lines zero leaves the line count
// at Herdr's default.
type PaneReadRequest struct {
	PaneID string     `json:"pane_id"`
	Source ReadSource `json:"source"`
	Lines  uint32     `json:"lines,omitempty"`
}

// CreateWorkspace opens a workspace (workspace.create).
func (c *Client) CreateWorkspace(ctx context.Context, p WorkspaceCreate) (WorkspaceCreated, error) {
	var out WorkspaceCreated
	err := c.call(ctx, c.timeout(), "workspace.create", p, "workspace_created", &out)
	return out, err
}

// CreateTab opens a tab (tab.create).
func (c *Client) CreateTab(ctx context.Context, p TabCreate) (TabCreated, error) {
	var out TabCreated
	err := c.call(ctx, c.timeout(), "tab.create", p, "tab_created", &out)
	return out, err
}

// SplitPane opens a pane beside an existing one (pane.split).
func (c *Client) SplitPane(ctx context.Context, p PaneSplit) (PaneInfo, error) {
	var out struct {
		Pane PaneInfo `json:"pane"`
	}
	err := c.call(ctx, c.timeout(), "pane.split", p, "pane_info", &out)
	return out.Pane, err
}

// SendText writes literal text to a pane without pressing any key
// (pane.send_text).
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	p := struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}{paneID, text}
	return c.call(ctx, c.timeout(), "pane.send_text", p, "ok", nil)
}

// SendKeys sends Herdr key-combo strings such as "enter", "esc" or "ctrl+c"
// to a pane (pane.send_keys).
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	p := struct {
		PaneID string   `json:"pane_id"`
		Keys   []string `json:"keys"`
	}{paneID, nonNil(keys)}
	return c.call(ctx, c.timeout(), "pane.send_keys", p, "ok", nil)
}

// SendInput writes text and then the keys in one request (pane.send_input),
// so a prompt and its submitting "enter" cannot be split by another writer.
func (c *Client) SendInput(ctx context.Context, paneID, text string, keys ...string) error {
	p := struct {
		PaneID string   `json:"pane_id"`
		Text   string   `json:"text,omitempty"`
		Keys   []string `json:"keys,omitempty"`
	}{paneID, text, keys}
	return c.call(ctx, c.timeout(), "pane.send_input", p, "ok", nil)
}

// ReadPane returns a pane's screen or scrollback text (pane.read).
func (c *Client) ReadPane(ctx context.Context, p PaneReadRequest) (PaneRead, error) {
	var out struct {
		Read PaneRead `json:"read"`
	}
	err := c.call(ctx, c.timeout(), "pane.read", p, "pane_read", &out)
	return out.Read, err
}

// ClosePane closes a pane and ends its process (pane.close).
func (c *Client) ClosePane(ctx context.Context, paneID string) error {
	p := struct {
		PaneID string `json:"pane_id"`
	}{paneID}
	return c.call(ctx, c.timeout(), "pane.close", p, "ok", nil)
}

// Agent returns the agent in a pane and its current status (agent.get).
// target is a pane id or an agent name.
func (c *Client) Agent(ctx context.Context, target string) (AgentInfo, error) {
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	p := struct {
		Target string `json:"target"`
	}{target}
	err := c.call(ctx, c.timeout(), "agent.get", p, "agent_info", &out)
	return out.Agent, err
}

// WaitGrace is added to a wait's server-side timeout to form its local
// deadline, so Herdr's own timeout error arrives before the connection is
// cut.
const WaitGrace = time.Second

// WaitAgent blocks until the agent at target reaches one of until
// (agent.wait). The wait runs in the server, which pins the pane's current
// occupant, so an agent started later in the same pane cannot satisfy it.
// With until empty, Herdr waits for idle, done or blocked.
//
// timeout must be positive: it is sent as the server's timeout_ms, and the
// call's local deadline is timeout plus WaitGrace (or the context's, if
// earlier). When the server's timeout elapses first the error matches
// ErrWaitTimeout.
func (c *Client) WaitAgent(ctx context.Context, target string, timeout time.Duration, until ...AgentStatus) (AgentInfo, error) {
	if timeout <= 0 {
		return AgentInfo{}, errors.New("herdr: agent.wait: timeout must be positive")
	}
	ms := timeout.Milliseconds()
	if ms == 0 {
		ms = 1
	}
	p := struct {
		Target    string        `json:"target"`
		Until     []AgentStatus `json:"until,omitempty"`
		TimeoutMS int64         `json:"timeout_ms"`
	}{target, until, ms}
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	err := c.call(ctx, timeout+WaitGrace, "agent.wait", p, "agent_info", &out)
	return out.Agent, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

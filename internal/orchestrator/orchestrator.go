// Package orchestrator composes the runtime primitives (tmux, worktree, harness,
// state) into the operations the manager drives: spawn, status, peek, send,
// teardown, plus launching the manager session and ad-hoc cc sessions.
package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nution101/orcha/internal/harness"
	"github.com/nution101/orcha/internal/paths"
	"github.com/nution101/orcha/internal/state"
	"github.com/nution101/orcha/internal/tmux"
	"github.com/nution101/orcha/internal/worktree"
)

// Manager performs runtime operations against a tmux session and the state store.
type Manager struct {
	P       paths.Paths
	Session string
	Store   state.Store
}

// New builds a Manager from the standard paths.
func New(p paths.Paths) *Manager {
	return &Manager{P: p, Session: tmux.SessionName(), Store: state.Store{Dir: p.StateDir()}}
}

func (m *Manager) requireTmux() error {
	if !tmux.Available() {
		return errors.New("tmux is required (run 'orcha doctor' to install it)")
	}
	return nil
}

// Spawn starts a worker for taskID against the repo containing projectPath. If
// rawCmd is empty, it launches the detected harness with the task brief; otherwise
// it runs rawCmd (used for testing and escape hatches).
func (m *Manager) Spawn(taskID, projectPath string, scout bool, rawCmd string) (state.Task, error) {
	var zero state.Task
	if err := m.requireTmux(); err != nil {
		return zero, err
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return zero, err
	}
	repo, err := worktree.RepoRoot(abs)
	if err != nil {
		return zero, fmt.Errorf("%s is not inside a git repository", projectPath)
	}

	window := "wk-" + taskID
	if tmux.WindowExists(m.Session, window) {
		return zero, fmt.Errorf("task %q already has a window; tear it down first", taskID)
	}

	wt, err := worktree.Acquire(m.P.Worktrees(), repo, taskID)
	if err != nil {
		return zero, err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return zero, err
	}
	if err := tmux.NewWindow(m.Session, window, wt); err != nil {
		return zero, err
	}

	kind := "ship"
	if scout {
		kind = "scout"
	}
	h := harness.Resolve()
	// Install the turn-end hook so the supervisor can detect this worker's turns.
	_ = harness.InstallTurnEndHook(h, wt, m.P.TurnEndMarker(taskID))
	cmd := rawCmd
	if cmd == "" {
		brief := m.P.BriefPath(taskID)
		if _, err := os.Stat(brief); os.IsNotExist(err) {
			_ = writeBriefStub(brief, taskID, kind)
		}
		cmd = harness.BriefCommand(h, brief)
	}
	if err := tmux.SendLine(m.Session, window, cmd); err != nil {
		return zero, err
	}

	t := state.Task{
		ID: taskID, Window: window, Worktree: wt, Project: repo,
		Harness: h, Kind: kind, Created: time.Now(),
	}
	if err := m.Store.Save(t); err != nil {
		return zero, err
	}
	return t, nil
}

// Live reports whether a task's tmux window is still present.
func (m *Manager) Live(t state.Task) bool {
	return tmux.WindowExists(m.Session, t.Window)
}

// Status returns all tracked tasks.
func (m *Manager) Status() ([]state.Task, error) { return m.Store.List() }

// Peek returns the last n lines of a worker's pane.
func (m *Manager) Peek(taskID string, lines int) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	return tmux.CapturePane(m.Session, t.Window, lines)
}

// Send types a line into a worker's pane.
func (m *Manager) Send(taskID, text string) error {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return fmt.Errorf("unknown task %q", taskID)
	}
	return tmux.SendLine(m.Session, t.Window, text)
}

// Teardown finishes a task: it refuses to discard a worktree with uncommitted
// changes unless force is set, then kills the window and returns the worktree.
func (m *Manager) Teardown(taskID string, force bool) ([]string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	var notes []string
	if !force {
		if dirty, _ := worktree.IsDirty(t.Worktree); dirty {
			return nil, fmt.Errorf("task %q has uncommitted changes; review it, then 'orcha teardown %s --force'", taskID, taskID)
		}
	}
	_ = tmux.KillWindow(m.Session, t.Window)
	if t.Project != "" {
		if err := worktree.Remove(t.Project, t.Worktree); err != nil {
			notes = append(notes, "worktree: "+err.Error())
		}
	}
	if err := m.Store.Remove(taskID); err != nil {
		notes = append(notes, "state: "+err.Error())
	}
	return notes, nil
}

// StartManager ensures the manager window exists (running the harness) and attaches
// the lead to it.
func (m *Manager) StartManager() error {
	if err := m.requireTmux(); err != nil {
		return err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	if !tmux.WindowExists(m.Session, "manager") {
		if err := tmux.NewWindow(m.Session, "manager", cwd()); err != nil {
			return err
		}
		_ = tmux.SendLine(m.Session, "manager", harness.InteractiveCommand(harness.Resolve()))
	}
	return tmux.Attach(m.Session, "manager")
}

// OpenCC opens an interactive harness session inside the orcha session as a tracked
// window, so the manager is aware of it. With isolated, it gets its own worktree.
func (m *Manager) OpenCC(isolated bool) error {
	if err := m.requireTmux(); err != nil {
		return err
	}
	dir := cwd()
	id := "cc-" + time.Now().Format("150405")
	window := id
	if isolated {
		if repo, err := worktree.RepoRoot(dir); err == nil {
			if wt, err := worktree.Acquire(m.P.Worktrees(), repo, id); err == nil {
				dir = wt
			}
		}
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	if err := tmux.NewWindow(m.Session, window, dir); err != nil {
		return err
	}
	_ = tmux.SendLine(m.Session, window, harness.InteractiveCommand(harness.Resolve()))
	_ = m.Store.Save(state.Task{
		ID: id, Window: window, Worktree: dir, Harness: harness.Resolve(),
		Kind: "cc", Created: time.Now(),
	})
	return tmux.Attach(m.Session, window)
}

func cwd() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}

func writeBriefStub(path, id, kind string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`# Task %s (%s)

The manager has not written a detailed brief yet. Wait for instructions, or ask
the manager via your window. Work only within this worktree; commit on a feature
branch. Do not address the lead directly.
`, id, kind)
	return os.WriteFile(path, []byte(body), 0o644)
}

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// managerWithUnreadableBoard builds a Manager whose Store is open but whose underlying DB has
// been closed, so every board read (ListTasks, the root of liveTasks) fails — the transient
// read failure the fail-closed paths must surface rather than swallow. It needs no tmux: the
// read errors before any liveness check, which is exactly the path under test.
func managerWithUnreadableBoard(t *testing.T) *Manager {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil { // close so every subsequent read errors
		t.Fatalf("Close: %v", err)
	}
	return &Manager{Store: s}
}

// TestStatusFailsClosedOnReadError proves Status propagates the board-read error instead of
// returning an empty fleet. The scheduler's RunOnce aborts the tick on this error; swallowing
// it (the original `m.liveTasks(), nil`) is what made the daemon dispatch against a phantom
// empty fleet on a transient read failure.
func TestStatusFailsClosedOnReadError(t *testing.T) {
	m := managerWithUnreadableBoard(t)
	tasks, err := m.Status()
	if err == nil {
		t.Fatal("Status must return the board-read error, not swallow it")
	}
	if tasks != nil {
		t.Fatalf("Status must not return a fleet on a read error, got %v", tasks)
	}
}

// TestCheckOverlapFailsClosedOnReadError proves CheckOverlap REFUSES (returns an error) when
// the board cannot be read, rather than returning an empty conflict list indistinguishable
// from a genuine "no conflict" — the fail-open that would let a dispatch land on shared files.
func TestCheckOverlapFailsClosedOnReadError(t *testing.T) {
	m := managerWithUnreadableBoard(t)
	conflicts, err := m.CheckOverlap("/repo", []string{"internal/cli"})
	if err == nil {
		t.Fatal("CheckOverlap must refuse (return an error) when it cannot read the board")
	}
	if conflicts != nil {
		t.Fatalf("CheckOverlap must not report a conflict set (which reads as 'no conflict') on a read error, got %v", conflicts)
	}
}

// TestSnapshotFailsClosedOnReadError proves the once-per-tick live-fleet snapshot fails closed:
// when the board cannot be read, Snapshot returns the error and a nil snapshot, so the
// scheduler's RunOnce aborts the tick rather than proceeding against an empty view (no live
// workers ⇒ full capacity ⇒ no overlap). The liveTasks read is the first thing Snapshot does,
// so a closed DB exercises the same fail-closed path the scheduler relies on.
func TestSnapshotFailsClosedOnReadError(t *testing.T) {
	m := managerWithUnreadableBoard(t)
	snap, err := m.Snapshot()
	if err == nil {
		t.Fatal("Snapshot must return the board-read error, not swallow it")
	}
	if snap != nil {
		t.Fatalf("Snapshot must not return a snapshot on a read error, got %v", snap)
	}
}

// TestSnapshotFailsClosedOnTmuxReadError proves the OTHER half of the snapshot's fail-closed
// contract (C2): when the board reads fine but the tmux window probe fails, Snapshot still
// returns the error (and a nil snapshot) rather than treating the unreadable window set as "no
// live windows". This is the half carrying the actual behavior change — the old hot path
// resolved liveness via tmux.WindowExists, which DISCARDS the tmux error and reads a failed
// probe as "not live" (a fail-OPEN on overlap); Snapshot propagates it and aborts the tick. It
// needs a readable board with >=1 live task (so Snapshot gets past the empty-board short-circuit
// that skips the probe) plus a tmux whose list-windows fails.
func TestSnapshotFailsClosedOnTmuxReadError(t *testing.T) {
	// A stub `tmux` on PATH whose list-windows exits non-zero — the transient `tmux
	// list-windows` hiccup the window probe must surface, modelled hermetically (no real tmux,
	// no host session state), mirroring the fakeTmux helper.
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  list-windows) echo 'tmux: list-windows failed' >&2; exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// A live (non-terminal) task so Snapshot proceeds PAST the empty-board short-circuit (which
	// skips the tmux probe) and actually reaches ListWindows.
	proj, err := s.UpsertProject(context.Background(), "/repo", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(context.Background(), db.Task{
		ID: "live1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Window: "wk-live1", Footprint: []string{"internal/cli"},
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	m := &Manager{Store: s, Session: "ttorch-tmuxfail"}

	snap, err := m.Snapshot()
	if err == nil {
		t.Fatal("Snapshot must propagate the tmux window-probe error, not swallow it (the fail-open the old per-task path had)")
	}
	if snap != nil {
		t.Fatalf("Snapshot must not return a snapshot on a tmux read error, got %v", snap)
	}
}

// TestInUseWorktreesFailsClosedOnReadError proves the worktree-capacity read fails closed: a
// read error must not collapse to an empty in-use set (which reads as full pool capacity and
// lets a fresh acquire reuse a worktree a live worker still holds).
func TestInUseWorktreesFailsClosedOnReadError(t *testing.T) {
	m := managerWithUnreadableBoard(t)
	wts, err := m.inUseWorktrees("/repo")
	if err == nil {
		t.Fatal("inUseWorktrees must return the read error, not an empty (full-capacity) set")
	}
	if wts != nil {
		t.Fatalf("inUseWorktrees must not return worktree paths on a read error, got %v", wts)
	}
}

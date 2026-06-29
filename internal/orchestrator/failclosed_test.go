package orchestrator

import (
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

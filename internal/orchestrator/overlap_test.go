package orchestrator

import (
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestNewLiveSnapshotLive proves the snapshot's in-memory liveness seam — the one the
// scheduler's fake fleet and Manager.Snapshot share: a task is Live iff its window is in the
// supplied live-window set, an empty window is never Live, and an empty set yields no live
// tasks. This is the pure, tmux-free core of the per-pair WindowExists the hot path replaced.
func TestNewLiveSnapshotLive(t *testing.T) {
	tasks := []db.Task{
		{ID: "live", Window: "wk-live"},
		{ID: "gone", Window: "wk-gone"},
		{ID: "windowless", Window: ""},
	}
	snap := NewLiveSnapshot(tasks, []string{"wk-live", ""}) // the "" is ignored, not a live window

	if !snap.Live(tasks[0]) {
		t.Error("a task whose window is in the live set must read Live")
	}
	if snap.Live(tasks[1]) {
		t.Error("a task whose window is absent from the live set must not read Live")
	}
	if snap.Live(tasks[2]) {
		t.Error("a window-less task must never read Live")
	}
	if got := len(snap.Tasks); got != 3 {
		t.Fatalf("snapshot Tasks = %d, want 3 (the snapshot carries the full live set)", got)
	}

	empty := NewLiveSnapshot(nil, nil)
	if empty.Live(db.Task{ID: "x", Window: "wk-x"}) {
		t.Error("no task is Live in an empty snapshot")
	}
}

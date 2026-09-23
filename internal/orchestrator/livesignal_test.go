package orchestrator

import (
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

// TestLiveState_HookTurnStartedIsWorking: `ttorch status` reports a Claude Code worker whose
// hook record says a turn started as working, even when its pane shows no busy text the
// heuristic knows, the same as the watcher's liveness sweep.
func TestLiveState_HookTurnStartedIsWorking(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	task := db.Task{ID: "t1", Harness: "claude"}
	if err := livestate.WriteRecord(p.HookRecordFile(task.ID), livestate.Record{
		Event: livestate.TurnStarted, TaskID: task.ID, At: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	for _, pane := range []string{"✢ Brewing… (4m 12s · ↓ 3.1k tokens)\n│ > ", ""} {
		if got := liveState(p, task, pane, now); got != "working" {
			t.Errorf("turn started a minute ago, pane %q: got %q, want working", pane, got)
		}
	}
}

// TestLiveState_NoHookSignalIsDeriveState: with no hook signal, whether from no record or a
// harness without lifecycle hooks, `ttorch status` reads every pane as DeriveState did.
func TestLiveState_NoHookSignalIsDeriveState(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// A fresh turn-started record exists for the codex task; it must be ignored.
	if err := livestate.WriteRecord(p.HookRecordFile("codex1"), livestate.Record{
		Event: livestate.TurnStarted, TaskID: "codex1", At: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	panes := []string{
		"", "all set\n> ", "✻ Working… (esc to interrupt)", "Thinking about the change",
		"brian@host ~ $ ", "API Error: Response stalled mid-stream\n│ > ", "✢ Brewing… (4m 12s)\n│ > ",
	}
	for _, task := range []db.Task{{ID: "none", Harness: "claude"}, {ID: "codex1", Harness: "codex"}} {
		for _, pane := range panes {
			if got, want := liveState(p, task, pane, now), DeriveState(true, pane); got != want {
				t.Errorf("%s (%s), pane %q: got %q, want DeriveState's %q", task.ID, task.Harness, pane, got, want)
			}
		}
	}
}

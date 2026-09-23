package watch

import (
	"context"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
)

// paneBrewing is a busy worker whose busy line the pane heuristic does not know, with the
// input box still drawn beneath it: livestate.Busy is false and livestate.Idle is true.
const paneBrewing = "✢ Brewing… (4m 12s · ↓ 3.1k tokens)\n│ > "

// seedHarnessTask creates an active task run under harness kind and returns its row.
func seedHarnessTask(t *testing.T, s *db.Store, id, kind string) db.Task {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Window: "wk-" + id, Kind: db.KindShip, Status: db.StatusActive, Harness: kind,
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task, _, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

// hookCase runs enough liveness sweeps over a static pane to flag an idle worker, with the
// task's hook record set to ev at the given age (no record when ev is ""), and reports
// whether idle_unreported fired. The clock is well past the dwell, so the only thing that can
// hold the flag back is the worker reading as busy.
func hookCase(t *testing.T, kind string, ev livestate.Event, age time.Duration, pane string) bool {
	t.Helper()
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	task := seedHarnessTask(t, s, "hk", kind)
	clk.t = task.Created.Add(w.dwell() + time.Hour)
	if ev != "" {
		rec := livestate.Record{Event: ev, TaskID: task.ID, At: clk.t.Add(-age)}
		if err := livestate.WriteRecord(w.P.HookRecordFile(task.ID), rec); err != nil {
			t.Fatal(err)
		}
	}
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: pane}
	}
	for i := 0; i <= idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	flagged, err := s.HasEventType(ctx, task.ID, db.EventIdleUnreported)
	if err != nil {
		t.Fatal(err)
	}
	return flagged
}

// TestPollLiveness_HookTurnStartedHoldsWorkerBusy: a Claude Code worker whose hook record says
// a turn started a minute ago is busy even though its pane shows no busy text the heuristic
// knows, so it is never flagged idle_unreported.
func TestPollLiveness_HookTurnStartedHoldsWorkerBusy(t *testing.T) {
	if hookCase(t, "claude", livestate.TurnStarted, time.Minute, paneBrewing) {
		t.Fatal("a worker the hook record says is mid-turn was flagged idle_unreported")
	}
}

// TestPollLiveness_HookSignalOtherwiseChangesNothing: every case where the hook record must not
// hold the worker busy still flags it, exactly as the pane alone does.
func TestPollLiveness_HookSignalOtherwiseChangesNothing(t *testing.T) {
	cases := []struct {
		name string
		kind string
		ev   livestate.Event
		age  time.Duration
	}{
		{"no record", "claude", "", 0},
		{"turn ended", "claude", livestate.TurnEnded, time.Minute},
		{"session ended", "claude", livestate.SessionEnded, time.Minute},
		{"turn started past the max age", "claude", livestate.TurnStarted, livestate.StartedMaxAge + time.Minute},
		{"another harness, fresh turn started", "codex", livestate.TurnStarted, time.Minute},
		{"legacy row with no harness, fresh turn started", "", livestate.TurnStarted, time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !hookCase(t, c.kind, c.ev, c.age, paneBrewing) {
				t.Fatal("a static, not-busy pane should be flagged idle_unreported")
			}
		})
	}
}

// TestPollLiveness_HookTurnStartedLeavesStallAutoResume: a turn that dies on an API error
// ends without a Stop, so its record still says turn-started. The stall text on the pane
// wins, and the watcher's auto-resume nudges it as before.
func TestPollLiveness_HookTurnStartedLeavesStallAutoResume(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	task := seedHarnessTask(t, s, "st", "claude")
	clk.t = task.Created.Add(time.Second)
	if err := livestate.WriteRecord(w.P.HookRecordFile(task.ID), livestate.Record{
		Event: livestate.TurnStarted, TaskID: task.ID, At: clk.t,
	}); err != nil {
		t.Fatal(err)
	}
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "API Error: Response stalled mid-stream\n│ >"}
	}
	nudged := 0
	w.nudge = func(string) error { nudged++; return nil }
	for i := 0; i <= idleStaleSweeps; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	if nudged != 1 {
		t.Fatalf("a stalled worker should be nudged once despite a turn-started record, got %d", nudged)
	}
}

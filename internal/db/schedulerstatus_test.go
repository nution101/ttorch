package db

import (
	"context"
	"testing"
	"time"
)

// TestRecordSchedulerTickInsertAndAccumulate proves the first tick inserts the singleton row and
// later ticks accumulate the counters + advance last_tick_at and tick_count — the durable
// heartbeat `ttorch scheduler status` and the watchdog read.
func TestRecordSchedulerTickInsertAndAccumulate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// No tick recorded yet ⇒ the row is absent (status renders "never ticked").
	if _, has, err := s.GetSchedulerStatus(ctx); err != nil || has {
		t.Fatalf("GetSchedulerStatus on a fresh DB: has=%v err=%v, want has=false", has, err)
	}

	t0 := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	row, err := s.RecordSchedulerTick(ctx, SchedulerTick{At: t0, Dispatched: 2, Recovered: 1})
	if err != nil {
		t.Fatalf("RecordSchedulerTick #1: %v", err)
	}
	if row.TickCount != 1 || row.Dispatched != 2 || row.Recovered != 1 {
		t.Fatalf("after tick #1: tick=%d dispatched=%d recovered=%d, want 1/2/1", row.TickCount, row.Dispatched, row.Recovered)
	}
	if !row.LastTickAt.Equal(t0) {
		t.Errorf("last_tick_at = %v, want %v", row.LastTickAt, t0)
	}
	if row.LastError != "" || !row.LastErrorAt.IsZero() {
		t.Errorf("no error yet, but last_error=%q last_error_at=%v", row.LastError, row.LastErrorAt)
	}

	// A second tick advances last_tick_at and ADDS its deltas to the cumulative counters.
	t1 := t0.Add(5 * time.Second)
	row, err = s.RecordSchedulerTick(ctx, SchedulerTick{At: t1, Dispatched: 3, Landed: 4, Gated: 1, Deferred: 2})
	if err != nil {
		t.Fatalf("RecordSchedulerTick #2: %v", err)
	}
	if row.TickCount != 2 || row.Dispatched != 5 || row.Landed != 4 || row.Gated != 1 || row.Recovered != 1 || row.Deferred != 2 {
		t.Fatalf("after tick #2: %+v, want tick=2 dispatched=5 landed=4 gated=1 recovered=1 deferred=2", row)
	}
	if !row.LastTickAt.Equal(t1) {
		t.Errorf("last_tick_at = %v, want %v (advanced)", row.LastTickAt, t1)
	}

	// GetSchedulerStatus returns the same accumulated row.
	got, has, err := s.GetSchedulerStatus(ctx)
	if err != nil || !has {
		t.Fatalf("GetSchedulerStatus: has=%v err=%v", has, err)
	}
	if got.TickCount != 2 || got.Dispatched != 5 {
		t.Errorf("read-back row = %+v, want tick=2 dispatched=5", got)
	}
}

// TestRecordSchedulerTickLastError proves a tick that carries an error records the error counter +
// last_error/last_error_at, and that a SUBSEQUENT error-free tick PRESERVES that last error (the
// "most recent error" survives clean ticks) while still counting the tick.
func TestRecordSchedulerTickLastError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	t0 := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if _, err := s.RecordSchedulerTick(ctx, SchedulerTick{At: t0}); err != nil { // clean tick
		t.Fatalf("tick #1: %v", err)
	}

	tErr := t0.Add(time.Second)
	row, err := s.RecordSchedulerTick(ctx, SchedulerTick{At: tErr, Errors: 1, LastError: "supervise tick error: board read failed"})
	if err != nil {
		t.Fatalf("error tick: %v", err)
	}
	if row.Errors != 1 || row.LastError != "supervise tick error: board read failed" {
		t.Fatalf("error tick row = %+v, want errors=1 + the message", row)
	}
	if !row.LastErrorAt.Equal(tErr) {
		t.Errorf("last_error_at = %v, want %v", row.LastErrorAt, tErr)
	}

	// A clean tick afterwards must NOT clear the last error — it is the most-recent-error record.
	tClean := tErr.Add(10 * time.Second)
	row, err = s.RecordSchedulerTick(ctx, SchedulerTick{At: tClean, Dispatched: 1})
	if err != nil {
		t.Fatalf("clean tick after error: %v", err)
	}
	if row.Errors != 1 {
		t.Errorf("errors counter = %d after a clean tick, want 1 (unchanged)", row.Errors)
	}
	if row.LastError != "supervise tick error: board read failed" || !row.LastErrorAt.Equal(tErr) {
		t.Errorf("clean tick clobbered the last error: last_error=%q at=%v", row.LastError, row.LastErrorAt)
	}
	if row.TickCount != 3 {
		t.Errorf("tick_count = %d, want 3", row.TickCount)
	}
}

// TestMigration0006SchedulerStatus is the focused up→down→up test for the observability migration:
// the table exists on a fresh DB with its singleton CHECK, MigrateDown(5) drops it, and re-up
// restores it — the "applies on top of the current latest; down reverses" acceptance.
func TestMigration0006SchedulerStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // Open() already migrated to the latest

	if v, err := s.schemaVersion(ctx); err != nil || v != 8 {
		t.Fatalf("fresh DB version = %d err=%v, want 8", v, err)
	}
	if !tableExists(t, s, "scheduler_status") {
		t.Fatal("scheduler_status missing after up")
	}
	// The singleton CHECK (id = 1) must reject any other id — proving the row can only ever be one.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO scheduler_status (id, last_tick_at, updated_at) VALUES (2, '', '')`); err == nil {
		t.Error("scheduler_status CHECK must reject id != 1")
	}

	// Down the 0006 step only: the table is dropped, the rest of the schema stays.
	if err := s.MigrateDown(ctx, 5); err != nil {
		t.Fatalf("MigrateDown(5): %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 5 {
		t.Fatalf("after down-to-5: version=%d err=%v, want 5", v, err)
	}
	if tableExists(t, s, "scheduler_status") {
		t.Error("scheduler_status must be gone at version 5")
	}
	if !tableExists(t, s, "tasks") {
		t.Error("0006 down must not touch the tasks table")
	}

	// Re-apply: the table comes back and is usable.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 8 {
		t.Fatalf("after re-up: version=%d err=%v, want 8", v, err)
	}
	if _, err := s.RecordSchedulerTick(ctx, SchedulerTick{At: time.Now()}); err != nil {
		t.Errorf("scheduler_status unusable after re-up: %v", err)
	}
}

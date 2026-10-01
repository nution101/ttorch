package db

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// hookEvents returns taskID's hook_turn_started rows, oldest first.
func hookEvents(t *testing.T, s *Store, taskID string) []Event {
	t.Helper()
	evs, err := s.collectEvents(context.Background(),
		`SELECT `+eventColumns+` FROM events WHERE entity_id = ? AND type = ? ORDER BY id`,
		taskID, EventHookTurnStarted)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestAppendHookTurnStarted_RateLimitedPerTask: the first turn-started of a task writes a
// row, every later one inside HookEventInterval writes nothing, and the first one at or past
// the interval writes again. Another task keeps its own budget.
func TestAppendHookTurnStarted_RateLimitedPerTask(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "h1")
	mkSpawnedWorker(t, s, "h2")

	want := []bool{true, false, false}
	for i, w := range want {
		got, err := s.AppendHookTurnStarted(ctx, "h1", "via=env")
		if err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Fatalf("call %d wrote=%v, want %v", i, got, w)
		}
		clk.advance(20 * time.Second) // 0s, 20s, 40s: all inside the first minute
	}
	if got, _ := s.AppendHookTurnStarted(ctx, "h2", "via=env"); !got {
		t.Fatal("a second task's first turn-started must not be held back by the first task's")
	}
	clk.advance(HookEventInterval - 60*time.Second) // now exactly one interval after the first row
	if got, err := s.AppendHookTurnStarted(ctx, "h1", "via=task-file"); err != nil || !got {
		t.Fatalf("turn-started one interval later wrote=%v err=%v, want a row", got, err)
	}

	evs := hookEvents(t, s, "h1")
	if len(evs) != 2 {
		t.Fatalf("h1 has %d hook rows, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Actionable || e.Actor != ActorSystem || e.EntityType != EntityTypeTask {
			t.Errorf("hook row %+v: want a non-actionable system row on the task", e)
		}
	}
	if evs[0].Payload != "via=env" || evs[1].Payload != "via=task-file" {
		t.Errorf("payloads = %q, %q", evs[0].Payload, evs[1].Payload)
	}
}

// TestAppendHookTurnStarted_UnknownTaskWritesNothing: a hook naming a task the DB does not
// hold leaves no row, so made-up ids cannot grow the events table either.
func TestAppendHookTurnStarted_UnknownTaskWritesNothing(t *testing.T) {
	s, _ := newTestStoreClock(t)
	got, err := s.AppendHookTurnStarted(context.Background(), "ghost", "via=env")
	if err != nil || got {
		t.Fatalf("unknown task wrote=%v err=%v, want nothing", got, err)
	}
	if evs := hookEvents(t, s, "ghost"); len(evs) != 0 {
		t.Fatalf("unknown task has %d hook rows", len(evs))
	}
}

// TestAppendHookTurnStarted_NotASignOfLife: the trace does not move the stall clock's sign
// of life, so writing it changes nothing on the ladder beyond what the hook record does.
func TestAppendHookTurnStarted_NotASignOfLife(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "h1")
	before, err := s.StallInfo(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppendHookTurnStarted(ctx, "h1", "via=env"); !got {
		t.Fatal("want a row")
	}
	after, err := s.StallInfo(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if after.SignOfLifeID != before.SignOfLifeID {
		t.Fatalf("hook row moved the sign of life from %d to %d", before.SignOfLifeID, after.SignOfLifeID)
	}
	if n, _ := s.MaxActionableEventID(ctx); n != 0 {
		t.Fatalf("hook row is actionable (max actionable id %d)", n)
	}
}

// TestAppendHookTurnStarted_InterleavedWritersWriteOne: a second writer (another process,
// so another connection) that tries to append while the first sits between its interval
// check and its insert cannot finish until the first commits, and then finds its row. The
// test-only hookEventBeforeInsert holds the first writer in that gap and starts the second.
func TestAppendHookTurnStarted_InterleavedWritersWriteOne(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() *Store {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	first, second := open(), open()
	mkSpawnedWorker(t, first, "h1")

	type result struct {
		wrote bool
		err   error
	}
	secondDone := make(chan result, 1)
	finishedInGap := false
	var held atomic.Bool
	hookEventBeforeInsert = func() {
		if !held.CompareAndSwap(false, true) {
			return // the second writer passing through, if it ever gets this far
		}
		go func() {
			got, err := second.AppendHookTurnStarted(ctx, "h1", "second")
			secondDone <- result{got, err}
		}()
		// The second writer should be stuck waiting for the write lock. If it finishes here,
		// its check and insert ran while the first writer's were apart.
		select {
		case r := <-secondDone:
			finishedInGap = true
			secondDone <- r
		case <-time.After(time.Second):
		}
	}
	t.Cleanup(func() { hookEventBeforeInsert = nil })

	wroteFirst, err := first.AppendHookTurnStarted(ctx, "h1", "first")
	if err != nil {
		t.Fatal(err)
	}
	r := <-secondDone
	if r.err != nil {
		t.Fatal(r.err)
	}
	if finishedInGap {
		t.Error("the second writer finished while the first sat between its check and its insert")
	}
	rows := hookEvents(t, first, "h1")
	if !wroteFirst || r.wrote || len(rows) != 1 {
		t.Fatalf("first wrote=%v, second wrote=%v, %d rows; want true, false, 1", wroteFirst, r.wrote, len(rows))
	}
}

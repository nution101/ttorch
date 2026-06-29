package scheduler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// TestRunTickRecordsStatus proves one daemon tick writes the durable status row: last_tick_at is
// set to the tick clock and the dispatch counter reflects the work the tick did.
func TestRunTickRecordsStatus(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "ship1", db.KindShip, []string{"internal/cli"})

	tick := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Dispatch: true, now: func() time.Time { return tick }}

	sc.runTick(ctx)

	row, has, err := s.GetSchedulerStatus(ctx)
	if err != nil || !has {
		t.Fatalf("GetSchedulerStatus: has=%v err=%v, want a recorded tick", has, err)
	}
	if row.TickCount != 1 {
		t.Errorf("tick_count = %d, want 1", row.TickCount)
	}
	if row.Dispatched != 1 {
		t.Errorf("dispatched = %d, want 1 (the one ready task)", row.Dispatched)
	}
	if !row.LastTickAt.Equal(tick) {
		t.Errorf("last_tick_at = %v, want %v", row.LastTickAt, tick)
	}
	if row.Errors != 0 || row.LastError != "" {
		t.Errorf("a clean tick must record no error, got errors=%d last_error=%q", row.Errors, row.LastError)
	}
}

// TestRunTickHandledErrorObservable proves a SWALLOWED pass error becomes observable — it
// increments the error counter and sets last_error — WITHOUT changing control flow: the daemon
// still completes the tick (records the row) and the task is left exactly as it was (not failed,
// not dispatched). This is the core "make the black box visible" guarantee for handled errors.
func TestRunTickHandledErrorObservable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "ship1", db.KindShip, []string{"internal/cli"})

	tick := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	// statusErr forces RunOnce to fail closed on the live-fleet read — a handled error runTick
	// logs and swallows. Nothing should be dispatched and the task must stay pending.
	f := &fakeFleet{statusErr: errors.New("board read failed")}
	var log bytes.Buffer
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Dispatch: true, Log: &log, now: func() time.Time { return tick }}

	sc.runTick(ctx)

	row, has, err := s.GetSchedulerStatus(ctx)
	if err != nil || !has {
		t.Fatalf("GetSchedulerStatus: has=%v err=%v", has, err)
	}
	if row.Errors != 1 {
		t.Errorf("errors = %d, want 1 (the swallowed pass error counted)", row.Errors)
	}
	if !strings.Contains(row.LastError, "board read failed") {
		t.Errorf("last_error = %q, want it to contain the pass error", row.LastError)
	}
	if !row.LastErrorAt.Equal(tick) {
		t.Errorf("last_error_at = %v, want %v", row.LastErrorAt, tick)
	}
	// Control flow unchanged: the tick still recorded (tick_count advanced) and nothing dispatched.
	if row.TickCount != 1 || row.Dispatched != 0 {
		t.Errorf("control flow changed: tick=%d dispatched=%d, want 1/0", row.TickCount, row.Dispatched)
	}
	if status(t, s, "ship1") != db.StatusPending {
		t.Errorf("the task must stay pending on a handled error, got %q", status(t, s, "ship1"))
	}
	// The handled error is also still logged to the daemon log (it was never un-swallowed).
	if !strings.Contains(log.String(), "tick error: board read failed") {
		t.Errorf("expected the handled error logged, got:\n%s", log.String())
	}
}

// TestHeartbeatGoesToLogThrottled proves the HEARTBEAT lands in the daemon LOG (sc.Log — a file in
// the auto-started daemon, never a console/TTY write from the loop) and is throttled to
// ~heartbeatInterval: it fires on the first tick (startup), is silent on an immediate next tick,
// and fires again only after the interval elapses. The durable row, by contrast, is written EVERY
// tick.
func TestHeartbeatGoesToLogThrottled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	var log bytes.Buffer
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Log: &log, now: func() time.Time { return now }}

	// Tick 1: lastHeartbeatAt is zero ⇒ a startup heartbeat fires.
	sc.recordTick(ctx, tickStats{dispatched: 0})
	if !strings.Contains(log.String(), "heartbeat: tick=1") {
		t.Fatalf("first tick must emit a startup heartbeat, got:\n%s", log.String())
	}

	// Tick 2 at the SAME clock: under the throttle ⇒ no new heartbeat, but the row still advances.
	log.Reset()
	sc.recordTick(ctx, tickStats{})
	if strings.Contains(log.String(), "heartbeat:") {
		t.Errorf("a sub-interval tick must not heartbeat, got:\n%s", log.String())
	}
	if row, _, _ := s.GetSchedulerStatus(ctx); row.TickCount != 2 {
		t.Errorf("the durable row must advance every tick: tick_count=%d, want 2", row.TickCount)
	}

	// Advance past the interval: the next tick heartbeats again.
	log.Reset()
	now = now.Add(heartbeatInterval + time.Second)
	sc.recordTick(ctx, tickStats{})
	if !strings.Contains(log.String(), "heartbeat: tick=3") {
		t.Errorf("a post-interval tick must heartbeat again, got:\n%s", log.String())
	}
}

// TestRecordTickNilLog proves recordTick is safe with no log sink (it still writes the durable
// row and never panics emitting a heartbeat to a nil Log).
func TestRecordTickNilLog(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sc := &Scheduler{Store: s, Fleet: &fakeFleet{}, Pool: worktree.Pool{Max: 100}} // Log nil
	sc.recordTick(ctx, tickStats{dispatched: 1})
	if row, has, _ := s.GetSchedulerStatus(ctx); !has || row.TickCount != 1 {
		t.Errorf("recordTick with a nil Log must still record the row: has=%v tick=%d", has, row.TickCount)
	}
}

// TestStatusViewStalled exercises the stalled-detection threshold the watchdog shells out to.
func TestStatusViewStalled(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	mk := func(has bool, age time.Duration) StatusView {
		return StatusView{
			Row:        db.SchedulerStatus{LastTickAt: now.Add(-age)},
			HasRow:     has,
			Now:        now,
			StaleAfter: StaleAfterDefault,
		}
	}
	if !mk(false, 0).Stalled() {
		t.Error("a never-ticked daemon (no row) must be reported stalled")
	}
	if mk(true, 5*time.Second).Stalled() {
		t.Error("a fresh last tick (5s, under the 90s threshold) must NOT be stalled")
	}
	if !mk(true, StaleAfterDefault+time.Second).Stalled() {
		t.Error("a last tick older than the threshold must be stalled")
	}
}

// TestStatusViewRender proves the rendered report carries the liveness facets, the counters, the
// last error, and the STALLED banner when stale — the human/watchdog-facing surface.
func TestStatusViewRender(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	healthy := StatusView{
		Row: db.SchedulerStatus{
			LastTickAt: now.Add(-3 * time.Second), TickCount: 10,
			Dispatched: 4, Landed: 2, Gated: 1, Recovered: 1, Deferred: 0, Errors: 0,
		},
		HasRow: true, Pending: 5, Active: 3, LockHeld: true, Now: now, StaleAfter: StaleAfterDefault,
	}
	got := healthy.Render()
	for _, want := range []string{"running (singleton lock held)", "last tick:    3s ago", "ticks:        10", "5 pending, 3 active", "dispatched:   4", "last error:   none"} {
		if !strings.Contains(got, want) {
			t.Errorf("healthy render missing %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "STALLED") {
		t.Errorf("a healthy daemon must not render STALLED, got:\n%s", got)
	}

	stalled := StatusView{
		Row: db.SchedulerStatus{
			LastTickAt: now.Add(-10 * time.Minute), TickCount: 99, Errors: 2,
			LastError: "land tick error: db locked", LastErrorAt: now.Add(-9 * time.Minute),
		},
		HasRow: true, Pending: 0, Active: 0, LockHeld: false, Now: now, StaleAfter: StaleAfterDefault,
	}
	got = stalled.Render()
	for _, want := range []string{"not running (no singleton lock holder)", "STALLED:", "errors:       2", "land tick error: db locked"} {
		if !strings.Contains(got, want) {
			t.Errorf("stalled render missing %q, got:\n%s", want, got)
		}
	}
}

// TestBoardGauges proves the live gauges count ready pending and active workers (cc excluded), the
// numbers the heartbeat and `scheduler status` show.
func TestBoardGauges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"a"})
	addPending(t, s, repo, "p2", db.KindShip, []string{"b"})
	addPending(t, s, repo, "cc1", db.KindCC, []string{"c"}) // cc is excluded from the gauge
	addActiveWorker(t, s, repo, "a1", []string{"d"})

	pending, active := BoardGauges(ctx, s)
	if pending != 2 {
		t.Errorf("pending gauge = %d, want 2 (cc excluded)", pending)
	}
	if active != 1 {
		t.Errorf("active gauge = %d, want 1", active)
	}
}

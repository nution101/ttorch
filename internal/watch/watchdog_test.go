package watch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// newWatchdog builds a Watchdog over a fresh temp-home DB with injected seams: a
// manually-set clock (default: far enough past the manager's last action to count as
// stalled) and a "no live watcher" arming probe. Tests override either seam and seed
// the DB through the returned Store. It returns the manager's recorded UpdatedAt so a
// test can place wd.now relative to it.
func newWatchdog(t *testing.T) (*Watchdog, *db.Store, int64) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	t.Setenv("TTORCH_DB", filepath.Join(home, "state.db"))
	s, err := db.Open(paths.Default().StateDB())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.SetManager(ctx, db.Manager{Dir: "/work", SessionID: "sess-1"}); err != nil {
		t.Fatalf("SetManager: %v", err)
	}
	proj, err := s.UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	wd := NewWatchdog(s, paths.Default())
	// Default clock: well past the stall threshold from the manager's last action, so a
	// test that wants the "stalled" path needs no extra setup; a "recent" test resets it.
	m, _, err := s.GetManager(ctx)
	if err != nil {
		t.Fatalf("GetManager: %v", err)
	}
	wd.now = func() time.Time { return m.UpdatedAt.Add(defaultStall + time.Minute) }
	wd.watchArmed = func() bool { return false }
	return wd, s, proj.ID
}

// seedTask inserts a task in the given status (cc excluded from the manager's scope).
func seedTask(t *testing.T, s *db.Store, projID int64, id, status, kind string) {
	t.Helper()
	if _, err := s.CreateTask(context.Background(), db.Task{
		ID: id, ProjectID: projID, Status: status, Kind: kind,
	}, "manager"); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
}

func mustCheck(t *testing.T, wd *Watchdog) WatchdogResult {
	t.Helper()
	res, err := wd.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func countManagerStalled(t *testing.T, s *db.Store) int {
	t.Helper()
	rows, err := s.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	n := 0
	for _, e := range rows {
		if e.Type == db.EventManagerStalled {
			if e.EntityType != db.EntityTypeManager {
				t.Errorf("manager_stalled entity_type = %q, want manager", e.EntityType)
			}
			if e.Actor != db.ActorSystem {
				t.Errorf("manager_stalled actor = %q, want system", e.Actor)
			}
			n++
		}
	}
	return n
}

// A stalled manager with a done task and nothing to wake it gets exactly one poke, and a
// second check stands down (the appended event is now the unconsumed event — the debounce).
func TestWatchdogPokesStalledManagerWithDoneTask(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "done-1", db.StatusDone, db.KindShip)

	res := mustCheck(t, wd)
	if !res.Poked {
		t.Fatalf("expected a poke, got: %q", res.Reason)
	}
	if n := countManagerStalled(t, s); n != 1 {
		t.Fatalf("manager_stalled events = %d, want 1", n)
	}
	// The poke must be surfaceable by the watch channel (actionable, past the watermark).
	rows, err := s.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(rows) == 0 || rows[len(rows)-1].Type != db.EventManagerStalled {
		t.Fatalf("the appended poke is not surfaced as the latest actionable event: %+v", rows)
	}

	// Debounce: a pending poke means the next check does nothing.
	res = mustCheck(t, wd)
	if res.Poked {
		t.Fatalf("second check re-poked while one was pending: %q", res.Reason)
	}
	if n := countManagerStalled(t, s); n != 1 {
		t.Fatalf("manager_stalled events after debounce = %d, want 1", n)
	}
}

// Once the manager consumes the poke (watermark advances) but the work remains and it
// stalls again, the watchdog re-pokes — it is self-correcting, not one-shot.
func TestWatchdogRepokesAfterWatermarkAdvances(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "blocked-1", db.StatusBlocked, db.KindShip)

	if !mustCheck(t, wd).Poked {
		t.Fatal("expected first poke")
	}
	// The manager woke, advanced its watermark past the poke, but stalled before clearing
	// the blocked task.
	maxEvt, err := s.MaxActionableEventID(context.Background())
	if err != nil {
		t.Fatalf("MaxActionableEventID: %v", err)
	}
	if err := s.SetWatermark(context.Background(), maxEvt); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}
	// SetWatermark bumped updated_at; re-stale the clock relative to it.
	m, _, _ := s.GetManager(context.Background())
	wd.now = func() time.Time { return m.UpdatedAt.Add(defaultStall + time.Minute) }

	if !mustCheck(t, wd).Poked {
		t.Fatal("expected a re-poke after the watermark advanced")
	}
	if n := countManagerStalled(t, s); n != 2 {
		t.Fatalf("manager_stalled events = %d, want 2", n)
	}
}

func TestWatchdogStandsDownWhenAwaitingLead(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "needs-1", db.StatusNeedsInput, db.KindShip)
	if err := s.SetAwaitingLead(context.Background(), true); err != nil {
		t.Fatalf("SetAwaitingLead: %v", err)
	}
	// Re-stale the clock relative to the awaiting-lead write's updated_at.
	m, _, _ := s.GetManager(context.Background())
	wd.now = func() time.Time { return m.UpdatedAt.Add(defaultStall + time.Minute) }

	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked a manager awaiting the lead: %q", res.Reason)
	}
	if n := countManagerStalled(t, s); n != 0 {
		t.Fatalf("manager_stalled events = %d, want 0", n)
	}
}

func TestWatchdogStandsDownWhenIdle(t *testing.T) {
	wd, _, _ := newWatchdog(t)
	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked with no actionable work: %q", res.Reason)
	}
}

func TestWatchdogStandsDownOnPendingActionableEvent(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "done-1", db.StatusDone, db.KindShip)
	// An unconsumed actionable event (e.g. a worker's status_changed the watch will surface)
	// past the watermark — the normal channel wakes the manager, so the watchdog stays out.
	to := db.StatusDone
	if _, err := s.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: "done-1", Type: db.EventStatusChanged,
		Actor: "worker:done-1", ToStatus: &to, Actionable: true,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked while an actionable event was pending: %q", res.Reason)
	}
}

func TestWatchdogStandsDownWhenManagerActedRecently(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "done-1", db.StatusDone, db.KindShip)
	m, _, _ := s.GetManager(context.Background())
	// Just inside the stall window: the manager acted recently, not stalled.
	wd.now = func() time.Time { return m.UpdatedAt.Add(defaultStall - time.Second) }
	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked a manager that acted recently: %q", res.Reason)
	}
}

// An active worker with a live watcher self-heals through the §4.4 liveness net, so the
// watchdog stays out; with no watcher in the loop it is genuine outstanding supervision.
func TestWatchdogActiveWorkerGatedOnWatcher(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	seedTask(t, s, proj, "active-1", db.StatusActive, db.KindShip)

	wd.watchArmed = func() bool { return true }
	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked an active worker with a live watcher: %q", res.Reason)
	}

	wd.watchArmed = func() bool { return false }
	if res := mustCheck(t, wd); !res.Poked {
		t.Fatalf("did not poke an unwatched active worker: %q", res.Reason)
	}
}

func TestWatchdogExcludesCC(t *testing.T) {
	wd, s, proj := newWatchdog(t)
	// A cc task in a manager-owned state must not count as outstanding work.
	seedTask(t, s, proj, "cc-1", db.StatusDone, db.KindCC)
	if res := mustCheck(t, wd); res.Poked {
		t.Fatalf("poked for a cc task: %q", res.Reason)
	}
}

func TestWatchdogNoManagerRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	t.Setenv("TTORCH_DB", filepath.Join(home, "state.db"))
	s, err := db.Open(paths.Default().StateDB())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	wd := NewWatchdog(s, paths.Default())
	res := mustCheck(t, wd)
	if res.Poked {
		t.Fatalf("poked with no manager record: %q", res.Reason)
	}
}

package db

import (
	"context"
	"testing"
)

// mkSpawnedWorker creates an active task with a window and appends a 'spawned' event — the
// dispatch marker the window-gone freshness check anchors the current incarnation on.
func mkSpawnedWorker(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, id)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: id, ProjectID: proj.ID, Status: StatusActive, Kind: KindShip,
		Owner: "worker:" + id, Window: "wk-" + id,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	appendSpawned(t, s, id)
}

// appendSpawned mimics the spawn path's 'spawned' audit event (every (re-)dispatch emits one).
func appendSpawned(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.AppendEvent(context.Background(), Event{
		EntityType: EntityTypeTask, EntityID: id, Type: EventSpawned, Actor: ActorManager,
		Payload: "kind=ship window=wk-" + id,
	}); err != nil {
		t.Fatalf("AppendEvent spawned %s: %v", id, err)
	}
}

// markWindowGone mimics the watcher's liveness poll recording a confirmed-gone window.
func markWindowGone(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.AppendEvent(context.Background(), Event{
		EntityType: EntityTypeTask, EntityID: id, Type: EventWindowGone, Actor: ActorSystem,
		Actionable: true, Payload: "wk-" + id,
	}); err != nil {
		t.Fatalf("AppendEvent window_gone %s: %v", id, err)
	}
}

// TestReclaimWindowGone proves the fast-path recovery: an active worker whose current
// incarnation's window is confirmed gone is moved back to pending (re-dispatchable),
// retry_count is incremented, the lease is cleared, and an actionable worker_reclaimed event
// (active → pending, system actor) is recorded and surfaces in the actionable feed.
func TestReclaimWindowGone(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "g1")
	if err := s.GrantLease(ctx, "g1", "worker:g1"); err != nil { // a live lease — irrelevant once the window is gone
		t.Fatal(err)
	}
	markWindowGone(t, s, "g1")

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatalf("ReclaimWindowGone: %v", err)
	}
	if len(out) != 1 || out[0].TaskID != "g1" || out[0].Failed || out[0].RetryCount != 1 {
		t.Fatalf("reclaim outcome = %+v, want one g1 reclaim with retry_count 1", out)
	}
	tk, _, _ := s.GetTask(ctx, "g1")
	if tk.Status != StatusPending {
		t.Errorf("status = %q, want pending (re-dispatchable)", tk.Status)
	}
	if tk.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", tk.RetryCount)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on reclaim: owner=%q expires=%v", tk.LeaseOwner, tk.LeaseExpiresAt)
	}

	evs := taskEvents(t, s, "g1")
	ev, ok := hasEventType(evs, EventWorkerReclaimed)
	if !ok {
		t.Fatalf("expected a worker_reclaimed event, got %+v", evs)
	}
	if !ev.Actionable || ev.Actor != ActorSystem ||
		ev.FromStatus == nil || *ev.FromStatus != StatusActive || ev.ToStatus == nil || *ev.ToStatus != StatusPending {
		t.Fatalf("worker_reclaimed event wrong: %+v", ev)
	}
	actionable, _ := s.EventsSince(ctx, 0, true)
	if _, ok := hasEventType(actionable, EventWorkerReclaimed); !ok {
		t.Error("worker_reclaimed must be in the actionable feed")
	}
}

// TestReclaimWindowGonePoisonPill proves bounded retry on the window-gone path: a crashed
// worker at the retry ceiling is moved to the terminal 'failed' status (not re-dispatched)
// with an actionable task_failed event — the restart-storm bound.
func TestReclaimWindowGonePoisonPill(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	proj, _ := s.UpsertProject(ctx, "/repo/p1", "p1")
	if _, err := s.CreateTask(ctx, Task{
		ID: "p1", ProjectID: proj.ID, Status: StatusActive, Owner: "worker:p1", Window: "wk-p1",
		RetryCount: 2, MaxRetries: 2, // retries already exhausted
	}, ActorManager); err != nil {
		t.Fatal(err)
	}
	appendSpawned(t, s, "p1")
	markWindowGone(t, s, "p1")

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].Failed {
		t.Fatalf("reclaim at the ceiling must poison-pill: %+v", out)
	}
	tk, _, _ := s.GetTask(ctx, "p1")
	if tk.Status != StatusFailed {
		t.Errorf("status = %q, want failed (terminal)", tk.Status)
	}
	if tk.RetryCount != 2 {
		t.Errorf("retry_count = %d, want 2 (unchanged at poison-pill)", tk.RetryCount)
	}
	evs := taskEvents(t, s, "p1")
	ev, ok := hasEventType(evs, EventTaskFailed)
	if !ok {
		t.Fatalf("expected a task_failed event, got %+v", evs)
	}
	if !ev.Actionable || ev.Actor != ActorSystem || ev.ToStatus == nil || *ev.ToStatus != StatusFailed {
		t.Fatalf("task_failed event wrong: %+v", ev)
	}
	// A failed task is terminal: a second sweep does nothing more (it is no longer active).
	if again, _ := s.ReclaimWindowGone(ctx); len(again) != 0 {
		t.Errorf("a failed task must not be reclaimed again: %+v", again)
	}
}

// TestReclaimWindowGoneIgnoresStaleSignal is the incarnation-safety test (and the
// re-dispatch idempotency guard): a window_gone left over from a PRIOR incarnation — one
// followed by a newer 'spawned' event, i.e. the task was already re-dispatched and is healthy
// again — is NOT reclaimed. The freshness anchor (window_gone.id > latest spawned.id) is what
// keeps a live, re-dispatched worker from being torn down on a stale crash signal.
func TestReclaimWindowGoneIgnoresStaleSignal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "s1") // spawned #1
	markWindowGone(t, s, "s1")  // crash of incarnation #1
	appendSpawned(t, s, "s1")   // re-dispatched: spawned #2 (id > the window_gone) ⇒ live again

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a window_gone older than the latest spawn must be ignored, got %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "s1"); tk.Status != StatusActive {
		t.Errorf("status = %q, want still active (the re-dispatched worker is live)", tk.Status)
	}
}

// TestReclaimWindowGoneRespectsHeartbeat proves the sign-of-life anchor: a worker that has
// reported progress (a worker-authored heartbeat) AFTER its window_gone is NOT reclaimed — it
// is alive again (e.g. resumed in place by `ttorch resume`, or a re-dispatch whose best-effort
// 'spawned' append was dropped) even though the stale window_gone still sits on the row. A
// heartbeat recorded BEFORE the window_gone does not rescue the task (the worker died after
// that report).
func TestReclaimWindowGoneRespectsHeartbeat(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)

	// h1: reported, THEN crashed (heartbeat precedes the window_gone) ⇒ still reclaimable.
	mkSpawnedWorker(t, s, "h1")
	if _, err := s.SetStage(ctx, "h1", "working", "worker:h1"); err != nil {
		t.Fatal(err)
	}
	markWindowGone(t, s, "h1")

	// h2: crashed, THEN a heartbeat landed (resumed / re-dispatched and reporting) ⇒ NOT
	// reclaimable — a worker-authored event newer than the window_gone is a live sign of life.
	mkSpawnedWorker(t, s, "h2")
	markWindowGone(t, s, "h2")
	if _, err := s.SetStage(ctx, "h2", "back at it", "worker:h2"); err != nil {
		t.Fatal(err)
	}

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TaskID != "h1" {
		t.Fatalf("only h1 (heartbeat BEFORE the crash) must be reclaimed, got %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "h1"); tk.Status != StatusPending {
		t.Errorf("h1 must be reclaimed to pending, status=%q", tk.Status)
	}
	if tk, _, _ := s.GetTask(ctx, "h2"); tk.Status != StatusActive {
		t.Errorf("h2 reported AFTER its window_gone (a live sign of life); it must stay active, status=%q", tk.Status)
	}
}

// TestReclaimWindowGoneSkipsLiveWorker proves a worker with NO window_gone signal for its
// current incarnation is never reclaimed — the core never-touch-a-live-worker property at the
// db layer.
func TestReclaimWindowGoneSkipsLiveWorker(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "live1") // active, spawned, but its window is present (no window_gone)

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a worker with no window_gone must not be reclaimed, got %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "live1"); tk.Status != StatusActive {
		t.Errorf("status = %q, want still active", tk.Status)
	}
}

// TestReclaimWindowGoneExcludesCC proves ad-hoc cc sessions are never reclaimed, mirroring the
// dispatch/land passes that never touch cc.
func TestReclaimWindowGoneExcludesCC(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	proj, _ := s.UpsertProject(ctx, "/repo/cc", "cc")
	if _, err := s.CreateTask(ctx, Task{
		ID: "ccsess", ProjectID: proj.ID, Status: StatusActive, Kind: KindCC, Window: "wk-ccsess",
	}, ActorManager); err != nil {
		t.Fatal(err)
	}
	appendSpawned(t, s, "ccsess")
	markWindowGone(t, s, "ccsess")

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a cc task must never be reclaimed: %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "ccsess"); tk.Status != StatusActive {
		t.Errorf("cc status = %q, want untouched active", tk.Status)
	}
}

// TestReclaimWindowGoneOnlyTargetsActive proves reclaim is scoped to active tasks: a worker
// that went done (or any non-active state) is not reclaimed even if a window_gone was recorded
// — only an in-flight worker is recoverable.
func TestReclaimWindowGoneOnlyTargetsActive(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "d1")
	markWindowGone(t, s, "d1")
	// Worker finished: now done (out of the active scope) before any reclaim runs.
	if _, err := s.ReportStatus(ctx, "d1", StatusDone, "worker:d1", "done"); err != nil {
		t.Fatal(err)
	}

	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a done task must not be reclaimed: %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "d1"); tk.Status != StatusDone {
		t.Errorf("status = %q, want done (untouched)", tk.Status)
	}
}

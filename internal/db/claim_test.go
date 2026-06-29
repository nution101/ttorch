package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// mkPendingTask creates a project + a pending backlog task with the given footprint.
func mkPendingTask(t *testing.T, s *Store, id string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, id)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: id, ProjectID: proj.ID, Status: StatusPending, Footprint: footprint,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

// TestClaimTaskFlipsPendingToActiveWithLease proves the happy-path claim: a pending task is
// flipped to active, assigned the owner, given a lease at now+DefaultLeaseDuration (attempt
// bumped), and a non-actionable status_changed event (pending → active, system actor) is
// recorded.
func TestClaimTaskFlipsPendingToActiveWithLease(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkPendingTask(t, s, "c1", []string{"internal/cli"})

	claimed, won, err := s.ClaimTask(ctx, "c1", "worker:c1")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if !won {
		t.Fatal("first claim of a pending task must win")
	}
	if claimed.Status != StatusActive {
		t.Errorf("claimed status = %q, want active", claimed.Status)
	}
	if claimed.Owner != "worker:c1" || claimed.LeaseOwner != "worker:c1" {
		t.Errorf("owner/lease_owner = %q/%q, want worker:c1", claimed.Owner, claimed.LeaseOwner)
	}
	if claimed.LeaseExpiresAt == nil || !claimed.LeaseExpiresAt.Equal(clk.now().Add(DefaultLeaseDuration)) {
		t.Errorf("lease_expires_at = %v, want %v", claimed.LeaseExpiresAt, clk.now().Add(DefaultLeaseDuration))
	}
	// The claim arms the lease but does NOT bump attempt — the dispatch path's GrantLease
	// owns the single increment, so claim+dispatch counts as one attempt (manager parity).
	if claimed.Attempt != 0 {
		t.Errorf("attempt = %d, want 0 after claim (GrantLease bumps it during dispatch)", claimed.Attempt)
	}
	// GrantLease (the dispatch path) then takes it to 1 and re-arms the same lease.
	if err := s.GrantLease(ctx, "c1", "worker:c1"); err != nil {
		t.Fatalf("GrantLease after claim: %v", err)
	}
	if tk, _, _ := s.GetTask(ctx, "c1"); tk.Attempt != 1 {
		t.Errorf("attempt after claim+GrantLease = %d, want 1 (manager-path parity)", tk.Attempt)
	}

	// The claim re-reads through the store consistently.
	got, _, _ := s.GetTask(ctx, "c1")
	if got.Status != StatusActive || got.LeaseOwner != "worker:c1" {
		t.Errorf("persisted row wrong after claim: %+v", got)
	}

	// A non-actionable status_changed (pending → active, system actor) was recorded.
	ev, ok := hasEventType(taskEvents(t, s, "c1"), EventStatusChanged)
	if !ok {
		t.Fatal("expected a status_changed event from the claim")
	}
	if ev.Actionable {
		t.Error("the claim event must be non-actionable (claiming must not wake the watcher)")
	}
	if ev.Actor != ActorSystem || ev.FromStatus == nil || *ev.FromStatus != StatusPending ||
		ev.ToStatus == nil || *ev.ToStatus != StatusActive {
		t.Errorf("claim event wrong: %+v", ev)
	}
	// It must NOT surface through the actionable watch feed (system actor on status_changed).
	actionable, _ := s.EventsSince(ctx, 0, true)
	if _, ok := hasEventType(actionable, EventStatusChanged); ok {
		t.Error("a system-actor status_changed claim must not be in the actionable feed")
	}

	// The claim stamps last_progress_at to the claim time — the re-dispatch incarnation
	// marker the watcher's staleness gates key on (parity with the manager spawn path's
	// pending → active ReportStatus). The returned row carries it directly (GrantLease,
	// run above, leaves last_progress_at untouched).
	if claimed.LastProgressAt == nil || !claimed.LastProgressAt.Equal(clk.now()) {
		t.Errorf("claimed last_progress_at = %v, want the claim time %v", claimed.LastProgressAt, clk.now())
	}
	// The claim also plants a non-actionable 'spawned' incarnation marker so the window-gone
	// supervisor's freshness anchor outranks any prior stale window_gone the instant the row
	// is active — the chokepoint that stops a re-dispatched, live worker being repeat-reclaimed.
	sp, ok := hasEventType(taskEvents(t, s, "c1"), EventSpawned)
	if !ok {
		t.Fatal("expected a spawned incarnation marker from the claim")
	}
	if sp.Actionable || sp.Actor != ActorSystem {
		t.Errorf("the claim's spawned marker must be non-actionable, system-actor: %+v", sp)
	}
	// Being non-actionable, it must stay out of the actionable watch feed (claiming must not
	// wake the watcher).
	if _, ok := hasEventType(actionable, EventSpawned); ok {
		t.Error("a non-actionable spawned incarnation marker must not be in the actionable feed")
	}
}

// TestClaimTaskSecondClaimLosesRace proves the core no-double-claim guarantee at the
// primitive level: once a task has been claimed (it is no longer pending), a second claim
// returns won=false without touching the row.
func TestClaimTaskSecondClaimLosesRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "c2", []string{"internal/db"})

	if _, won, err := s.ClaimTask(ctx, "c2", "worker:c2"); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	before, _, _ := s.GetTask(ctx, "c2")
	claimed, won, err := s.ClaimTask(ctx, "c2", "someone-else")
	if err != nil {
		t.Fatalf("second claim returned an error: %v", err)
	}
	if won {
		t.Fatal("a second claim of an already-claimed task must lose")
	}
	if claimed.ID != "" {
		t.Errorf("a lost claim must return a zero task, got %+v", claimed)
	}
	after, _, _ := s.GetTask(ctx, "c2")
	if after.Owner != before.Owner || after.LeaseOwner != before.LeaseOwner || after.Attempt != before.Attempt {
		t.Errorf("a lost claim must not mutate the row: before=%+v after=%+v", before, after)
	}
}

// TestClaimTaskRejectsNonPendingAndMissing proves the claim only ever fires on a pending
// row: an active task and a missing id both return won=false (not an error).
func TestClaimTaskRejectsNonPendingAndMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkActiveTask(t, s, "a1") // created directly in active

	if _, won, err := s.ClaimTask(ctx, "a1", "worker:a1"); err != nil || won {
		t.Errorf("claiming an active task: won=%v err=%v, want won=false nil", won, err)
	}
	if _, won, err := s.ClaimTask(ctx, "ghost", "worker:ghost"); err != nil || won {
		t.Errorf("claiming a missing task: won=%v err=%v, want won=false nil", won, err)
	}
}

// TestClaimTaskConcurrentSingleStore proves exactly one of N concurrent claims of the same
// task wins on a single store (one connection ⇒ serialized writers).
func TestClaimTaskConcurrentSingleStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "race1", []string{"internal/x"})

	const n = 8
	var wins int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, won, err := s.ClaimTask(ctx, "race1", "worker:race1"); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one concurrent claim must win, got %d", wins)
	}
}

// TestClaimTaskConcurrentTwoStores proves the cross-process guarantee: two separate Store
// handles on the SAME db file (simulating two scheduler instances) racing to claim the same
// task yield exactly one winner — WAL + busy_timeout + BEGIN IMMEDIATE serialize the
// writers across connections, and the in-tx status re-check rejects the loser.
func TestClaimTaskConcurrentTwoStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	defer s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	defer s2.Close()

	mkPendingTask(t, s1, "race2", []string{"internal/y"})

	var wins int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range []*Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, won, err := s.ClaimTask(ctx, "race2", "worker:race2"); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one of two store instances must win the claim, got %d", wins)
	}
	if tk, _, _ := s1.GetTask(ctx, "race2"); tk.Status != StatusActive {
		t.Errorf("the claimed task must be active, got %q", tk.Status)
	}
}

// TestReleaseClaimRevertsGuarded proves the dispatch-failure undo: ReleaseClaim returns a
// claimed (active + leased) task to pending with the lease cleared and a non-actionable
// status_changed event — but declines (returns false, no mutation) when the task is no
// longer the active row that owner holds.
func TestReleaseClaimRevertsGuarded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "rel1", []string{"internal/z"})
	if _, won, err := s.ClaimTask(ctx, "rel1", "worker:rel1"); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	released, err := s.ReleaseClaim(ctx, "rel1", "worker:rel1")
	if err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if !released {
		t.Fatal("releasing the active task we hold must revert it")
	}
	tk, _, _ := s.GetTask(ctx, "rel1")
	if tk.Status != StatusPending {
		t.Errorf("status = %q, want pending after release", tk.Status)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on release: %+v", tk)
	}
	ev, ok := hasEventType(taskEvents(t, s, "rel1"), EventStatusChanged)
	if !ok || ev.Actionable {
		t.Errorf("expected a non-actionable status_changed release event, got ok=%v ev=%+v", ok, ev)
	}

	// The reverted task is pending again and re-claimable.
	if _, won, err := s.ClaimTask(ctx, "rel1", "worker:rel1"); err != nil || !won {
		t.Fatalf("a released task must be re-claimable: won=%v err=%v", won, err)
	}
	// A release by the WRONG owner is a guarded no-op (the row moved on / not ours).
	if released, err := s.ReleaseClaim(ctx, "rel1", "someone-else"); err != nil || released {
		t.Errorf("release by a non-owner must decline: released=%v err=%v", released, err)
	}
	// A release of a missing task is a guarded no-op, not an error.
	if released, err := s.ReleaseClaim(ctx, "ghost", "worker:ghost"); err != nil || released {
		t.Errorf("release of a missing task must decline: released=%v err=%v", released, err)
	}
}

// TestClaimTaskRetiresStaleWindowGone is the repeat-crash regression: after a crashed worker
// is reclaimed and the scheduler RE-DISPATCHES it via ClaimTask, the prior incarnation's
// window_gone must NOT reclaim the now-live worker — even before the spawn path appends its own
// 'spawned' event (and even if that best-effort append is later dropped). ClaimTask plants a
// 'spawned' incarnation marker in the claim transaction, so the window-gone freshness anchor
// outranks the stale window_gone the instant the task is active. Without it the supervisor
// would reclaim the live worker every tick, burning retries to a poison-pill — the "repeat
// crash". (TestReclaimWindowGoneIgnoresStaleSignal covers only a literal new 'spawned' append,
// not the real ClaimTask re-dispatch path.)
func TestClaimTaskRetiresStaleWindowGone(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "r1") // spawned #1, active
	markWindowGone(t, s, "r1")  // incarnation #1 crashed

	// First reclaim: the crash is the current sign of life ⇒ back to pending, retry 1.
	out, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TaskID != "r1" || out[0].RetryCount != 1 {
		t.Fatalf("first reclaim = %+v, want one r1 reclaim at retry 1", out)
	}
	if tk, _, _ := s.GetTask(ctx, "r1"); tk.Status != StatusPending {
		t.Fatalf("after reclaim status = %q, want pending", tk.Status)
	}

	// Scheduler re-dispatch: claim the pending task back to active. The claim ALONE (no
	// spawn.go 'spawned' append yet) must retire the stale window_gone.
	claimed, won, err := s.ClaimTask(ctx, "r1", "worker:r1")
	if err != nil || !won {
		t.Fatalf("re-claim: won=%v err=%v", won, err)
	}
	if claimed.Status != StatusActive {
		t.Fatalf("re-claimed status = %q, want active", claimed.Status)
	}

	// The supervisor must now leave the live, just-re-dispatched worker alone.
	again, err := s.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a re-dispatched worker must not be reclaimed on the prior incarnation's window_gone, got %+v", again)
	}
	tk, _, _ := s.GetTask(ctx, "r1")
	if tk.Status != StatusActive {
		t.Errorf("status = %q, want still active (live re-dispatched worker)", tk.Status)
	}
	if tk.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1 (no extra retry burned by a false reclaim)", tk.RetryCount)
	}
}

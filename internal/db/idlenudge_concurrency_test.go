package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mkIdleLeasedWorker creates an active, briefed worker with a live window, a valid (future)
// lease, a controllable last_progress_at, and the 'spawned' anchor a real dispatch leaves —
// the candidate ClaimIdleNudge evaluates.
func mkIdleLeasedWorker(t *testing.T, s *Store, id string, progress time.Time) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	lease := time.Now().Add(time.Hour)
	if _, err := s.CreateTask(ctx, Task{
		ID: id, ProjectID: proj.ID, Status: StatusActive, Kind: KindShip,
		Owner: "worker:" + id, Window: "wk-" + id, Footprint: []string{"pkg/" + id},
		LeaseOwner: "worker:" + id, LeaseExpiresAt: &lease,
		LastProgressAt: &progress, HasBrief: true,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: id, Type: EventSpawned, Actor: ActorSystem,
		Actionable: false, Payload: "spawned",
	}); err != nil {
		t.Fatalf("spawned %s: %v", id, err)
	}
}

// TestClaimIdleNudgeConcurrentSingleStore proves the atomic claim: N goroutines racing to nudge
// the SAME idle screen against one Store yield EXACTLY one Send — the BEGIN IMMEDIATE write lock
// (DSN _txlock=immediate) + SetMaxOpenConns(1) serialize them, and the loser sees the recorded
// nudge (LastHash) and backs off. This is the in-process arm of the documented multi-instance
// safety, mirroring db.ClaimTask.
func TestClaimIdleNudgeConcurrentSingleStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkIdleLeasedWorker(t, s, "race1", time.Now().Add(-10*time.Minute))

	const n = 8
	var mu sync.Mutex
	var sends, giveups int
	var wg sync.WaitGroup
	wg.Add(n)
	now := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			d, _, err := s.ClaimIdleNudge(ctx, "race1", "samehash", now, 0, 2)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			switch d {
			case IdleNudgeSend:
				sends++
			case IdleNudgeGiveUp:
				giveups++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if sends != 1 {
		t.Fatalf("exactly one concurrent claim must Send for ONE idle screen, got %d (giveups=%d)", sends, giveups)
	}
}

// TestClaimIdleNudgeConcurrentTwoStores proves the cross-process arm: two Store handles on the
// SAME db file racing to nudge one screen yield exactly one Send — WAL + busy_timeout + BEGIN
// IMMEDIATE serialize the two transactions, so two scheduler instances never double-nudge.
func TestClaimIdleNudgeConcurrentTwoStores(t *testing.T) {
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

	mkIdleLeasedWorker(t, s1, "race2", time.Now().Add(-10*time.Minute))

	var mu sync.Mutex
	var sends int
	var wg sync.WaitGroup
	now := time.Now()
	for _, s := range []*Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _, err := s.ClaimIdleNudge(ctx, "race2", "samehash", now, 0, 2); err == nil && d == IdleNudgeSend {
				mu.Lock()
				sends++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if sends != 1 {
		t.Fatalf("exactly one of two store instances must Send, got %d", sends)
	}
}

// TestClaimIdleNudgeConcurrentGiveUpOnce proves the give-up is recorded exactly once per episode
// even under a race: with the budget already spent, N concurrent claims produce exactly one
// GiveUp (the capped marker is written under the lock; the rest read Capped and Skip).
func TestClaimIdleNudgeConcurrentGiveUpOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkIdleLeasedWorker(t, s, "race3", time.Now().Add(-10*time.Minute))
	// Spend the budget (max 1): one Send makes Count == 1 >= 1.
	if d, _, err := s.ClaimIdleNudge(ctx, "race3", "h1", time.Now(), 0, 1); err != nil || d != IdleNudgeSend {
		t.Fatalf("seed send = %v err=%v", d, err)
	}

	const n = 8
	var mu sync.Mutex
	var giveups int
	var wg sync.WaitGroup
	wg.Add(n)
	now := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			d, _, err := s.ClaimIdleNudge(ctx, "race3", "h2", now, 0, 1)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if d == IdleNudgeGiveUp {
				mu.Lock()
				giveups++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if giveups != 1 {
		t.Fatalf("GiveUp must be recorded exactly once across concurrent claims, got %d", giveups)
	}
}

// TestClaimIdleNudgeDrainsBeforeExec proves idleNudgeInfoTx's multi-row SELECT is fully
// drained+closed before ClaimIdleNudge issues its appendEvent Exec on the SAME transaction —
// required under SetMaxOpenConns(1), where an open cursor would otherwise block the write.
func TestClaimIdleNudgeDrainsBeforeExec(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkIdleLeasedWorker(t, s, "multi", time.Now().Add(-10*time.Minute))
	// Seed several REAL idle_nudged events so idleNudgeInfoTx's SELECT returns multiple rows.
	for _, h := range []string{"a", "b", "c", "d"} {
		if _, err := s.AppendEvent(ctx, Event{
			EntityType: EntityTypeTask, EntityID: "multi", Type: EventIdleNudged, Actor: ActorSystem,
			Actionable: false, Payload: h,
		}); err != nil {
			t.Fatalf("seed %s: %v", h, err)
		}
	}
	// Count == 4; with max 10 + a fresh hash + grace 0 the claim must Send, which requires the
	// appendEvent Exec to run on the tx AFTER the multi-row read is drained.
	d, count, err := s.ClaimIdleNudge(ctx, "multi", "freshhash", time.Now(), 0, 10)
	if err != nil {
		t.Fatalf("ClaimIdleNudge with a multi-row read failed (drain bug?): %v", err)
	}
	if d != IdleNudgeSend {
		t.Fatalf("decision = %v, want Send", d)
	}
	if count != 4 {
		t.Fatalf("count = %d, want 4", count)
	}
}

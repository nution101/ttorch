package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mkDoneTask creates a project + a done task awaiting landing. withWorkerLease arms a worker
// lease first (as a real done task carries: the worker's progress path extends the lease and
// done does not clear it), so the claim's overwrite-of-a-moot-worker-lease path is exercised.
func mkDoneTask(t *testing.T, s *Store, id string, withWorkerLease bool) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, id)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: id, ProjectID: proj.ID, Status: StatusDone, Footprint: []string{"pkg/" + id},
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if withWorkerLease {
		if err := s.GrantLease(ctx, id, "worker:"+id); err != nil {
			t.Fatalf("GrantLease: %v", err)
		}
	}
}

// TestClaimForLandClaimsDoneTask proves the happy path: a done task (carrying a moot worker
// lease) is claimed for landing — its lease is overwritten with the lander's name and a fresh
// expiry, and its STATUS stays done — and a second claim by the same owner backs off because a
// live land claim is already in flight.
func TestClaimForLandClaimsDoneTask(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkDoneTask(t, s, "d1", true)

	won, err := s.ClaimForLand(ctx, "d1", "lander:d1")
	if err != nil {
		t.Fatalf("ClaimForLand: %v", err)
	}
	if !won {
		t.Fatal("first land claim of a done task must win")
	}
	tk, _, _ := s.GetTask(ctx, "d1")
	if tk.Status != StatusDone {
		t.Errorf("status = %q, want done (the land claim must NOT change status)", tk.Status)
	}
	if tk.LeaseOwner != "lander:d1" {
		t.Errorf("lease_owner = %q, want lander:d1 (worker lease overwritten)", tk.LeaseOwner)
	}
	if tk.LeaseExpiresAt == nil || !tk.LeaseExpiresAt.Equal(clk.now().Add(DefaultLeaseDuration)) {
		t.Errorf("lease_expires_at = %v, want %v", tk.LeaseExpiresAt, clk.now().Add(DefaultLeaseDuration))
	}

	// A second claim while the land claim is live (same owner, unexpired) must lose.
	won, err = s.ClaimForLand(ctx, "d1", "lander:d1")
	if err != nil {
		t.Fatalf("second ClaimForLand: %v", err)
	}
	if won {
		t.Error("a second claim over a live land claim must lose")
	}
}

// TestClaimForLandRejectsNonDone proves only done tasks are claimable for landing — pending,
// active, and delivered tasks are never claimed.
func TestClaimForLandRejectsNonDone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mkPendingTask(t, s, "p1", []string{"pkg/p1"})
	for _, st := range []struct {
		id, status string
	}{
		{"p1", StatusPending},
		{"a1", StatusActive},
		{"x1", StatusDelivered},
	} {
		if st.id != "p1" {
			proj, _ := s.UpsertProject(ctx, "/repo/"+st.id, st.id)
			if _, err := s.CreateTask(ctx, Task{ID: st.id, ProjectID: proj.ID, Status: st.status}, ActorManager); err != nil {
				t.Fatalf("CreateTask %s: %v", st.id, err)
			}
		}
		won, err := s.ClaimForLand(ctx, st.id, "lander:"+st.id)
		if err != nil {
			t.Fatalf("ClaimForLand %s: %v", st.id, err)
		}
		if won {
			t.Errorf("claimed a %s task for landing; only done is claimable", st.status)
		}
	}

	// A nonexistent task is not claimable (and not an error).
	if won, err := s.ClaimForLand(ctx, "ghost", "lander:ghost"); err != nil || won {
		t.Errorf("ClaimForLand(ghost) = (%v, %v), want (false, nil)", won, err)
	}
}

// TestClaimForLandReclaimsExpiredLandClaim proves a land claim that has lapsed (a lander died
// mid-land) becomes re-claimable, so a crashed land never strands a done task forever.
func TestClaimForLandReclaimsExpiredLandClaim(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkDoneTask(t, s, "d1", false)

	if won, err := s.ClaimForLand(ctx, "d1", "lander:d1"); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	// Before expiry, a re-claim loses.
	if won, _ := s.ClaimForLand(ctx, "d1", "lander:d1"); won {
		t.Fatal("a re-claim before the land claim expires must lose")
	}
	// After the lease lapses, the claim is recoverable.
	clk.advance(DefaultLeaseDuration + time.Minute)
	if won, err := s.ClaimForLand(ctx, "d1", "lander:d1"); err != nil || !won {
		t.Fatalf("re-claim after expiry: won=%v err=%v, want recoverable", won, err)
	}
}

// TestReleaseLandClaim proves the fast-path undo: releasing a held claim clears the lease
// (status unchanged) so a later tick can re-claim immediately, while a foreign owner or an
// already-released task is left untouched.
func TestReleaseLandClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkDoneTask(t, s, "d1", false)

	if won, err := s.ClaimForLand(ctx, "d1", "lander:d1"); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	// A foreign owner cannot release someone else's claim.
	if released, err := s.ReleaseLandClaim(ctx, "d1", "lander:other"); err != nil || released {
		t.Errorf("ReleaseLandClaim(foreign) = (%v, %v), want (false, nil)", released, err)
	}
	if lo := mustGet(t, s, "d1").LeaseOwner; lo != "lander:d1" {
		t.Errorf("a foreign release must not clear the lease, got %q", lo)
	}

	// The owner releases: lease cleared, status still done.
	released, err := s.ReleaseLandClaim(ctx, "d1", "lander:d1")
	if err != nil || !released {
		t.Fatalf("ReleaseLandClaim(owner) = (%v, %v), want (true, nil)", released, err)
	}
	tk := mustGet(t, s, "d1")
	if tk.Status != StatusDone {
		t.Errorf("status = %q, want done after release", tk.Status)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared after release, got owner=%q exp=%v", tk.LeaseOwner, tk.LeaseExpiresAt)
	}

	// Releasing again is a no-op (nothing to clear).
	if released, _ := s.ReleaseLandClaim(ctx, "d1", "lander:d1"); released {
		t.Error("a second release must report nothing cleared")
	}
	// And after release the task is freely re-claimable.
	if won, _ := s.ClaimForLand(ctx, "d1", "lander:d1"); !won {
		t.Error("a released task must be re-claimable immediately")
	}
}

// TestConcurrentClaimForLandSingleWinner is the db-level land race: two Store handles on one db
// file (two processes) claiming the same done task concurrently produce exactly one winner —
// the BEGIN IMMEDIATE serialized re-read, not owner uniqueness, makes the claim exclusive.
func TestConcurrentClaimForLandSingleWinner(t *testing.T) {
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
	mkDoneTask(t, s1, "d1", true)

	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for _, s := range []*Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Same per-task owner on both instances: exclusivity must come from the claim tx.
			won, err := s.ClaimForLand(ctx, "d1", "lander:d1")
			if err != nil {
				t.Errorf("concurrent ClaimForLand: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("land claim won %d times, want exactly 1", wins)
	}
}

func mustGet(t *testing.T, s *Store, id string) Task {
	t.Helper()
	tk, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask %s: ok=%v err=%v", id, ok, err)
	}
	return tk
}

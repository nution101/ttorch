package db

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClaimMarkerIsHeldUntilReleased(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := func(task, key string) bool {
		t.Helper()
		won, err := s.ClaimMarker(ctx, task, "x_claim", "x_release", key, ActorLead)
		if err != nil {
			t.Fatal(err)
		}
		return won
	}
	if !claim("t1", "k=1") {
		t.Fatal("first claim lost")
	}
	if claim("t1", "k=1") {
		t.Fatal("second claim of a held key won")
	}
	if !claim("t1", "k=2") || !claim("t2", "k=1") {
		t.Fatal("a different key or task was treated as held")
	}
	if _, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t1", Type: "x_release", Actor: ActorLead, Payload: "k=1"}); err != nil {
		t.Fatal(err)
	}
	if !claim("t1", "k=1") {
		t.Fatal("a released key could not be claimed again")
	}
	if claim("t1", "k=1") {
		t.Fatal("a re-claimed key was claimed twice")
	}
	if _, err := s.ClaimMarker(ctx, "t1", "same", "same", "k", ActorLead); err == nil {
		t.Fatal("identical claim and release types were accepted")
	}
}

// TestClaimMarkerHasOneWinnerAcrossStores: two Stores on one DB file stand in for two
// processes. Of every claimant on either, exactly one wins.
func TestClaimMarkerHasOneWinnerAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	var stores []*Store
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores = append(stores, s)
	}
	for round := 0; round < 20; round++ {
		key := "question=" + string(rune('a'+round))
		var wins atomic.Int32
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(s *Store) {
				defer wg.Done()
				<-start
				won, err := s.ClaimMarker(context.Background(), "t1", "x_claim", "x_release", key, ActorLead)
				if err != nil {
					t.Error(err)
				}
				if won {
					wins.Add(1)
				}
			}(stores[i%2])
		}
		close(start)
		wg.Wait()
		if n := wins.Load(); n != 1 {
			t.Fatalf("round %d: %d claimants won, want exactly 1", round, n)
		}
	}
}

package db

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConsumeActionable covers the inbox claim: it takes only rows that should wake the
// manager, advances the watermark to the highest one, and a repeat call with nothing new
// takes nothing and leaves the watermark where it was.
func TestConsumeActionable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "t1", ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
		t.Fatal(err)
	}
	done, err := s.ReportStatus(ctx, "t1", StatusDone, "worker:t1", "finished")
	if err != nil {
		t.Fatal(err)
	}
	// A non-actionable row above it must not be taken, and must not drag the watermark up.
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "t1", Type: EventNote, Actor: ActorSystem,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConsumeActionable(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != done.ID {
		t.Fatalf("first consume = %+v, want exactly the done transition #%d", got.Events, done.ID)
	}
	if got.Since != 0 || got.Watermark != done.ID {
		t.Fatalf("since/watermark = %d/%d, want 0/%d", got.Since, got.Watermark, done.ID)
	}
	m, _, _ := s.GetManager(ctx)
	if m.WatchWatermark != done.ID {
		t.Fatalf("stored watermark = %d, want %d", m.WatchWatermark, done.ID)
	}

	again, err := s.ConsumeActionable(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != 0 || again.Since != done.ID || again.Watermark != done.ID {
		t.Fatalf("repeat consume = %+v, want nothing taken at watermark %d", again, done.ID)
	}
}

// TestConsumeActionable_FloorAboveWatermark: a caller holding a newer watermark than the
// stored one never re-takes rows at or below it.
func TestConsumeActionable_FloorAboveWatermark(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	for _, id := range []string{"a", "b"} {
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := s.ReportStatus(ctx, "a", StatusDone, "worker:a", "")
	second, _ := s.ReportStatus(ctx, "b", StatusBlocked, "worker:b", "")

	got, err := s.ConsumeActionable(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != second.ID {
		t.Fatalf("consume above floor #%d = %+v, want only #%d", first.ID, got.Events, second.ID)
	}
}

// TestConsumeActionable_RacingConsumersTakeEachRowOnce runs two stores on one database
// file (two processes, as `ttorch inbox` and `ttorch watch` are) against updates that
// keep arriving while they read. Every update must be taken by exactly one of them.
func TestConsumeActionable_RacingConsumersTakeEachRowOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ctx := context.Background()
	proj, _ := writer.UpsertProject(ctx, "/r", "r")

	const n = 60
	var mu sync.Mutex
	taken := map[int64]int{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for c := 0; c < 2; c++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			for {
				got, err := s.ConsumeActionable(ctx, 0)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, e := range got.Events {
					taken[e.ID]++
				}
				mu.Unlock()
				select {
				case <-stop:
					if len(got.Events) == 0 {
						return
					}
				default:
				}
			}
		}(s)
	}

	var ids []int64
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%02d", i)
		if _, err := writer.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
			t.Fatal(err)
		}
		ev, err := writer.ReportStatus(ctx, id, StatusDone, "worker:"+id, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	close(stop)
	wg.Wait()

	for _, id := range ids {
		if taken[id] != 1 {
			t.Errorf("event #%d taken %d times, want exactly once", id, taken[id])
		}
	}
}

// TestLatestEvent returns the newest row of the requested type for the entity, ignoring
// other types and other entities.
func TestLatestEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, err := s.LatestEvent(ctx, EntityTypeManager, "manager", "manager_woken"); err != nil || ok {
		t.Fatalf("no rows yet: ok=%v err=%v", ok, err)
	}
	add := func(entityType, entityID, typ, payload string) int64 {
		id, err := s.AppendEvent(ctx, Event{EntityType: entityType, EntityID: entityID, Type: typ, Actor: ActorSystem, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add(EntityTypeManager, "manager", "manager_woken", "1")
	want := add(EntityTypeManager, "manager", "manager_woken", "2")
	add(EntityTypeManager, "manager", EventManagerStalled, "")
	add(EntityTypeTask, "manager", "manager_woken", "3")

	got, ok, err := s.LatestEvent(ctx, EntityTypeManager, "manager", "manager_woken")
	if err != nil || !ok {
		t.Fatalf("LatestEvent: ok=%v err=%v", ok, err)
	}
	if got.ID != want || got.Payload != "2" {
		t.Fatalf("LatestEvent = #%d %q, want #%d %q", got.ID, got.Payload, want, "2")
	}
}

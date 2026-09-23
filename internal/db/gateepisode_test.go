package db

import (
	"context"
	"testing"
	"time"
)

// TestGateEpisodeOpenedAt_TakesTheFirstOpeningOfTheCurrentRun pins the two rules the stall
// clock relies on: a re-opened episode for the same head keeps its first opening, and moving
// to another head and back starts the clock again instead of inheriting a stale opening.
//
// The first pair is also the ordering check. t0 is a whole second, so its RFC3339Nano form
// has no fraction and sorts AFTER t0+500ms as a string. MIN(ts) would return the later one.
func TestGateEpisodeOpenedAt_TakesTheFirstOpeningOfTheCurrentRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	open := func(head string, at time.Time) {
		t.Helper()
		if _, err := s.AppendEvent(ctx, Event{
			EntityType: EntityTypeTask, EntityID: "t1", Type: EventGateEpisodeOpened,
			Actor: ActorSystem, TS: at, Payload: head,
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := func(head string, at time.Time, ok bool) {
		t.Helper()
		got, found, err := s.GateEpisodeOpenedAt(ctx, "t1", head)
		if err != nil {
			t.Fatal(err)
		}
		if found != ok || (ok && !got.Equal(at)) {
			t.Fatalf("GateEpisodeOpenedAt(%s) = (%v, %v), want (%v, %v)", head, got, found, at, ok)
		}
	}

	want("A", time.Time{}, false)
	open("A", t0)
	open("A", t0.Add(500*time.Millisecond)) // the same head re-opened
	want("A", t0, true)

	open("B", t0.Add(time.Hour))
	want("B", t0.Add(time.Hour), true)
	want("A", time.Time{}, false) // A has no current run once the task moved on

	open("A", t0.Add(2*time.Hour))
	want("A", t0.Add(2*time.Hour), true)

	// Another task's markers never count.
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "t2", Type: EventGateEpisodeOpened,
		Actor: ActorSystem, TS: t0.Add(-time.Hour), Payload: "A",
	}); err != nil {
		t.Fatal(err)
	}
	want("A", t0.Add(2*time.Hour), true)
}

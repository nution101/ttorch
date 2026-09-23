package db

import (
	"context"
	"testing"
	"time"
)

func appendStall(t *testing.T, s *Store, id, typ, payload string, actionable bool) int64 {
	t.Helper()
	eid, err := s.AppendEvent(context.Background(), Event{
		EntityType: EntityTypeTask, EntityID: id, Type: typ, Actor: ActorSystem,
		Actionable: actionable, Payload: payload,
	})
	if err != nil {
		t.Fatalf("AppendEvent %s: %v", typ, err)
	}
	return eid
}

// TestStallInfo_NoClock: a task the watcher has never observed has no clock and no raises.
func TestStallInfo_NoClock(t *testing.T) {
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "n1")
	st, err := s.StallInfo(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if st.HasClock || len(st.Raises) != 0 {
		t.Fatalf("want no clock and no raises, got %+v", st)
	}
	if st.SignOfLifeID == 0 {
		t.Fatal("the spawned event should count as a sign of life")
	}
}

// TestStallInfo_RaisesCountFromLatestClock: only the stalled events newer than the latest
// stall_clock count, so a clock restart puts the task back at the bottom of the ladder.
func TestStallInfo_RaisesCountFromLatestClock(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "r1")

	appendStall(t, s, "r1", EventStallClock, "aaaa", false)
	c.advance(10 * time.Minute)
	appendStall(t, s, "r1", EventStalled, `{"raise":1}`, true)
	c.advance(15 * time.Minute)
	appendStall(t, s, "r1", EventStalled, `{"raise":2}`, true)

	st, err := s.StallInfo(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasClock || st.ClockPayload != "aaaa" {
		t.Fatalf("clock = %+v, want pane aaaa", st)
	}
	want0 := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	if len(st.Raises) != 2 || !st.Raises[0].Equal(want0) {
		t.Fatalf("raises = %v, want two starting at %v", st.Raises, want0)
	}

	c.advance(time.Minute)
	clockID := appendStall(t, s, "r1", EventStallClock, "", false)
	st, err = s.StallInfo(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if st.ClockID != clockID || st.ClockPayload != "" || len(st.Raises) != 0 {
		t.Fatalf("after a restart want the new clock and no raises, got %+v", st)
	}
}

// TestStallInfo_SignOfLife: a worker-authored event or a status change after the clock
// shows up as a newer sign of life; the watcher's own system events do not.
func TestStallInfo_SignOfLife(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreClock(t)
	mkSpawnedWorker(t, s, "l1")
	clockID := appendStall(t, s, "l1", EventStallClock, "aaaa", false)
	appendStall(t, s, "l1", EventStalled, `{}`, true)
	appendStall(t, s, "l1", EventIdleNudged, "bbbb", false)

	st, err := s.StallInfo(ctx, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if st.SignOfLifeID > clockID {
		t.Fatalf("system events must not read as a sign of life (sol=%d clock=%d)", st.SignOfLifeID, clockID)
	}

	if _, err := s.SetStage(ctx, "l1", "writing tests", "worker:l1"); err != nil {
		t.Fatal(err)
	}
	st, err = s.StallInfo(ctx, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if st.SignOfLifeID <= clockID {
		t.Fatalf("a worker stage update must read as a newer sign of life (sol=%d clock=%d)", st.SignOfLifeID, clockID)
	}
}

package db

import (
	"context"
	"testing"
	"time"
)

// TestHasEventType covers the watcher's durable once-only guard for external events
// (e.g. pr_merged): false until an event of that type exists for the task, true
// after, and scoped to the task.
func TestHasEventType(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "t1", ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasEventType(ctx, "t1", EventPRMerged); err != nil || has {
		t.Fatalf("no pr_merged yet: has=%v err=%v", has, err)
	}
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "t1", Type: EventPRMerged, Actor: ActorSystem, Actionable: true,
	}); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasEventType(ctx, "t1", EventPRMerged); err != nil || !has {
		t.Fatalf("pr_merged should now exist: has=%v err=%v", has, err)
	}
	if has, _ := s.HasEventType(ctx, "other", EventPRMerged); has {
		t.Fatal("event must be scoped to its task, not leak to another id")
	}
}

// TestHasActionableEventForTask covers the watcher's liveness exclusion: a
// non-actionable event never counts, an actionable one does, and the optional cutoff
// (the task's last_progress_at) bounds the window with a >= comparison.
func TestHasActionableEventForTask(t *testing.T) {
	s, clk := newTestStoreClock(t)
	ctx := context.Background()
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "t1", ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
		t.Fatal(err)
	}
	// The 'created' event is non-actionable, so it must not count.
	if has, err := s.HasActionableEventForTask(ctx, "t1", nil); err != nil || has {
		t.Fatalf("created (non-actionable) must not count: has=%v err=%v", has, err)
	}

	clk.advance(time.Hour)
	ev, err := s.ReportStatus(ctx, "t1", StatusBlocked, "worker:t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasActionableEventForTask(ctx, "t1", nil); err != nil || !has {
		t.Fatalf("an actionable worker transition must count: has=%v err=%v", has, err)
	}
	after := ev.TS.Add(time.Minute)
	if has, _ := s.HasActionableEventForTask(ctx, "t1", &after); has {
		t.Fatal("an event before the cutoff must not count")
	}
	at := ev.TS
	if has, _ := s.HasActionableEventForTask(ctx, "t1", &at); !has {
		t.Fatal("an event at the cutoff must count (>=)")
	}
}

// TestActionableEventTypesForTask: the types of a task's actionable events, bounded by the
// same >= cutoff as HasActionableEventForTask, with non-actionable events and other tasks'
// events left out.
func TestActionableEventTypesForTask(t *testing.T) {
	s, clk := newTestStoreClock(t)
	ctx := context.Background()
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	for _, id := range []string{"t1", "t2"} {
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.ActionableEventTypesForTask(ctx, "t1", nil); err != nil || len(got) != 0 {
		t.Fatalf("created (non-actionable) must not count: got=%v err=%v", got, err)
	}
	clk.advance(time.Hour)
	early := clk.t
	_, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t1", Type: EventIdleUnreported, Actor: ActorSystem, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	late := clk.t
	if _, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t1", Type: EventAgentExited, Actor: ActorSystem, Actionable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t2", Type: EventWindowGone, Actor: ActorSystem, Actionable: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActionableEventTypesForTask(ctx, "t1", nil)
	if err != nil || len(got) != 2 || !got[EventIdleUnreported] || !got[EventAgentExited] {
		t.Fatalf("all actionable types = %v (err %v), want idle_unreported and agent_exited", got, err)
	}
	cut := early.Add(time.Minute)
	if got, _ := s.ActionableEventTypesForTask(ctx, "t1", &cut); len(got) != 1 || !got[EventAgentExited] {
		t.Fatalf("types after the cutoff = %v, want only agent_exited", got)
	}
	at := late
	if got, _ := s.ActionableEventTypesForTask(ctx, "t1", &at); !got[EventAgentExited] {
		t.Fatalf("an event at the cutoff must count (>=): %v", got)
	}
}

package db

import (
	"context"
	"testing"
)

func TestRecentTaskEventsFiltersOrdersAndLimits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	add := func(entityType, id, typ string) {
		t.Helper()
		if _, err := s.AppendEvent(ctx, Event{EntityType: entityType, EntityID: id, Type: typ, Actor: ActorSystem}); err != nil {
			t.Fatal(err)
		}
	}
	add(EntityTypeTask, "a", EventMerged)
	add(EntityTypeTask, "b", EventNote)
	add(EntityTypeTask, "c", EventDelivered)
	add(EntityTypeSystem, "sys", EventMerged) // not task-scoped: never returned
	add(EntityTypeTask, "d", EventPRMerged)
	add(EntityTypeTask, "e", EventMerged)

	got, err := s.RecentTaskEvents(ctx, []string{EventMerged, EventDelivered, EventPRMerged}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.EntityID)
	}
	if want := []string{"e", "d", "c"}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("RecentTaskEvents = %v, want %v (newest first, task events of the asked types only, limited)", ids, want)
	}

	all, err := s.RecentTaskEvents(ctx, []string{EventMerged}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].EntityID != "e" || all[1].EntityID != "a" {
		t.Fatalf("RecentTaskEvents(merged) = %+v, want e then a", all)
	}
}

func TestRecentTaskEventsEmptyInputsReturnNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "a", Type: EventMerged, Actor: ActorSystem}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		types []string
		limit int
	}{{nil, 5}, {[]string{EventMerged}, 0}, {[]string{EventMerged}, -1}} {
		got, err := s.RecentTaskEvents(ctx, tc.types, tc.limit)
		if err != nil || len(got) != 0 {
			t.Fatalf("RecentTaskEvents(%v, %d) = %v, %v; want nothing", tc.types, tc.limit, got, err)
		}
	}
}

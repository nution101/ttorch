package db

import (
	"context"
	"testing"
)

// TestEffortColumnPresent proves migration 0004 added the effort column to a fresh DB.
func TestEffortColumnPresent(t *testing.T) {
	s := newTestStore(t)
	if !taskColumns(t, s)["effort"] {
		t.Fatal("effort column missing after migrate")
	}
}

// TestEffortPersistsThroughInsertAndGet round-trips a task's effort through CreateTask
// (insert path) and GetTask (the scan path), proving the new column reads and writes.
func TestEffortPersistsThroughInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/eff", "eff")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "e1", ProjectID: proj.ID, Kind: KindShip, Status: StatusActive, Effort: "medium",
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.GetTask(ctx, "e1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Effort != "medium" {
		t.Fatalf("effort = %q, want medium", got.Effort)
	}
}

// TestEffortSyncedOnUpsertUpdate proves the spawn-from-backlog path persists effort: a
// pending row (no effort) is UPDATEd in place by UpsertTask, and the resume-relevant
// effort column carries the dispatched level. An empty effort defaults to "".
func TestEffortSyncedOnUpsertUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/eff2", "eff2")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// A backlog row created by `task add` carries no effort.
	if _, err := s.CreateTask(ctx, Task{
		ID: "e2", ProjectID: proj.ID, Status: StatusPending,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "e2"); got.Effort != "" {
		t.Fatalf("fresh backlog effort = %q, want empty", got.Effort)
	}
	// Spawn-from-backlog upserts the runtime fields, including the dispatched effort.
	if _, err := s.UpsertTask(ctx, Task{
		ID: "e2", ProjectID: proj.ID, Window: "wk-e2", Effort: "low",
	}, ActorManager); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	got, _, err := s.GetTask(ctx, "e2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Effort != "low" {
		t.Fatalf("effort after upsert = %q, want low", got.Effort)
	}
}

// TestEffortViaSetTaskFields proves the partial-update path can set effort independently.
func TestEffortViaSetTaskFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/eff3", "eff3")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "e3", ProjectID: proj.ID, Status: StatusActive, Effort: "high",
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	want := "xhigh"
	if err := s.SetTaskFields(ctx, "e3", TaskFields{Effort: &want}); err != nil {
		t.Fatalf("SetTaskFields: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "e3"); got.Effort != "xhigh" {
		t.Fatalf("effort after SetTaskFields = %q, want xhigh", got.Effort)
	}
}

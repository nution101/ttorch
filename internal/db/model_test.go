package db

import (
	"context"
	"testing"
)

// TestModelColumnPresent proves migration 0007 added the model column to a fresh DB.
func TestModelColumnPresent(t *testing.T) {
	s := newTestStore(t)
	if !taskColumns(t, s)["model"] {
		t.Fatal("model column missing after migrate")
	}
}

// TestModelPersistsThroughInsertAndGet round-trips a task's model through CreateTask
// (insert path) and GetTask (the scan path), proving the new column reads and writes.
func TestModelPersistsThroughInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/mdl", "mdl")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "m1", ProjectID: proj.ID, Kind: KindShip, Status: StatusActive, Model: "sonnet",
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.GetTask(ctx, "m1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Model != "sonnet" {
		t.Fatalf("model = %q, want sonnet", got.Model)
	}
}

// TestModelSyncedOnUpsertUpdate proves the spawn-from-backlog path persists model: a
// pending row (no model) is UPDATEd in place by UpsertTask with the dispatched model.
func TestModelSyncedOnUpsertUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/mdl2", "mdl2")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// A backlog row created by `task add` carries no model.
	if _, err := s.CreateTask(ctx, Task{
		ID: "m2", ProjectID: proj.ID, Status: StatusPending,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "m2"); got.Model != "" {
		t.Fatalf("fresh backlog model = %q, want empty", got.Model)
	}
	// Spawn-from-backlog upserts the runtime fields, including the dispatched model.
	if _, err := s.UpsertTask(ctx, Task{
		ID: "m2", ProjectID: proj.ID, Window: "wk-m2", Model: "opus",
	}, ActorManager); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	got, _, err := s.GetTask(ctx, "m2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Model != "opus" {
		t.Fatalf("model after upsert = %q, want opus", got.Model)
	}
}

// TestModelViaSetTaskFields proves the partial-update path can set model independently.
func TestModelViaSetTaskFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/mdl3", "mdl3")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "m3", ProjectID: proj.ID, Status: StatusActive, Model: "haiku",
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	want := "opus"
	if err := s.SetTaskFields(ctx, "m3", TaskFields{Model: &want}); err != nil {
		t.Fatalf("SetTaskFields: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "m3"); got.Model != "opus" {
		t.Fatalf("model after SetTaskFields = %q, want opus", got.Model)
	}
}

// TestEffortAndModelCoexist proves the two orthogonal dials round-trip together on one row.
// It guards the INSERT column list / scan-order alignment after model was added next to
// effort — a misalignment there would swap or drop one of the two values.
func TestEffortAndModelCoexist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/mdl4", "mdl4")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "m4", ProjectID: proj.ID, Status: StatusActive, Effort: "ultracode", Model: "opus",
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.GetTask(ctx, "m4")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Effort != "ultracode" || got.Model != "opus" {
		t.Fatalf("got effort=%q model=%q, want ultracode/opus", got.Effort, got.Model)
	}
}

// TestMigration0007TaskModel is the focused up→down→up test for the model migration: the
// column exists on a fresh DB, MigrateDown(6) drops it (leaving the rest, incl. effort), and
// re-up restores it — matching the per-migration acceptance the other migrations carry.
func TestMigration0007TaskModel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // Open() already migrated to the latest

	if v, err := s.schemaVersion(ctx); err != nil || v != 8 {
		t.Fatalf("fresh DB version = %d err=%v, want 8", v, err)
	}
	if !taskColumns(t, s)["model"] {
		t.Fatal("model column missing after up")
	}

	// Down the 0007 step only: the model column is dropped, the rest of the schema stays.
	if err := s.MigrateDown(ctx, 6); err != nil {
		t.Fatalf("MigrateDown(6): %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 6 {
		t.Fatalf("after down-to-6: version=%d err=%v, want 6", v, err)
	}
	if taskColumns(t, s)["model"] {
		t.Error("model column must be gone at version 6")
	}
	if !taskColumns(t, s)["effort"] {
		t.Error("0007 down must not touch the effort column")
	}

	// Re-apply: the column comes back.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	if !taskColumns(t, s)["model"] {
		t.Error("model column missing after re-up")
	}
}

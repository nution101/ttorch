package db

import (
	"context"
	"testing"
)

// TestHasBriefColumnPresent proves migration 0005 added the has_brief column to a fresh DB.
func TestHasBriefColumnPresent(t *testing.T) {
	s := newTestStore(t)
	if !taskColumns(t, s)["has_brief"] {
		t.Fatal("has_brief column missing after migrate")
	}
}

// TestHasBriefDefaultsFalse proves a fresh task has no stored brief unless one is recorded —
// the conservative default that makes the scheduler skip (not silently dispatch) a task whose
// brief presence was never asserted.
func TestHasBriefDefaultsFalse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/hb0", "hb0")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "hb0", ProjectID: proj.ID, Status: StatusPending,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "hb0"); got.HasBrief {
		t.Fatal("fresh task has_brief = true, want false")
	}
}

// TestHasBriefPersistsThroughInsertAndGet round-trips has_brief through CreateTask (insert
// path) and GetTask (the scan path), proving the new column reads and writes.
func TestHasBriefPersistsThroughInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/hb1", "hb1")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{
		ID: "hb1", ProjectID: proj.ID, Kind: KindShip, Status: StatusPending, HasBrief: true,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.GetTask(ctx, "hb1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if !got.HasBrief {
		t.Fatal("has_brief = false, want true after an insert with HasBrief")
	}
}

// TestSetBriefStored proves the brief-writing seam: SetBriefStored raises has_brief on an
// existing row, is a no-op (no error) on a not-yet-created row — the spawn path writes the
// brief file before creating the task row — and is idempotent.
func TestSetBriefStored(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/hb2", "hb2")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// No-op (and NO error) when the row does not exist yet — the `ttorch spawn --brief`
	// ordering, where the brief is written before the spawn creates the row.
	if err := s.SetBriefStored(ctx, "missing"); err != nil {
		t.Fatalf("SetBriefStored on a missing row must be a no-op, got: %v", err)
	}
	if _, ok, _ := s.GetTask(ctx, "missing"); ok {
		t.Fatal("SetBriefStored must not create a row")
	}

	// Raises the flag on an existing row, and stays raised on a second call.
	if _, err := s.CreateTask(ctx, Task{
		ID: "hb2", ProjectID: proj.ID, Status: StatusPending,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "hb2"); got.HasBrief {
		t.Fatal("has_brief must start false")
	}
	if err := s.SetBriefStored(ctx, "hb2"); err != nil {
		t.Fatalf("SetBriefStored: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "hb2"); !got.HasBrief {
		t.Fatal("has_brief = false after SetBriefStored, want true")
	}
	if err := s.SetBriefStored(ctx, "hb2"); err != nil {
		t.Fatalf("SetBriefStored (idempotent): %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "hb2"); !got.HasBrief {
		t.Fatal("has_brief must remain true after a repeat SetBriefStored")
	}
}

// TestHasBriefPreservedOnUpsertUpdate proves the spawn-from-backlog UpsertTask UPDATE path
// does NOT clobber has_brief: a task add --brief row (has_brief=1) keeps its brief flag when
// the daemon's spawn upserts its runtime/coupling fields. This is what lets a reclaimed,
// re-dispatched task stay dispatchable.
func TestHasBriefPreservedOnUpsertUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, err := s.UpsertProject(ctx, "/repo/hb3", "hb3")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// A backlog row that was added WITH a brief.
	if _, err := s.CreateTask(ctx, Task{
		ID: "hb3", ProjectID: proj.ID, Status: StatusPending, HasBrief: true,
	}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// Spawn-from-backlog upserts runtime fields (window, etc.) without touching has_brief.
	if _, err := s.UpsertTask(ctx, Task{
		ID: "hb3", ProjectID: proj.ID, Window: "wk-hb3",
	}, ActorManager); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if got, _, _ := s.GetTask(ctx, "hb3"); !got.HasBrief {
		t.Fatal("has_brief was clobbered to false by the UpsertTask update path")
	}
}

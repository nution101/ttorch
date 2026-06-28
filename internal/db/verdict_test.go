package db

import (
	"context"
	"testing"
	"time"
)

// mkTaskForVerdict creates a project + task so a verdict (FK → tasks) can attach.
func mkTaskForVerdict(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, id)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive}, "manager"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func TestVerdictSaveFetchRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkTaskForVerdict(t, s, "vr1")

	in := Verdict{
		TaskID:      "vr1",
		Overall:     "pass",
		ReviewedSHA: "abc123",
		DiffID:      "deadbeef",
		Findings:    `[{"dimension":"correctness","severity":"low","reviewer":"corr","summary":"nit"}]`,
		ApprovedBy:  "auto",
		ApprovalSHA: "abc123",
	}
	if err := s.SaveVerdict(ctx, in); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	got, ok, err := s.GetVerdict(ctx, "vr1")
	if err != nil || !ok {
		t.Fatalf("GetVerdict: ok=%v err=%v", ok, err)
	}
	if got.Overall != "pass" || got.ReviewedSHA != "abc123" || got.DiffID != "deadbeef" ||
		got.Findings != in.Findings || got.ApprovedBy != "auto" || got.ApprovalSHA != "abc123" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", got)
	}
}

// TestVerdictUpsertReplacesPreservingCreatedAt proves a re-record overwrites the row in
// place (one row per task), preserves the original created_at, and advances updated_at —
// the carry-forward / re-record contract.
func TestVerdictUpsertReplacesPreservingCreatedAt(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkTaskForVerdict(t, s, "vu1")

	clk.advance(time.Second)
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "vu1", Overall: "block", ReviewedSHA: "sha1", DiffID: "d1"}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := s.GetVerdict(ctx, "vu1")

	clk.advance(time.Minute)
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "vu1", Overall: "pass", ReviewedSHA: "sha2", DiffID: "d2", ApprovedBy: "human", ApprovalSHA: "sha2"}); err != nil {
		t.Fatal(err)
	}
	second, ok, _ := s.GetVerdict(ctx, "vu1")
	if !ok {
		t.Fatal("verdict vanished after replace")
	}
	if second.Overall != "pass" || second.ReviewedSHA != "sha2" || second.DiffID != "d2" || second.ApprovedBy != "human" {
		t.Fatalf("replace did not overwrite mutable columns: %+v", second)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at must be preserved across a replace: first=%v second=%v", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at must advance across a replace: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}

	// Exactly one row — a replace must not append.
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM verdicts WHERE task_id='vu1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("verdicts rows for task = %d, want 1 (upsert, not append)", rows)
	}
}

func TestVerdictDeleteIdempotentAndMissingFetch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkTaskForVerdict(t, s, "vd1")

	// Missing fetch → not found, no error (fail-closed at the gate).
	if _, ok, err := s.GetVerdict(ctx, "vd1"); err != nil || ok {
		t.Fatalf("missing GetVerdict = ok=%v err=%v, want false/nil", ok, err)
	}
	// Delete of an absent row is a no-op, not an error (idempotent consume).
	if err := s.DeleteVerdict(ctx, "vd1"); err != nil {
		t.Fatalf("DeleteVerdict on absent row: %v", err)
	}

	if err := s.SaveVerdict(ctx, Verdict{TaskID: "vd1", Overall: "pass", ReviewedSHA: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVerdict(ctx, "vd1"); err != nil {
		t.Fatalf("DeleteVerdict: %v", err)
	}
	if _, ok, _ := s.GetVerdict(ctx, "vd1"); ok {
		t.Error("verdict should be gone after delete (consumed)")
	}
}

// TestVerdictRequiresExistingTask proves the FK: a verdict can only attach to a real task,
// so a stray verdict can never outlive (or precede) its task.
func TestVerdictRequiresExistingTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "ghost", Overall: "pass", ReviewedSHA: "x"}); err == nil {
		t.Error("expected FK violation saving a verdict for a non-existent task")
	}
}

// TestRecordDeliveryWritesVerdictAtomically proves RecordDelivery upserts the verdict row
// in the SAME transaction as the summary columns, so the two never drift.
func TestRecordDeliveryWritesVerdictAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkTaskForVerdict(t, s, "rd1")

	v := Verdict{TaskID: "rd1", Overall: "pass", ReviewedSHA: "sha9", DiffID: "dd", ApprovedBy: "auto", ApprovalSHA: "sha9"}
	if err := s.RecordDelivery(ctx, "rd1", Delivery{
		GatePassed: true, ApprovedBy: "auto", ReviewedSHA: "sha9",
		EventType: EventReviewRecorded, Verdict: &v,
	}); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	// Summary columns.
	tk, _, _ := s.GetTask(ctx, "rd1")
	if !tk.GatePassed || tk.ApprovedBy != "auto" || tk.ReviewedSHA != "sha9" {
		t.Fatalf("summary not written: %+v", tk)
	}
	// Verdict row.
	gv, ok, _ := s.GetVerdict(ctx, "rd1")
	if !ok || gv.Overall != "pass" || gv.ReviewedSHA != "sha9" || gv.DiffID != "dd" || gv.ApprovedBy != "auto" {
		t.Fatalf("verdict row not written atomically: %+v ok=%v", gv, ok)
	}
	// A nil Verdict leaves the row in place (summary-only update).
	if err := s.RecordDelivery(ctx, "rd1", Delivery{
		GatePassed: true, ApprovedBy: "human", ReviewedSHA: "sha9", EventType: EventApproved,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetVerdict(ctx, "rd1"); !ok {
		t.Error("a nil-Verdict RecordDelivery must not delete the existing verdict row")
	}
}

// TestMigration0002OnExisting0001 proves the new migration applies cleanly on top of an
// existing 0001 database (acceptance criterion 1): migrate down to 1 (verdicts gone, the
// 0001 tables intact), then back up to 2 (verdicts restored), with no data loss to tasks.
func TestMigration0002OnExisting0001(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // Open already migrated to 2
	mkTaskForVerdict(t, s, "mt1")

	// Roll back the 0002 step only, leaving the 0001 schema in place.
	if err := s.MigrateDown(ctx, 1); err != nil {
		t.Fatalf("MigrateDown(1): %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("after down-to-1: version=%d err=%v, want 1", v, err)
	}
	if tableExists(t, s, "verdicts") {
		t.Error("verdicts must be gone at version 1")
	}
	if !tableExists(t, s, "tasks") {
		t.Error("0001 tables (tasks) must survive the 0002 rollback")
	}
	// GetTask can't be used here: at version 1 the tasks table lacks the lease columns
	// the HEAD taskSelect references, so assert survival with a raw count instead.
	var mt1 int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id='mt1'`).Scan(&mt1); err != nil || mt1 != 1 {
		t.Fatalf("task data must survive the 0002 rollback: count=%d err=%v", mt1, err)
	}

	// Re-apply the pending migrations on top of the existing 0001 DB.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 3 {
		t.Fatalf("after re-up: version=%d err=%v, want 3", v, err)
	}
	if !tableExists(t, s, "verdicts") {
		t.Error("verdicts must be present again after re-migrate")
	}
	// The restored table is usable.
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "mt1", Overall: "pass", ReviewedSHA: "z"}); err != nil {
		t.Fatalf("SaveVerdict after re-migrate: %v", err)
	}
}

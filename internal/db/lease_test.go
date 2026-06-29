package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// mkActiveTask creates a project + an active task and returns the store's clock zero.
func mkActiveTask(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, id)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive, Owner: "worker:" + id}, ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

// taskColumns returns the set of column names on the tasks table (PRAGMA table_info).
func taskColumns(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `PRAGMA table_info(tasks)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	return cols
}

func indexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// --- migration 0003 ---------------------------------------------------------------

// TestMigration0003FreshSchema proves a fresh DB migrates to the lease schema: the five
// new columns exist with the documented defaults, the terminal 'failed' status is
// accepted, and the reclaim index is present.
func TestMigration0003FreshSchema(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if v, err := s.schemaVersion(ctx); err != nil || v != 5 {
		t.Fatalf("fresh DB version = %d err=%v, want 5", v, err)
	}
	cols := taskColumns(t, s)
	for _, c := range []string{"lease_owner", "lease_expires_at", "retry_count", "max_retries", "attempt"} {
		if !cols[c] {
			t.Errorf("tasks is missing the %q column after 0003", c)
		}
	}
	if !indexExists(t, s, "idx_tasks_lease") {
		t.Error("idx_tasks_lease missing after 0003")
	}

	// A fresh task carries the documented lease/retry defaults.
	mkActiveTask(t, s, "f1")
	tk, _, _ := s.GetTask(ctx, "f1")
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil || tk.RetryCount != 0 || tk.Attempt != 0 {
		t.Errorf("fresh lease defaults wrong: %+v", tk)
	}
	if tk.MaxRetries != DefaultMaxRetries {
		t.Errorf("fresh max_retries = %d, want %d", tk.MaxRetries, DefaultMaxRetries)
	}

	// The terminal 'failed' status is now accepted by the CHECK constraint.
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='failed' WHERE id='f1'`); err != nil {
		t.Errorf("0003 CHECK must accept 'failed': %v", err)
	}
}

// TestMigration0003RebuildPreservesChildren is the cascade-safety regression test: the
// table rebuild that adds the lease columns + 'failed' status must NOT cascade-delete the
// tasks table's ON DELETE CASCADE children (verdicts, notes) — the whole reason the runner
// disables foreign keys around it. It also proves the down half restores the 0001 shape
// (lease columns gone, 'failed' mapped to 'abandoned') without dropping task/child data,
// and the up half restores everything.
func TestMigration0003RebuildPreservesChildren(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // already at version 3

	mkActiveTask(t, s, "t1")
	// A child verdict and a child note (both ON DELETE CASCADE → tasks): these are what a
	// careless rebuild would silently destroy.
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "t1", Overall: "pass", ReviewedSHA: "sha"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNote(ctx, "t1", "worker:t1", "a note"); err != nil {
		t.Fatal(err)
	}
	// A second task parked in the terminal 'failed' status, to exercise the down mapping.
	proj, _ := s.UpsertProject(ctx, "/repo/t2", "t2")
	if _, err := s.CreateTask(ctx, Task{ID: "t2", ProjectID: proj.ID, Status: StatusFailed}, ActorManager); err != nil {
		t.Fatal(err)
	}

	// --- down to 2: the 0003 rebuild runs in reverse ---
	if err := s.MigrateDown(ctx, 2); err != nil {
		t.Fatalf("MigrateDown(2): %v", err)
	}
	if cols := taskColumns(t, s); cols["lease_owner"] || cols["retry_count"] {
		t.Error("lease columns must be gone at version 2")
	}
	// Children must survive the rebuild DROP (the cascade-safety guarantee).
	assertCount(t, s, `SELECT count(*) FROM verdicts WHERE task_id='t1'`, 1, "verdict must survive the 0003 rollback")
	assertCount(t, s, `SELECT count(*) FROM notes WHERE task_id='t1'`, 1, "note must survive the 0003 rollback")
	assertCount(t, s, `SELECT count(*) FROM tasks`, 2, "both tasks must survive the 0003 rollback")
	// 'failed' is absent from the 0001 CHECK, so the down copy mapped it to 'abandoned'.
	var st string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id='t2'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != StatusAbandoned {
		t.Errorf("failed task mapped to %q on down, want abandoned", st)
	}

	// --- back up to the latest ---
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate to latest: %v", err)
	}
	if v, _ := s.schemaVersion(ctx); v != 5 {
		t.Fatalf("version after re-up = %d, want 5", v)
	}
	if !taskColumns(t, s)["lease_owner"] {
		t.Error("lease columns must be back after re-up")
	}
	// Data still intact and usable through the store.
	if tk, ok, err := s.GetTask(ctx, "t1"); err != nil || !ok || tk.MaxRetries != DefaultMaxRetries {
		t.Fatalf("t1 after round-trip: ok=%v err=%v maxRetries=%d", ok, err, tk.MaxRetries)
	}
	assertCount(t, s, `SELECT count(*) FROM verdicts WHERE task_id='t1'`, 1, "verdict must survive the full round-trip")
}

func assertCount(t *testing.T, s *Store, query string, want int, msg string) {
	t.Helper()
	var got int
	if err := s.db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("%s: query error %v", msg, err)
	}
	if got != want {
		t.Errorf("%s: count=%d, want %d", msg, got, want)
	}
}

// --- lease primitives -------------------------------------------------------------

// TestGrantLeaseSetsOwnerExpiryAttempt proves dispatch arms a lease: owner + expiry at
// now+DefaultLeaseDuration, and attempt is bumped each grant.
func TestGrantLeaseSetsOwnerExpiryAttempt(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkActiveTask(t, s, "g1")

	if err := s.GrantLease(ctx, "g1", "worker:g1"); err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	tk, _, _ := s.GetTask(ctx, "g1")
	if tk.LeaseOwner != "worker:g1" {
		t.Errorf("lease_owner = %q, want worker:g1", tk.LeaseOwner)
	}
	if tk.LeaseExpiresAt == nil || !tk.LeaseExpiresAt.Equal(clk.now().Add(DefaultLeaseDuration)) {
		t.Errorf("lease_expires_at = %v, want %v", tk.LeaseExpiresAt, clk.now().Add(DefaultLeaseDuration))
	}
	if tk.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 after first grant", tk.Attempt)
	}

	// A second grant (re-dispatch) bumps attempt and re-arms the expiry.
	clk.advance(time.Minute)
	if err := s.GrantLease(ctx, "g1", "worker:g1"); err != nil {
		t.Fatal(err)
	}
	tk, _, _ = s.GetTask(ctx, "g1")
	if tk.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 after second grant", tk.Attempt)
	}

	// A missing task is an error (not a silent no-op).
	if err := s.GrantLease(ctx, "ghost", "worker:ghost"); err == nil {
		t.Error("GrantLease on a missing task must error")
	}
}

// TestLeaseExtendOnProgress proves the heartbeat: a worker's ReportStatus and SetStage
// push a held lease's expiry forward, while a task that holds NO lease is untouched.
func TestLeaseExtendOnProgress(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkActiveTask(t, s, "h1")
	if err := s.GrantLease(ctx, "h1", "worker:h1"); err != nil {
		t.Fatal(err)
	}

	// ReportStatus extends the lease to now+duration.
	clk.advance(time.Hour)
	if _, err := s.ReportStatus(ctx, "h1", StatusActive, "worker:h1", "still going"); err != nil {
		t.Fatal(err)
	}
	tk, _, _ := s.GetTask(ctx, "h1")
	wantA := clk.now().Add(DefaultLeaseDuration)
	if tk.LeaseExpiresAt == nil || !tk.LeaseExpiresAt.Equal(wantA) {
		t.Fatalf("after ReportStatus lease_expires_at = %v, want %v", tk.LeaseExpiresAt, wantA)
	}

	// SetStage extends it again.
	clk.advance(time.Hour)
	if _, err := s.SetStage(ctx, "h1", "testing", "worker:h1"); err != nil {
		t.Fatal(err)
	}
	tk, _, _ = s.GetTask(ctx, "h1")
	wantB := clk.now().Add(DefaultLeaseDuration)
	if tk.LeaseExpiresAt == nil || !tk.LeaseExpiresAt.Equal(wantB) {
		t.Fatalf("after SetStage lease_expires_at = %v, want %v", tk.LeaseExpiresAt, wantB)
	}

	// A task with no lease is never given one by the progress path.
	mkActiveTask(t, s, "h2")
	if _, err := s.ReportStatus(ctx, "h2", StatusActive, "worker:h2", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStage(ctx, "h2", "x", "worker:h2"); err != nil {
		t.Fatal(err)
	}
	if tk2, _, _ := s.GetTask(ctx, "h2"); tk2.LeaseExpiresAt != nil || tk2.LeaseOwner != "" {
		t.Errorf("progress must not mint a lease for an unleased task: %+v", tk2)
	}
}

// TestExtendLeasePrimitive proves the standalone heartbeat: it pushes a held lease forward
// and is a no-op for an unleased task.
func TestExtendLeasePrimitive(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkActiveTask(t, s, "e1")
	if err := s.GrantLease(ctx, "e1", "worker:e1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	if err := s.ExtendLease(ctx, "e1"); err != nil {
		t.Fatal(err)
	}
	tk, _, _ := s.GetTask(ctx, "e1")
	if tk.LeaseExpiresAt == nil || !tk.LeaseExpiresAt.Equal(clk.now().Add(DefaultLeaseDuration)) {
		t.Errorf("ExtendLease expiry = %v, want %v", tk.LeaseExpiresAt, clk.now().Add(DefaultLeaseDuration))
	}

	mkActiveTask(t, s, "e2")
	if err := s.ExtendLease(ctx, "e2"); err != nil {
		t.Fatal(err)
	}
	if tk2, _, _ := s.GetTask(ctx, "e2"); tk2.LeaseExpiresAt != nil {
		t.Error("ExtendLease must be a no-op for an unleased task")
	}
}

// --- reclaim ----------------------------------------------------------------------

// TestReclaimExpiredLease proves the core reclaim: an expired-lease active task is moved
// back to pending (re-dispatchable), retry_count is incremented, the lease is cleared, and
// an actionable lease_expired event is recorded.
func TestReclaimExpiredLease(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkActiveTask(t, s, "r1")
	if err := s.GrantLease(ctx, "r1", "worker:r1"); err != nil {
		t.Fatal(err)
	}

	// Walk past the lease expiry — verifiable ground truth that the worker is gone.
	clk.advance(DefaultLeaseDuration + time.Minute)
	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredLeases: %v", err)
	}
	if len(out) != 1 || out[0].TaskID != "r1" || out[0].Failed || out[0].RetryCount != 1 {
		t.Fatalf("reclaim outcome = %+v, want one r1 reclaim with retry_count 1", out)
	}

	tk, _, _ := s.GetTask(ctx, "r1")
	if tk.Status != StatusPending {
		t.Errorf("status = %q, want pending (re-dispatchable)", tk.Status)
	}
	if tk.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", tk.RetryCount)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on reclaim: owner=%q expires=%v", tk.LeaseOwner, tk.LeaseExpiresAt)
	}

	// An actionable lease_expired event was recorded (active → pending, system actor).
	evs := taskEvents(t, s, "r1")
	ev, ok := hasEventType(evs, EventLeaseExpired)
	if !ok {
		t.Fatalf("expected a lease_expired event, got %+v", evs)
	}
	if !ev.Actionable || ev.Actor != ActorSystem ||
		ev.FromStatus == nil || *ev.FromStatus != StatusActive || ev.ToStatus == nil || *ev.ToStatus != StatusPending {
		t.Fatalf("lease_expired event wrong: %+v", ev)
	}
	// It surfaces through the actionable watch feed.
	actionable, _ := s.EventsSince(ctx, 0, true)
	if _, ok := hasEventType(actionable, EventLeaseExpired); !ok {
		t.Error("lease_expired must be in the actionable feed")
	}
}

// TestReclaimExpiryBoundaryFractionalNow guards the SQL-vs-Go comparison skew: a lease
// expiring exactly on a whole second (formatted "…00Z") is reclaimed once now is a
// fractional moment past it ("…00.5Z"). A byte-wise SQL `lease_expires_at < now` predicate
// would wrongly skip it ('Z' sorts after '.'); the Go-side expiry check must reclaim it.
func TestReclaimExpiryBoundaryFractionalNow(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t) // anchored at a whole second (…00:00:00Z)
	mkActiveTask(t, s, "b1")
	if err := s.GrantLease(ctx, "b1", "worker:b1"); err != nil { // expires on a whole second
		t.Fatal(err)
	}
	// Land now just past the whole-second expiry by a fraction of a second.
	clk.advance(DefaultLeaseDuration + 500*time.Millisecond)

	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TaskID != "b1" {
		t.Fatalf("a lease expired by 500ms past a whole second must be reclaimed, got %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "b1"); tk.Status != StatusPending {
		t.Errorf("status = %q, want pending", tk.Status)
	}
}

// TestReclaimSkipsLiveLeases proves a NON-expired lease is never reclaimed — neither one
// still within its window, nor one whose original window has passed but a heartbeat
// extended it past now. Timeline (t0 = clock start, lease = 2h):
//
//	t0    : grant beat1            → beat1 expires t0+2h
//	t0+1h : grant live1, beat live1 progress → live1 expires t0+3h, beat1 re-armed to t0+3h
//	t0+2.5h: reclaim — beat1's ORIGINAL t0+2h is past, but the heartbeat moved it to t0+3h
func TestReclaimSkipsLiveLeases(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)

	mkActiveTask(t, s, "beat1")
	if err := s.GrantLease(ctx, "beat1", "worker:beat1"); err != nil { // expires t0+2h
		t.Fatal(err)
	}

	clk.advance(time.Hour) // t0+1h
	mkActiveTask(t, s, "live1")
	if err := s.GrantLease(ctx, "live1", "worker:live1"); err != nil { // expires t0+3h
		t.Fatal(err)
	}
	if _, err := s.ReportStatus(ctx, "beat1", StatusActive, "worker:beat1", "heartbeat"); err != nil {
		t.Fatal(err) // re-arms beat1 to t0+3h, rescuing it from its original t0+2h expiry
	}

	clk.advance(90 * time.Minute) // t0+2.5h: beat1's original window has passed
	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("no lease has genuinely expired; reclaim returned %+v", out)
	}
	for _, id := range []string{"live1", "beat1"} {
		if tk, _, _ := s.GetTask(ctx, id); tk.Status != StatusActive {
			t.Errorf("%s status = %q, want still active (not reclaimed)", id, tk.Status)
		}
	}
}

// TestReclaimPoisonPill proves bounded retry: a task at the retry ceiling is moved to the
// terminal 'failed' status (not re-dispatched) with an actionable task_failed event.
func TestReclaimPoisonPill(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	proj, _ := s.UpsertProject(ctx, "/repo/p1", "p1")
	// Seed a task that has already used up its retries (retry_count == max_retries).
	if _, err := s.CreateTask(ctx, Task{
		ID: "p1", ProjectID: proj.ID, Status: StatusActive, Owner: "worker:p1",
		RetryCount: 2, MaxRetries: 2,
	}, ActorManager); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantLease(ctx, "p1", "worker:p1"); err != nil {
		t.Fatal(err)
	}

	clk.advance(DefaultLeaseDuration + time.Minute)
	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].Failed {
		t.Fatalf("reclaim at the ceiling must poison-pill: %+v", out)
	}
	tk, _, _ := s.GetTask(ctx, "p1")
	if tk.Status != StatusFailed {
		t.Errorf("status = %q, want failed (terminal)", tk.Status)
	}
	if tk.RetryCount != 2 {
		t.Errorf("retry_count = %d, want 2 (unchanged at poison-pill)", tk.RetryCount)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on poison-pill: %+v", tk)
	}
	evs := taskEvents(t, s, "p1")
	ev, ok := hasEventType(evs, EventTaskFailed)
	if !ok {
		t.Fatalf("expected a task_failed event, got %+v", evs)
	}
	if !ev.Actionable || ev.Actor != ActorSystem || ev.ToStatus == nil || *ev.ToStatus != StatusFailed {
		t.Fatalf("task_failed event wrong: %+v", ev)
	}
	// A poison-pilled task is terminal: a second sweep does nothing more to it.
	if again, _ := s.ReclaimExpiredLeases(ctx); len(again) != 0 {
		t.Errorf("a failed task must not be reclaimed again: %+v", again)
	}
}

// TestReclaimRetryProgression walks a task through its full bounded-retry life: with
// max_retries 2 it is reclaimed exactly twice, then poison-pilled on the third expiry.
func TestReclaimRetryProgression(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	proj, _ := s.UpsertProject(ctx, "/repo/prog", "prog")
	if _, err := s.CreateTask(ctx, Task{ID: "prog", ProjectID: proj.ID, Status: StatusActive, MaxRetries: 2}, ActorManager); err != nil {
		t.Fatal(err)
	}

	redispatch := func() {
		t.Helper()
		// Simulate a re-dispatch of the reclaimed (pending) task: back to active + a new lease.
		if _, err := s.ReportStatus(ctx, "prog", StatusActive, "worker:prog", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantLease(ctx, "prog", "worker:prog"); err != nil {
			t.Fatal(err)
		}
	}

	redispatch() // attempt 1
	for want := 1; want <= 2; want++ {
		clk.advance(DefaultLeaseDuration + time.Minute)
		out, err := s.ReclaimExpiredLeases(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Failed || out[0].RetryCount != want {
			t.Fatalf("reclaim #%d outcome = %+v, want retry_count %d (not failed)", want, out, want)
		}
		if tk, _, _ := s.GetTask(ctx, "prog"); tk.Status != StatusPending {
			t.Fatalf("after reclaim #%d status = %q, want pending", want, tk.Status)
		}
		redispatch()
	}

	// Third expiry: retry_count (2) has reached max_retries (2) ⇒ terminal failed.
	clk.advance(DefaultLeaseDuration + time.Minute)
	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].Failed {
		t.Fatalf("third expiry must poison-pill: %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "prog"); tk.Status != StatusFailed {
		t.Errorf("final status = %q, want failed", tk.Status)
	}
}

// TestReclaimOnlyTargetsActive proves reclaim is scoped to active tasks: a task that went
// needs_input/blocked (a surfaced state the worker may be legitimately waiting in) is not
// reclaimed even if its lease has lapsed.
func TestReclaimOnlyTargetsActive(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkActiveTask(t, s, "w1")
	if err := s.GrantLease(ctx, "w1", "worker:w1"); err != nil {
		t.Fatal(err)
	}
	// Worker asks a question: now needs_input (out of the active scope), lease left as-is.
	if _, err := s.ReportStatus(ctx, "w1", StatusNeedsInput, "worker:w1", "which way?"); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * DefaultLeaseDuration) // lease would be long expired

	out, err := s.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a needs_input task must not be reclaimed: %+v", out)
	}
	if tk, _, _ := s.GetTask(ctx, "w1"); tk.Status != StatusNeedsInput {
		t.Errorf("status = %q, want needs_input (untouched)", tk.Status)
	}
}

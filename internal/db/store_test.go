package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTaskCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)

	proj, err := s.UpsertProject(ctx, "/repo/foo", "foo")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if proj.ID == 0 || proj.RepoPath != "/repo/foo" || proj.DeliveryMode != "pr" || proj.Status != "active" {
		t.Fatalf("unexpected project: %+v", proj)
	}

	clk.advance(time.Second)
	in := Task{
		ID: "auth-refactor", ProjectID: proj.ID, Kind: KindShip, Status: StatusActive,
		Owner: "worker:auth-refactor", Window: "wk-auth-refactor", Worktree: "/wt/auth",
		Harness: "claude", SessionID: "sess-1", Title: "Refactor auth", CreatedBy: "manager",
		Footprint: []string{"internal/cli", "internal/db"},
	}
	created, err := s.CreateTask(ctx, in, "manager")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !reflect.DeepEqual(created.Footprint, []string{"internal/cli", "internal/db"}) {
		t.Errorf("created footprint = %v", created.Footprint)
	}

	got, ok, err := s.GetTask(ctx, "auth-refactor")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Project != "/repo/foo" {
		t.Errorf("Project (joined repo path) = %q, want /repo/foo", got.Project)
	}
	if got.Window != "wk-auth-refactor" || got.Worktree != "/wt/auth" || got.SessionID != "sess-1" {
		t.Errorf("runtime fields not round-tripped: %+v", got)
	}
	if !reflect.DeepEqual(got.Footprint, in.Footprint) {
		t.Errorf("footprint round-trip = %v, want %v", got.Footprint, in.Footprint)
	}
	if got.Status != StatusActive || got.Kind != KindShip || got.CreatedBy != "manager" {
		t.Errorf("lifecycle fields wrong: %+v", got)
	}
	if !got.Created.Equal(clk.t) {
		t.Errorf("Created = %v, want %v", got.Created, clk.t)
	}

	// A 'created' event was recorded, non-actionable.
	evs, err := s.collectEvents(ctx, `SELECT `+eventColumns+` FROM events WHERE entity_type='task' AND entity_id=? ORDER BY id`, "auth-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != EventCreated || evs[0].Actionable {
		t.Errorf("created event wrong: %+v", evs)
	}
}

func TestFootprintNilRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	created, err := s.CreateTask(ctx, Task{ID: "no-fp", ProjectID: proj.ID, Status: StatusPending}, "manager")
	if err != nil {
		t.Fatal(err)
	}
	if created.Footprint != nil {
		t.Errorf("nil footprint should round-trip to nil, got %#v", created.Footprint)
	}
	got, _, _ := s.GetTask(ctx, "no-fp")
	if got.Footprint != nil {
		t.Errorf("nil footprint read back as %#v, want nil", got.Footprint)
	}
}

func TestFKAndCheckRejections(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")

	// FK: epic under a non-existent project.
	if _, err := s.CreateEpic(ctx, 99999, "x", ""); err == nil {
		t.Error("expected FK violation creating epic under missing project")
	}
	// FK: task under a non-existent project.
	if _, err := s.CreateTask(ctx, Task{ID: "fk1", ProjectID: 99999, Status: StatusPending}, "manager"); err == nil {
		t.Error("expected FK violation creating task under missing project")
	}
	// CHECK: bad task status.
	if _, err := s.CreateTask(ctx, Task{ID: "ck1", ProjectID: proj.ID, Status: "bogus"}, "manager"); err == nil {
		t.Error("expected CHECK violation on bad status")
	}
	// CHECK: bad kind.
	if _, err := s.CreateTask(ctx, Task{ID: "ck2", ProjectID: proj.ID, Status: StatusPending, Kind: "bogus"}, "manager"); err == nil {
		t.Error("expected CHECK violation on bad kind")
	}
	// CHECK: bad approved_by.
	if _, err := s.CreateTask(ctx, Task{ID: "ck3", ProjectID: proj.ID, Status: StatusPending, ApprovedBy: "bogus"}, "manager"); err == nil {
		t.Error("expected CHECK violation on bad approved_by")
	}
	// CHECK: bad epic status via SetEntityStatus.
	epic, err := s.CreateEpic(ctx, proj.ID, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntityStatus(ctx, EntityEpic, epic.ID, "nonsense", "manager"); err == nil {
		t.Error("expected CHECK violation on bad epic status")
	}
}

func TestReportStatusActionability(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	mk := func(id string) {
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: proj.ID, Status: StatusActive, Owner: "worker:" + id}, "manager"); err != nil {
			t.Fatal(err)
		}
	}

	mk("w-done")
	e, err := s.ReportStatus(ctx, "w-done", StatusDone, "worker:w-done", "")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Actionable {
		t.Error("worker → done must be actionable")
	}
	if e.FromStatus == nil || *e.FromStatus != StatusActive || e.ToStatus == nil || *e.ToStatus != StatusDone {
		t.Errorf("from/to wrong: from=%v to=%v", e.FromStatus, e.ToStatus)
	}

	mk("w-active")
	if e, _ = s.ReportStatus(ctx, "w-active", StatusActive, "worker:w-active", ""); e.Actionable {
		t.Error("worker → active must NOT be actionable (active is not an actionable status)")
	}

	mk("m-done")
	if e, _ = s.ReportStatus(ctx, "m-done", StatusDone, "manager", ""); e.Actionable {
		t.Error("manager → done must NOT be actionable (non-worker actor)")
	}

	// A message is stored on the event payload AND as a note (one tx, §3.1/§4.3).
	mk("w-blocked")
	e, _ = s.ReportStatus(ctx, "w-blocked", StatusBlocked, "worker:w-blocked", "needs schema decision")
	if !e.Actionable || e.Payload != "needs schema decision" {
		t.Errorf("blocked event wrong: actionable=%v payload=%q", e.Actionable, e.Payload)
	}
	notes, _ := s.collectNotes(ctx, `SELECT `+noteColumns+` FROM notes WHERE task_id=? ORDER BY id`, "w-blocked")
	if len(notes) != 1 || notes[0].Body != "needs schema decision" || notes[0].Author != "worker:w-blocked" {
		t.Errorf("note not written: %+v", notes)
	}

	// last_progress_at was touched.
	got, _, _ := s.GetTask(ctx, "w-blocked")
	if got.LastProgressAt == nil {
		t.Error("last_progress_at should be set after a report")
	}

	// ReportStatus on a missing task errors.
	if _, err := s.ReportStatus(ctx, "ghost", StatusDone, "worker:ghost", ""); err == nil {
		t.Error("expected error reporting on a missing task")
	}
}

func TestEventsSinceAndWatermark(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "t", ProjectID: proj.ID, Status: StatusActive, Owner: "worker:t"}, "manager"); err != nil {
		t.Fatal(err)
	}

	// Worker → done: actionable.
	done, _ := s.ReportStatus(ctx, "t", StatusDone, "worker:t", "")
	// Buggy/mislabeled manager status_changed marked actionable: must be filtered out
	// of the actionable read (§1.3/§4.2) even though it counts toward the raw max.
	badID, _ := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t", Type: EventStatusChanged, Actor: "manager", Actionable: true})
	// Watcher-generated system event: actionable and kept (not a status_changed).
	winGone, _ := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "t", Type: EventWindowGone, Actor: "system", Actionable: true})

	all, _ := s.EventsSince(ctx, 0, false)
	if len(all) < 3 {
		t.Fatalf("EventsSince(0,false) returned %d events, want ≥3", len(all))
	}

	act, _ := s.EventsSince(ctx, 0, true)
	has := map[int64]bool{}
	for _, e := range act {
		has[e.ID] = true
	}
	if !has[done.ID] {
		t.Error("worker done should be in actionable set")
	}
	if !has[winGone] {
		t.Error("window_gone should be in actionable set")
	}
	if has[badID] {
		t.Error("manager-actor status_changed must be filtered from the actionable set (§4.2)")
	}

	// since-watermark: nothing actionable strictly after the last actionable id.
	after, _ := s.EventsSince(ctx, winGone, true)
	if len(after) != 0 {
		t.Errorf("EventsSince(%d,true) = %d, want 0", winGone, len(after))
	}

	// MaxActionableEventID is the literal max over actionable=1 (no actor filter, §2.2).
	max, _ := s.MaxActionableEventID(ctx)
	if max != winGone {
		t.Errorf("MaxActionableEventID = %d, want %d", max, winGone)
	}

	// Empty DB → 0 (COALESCE).
	s2 := newTestStore(t)
	if m, err := s2.MaxActionableEventID(ctx); err != nil || m != 0 {
		t.Errorf("empty MaxActionableEventID = %d err=%v, want 0/nil", m, err)
	}
}

func TestManagerSingleton(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, ok, err := s.GetManager(ctx); err != nil || ok {
		t.Fatalf("expected no manager initially: ok=%v err=%v", ok, err)
	}
	if err := s.SetManager(ctx, Manager{Dir: "/work", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWatermark(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAwaitingLead(ctx, true); err != nil {
		t.Fatal(err)
	}
	// A later SetManager (new session) must NOT clobber watermark/awaiting_lead.
	if err := s.SetManager(ctx, Manager{Dir: "/work2", SessionID: "sess-2"}); err != nil {
		t.Fatal(err)
	}
	m, ok, err := s.GetManager(ctx)
	if err != nil || !ok {
		t.Fatalf("GetManager: ok=%v err=%v", ok, err)
	}
	if m.Dir != "/work2" || m.SessionID != "sess-2" {
		t.Errorf("identity not updated: %+v", m)
	}
	if m.WatchWatermark != 42 {
		t.Errorf("watermark clobbered by SetManager: %d", m.WatchWatermark)
	}
	if !m.AwaitingLead {
		t.Error("awaiting_lead clobbered by SetManager")
	}

	// The CHECK(id=1) makes a second manager row impossible.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO manager (id, updated_at) VALUES (2, '2026-01-01T00:00:00Z')`); err == nil {
		t.Error("expected CHECK(id=1) to reject a second manager row")
	}
}

func TestTimelineMergesEventsAndNotes(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	clk.advance(time.Second)
	if _, err := s.CreateTask(ctx, Task{ID: "t", ProjectID: proj.ID, Status: StatusActive, Owner: "worker:t"}, "manager"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if _, err := s.SetStage(ctx, "t", "implementing", "worker:t"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if _, err := s.ReportStatus(ctx, "t", StatusBlocked, "worker:t", "need a decision"); err != nil {
		t.Fatal(err)
	}

	items, err := s.Timeline(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	// created(event), stage_changed(event), status_changed(event), note — by ts; the
	// status event and its note share a ts, events before notes on a tie.
	if len(items) != 4 {
		t.Fatalf("timeline len = %d, want 4: %+v", len(items), items)
	}
	wantKinds := []string{"event", "event", "event", "note"}
	for i, it := range items {
		if it.Kind != wantKinds[i] {
			t.Errorf("item[%d].Kind = %q, want %q", i, it.Kind, wantKinds[i])
		}
	}
	if items[0].Event.Type != EventCreated || items[1].Event.Type != EventStageChanged || items[2].Event.Type != EventStatusChanged {
		t.Errorf("event ordering wrong: %s,%s,%s", items[0].Event.Type, items[1].Event.Type, items[2].Event.Type)
	}
	if items[3].Note.Body != "need a decision" {
		t.Errorf("note body = %q", items[3].Note.Body)
	}
}

func TestListTasksFilters(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	p1, _ := s.UpsertProject(ctx, "/r1", "r1")
	p2, _ := s.UpsertProject(ctx, "/r2", "r2")
	mk := func(id string, pid int64, status, kind, owner string) {
		clk.advance(time.Second)
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: pid, Status: status, Kind: kind, Owner: owner}, "manager"); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", p1.ID, StatusActive, KindShip, "worker:a")
	mk("b", p1.ID, StatusDone, KindShip, "worker:b")
	mk("c", p2.ID, StatusPending, KindCC, "")

	ids := func(ts []Task) []string {
		out := make([]string, len(ts))
		for i, x := range ts {
			out[i] = x.ID
		}
		return out
	}

	all, _ := s.ListTasks(ctx, TaskFilter{})
	if got := ids(all); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("all (oldest-first) = %v, want [a b c]", got)
	}
	sel, _ := s.ListTasks(ctx, TaskFilter{Status: []string{StatusDone, StatusPending}})
	if got := ids(sel); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("status filter = %v, want [b c]", got)
	}
	pj, _ := s.ListTasks(ctx, TaskFilter{ProjectID: p1.ID})
	if got := ids(pj); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("project filter = %v, want [a b]", got)
	}
	nocc, _ := s.ListTasks(ctx, TaskFilter{ExcludeKind: []string{KindCC}})
	if got := ids(nocc); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("exclude-kind filter = %v, want [a b]", got)
	}
	own, _ := s.ListTasks(ctx, TaskFilter{Owner: "worker:a"})
	if got := ids(own); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("owner filter = %v, want [a]", got)
	}
}

func TestListChildren(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")
	clk.advance(time.Second)
	if _, err := s.CreateTask(ctx, Task{ID: "parent", ProjectID: p.ID, Status: StatusActive}, "manager"); err != nil {
		t.Fatal(err)
	}
	parent := "parent"
	for _, id := range []string{"child1", "child2"} {
		clk.advance(time.Second)
		if _, err := s.CreateTask(ctx, Task{ID: id, ProjectID: p.ID, Status: StatusPending, ParentTaskID: &parent}, "worker:parent"); err != nil {
			t.Fatal(err)
		}
	}
	kids, err := s.ListChildren(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("children = %d, want 2", len(kids))
	}
	for _, k := range kids {
		if k.ParentTaskID == nil || *k.ParentTaskID != "parent" {
			t.Errorf("child %s parent = %v", k.ID, k.ParentTaskID)
		}
	}
}

func TestUpsertTaskInsertThenFieldSync(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")

	clk.advance(time.Second)
	first, err := s.UpsertTask(ctx, Task{ID: "x", ProjectID: p.ID, Status: StatusPending, Title: "backlog"}, "manager")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusPending {
		t.Errorf("insert status = %q, want pending", first.Status)
	}

	countCreated := func() int {
		evs, _ := s.collectEvents(ctx, `SELECT `+eventColumns+` FROM events WHERE entity_type='task' AND entity_id=? ORDER BY id`, "x")
		n := 0
		for _, e := range evs {
			if e.Type == EventCreated {
				n++
			}
		}
		return n
	}
	if countCreated() != 1 {
		t.Fatalf("after insert, created events = %d, want 1", countCreated())
	}

	// Update path: spawn-style field sync. Status is preserved; no PK collision; no
	// second 'created' event.
	clk.advance(time.Second)
	second, err := s.UpsertTask(ctx, Task{ID: "x", ProjectID: p.ID, Status: StatusActive, Window: "wk-x", Worktree: "/wt/x", Owner: "worker:x"}, "manager")
	if err != nil {
		t.Fatalf("UpsertTask update: %v", err)
	}
	if second.Status != StatusPending {
		t.Errorf("update must not change status: got %q", second.Status)
	}
	if second.Window != "wk-x" || second.Worktree != "/wt/x" || second.Owner != "worker:x" {
		t.Errorf("fields not synced: %+v", second)
	}
	if countCreated() != 1 {
		t.Errorf("update must not emit a second created event: %d", countCreated())
	}
}

func TestSetTaskFieldsAndLivenessAndDelivery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "t", ProjectID: p.ID, Status: StatusActive}, "manager"); err != nil {
		t.Fatal(err)
	}

	pr := "https://example/pr/1"
	if err := s.SetTaskFields(ctx, "t", TaskFields{PR: &pr, Footprint: &[]string{"x/y"}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetTask(ctx, "t")
	if got.PR != pr || !reflect.DeepEqual(got.Footprint, []string{"x/y"}) {
		t.Errorf("SetTaskFields: %+v", got)
	}
	// SetTaskFields writes no event.
	evs, _ := s.collectEvents(ctx, `SELECT `+eventColumns+` FROM events WHERE entity_type='task' AND entity_id=? ORDER BY id`, "t")
	if len(evs) != 1 { // only the 'created' event
		t.Errorf("SetTaskFields should not emit an event; events=%d", len(evs))
	}

	if err := s.SetLiveness(ctx, "t", "hash123", 2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = s.GetTask(ctx, "t"); got.LastPaneHash != "hash123" || got.IdleSweeps != 2 {
		t.Errorf("SetLiveness: hash=%q sweeps=%d", got.LastPaneHash, got.IdleSweeps)
	}

	if err := s.RecordDelivery(ctx, "t", Delivery{GatePassed: true, ApprovedBy: "human", ReviewedSHA: "abc", EventType: EventReviewRecorded}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = s.GetTask(ctx, "t"); !got.GatePassed || got.ApprovedBy != "human" || got.ReviewedSHA != "abc" {
		t.Errorf("RecordDelivery: %+v", got)
	}
	// The delivery event is manager-authored ⇒ non-actionable (§1.3).
	act, _ := s.EventsSince(ctx, 0, true)
	if len(act) != 0 {
		t.Errorf("delivery/lifecycle events must be non-actionable; got %d actionable", len(act))
	}
}

func TestSetEntityStatusUnknownKind(t *testing.T) {
	if err := newTestStore(t).SetEntityStatus(context.Background(), EntityKind("bogus"), 1, "done", "manager"); err == nil {
		t.Error("expected error for unknown entity kind")
	}
}

// TestImmediateTxPreventsBusySnapshot uses two separate *sql.DB pools (each its own
// connection to the same file) to exercise cross-connection write contention. With
// _txlock=immediate, the second writer's BEGIN blocks on busy_timeout until the
// first commits, so both read-then-write transactions apply and the counter reaches
// 2. A regression to a deferred BEGIN would let the second read a stale snapshot and
// the first writer would then fail with SQLITE_BUSY_SNAPSHOT (which busy_timeout
// does NOT retry). A single shared MaxOpenConns(1) pool cannot exercise this.
func TestImmediateTxPreventsBusySnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	sA, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()
	sB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	if _, err := sA.db.ExecContext(ctx, `CREATE TABLE ctr (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sA.db.ExecContext(ctx, `INSERT INTO ctr (id, n) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}

	readThenInc := func(s *Store) error {
		tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE via DSN
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT n FROM ctr WHERE id=1`).Scan(&n); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ctr SET n=? WHERE id=1`, n+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	// A begins a read-then-write tx and holds it open after the read.
	txA, err := sA.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var nA int
	if err := txA.QueryRowContext(ctx, `SELECT n FROM ctr WHERE id=1`).Scan(&nA); err != nil {
		t.Fatal(err)
	}

	bDone := make(chan error, 1)
	go func() { bDone <- readThenInc(sB) }()

	// Let B reach (and, under immediate, block in) its BEGIN before A commits.
	time.Sleep(200 * time.Millisecond)

	if _, err := txA.ExecContext(ctx, `UPDATE ctr SET n=? WHERE id=1`, nA+1); err != nil {
		_ = txA.Rollback()
		t.Fatalf("A write failed — regression away from _txlock=immediate (SQLITE_BUSY_SNAPSHOT): %v", err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B read-then-inc failed: %v", err)
	}

	var final int
	if err := sA.db.QueryRowContext(ctx, `SELECT n FROM ctr WHERE id=1`).Scan(&final); err != nil {
		t.Fatal(err)
	}
	if final != 2 {
		t.Fatalf("counter = %d, want 2 (both increments must serialize; a lost update means no real serialization)", final)
	}
}

// TestNestedDBInsideTxDeadlocks proves the §2.3 hazard: with SetMaxOpenConns(1), a
// statement issued against s.db while a withTx transaction holds the only
// connection blocks until the context deadline. The guard is the deadline, so the
// test cannot hang.
func TestNestedDBInsideTxDeadlocks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		_, e := s.db.ExecContext(cctx, `SELECT 1`) // WRONG: uses s.db, not tx
		return e
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded from a nested s.db call, got %v", err)
	}
}

func TestVerify(t *testing.T) {
	s := newTestStore(t)
	p, _ := s.UpsertProject(context.Background(), "/r", "r")
	_, _ = s.CreateTask(context.Background(), Task{ID: "t", ProjectID: p.ID, Status: StatusActive}, "manager")
	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestExportRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, _ := s.UpsertProject(ctx, "/repo/foo", "foo")
	if _, err := s.CreateTask(ctx, Task{ID: "t1", ProjectID: p.ID, Status: StatusActive, Footprint: []string{"a/b"}}, "manager"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.Export(ctx, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	dump := buf.String()
	if !strings.Contains(dump, "CREATE TABLE tasks") {
		t.Error("dump missing tasks schema")
	}
	if !strings.Contains(dump, "'t1'") {
		t.Error("dump missing the task row")
	}

	// Replay the dump into a fresh raw DB and confirm the data restores.
	rdb, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "restored.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if _, err := rdb.ExecContext(ctx, dump); err != nil {
		t.Fatalf("replay dump: %v", err)
	}
	var n int
	if err := rdb.QueryRowContext(ctx, `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("restored tasks count = %d, want 1", n)
	}
	var fp string
	if err := rdb.QueryRowContext(ctx, `SELECT footprint FROM tasks WHERE id='t1'`).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	if fp != `["a/b"]` {
		t.Errorf("restored footprint = %q, want [\"a/b\"]", fp)
	}
}

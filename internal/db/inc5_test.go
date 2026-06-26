package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// taskEvents returns a task's events in id order.
func taskEvents(t *testing.T, s *Store, id string) []Event {
	t.Helper()
	evs, err := s.collectEvents(context.Background(),
		`SELECT `+eventColumns+` FROM events WHERE entity_type='task' AND entity_id=? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func hasEventType(evs []Event, typ string) (Event, bool) {
	for _, e := range evs {
		if e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

// TestRecordTransition_AlwaysNonActionable is the §1.3 invariant at the db layer:
// RecordTransition writes a manager-authored lifecycle event that is NEVER actionable
// — even when the new status is itself an actionable one (blocked/done/needs_input) —
// so a manager action can never self-trigger the watcher. It also proves the status,
// the partial fields, and the typed event commit together (§1.4) with from/to set only
// when the status moved.
func TestRecordTransition_AlwaysNonActionable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "tk", ProjectID: proj.ID, Status: StatusActive, Kind: KindScout}, ActorManager); err != nil {
		t.Fatal(err)
	}

	// Status move → typed event records from/to, non-actionable.
	ev, err := s.RecordTransition(ctx, "tk", StatusDelivered, TaskFields{}, EventDelivered, ActorManager, "merged")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actionable {
		t.Fatal("a manager-authored delivered event must be non-actionable")
	}
	if ev.FromStatus == nil || *ev.FromStatus != StatusActive || ev.ToStatus == nil || *ev.ToStatus != StatusDelivered {
		t.Fatalf("from/to wrong: from=%v to=%v", ev.FromStatus, ev.ToStatus)
	}
	if got, _, _ := s.GetTask(ctx, "tk"); got.Status != StatusDelivered {
		t.Fatalf("status = %q, want delivered", got.Status)
	}

	// Field-only change (no status) → from/to nil, fields updated, non-actionable.
	ship := KindShip
	ev, err = s.RecordTransition(ctx, "tk", "", TaskFields{Kind: &ship}, EventPromoted, ActorManager, "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actionable || ev.FromStatus != nil || ev.ToStatus != nil {
		t.Fatalf("field-only transition: actionable=%v from=%v to=%v", ev.Actionable, ev.FromStatus, ev.ToStatus)
	}
	if got, _, _ := s.GetTask(ctx, "tk"); got.Kind != KindShip || got.Status != StatusDelivered {
		t.Fatalf("kind=%q status=%q, want ship/delivered (status must not change on a field-only transition)", got.Kind, got.Status)
	}

	// Even a transition INTO an actionable status stays non-actionable when it goes
	// through RecordTransition (the manager-authored path), unlike worker ReportStatus.
	ev, err = s.RecordTransition(ctx, "tk", StatusBlocked, TaskFields{}, EventStatusChanged, ActorManager, "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actionable {
		t.Fatal("RecordTransition into blocked must STILL be non-actionable (manager-authored)")
	}

	// Blanking via TaskFields works (the teardown worktree-blank path).
	blank := ""
	if _, err := s.RecordTransition(ctx, "tk", StatusTornDown, TaskFields{Worktree: &blank}, EventTornDown, ActorManager, ""); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetTask(ctx, "tk"); got.Status != StatusTornDown || got.Worktree != "" {
		t.Fatalf("after torn_down: status=%q worktree=%q, want torn_down/empty", got.Status, got.Worktree)
	}

	// None of these manager actions produced an actionable event.
	if actionable, _ := s.EventsSince(ctx, 0, true); len(actionable) != 0 {
		t.Fatalf("RecordTransition must never write an actionable event, got %+v", actionable)
	}

	// A missing task errors (and writes nothing).
	if _, err := s.RecordTransition(ctx, "ghost", StatusDelivered, TaskFields{}, EventDelivered, ActorManager, ""); err == nil {
		t.Fatal("RecordTransition on a missing task must error")
	}
}

// TestCreateFollowOn_Atomic proves the §1.4 fix: the child task and BOTH its 'created'
// and 'follow_on_created' events commit in one transaction (never one without the
// other), both non-actionable, and a failed insert rolls the whole thing back.
func TestCreateFollowOn_Atomic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	proj, _ := s.UpsertProject(ctx, "/r", "r")
	if _, err := s.CreateTask(ctx, Task{ID: "parent", ProjectID: proj.ID, Status: StatusActive}, ActorManager); err != nil {
		t.Fatal(err)
	}

	parentID := "parent"
	actor := "worker:parent"
	child, err := s.CreateFollowOn(ctx, Task{
		ID: "child", ProjectID: proj.ID, ParentTaskID: &parentID, CreatedBy: actor,
		Title: "do the follow-up", Kind: KindShip, Status: StatusPending,
		Footprint: []string{"internal/x"},
	}, actor, "do the follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentTaskID == nil || *child.ParentTaskID != parentID || child.CreatedBy != actor || child.Status != StatusPending {
		t.Fatalf("child fields wrong: %+v", child)
	}

	evs := taskEvents(t, s, "child")
	created, hasCreated := hasEventType(evs, EventCreated)
	fo, hasFO := hasEventType(evs, EventFollowOnCreated)
	if !hasCreated || !hasFO {
		t.Fatalf("expected both created and follow_on_created events, got %+v", evs)
	}
	if created.Actionable || fo.Actionable {
		t.Fatalf("follow-on events must be non-actionable: created=%v follow_on=%v", created.Actionable, fo.Actionable)
	}
	if fo.Payload != "do the follow-up" || fo.Actor != actor {
		t.Fatalf("follow_on_created event wrong: %+v", fo)
	}

	// Atomic rollback: a duplicate id fails and writes NEITHER a row change NOR events.
	before := len(taskEvents(t, s, "child"))
	allBefore, _ := s.EventsSince(ctx, 0, false)
	if _, err := s.CreateFollowOn(ctx, Task{
		ID: "child", ProjectID: proj.ID, ParentTaskID: &parentID, CreatedBy: actor, Status: StatusPending,
	}, actor, "dup"); err == nil {
		t.Fatal("a duplicate follow-on id must fail")
	}
	if after := len(taskEvents(t, s, "child")); after != before {
		t.Fatalf("a failed CreateFollowOn must write no new events for the id (%d → %d)", before, after)
	}
	allAfter, _ := s.EventsSince(ctx, 0, false)
	if len(allAfter) != len(allBefore) {
		t.Fatalf("a failed CreateFollowOn must roll back entirely (events %d → %d)", len(allBefore), len(allAfter))
	}
}

// TestOpen_RestrictivePerms proves the state DB is created with finance-grade
// permissions: a directory we create is 0700 and the db file is 0600 (Part E).
func TestOpen_RestrictivePerms(t *testing.T) {
	// A parent dir that does not yet exist, so Open must create (and tighten) it.
	dir := filepath.Join(t.TempDir(), "ttorch-home")
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir perms = %o, want 700", perm)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state.db perms = %o, want 600", perm)
	}
	// The file is owned at 0600 BEFORE migrations run, so its DDL never lands
	// world-readable; the WAL/SHM sidecars sqlite mints during Open inherit that and
	// must not be group/world-readable either.
	for _, suffix := range []string{"-wal", "-shm"} {
		si, statErr := os.Stat(path + suffix)
		if statErr != nil {
			continue // sidecar may not exist at this instant
		}
		if perm := si.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("state.db%s perms = %o, must not be group/world-accessible", suffix, perm)
		}
	}
}

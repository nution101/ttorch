package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// newWorkerDB creates an isolated SQLite store with one task in the given status,
// points $TTORCH_DB at it, and pins $TTORCH_TASK_ID to that task — simulating a spawned
// worker whose unforgeable identity IS its own task (what the manager sets at spawn,
// §3.1). The worker-facing commands therefore attribute to worker:<taskID> and scope
// their mutation to it. Returns the db path + task id. The package TestMain also pins
// TTORCH_HOME at a temp dir, so the worker commands can never resolve to the real
// ~/.ttorch (the db.Open guard is the final backstop).
func newWorkerDB(t *testing.T, status string) (dbPath, taskID string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	proj, err := store.UpsertProject(context.Background(), filepath.Join(t.TempDir(), "repo"), "")
	if err != nil {
		t.Fatal(err)
	}
	taskID = "wt1"
	if _, err := store.CreateTask(context.Background(), db.Task{
		ID: taskID, ProjectID: proj.ID, Status: status, Owner: "worker:" + taskID,
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTORCH_TASK_ID", taskID)
	return dbPath, taskID
}

// addTask inserts a second task the test's worker does NOT own (its victim), inheriting
// the existing task's project, and returns its id.
func addTask(t *testing.T, dbPath, id, status string) string {
	t.Helper()
	s := reopen(t, dbPath)
	proj := mustTask(t, s, "wt1").ProjectID
	if _, err := s.CreateTask(context.Background(), db.Task{
		ID: id, ProjectID: proj, Status: status, Owner: "worker:" + id,
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	return id
}

func reopen(t *testing.T, dbPath string) *db.Store {
	t.Helper()
	s, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustTask(t *testing.T, s *db.Store, id string) db.Task {
	t.Helper()
	task, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("task %q missing: ok=%v err=%v", id, ok, err)
	}
	return task
}

// TestReport_DoneIsActionable: a worker reporting done sets status=done, touches
// last_progress_at, and writes an ACTIONABLE status_changed event (§1.3).
func TestReport_DoneIsActionable(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	if err := cmdReport([]string{"done", "--task", id}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	task := mustTask(t, s, id)
	if task.Status != db.StatusDone {
		t.Fatalf("status = %q, want done", task.Status)
	}
	if task.LastProgressAt == nil {
		t.Fatal("last_progress_at was not touched")
	}
	actionable, _ := s.EventsSince(context.Background(), 0, true)
	if len(actionable) != 1 || actionable[0].Type != db.EventStatusChanged ||
		actionable[0].Actor != "worker:"+id || actionable[0].ToStatus == nil || *actionable[0].ToStatus != db.StatusDone {
		t.Fatalf("want one actionable worker status_changed→done, got %+v", actionable)
	}
}

// TestReport_ActiveNotActionable: reporting active is a worker transition but active
// is not an actionable status, so it must NOT wake the manager (§1.2/§1.3).
func TestReport_ActiveNotActionable(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusBlocked)
	if err := cmdReport([]string{"active", "--task", id}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	if task := mustTask(t, s, id); task.Status != db.StatusActive {
		t.Fatalf("status = %q, want active", task.Status)
	}
	if actionable, _ := s.EventsSince(context.Background(), 0, true); len(actionable) != 0 {
		t.Fatalf("reporting active must wake no watcher, got %+v", actionable)
	}
}

// TestReport_MessageWritesNoteInSameTx: report -m records the note (author worker:id)
// AND carries the message on the event payload, in one transaction (§3.1).
func TestReport_MessageWritesNoteInSameTx(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	if err := cmdReport([]string{"blocked", "--task", id, "-m", "need credentials"}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	if task := mustTask(t, s, id); task.Status != db.StatusBlocked {
		t.Fatalf("status = %q, want blocked", task.Status)
	}
	items, _ := s.Timeline(context.Background(), id)
	var sawNote, sawEventPayload bool
	for _, it := range items {
		if it.Kind == "note" && it.Note.Body == "need credentials" && it.Note.Author == "worker:"+id {
			sawNote = true
		}
		if it.Kind == "event" && it.Event.Type == db.EventStatusChanged && it.Event.Payload == "need credentials" {
			sawEventPayload = true
		}
	}
	if !sawNote {
		t.Fatal("report -m must write a note authored by the worker")
	}
	if !sawEventPayload {
		t.Fatal("report -m must carry the message on the status_changed event payload")
	}
}

func TestReport_RejectsUnknownStatus(t *testing.T) {
	_, id := newWorkerDB(t, db.StatusActive)
	if err := cmdReport([]string{"finished", "--task", id}); err == nil {
		t.Fatal("an unknown status verb must be rejected")
	}
}

// --- attribution hardening (§3.1): the audit actor is the REAL caller, never --task ---

// TestReport_WorkerOwnTaskNoFlag: a worker reporting WITHOUT --task targets its own task
// (resolved from $TTORCH_TASK_ID) and is attributed to worker:<id>.
func TestReport_WorkerOwnTaskNoFlag(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	if err := cmdReport([]string{"done"}); err != nil { // no --task
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	if task := mustTask(t, s, id); task.Status != db.StatusDone {
		t.Fatalf("status = %q, want done", task.Status)
	}
	actionable, _ := s.EventsSince(context.Background(), 0, true)
	if len(actionable) != 1 || actionable[0].Actor != "worker:"+id || actionable[0].EntityID != id {
		t.Fatalf("want one actionable event by worker:%s on %s, got %+v", id, id, actionable)
	}
}

// TestReport_RejectsForgedAttribution is the core of the hardening: a worker (identity A)
// passing --task B is rejected — B's row is untouched and no event is forged against it
// under either worker's name.
func TestReport_RejectsForgedAttribution(t *testing.T) {
	dbPath, _ := newWorkerDB(t, db.StatusActive) // identity A = wt1, pinned by TTORCH_TASK_ID
	victim := addTask(t, dbPath, "victim", db.StatusActive)
	if err := cmdReport([]string{"done", "--task", victim}); err == nil {
		t.Fatal("a worker reporting another worker's task must be rejected")
	}
	s := reopen(t, dbPath)
	if vt := mustTask(t, s, victim); vt.Status != db.StatusActive {
		t.Fatalf("victim status = %q, want active (must be untouched)", vt.Status)
	}
	all, _ := s.EventsSince(context.Background(), 0, false)
	for _, e := range all {
		if e.EntityID == victim && e.Type == db.EventStatusChanged {
			t.Fatalf("a forged status_changed leaked onto the victim: %+v", e)
		}
		if e.Actor == "worker:"+victim {
			t.Fatalf("an event was forged under the victim's identity: %+v", e)
		}
	}
	if actionable, _ := s.EventsSince(context.Background(), 0, true); len(actionable) != 0 {
		t.Fatalf("a forged report must wake no watcher, got %+v", actionable)
	}
}

// TestReport_ManagerContextActorIsManager: with no worker identity the caller is the
// manager — it may target any task via --task, but the event is attributed to the manager
// and is therefore non-actionable (§1.3), so it cannot be abused to wake the watcher.
func TestReport_ManagerContextActorIsManager(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	t.Setenv("TTORCH_TASK_ID", "") // drop the worker identity → manager context
	t.Chdir(t.TempDir())           // and ensure no .ttorch/task is found up the tree
	if err := cmdReport([]string{"done", "--task", id}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	if task := mustTask(t, s, id); task.Status != db.StatusDone {
		t.Fatalf("status = %q, want done", task.Status)
	}
	var saw bool
	all, _ := s.EventsSince(context.Background(), 0, false)
	for _, e := range all {
		if e.Type == db.EventStatusChanged && e.EntityID == id {
			saw = true
			if e.Actor != db.ActorManager {
				t.Fatalf("manager-context actor = %q, want manager", e.Actor)
			}
			if e.Actionable {
				t.Fatalf("a manager-authored status change must be non-actionable: %+v", e)
			}
		}
	}
	if !saw {
		t.Fatal("the manager's status change was not recorded")
	}
	if actionable, _ := s.EventsSince(context.Background(), 0, true); len(actionable) != 0 {
		t.Fatalf("manager-context report must wake no watcher, got %+v", actionable)
	}
}

// TestStage sets the free-text stage + last_progress_at and writes a NON-actionable
// stage_changed event; flag order (text first or --task first) is handled.
func TestStage(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	if err := cmdStage([]string{"--task", id, "addressing", "review"}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	task := mustTask(t, s, id)
	if task.Stage != "addressing review" {
		t.Fatalf("stage = %q, want %q", task.Stage, "addressing review")
	}
	if task.LastProgressAt == nil {
		t.Fatal("last_progress_at was not touched")
	}
	if actionable, _ := s.EventsSince(context.Background(), 0, true); len(actionable) != 0 {
		t.Fatalf("stage must wake no watcher, got %+v", actionable)
	}
	all, _ := s.EventsSince(context.Background(), 0, false)
	if !hasEvent(all, db.EventStageChanged) {
		t.Fatal("a stage_changed event must be recorded")
	}
}

// TestNote_ReusesResolveSendMessage proves note routes its body through send's safe
// resolver: inline words are joined, and a --message-file's bytes (shell
// metacharacters and all) reach the note verbatim.
func TestNote_Inline(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	if err := cmdNote([]string{"hello", "world", "--task", id}); err != nil {
		t.Fatal(err)
	}
	if body := lastNoteBody(t, reopen(t, dbPath), id); body != "hello world" {
		t.Fatalf("inline note body = %q, want %q", body, "hello world")
	}
}

func TestNote_MessageFilePreservesBytes(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	p := filepath.Join(t.TempDir(), "msg.txt")
	if err := os.WriteFile(p, []byte(nasty+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdNote([]string{"--message-file", p, "--task", id}); err != nil {
		t.Fatal(err)
	}
	if body := lastNoteBody(t, reopen(t, dbPath), id); body != nasty {
		t.Fatalf("message-file note body = %q, want %q", body, nasty)
	}
}

func TestNote_Stdin(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	_, _ = w.WriteString("from stdin\n")
	_ = w.Close()
	if err := cmdNote([]string{"-", "--task", id}); err != nil {
		t.Fatal(err)
	}
	if body := lastNoteBody(t, reopen(t, dbPath), id); body != "from stdin" {
		t.Fatalf("stdin note body = %q, want %q", body, "from stdin")
	}
}

func TestNote_RejectsEmpty(t *testing.T) {
	_, id := newWorkerDB(t, db.StatusActive)
	p := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdNote([]string{"--message-file", p, "--task", id}); err == nil {
		t.Fatal("an empty note body must be rejected")
	}
}

// TestFollowOn creates a pending backlog child of the parent: parent linkage,
// worker authorship, footprint, and BOTH a created and a (non-actionable)
// follow_on_created event (§3.1).
func TestFollowOn(t *testing.T) {
	dbPath, parent := newWorkerDB(t, db.StatusActive)
	if err := cmdFollowOn([]string{"child-1", "--title", "do the thing", "--touches", "internal/cli, internal/db", "--task", parent}); err != nil {
		t.Fatal(err)
	}
	s := reopen(t, dbPath)
	child := mustTask(t, s, "child-1")
	if child.Status != db.StatusPending {
		t.Fatalf("child status = %q, want pending", child.Status)
	}
	if child.ParentTaskID == nil || *child.ParentTaskID != parent {
		t.Fatalf("child parent = %v, want %q", child.ParentTaskID, parent)
	}
	if child.CreatedBy != "worker:"+parent {
		t.Fatalf("child created_by = %q, want worker:%s", child.CreatedBy, parent)
	}
	if child.Kind != db.KindShip {
		t.Fatalf("child kind = %q, want ship", child.Kind)
	}
	if strings.Join(child.Footprint, ",") != "internal/cli,internal/db" {
		t.Fatalf("child footprint = %v, want [internal/cli internal/db]", child.Footprint)
	}
	// The parent's project is inherited.
	if parentTask := mustTask(t, s, parent); child.ProjectID != parentTask.ProjectID {
		t.Fatalf("child project %d != parent project %d", child.ProjectID, parentTask.ProjectID)
	}
	// Both lifecycle events on the child, and neither is actionable.
	all, _ := s.EventsSince(context.Background(), 0, false)
	if !hasEvent(all, db.EventCreated) || !hasEvent(all, db.EventFollowOnCreated) {
		t.Fatalf("want both created and follow_on_created events, got %+v", all)
	}
	if actionable, _ := s.EventsSince(context.Background(), 0, true); len(actionable) != 0 {
		t.Fatalf("a follow-on must wake no watcher, got %+v", actionable)
	}
	if kids, _ := s.ListChildren(context.Background(), parent); len(kids) != 1 || kids[0].ID != "child-1" {
		t.Fatalf("ListChildren(%s) = %+v, want [child-1]", parent, kids)
	}
}

func TestFollowOn_RequiresTitle(t *testing.T) {
	_, parent := newWorkerDB(t, db.StatusActive)
	if err := cmdFollowOn([]string{"c2", "--task", parent}); err == nil {
		t.Fatal("follow-on without --title must be rejected")
	}
}

func TestFollowOn_RejectsDuplicateAndMissingParent(t *testing.T) {
	dbPath, parent := newWorkerDB(t, db.StatusActive)
	if err := cmdFollowOn([]string{"dup", "--title", "x", "--task", parent}); err != nil {
		t.Fatal(err)
	}
	if err := cmdFollowOn([]string{"dup", "--title", "x", "--task", parent}); err == nil {
		t.Fatal("a duplicate follow-on id must be rejected")
	}
	t.Setenv("TTORCH_DB", dbPath)
	// The missing-parent check lives past the identity gate, which a worker can only
	// reach for its OWN task — so exercise it from the manager context (no
	// TTORCH_TASK_ID), where --task may name any (here non-existent) parent.
	t.Setenv("TTORCH_TASK_ID", "")
	if err := cmdFollowOn([]string{"orphan", "--title", "x", "--task", "no-such-parent"}); err == nil {
		t.Fatal("a follow-on for a missing parent must be rejected")
	}
}

// --- resolution + attribution: who is calling, which task they may write (§3.1) ---

// TestResolveWorkerAuth_WorkerOwnTask: a worker's identity comes from $TTORCH_TASK_ID;
// it may omit --task (defaults to its own id) or pass its own id, and is attributed to
// worker:<id> either way.
func TestResolveWorkerAuth_WorkerOwnTask(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "A")
	t.Setenv("TTORCH_DB", "/env/db")
	for _, flag := range []string{"", "A"} {
		id, dbp, actor, err := resolveWorkerAuth(flag)
		if err != nil || id != "A" || actor != "worker:A" || dbp != "/env/db" {
			t.Fatalf("flag=%q: got (%q,%q,%q,%v), want (A,/env/db,worker:A,nil)", flag, id, dbp, actor, err)
		}
	}
}

// TestResolveWorkerAuth_RejectsForeignTask: the forgery the hardening closes — a worker
// (identity A) may not target another task B via --task, and the actor is never derived
// from --task.
func TestResolveWorkerAuth_RejectsForeignTask(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "A")
	t.Setenv("TTORCH_DB", "/env/db")
	if _, _, _, err := resolveWorkerAuth("B"); err == nil {
		t.Fatal("a worker targeting another task via --task must be rejected")
	}
}

// TestResolveWorkerAuth_FileIdentity: with no env, the identity (and thus the actor) come
// from the worktree's .ttorch/task found by walking up from cwd — never from --task — and
// a file-identified worker still cannot forge a foreign target.
func TestResolveWorkerAuth_FileIdentity(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "")
	root := t.TempDir()
	writeTaskFile(t, root, "fileid", "/file/db")
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	id, dbp, actor, err := resolveWorkerAuth("")
	if err != nil || id != "fileid" || actor != "worker:fileid" || dbp != "/file/db" {
		t.Fatalf("got (%q,%q,%q,%v), want (fileid,/file/db,worker:fileid,nil)", id, dbp, actor, err)
	}
	if _, _, _, err := resolveWorkerAuth("other"); err == nil {
		t.Fatal("a file-identified worker must not target another task")
	}
}

// TestResolveWorkerAuth_EnvBeatsFileForIdentity: $TTORCH_TASK_ID outranks the file for
// the identity; with TTORCH_DB unset the file's db is still the fallback.
func TestResolveWorkerAuth_EnvBeatsFileForIdentity(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "envid")
	t.Setenv("TTORCH_DB", "")
	dir := t.TempDir()
	writeTaskFile(t, dir, "fileid", "/file/db")
	t.Chdir(dir)
	id, dbp, actor, err := resolveWorkerAuth("")
	if err != nil || id != "envid" || actor != "worker:envid" || dbp != "/file/db" {
		t.Fatalf("got (%q,%q,%q,%v), want (envid,/file/db,worker:envid,nil)", id, dbp, actor, err)
	}
}

// TestResolveWorkerAuth_DBEnvBeatsFile: $TTORCH_DB outranks the file's recorded db.
func TestResolveWorkerAuth_DBEnvBeatsFile(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "/env/db")
	dir := t.TempDir()
	writeTaskFile(t, dir, "fileid", "/file/db")
	t.Chdir(dir)
	_, dbp, _, err := resolveWorkerAuth("")
	if err != nil || dbp != "/env/db" {
		t.Fatalf("db got (%q,%v), want (/env/db,nil)", dbp, err)
	}
}

// TestResolveWorkerAuth_ManagerContext: with no worker identity (no env, no file) the
// caller is the manager/lead — it may target any task via --task and is attributed to the
// manager; without --task there is nothing to resolve.
func TestResolveWorkerAuth_ManagerContext(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "/env/db")
	t.Chdir(t.TempDir()) // no .ttorch/task anywhere up the tree
	id, _, actor, err := resolveWorkerAuth("anytask")
	if err != nil || id != "anytask" || actor != db.ActorManager {
		t.Fatalf("got (%q,%q,%v), want (anytask,manager,nil)", id, actor, err)
	}
	if _, _, _, err := resolveWorkerAuth(""); err == nil {
		t.Fatal("the manager context with no --task must be unresolvable")
	}
}

func TestExtractTaskFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFlag string
		wantRest []string
		wantErr  bool
	}{
		{"separate", []string{"a", "--task", "x", "b"}, "x", []string{"a", "b"}, false},
		{"equals", []string{"--task=y", "z"}, "y", []string{"z"}, false},
		{"none", []string{"plain", "text"}, "", []string{"plain", "text"}, false},
		{"missing value", []string{"--task"}, "", nil, true},
	}
	for _, c := range cases {
		gotFlag, gotRest, err := extractTaskFlag(c.args)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
		if c.wantErr {
			continue
		}
		if gotFlag != c.wantFlag || strings.Join(gotRest, ",") != strings.Join(c.wantRest, ",") {
			t.Fatalf("%s: got (%q,%v), want (%q,%v)", c.name, gotFlag, gotRest, c.wantFlag, c.wantRest)
		}
	}
}

func TestReportStatusValue(t *testing.T) {
	cases := map[string]string{
		"done": db.StatusDone, "blocked": db.StatusBlocked,
		"needs-input": db.StatusNeedsInput, "active": db.StatusActive,
	}
	for verb, want := range cases {
		if got, ok := reportStatusValue(verb); !ok || got != want {
			t.Fatalf("reportStatusValue(%q) = (%q,%v), want (%q,true)", verb, got, ok, want)
		}
	}
	if _, ok := reportStatusValue("needs_input"); ok {
		t.Fatal("the enum form needs_input is not a CLI verb")
	}
}

func writeTaskFile(t *testing.T, dir, id, dbPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".ttorch"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "task_id=" + id + "\ndb=" + dbPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".ttorch", "task"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasEvent(evs []db.Event, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func lastNoteBody(t *testing.T, s *db.Store, taskID string) string {
	t.Helper()
	items, err := s.Timeline(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, it := range items {
		if it.Kind == "note" {
			body = it.Note.Body
		}
	}
	return body
}

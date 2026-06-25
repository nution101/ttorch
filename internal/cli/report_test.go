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
// points $TTORCH_DB at it, and returns the db path + task id. The package TestMain
// also pins TTORCH_HOME at a temp dir, so the worker commands can never resolve to
// the real ~/.ttorch (the db.Open guard is the final backstop).
func newWorkerDB(t *testing.T, status string) (dbPath, taskID string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	t.Setenv("TTORCH_TASK_ID", "")
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
	return dbPath, taskID
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
	if err := cmdFollowOn([]string{"orphan", "--title", "x", "--task", "no-such-parent"}); err == nil {
		t.Fatal("a follow-on for a missing parent must be rejected")
	}
}

// --- resolution: task id + DB discovery (§3.1) ---

func TestResolveWorkerTarget_FlagBeatsEnv(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "envid")
	t.Setenv("TTORCH_DB", "/env/db")
	id, dbp, err := resolveWorkerTarget("flagid")
	if err != nil || id != "flagid" || dbp != "/env/db" {
		t.Fatalf("got (%q,%q,%v), want (flagid,/env/db,nil)", id, dbp, err)
	}
}

func TestResolveWorkerTarget_EnvBeatsFile(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "envid")
	t.Setenv("TTORCH_DB", "")
	dir := t.TempDir()
	writeTaskFile(t, dir, "fileid", "/file/db")
	t.Chdir(dir)
	id, dbp, err := resolveWorkerTarget("")
	// env supplies the id; with TTORCH_DB unset the file's db is the fallback.
	if err != nil || id != "envid" || dbp != "/file/db" {
		t.Fatalf("got (%q,%q,%v), want (envid,/file/db,nil)", id, dbp, err)
	}
}

func TestResolveWorkerTarget_CwdWalkUp(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "")
	root := t.TempDir()
	writeTaskFile(t, root, "fileid", "/file/db")
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	id, dbp, err := resolveWorkerTarget("")
	if err != nil || id != "fileid" || dbp != "/file/db" {
		t.Fatalf("walk-up got (%q,%q,%v), want (fileid,/file/db,nil)", id, dbp, err)
	}
}

func TestResolveWorkerTarget_DBEnvBeatsFile(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "/env/db")
	dir := t.TempDir()
	writeTaskFile(t, dir, "fileid", "/file/db")
	t.Chdir(dir)
	_, dbp, err := resolveWorkerTarget("")
	if err != nil || dbp != "/env/db" {
		t.Fatalf("db got (%q,%v), want (/env/db,nil)", dbp, err)
	}
}

func TestResolveWorkerTarget_Unresolvable(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "")
	t.Chdir(t.TempDir()) // no .ttorch/task anywhere up the tree
	if _, _, err := resolveWorkerTarget(""); err == nil {
		t.Fatal("with no flag, env, or file the task must be unresolvable")
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

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

// hookWorker isolates the ttorch home and sets the caller's worker identity to id. It
// returns the path the hook record for id lands at.
func hookWorker(t *testing.T, id string) string {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TASK_ID", id)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // no .ttorch/task up the tree
	return paths.Default().HookRecordFile(id)
}

// withStdin points os.Stdin at a file holding payload for the rest of the test, so Main's
// dispatch reads it instead of the test binary's own stdin.
func withStdin(t *testing.T, payload string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(f, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// TestHook_DispatchedFromMain: `ttorch hook turn-started` is a command, exits 0, and records
// the event for the calling worker.
func TestHook_DispatchedFromMain(t *testing.T) {
	path := hookWorker(t, "wt1")
	withStdin(t, `{"hook_event_name":"UserPromptSubmit","prompt":"go"}`)
	if code := Main([]string{"hook", "turn-started"}); code != 0 {
		t.Fatalf("ttorch hook turn-started exited %d, want 0", code)
	}
	if r, ok := livestate.ReadRecord(path, "wt1"); !ok || r.Event != livestate.TurnStarted {
		t.Fatalf("record after ttorch hook = (%+v, %v), want turn-started for wt1", r, ok)
	}
}

// TestHook_RecordsEachEvent: each event replaces the record with itself, stamped with the
// time it was recorded, and the payload on stdin is drained.
func TestHook_RecordsEachEvent(t *testing.T) {
	path := hookWorker(t, "wt1")
	for _, ev := range livestate.Events {
		payload := strings.NewReader(`{"session_id":"s","prompt":"` + strings.Repeat("x", 200_000) + `"}`)
		before := time.Now()
		if err := cmdHook([]string{string(ev)}, payload); err != nil {
			t.Fatalf("hook %s: %v", ev, err)
		}
		after := time.Now()
		r, ok := livestate.ReadRecord(path, "wt1")
		if !ok || r.Event != ev || r.TaskID != "wt1" {
			t.Fatalf("after hook %s the record is (%+v, %v)", ev, r, ok)
		}
		if r.At.Before(before.Add(-time.Second)) || r.At.After(after.Add(time.Second)) {
			t.Errorf("hook %s stamped %v, want between %v and %v", ev, r.At, before, after)
		}
		if payload.Len() != 0 {
			t.Errorf("hook %s left %d payload bytes unread", ev, payload.Len())
		}
	}
}

// TestHook_UsageErrorsNeverExitTwo: a missing or unknown event is an error, records nothing,
// and exits 1. Claude Code reads exit 2 from a UserPromptSubmit or Stop hook as a block, so
// the hook verb must never produce it.
func TestHook_UsageErrorsNeverExitTwo(t *testing.T) {
	path := hookWorker(t, "wt1")
	for _, args := range [][]string{nil, {"turn-paused"}, {"Stop"}, {"turn-started", "extra"}} {
		if err := cmdHook(args, strings.NewReader("")); err == nil {
			t.Errorf("cmdHook(%q) = nil, want a usage error", args)
		}
		withStdin(t, "")
		if code := Main(append([]string{"hook"}, args...)); code != 1 {
			t.Errorf("ttorch hook %q exited %d, want 1", args, code)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a usage error must record nothing (stat err = %v)", err)
	}
}

// TestHook_NotAWorkerRecordsNothing: with no worker identity (the manager, `ttorch cc`, a
// user's own session) the hook succeeds and writes nothing.
func TestHook_NotAWorkerRecordsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	t.Setenv("TTORCH_TASK_ID", "")
	t.Chdir(t.TempDir())
	if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
		t.Fatalf("a non-worker hook should succeed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
		t.Fatalf("a non-worker hook must write nothing (stat err = %v)", err)
	}
}

// TestHook_ResolvesIdentityFromTaskFile: a resumed worker has lost its launch env; the hook
// finds its task from the worktree's .ttorch/task, as `ttorch report` does.
func TestHook_ResolvesIdentityFromTaskFile(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TASK_ID", "")
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".ttorch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "task"), []byte("task_id=from-file\ndb=/nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if err := cmdHook([]string{"turn-ended"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if r, ok := livestate.ReadRecord(paths.Default().HookRecordFile("from-file"), "from-file"); !ok || r.Event != livestate.TurnEnded {
		t.Fatalf("record for the task-file identity = (%+v, %v), want turn-ended", r, ok)
	}
}

// TestHook_UnsafeTaskIDRecordsNothing: a task id that is not a single path element never
// names a place to write, inside or outside the state directory.
func TestHook_UnsafeTaskIDRecordsNothing(t *testing.T) {
	for _, id := range []string{"..", ".", "../../escape", "a/b", `a\b`} {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("TTORCH_HOME", filepath.Join(home, "ttorch"))
			t.Setenv("TTORCH_TASK_ID", id)
			t.Chdir(t.TempDir())
			if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
				t.Fatalf("an unsafe id should fail open, got %v", err)
			}
			var written []string
			_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					written = append(written, p)
				}
				return nil
			})
			if len(written) != 0 {
				t.Fatalf("task id %q wrote %v", id, written)
			}
		})
	}
}

// TestHook_WriteFailureFailsOpen: when the record cannot be written the hook still succeeds,
// so a broken state directory never disturbs the worker.
func TestHook_WriteFailureFailsOpen(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, []byte("a file where the ttorch home should be"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTORCH_HOME", home)
	t.Setenv("TTORCH_TASK_ID", "wt1")
	t.Chdir(t.TempDir())
	if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
		t.Fatalf("a failed write should fail open, got %v", err)
	}
}

// workerWorktree makes a directory holding .ttorch/task for id, as spawn writes it.
func workerWorktree(t *testing.T, id string) string {
	t.Helper()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".ttorch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "task"), []byte("task_id="+id+"\ndb=/nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestHook_ReviewWorkspaceRecordsNothing: a reviewer session runs from a review workspace
// with the same worker settings, so it carries the lifecycle hooks. It has no task of its
// own, and must never write the record of the worker it reviews: not if it inherits that
// worker's TTORCH_TASK_ID, and not if it cds into the worker's worktree and so finds its
// .ttorch/task. Claude Code's CLAUDE_PROJECT_DIR stays where the session started when it
// cds, so it is what the hook checks; the process's cwd is the fallback when it is unset.
func TestHook_ReviewWorkspaceRecordsNothing(t *testing.T) {
	cases := []struct {
		name string
		// envTask is TTORCH_TASK_ID; projectInReview puts CLAUDE_PROJECT_DIR in the review
		// workspace; cwd is "review", "worktree" (the worker's, with its .ttorch/task) or "".
		envTask         string
		projectInReview bool
		cwd             string
	}{
		{"inherited task id, project dir in the review workspace", "wt1", true, ""},
		{"cd into the worker's worktree, project dir in the review workspace", "", true, "worktree"},
		{"inherited task id, no project dir, cwd in the review workspace", "wt1", false, "review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := hookWorker(t, "wt1")
			review := filepath.Join(paths.Default().ReviewWorkspaceDir("wt1"), "correctness")
			if err := os.MkdirAll(review, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TTORCH_TASK_ID", c.envTask)
			if c.projectInReview {
				t.Setenv("CLAUDE_PROJECT_DIR", review)
			}
			switch c.cwd {
			case "review":
				t.Chdir(review)
			case "worktree":
				t.Chdir(workerWorktree(t, "wt1"))
			}
			if err := cmdHook([]string{"turn-ended"}, strings.NewReader("")); err != nil {
				t.Fatalf("a hook in a review workspace should succeed, got %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a hook in a review workspace wrote the worker's record (stat err = %v)", err)
			}
		})
	}
}

// TestHook_OutsideReviewWorkspaceStillRecords: the refusal is scoped to the review workspaces.
// A worker whose project dir is its own worktree records, and so does one in a directory whose
// name only starts with the review workspaces' name.
func TestHook_OutsideReviewWorkspaceStillRecords(t *testing.T) {
	for _, name := range []string{"worktree", "lookalike"} {
		t.Run(name, func(t *testing.T) {
			path := hookWorker(t, "wt1")
			dir := workerWorktree(t, "wt1")
			if name == "lookalike" {
				// A sibling of the review workspaces root: <home>/review-workspaces-old.
				dir = filepath.Dir(paths.Default().ReviewWorkspaceDir("x")) + "-old"
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CLAUDE_PROJECT_DIR", dir)
			t.Chdir(dir)
			if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if r, ok := livestate.ReadRecord(path, "wt1"); !ok || r.Event != livestate.TurnStarted {
				t.Fatalf("project dir %s: record = (%+v, %v), want turn-started", dir, r, ok)
			}
		})
	}
}

// TestHook_ReviewWorkspaceThroughASymlinkedHome: the ttorch home is reached through a symlink
// and the session's project dir is given as the real path, as a harness that resolves its cwd
// would report it. The two spellings still name a review workspace, so nothing is recorded.
func TestHook_ReviewWorkspaceThroughASymlinkedHome(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTORCH_HOME", link)
	t.Setenv("TTORCH_TASK_ID", "wt1")
	t.Chdir(t.TempDir())
	rel, err := filepath.Rel(link, filepath.Join(paths.Default().ReviewWorkspaceDir("wt1"), "correctness"))
	if err != nil {
		t.Fatal(err)
	}
	review := filepath.Join(real, rel)
	if err := os.MkdirAll(review, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", review)
	if err := cmdHook([]string{"turn-ended"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Default().HookRecordFile("wt1")); !os.IsNotExist(err) {
		t.Fatalf("a hook in a review workspace reached through a symlink wrote the record (stat err = %v)", err)
	}
}

// assertNoHookRecord fails when any hook record exists for the given tasks.
func assertNoHookRecord(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := os.Stat(paths.Default().HookRecordFile(id)); !os.IsNotExist(err) {
			t.Errorf("a hook record for %s was written (stat err = %v)", id, err)
		}
	}
}

// TestHook_DisagreeingIdentityRecordsNothing: when the sources of a hook's identity name
// different tasks, the hook writes no record for either and still succeeds. A subprocess
// that inherited one worker's TTORCH_TASK_ID and runs in another task's worktree must not
// write the first worker's record (holding its stall ladder quiet), nor the second's.
func TestHook_DisagreeingIdentityRecordsNothing(t *testing.T) {
	cases := []struct {
		name string
		// env is TTORCH_TASK_ID; project and cwd name the task whose worktree
		// CLAUDE_PROJECT_DIR and the cwd lie in ("" for a directory with no .ttorch/task).
		env, project, cwd string
	}{
		{"env and the cwd's task file disagree", "wt1", "", "wt2"},
		{"env and the project dir's task file disagree", "wt1", "wt2", ""},
		{"env agrees with the project dir, the cwd is in another worktree", "wt1", "wt1", "wt2"},
		{"no env, the project dir and the cwd are in different worktrees", "", "wt1", "wt2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hookWorker(t, "wt1")
			t.Setenv("TTORCH_TASK_ID", c.env)
			if c.project != "" {
				t.Setenv("CLAUDE_PROJECT_DIR", workerWorktree(t, c.project))
			}
			if c.cwd != "" {
				t.Chdir(workerWorktree(t, c.cwd))
			}
			for _, ev := range livestate.Events {
				if err := cmdHook([]string{string(ev)}, strings.NewReader("")); err != nil {
					t.Fatalf("hook %s with disagreeing identities should succeed, got %v", ev, err)
				}
			}
			withStdin(t, "")
			if code := Main([]string{"hook", "turn-started"}); code != 0 {
				t.Fatalf("ttorch hook turn-started exited %d, want 0", code)
			}
			assertNoHookRecord(t, "wt1", "wt2")
		})
	}
}

// TestHook_AgreeingIdentityRecords: sources that all name the same task, or that leave some
// unresolved, still record for it. That includes a resumed worker that lost its env and cd'd
// out of its worktree: the project dir still finds its task file.
func TestHook_AgreeingIdentityRecords(t *testing.T) {
	cases := []struct {
		name              string
		env, project, cwd string
	}{
		{"env and the cwd's task file agree", "wt1", "", "wt1"},
		{"env, project dir and cwd agree", "wt1", "wt1", "wt1"},
		{"env and the project dir agree, cwd outside any worktree", "wt1", "wt1", ""},
		{"no env, project dir only", "", "wt1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := hookWorker(t, "wt1")
			t.Setenv("TTORCH_TASK_ID", c.env)
			if c.project != "" {
				t.Setenv("CLAUDE_PROJECT_DIR", workerWorktree(t, c.project))
			}
			if c.cwd != "" {
				t.Chdir(workerWorktree(t, c.cwd))
			}
			if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if r, ok := livestate.ReadRecord(path, "wt1"); !ok || r.Event != livestate.TurnStarted {
				t.Fatalf("record = (%+v, %v), want turn-started for wt1", r, ok)
			}
		})
	}
}

// seedHookDB creates a state DB at path holding a spawned task for each id.
func seedHookDB(t *testing.T, path string, ids ...string) {
	t.Helper()
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p, err := s.UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := s.CreateTask(ctx, db.Task{ID: id, ProjectID: p.ID, Status: db.StatusActive, Window: "wk-" + id, Owner: "worker:" + id}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
	}
}

// hookTraces returns the hook_turn_started events recorded for id in the DB at path.
func hookTraces(t *testing.T, path, id string) []db.Event {
	t.Helper()
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Event
	for _, it := range items {
		if it.Event != nil && it.Event.Type == db.EventHookTurnStarted {
			out = append(out, *it.Event)
		}
	}
	return out
}

// TestHook_TurnStartedLeavesOneTracePerMinute: the first turn-started record of a task puts
// a non-actionable hook_turn_started event in its timeline naming the writer, a second
// turn-started inside the minute adds none, and the other events never add one.
func TestHook_TurnStartedLeavesOneTracePerMinute(t *testing.T) {
	hookWorker(t, "wt1")
	// A spawned worker carries TTORCH_DB, which wins over the task file's db (here /nowhere).
	dbPath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	seedHookDB(t, dbPath, "wt1")
	project := workerWorktree(t, "wt1")
	t.Setenv("CLAUDE_PROJECT_DIR", project)

	hook := func(evs ...livestate.Event) {
		t.Helper()
		for _, ev := range evs {
			if err := cmdHook([]string{string(ev)}, strings.NewReader("")); err != nil {
				t.Fatalf("hook %s: %v", ev, err)
			}
		}
	}
	hook(livestate.TurnEnded, livestate.SessionEnded)
	if n := len(hookTraces(t, dbPath, "wt1")); n != 0 {
		t.Fatalf("turn-ended and session-ended left %d traces, want none", n)
	}
	hook(livestate.TurnStarted, livestate.TurnEnded, livestate.TurnStarted, livestate.SessionEnded)
	traces := hookTraces(t, dbPath, "wt1")
	if len(traces) != 1 {
		t.Fatalf("got %d hook_turn_started events, want 1: %+v", len(traces), traces)
	}
	e := traces[0]
	if e.Actionable || e.Actor != db.ActorSystem {
		t.Errorf("trace %+v: want a non-actionable system event", e)
	}
	want := fmt.Sprintf("via=env dir=%q ppid=%d", project, os.Getppid())
	if e.Payload != want {
		t.Errorf("trace payload = %q, want %q", e.Payload, want)
	}
}

// TestHook_TraceUsesTheTaskFileDB: with no TTORCH_DB, the trace lands in the DB the
// worktree's .ttorch/task names, as `ttorch report` resolves it, and says the task came
// from the task file.
func TestHook_TraceUsesTheTaskFileDB(t *testing.T) {
	hookWorker(t, "wt1")
	t.Setenv("TTORCH_TASK_ID", "")
	dbPath := filepath.Join(t.TempDir(), "elsewhere.db")
	seedHookDB(t, dbPath, "wt1")
	wt := workerWorktree(t, "wt1")
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "task"), []byte("task_id=wt1\ndb="+dbPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	traces := hookTraces(t, dbPath, "wt1")
	if len(traces) != 1 || !strings.HasPrefix(traces[0].Payload, "via=task-file ") {
		t.Fatalf("traces in the task file's DB = %+v, want one via=task-file", traces)
	}
}

// TestHook_RefusedWriteLeavesNoTrace: a hook whose identity sources disagree writes no
// record, so it leaves no trace for either task.
func TestHook_RefusedWriteLeavesNoTrace(t *testing.T) {
	hookWorker(t, "wt1")
	dbPath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	seedHookDB(t, dbPath, "wt1", "wt2")
	t.Chdir(workerWorktree(t, "wt2"))
	if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"wt1", "wt2"} {
		if n := len(hookTraces(t, dbPath, id)); n != 0 {
			t.Errorf("refused hook left %d traces for %s", n, id)
		}
	}
}

// TestHook_RefusedHookNeverOpensADisagreeingTaskFilesDB: a .ttorch/task that disagrees with
// the other identity sources may have been planted, so the db= path it names is never
// opened. Opening it would chmod the file to 0600 and migrate ttorch tables into it. The
// victim is left byte-for-byte and mode-for-mode as it was, for every hook event, whether
// it is a plain file or an existing SQLite database, and whether the disagreeing file is
// found from the cwd or from CLAUDE_PROJECT_DIR.
func TestHook_RefusedHookNeverOpensADisagreeingTaskFilesDB(t *testing.T) {
	for _, victimKind := range []string{"plain file", "sqlite db"} {
		for _, where := range []string{"cwd", "project dir"} {
			t.Run(victimKind+" via "+where, func(t *testing.T) {
				hookWorker(t, "wt1") // TTORCH_TASK_ID=wt1, no TTORCH_DB
				victim := filepath.Join(t.TempDir(), "victim")
				if victimKind == "sqlite db" {
					seedHookDB(t, victim, "wt1", "wt2")
				} else if err := os.WriteFile(victim, []byte("not a database\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(victim, 0o644); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(victim)
				if err != nil {
					t.Fatal(err)
				}
				planted := workerWorktree(t, "wt2")
				if err := os.WriteFile(filepath.Join(planted, ".ttorch", "task"), []byte("task_id=wt2\ndb="+victim+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if where == "cwd" {
					t.Chdir(planted)
				} else {
					t.Setenv("CLAUDE_PROJECT_DIR", planted)
				}
				for _, ev := range livestate.Events {
					if err := cmdHook([]string{string(ev)}, strings.NewReader("")); err != nil {
						t.Fatalf("hook %s: %v", ev, err)
					}
				}
				fi, err := os.Stat(victim)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0o644 {
					t.Errorf("victim mode = %v, want 0644: the hook opened the disagreeing task file's db", fi.Mode().Perm())
				}
				after, err := os.ReadFile(victim)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Errorf("victim contents changed (%d bytes to %d): the hook wrote to the disagreeing task file's db", len(before), len(after))
				}
				for _, side := range []string{"-wal", "-shm"} {
					if _, err := os.Stat(victim + side); !os.IsNotExist(err) {
						t.Errorf("victim%s exists (stat err = %v): the hook opened the disagreeing task file's db", side, err)
					}
				}
			})
		}
	}
}

// TestHook_TraceNeverCreatesADB: a worker whose state DB does not exist still records its
// turn, and the hook does not create a DB to hold the trace.
func TestHook_TraceNeverCreatesADB(t *testing.T) {
	path := hookWorker(t, "wt1")
	if err := cmdHook([]string{"turn-started"}, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if r, ok := livestate.ReadRecord(path, "wt1"); !ok || r.Event != livestate.TurnStarted {
		t.Fatalf("record = (%+v, %v), want turn-started", r, ok)
	}
	if _, err := os.Stat(paths.Default().StateDB()); !os.IsNotExist(err) {
		t.Fatalf("the hook created a state DB (stat err = %v)", err)
	}
}

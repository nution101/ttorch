package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

// hookWorker isolates the ttorch home and sets the caller's worker identity to id. It
// returns the path the hook record for id lands at.
func hookWorker(t *testing.T, id string) string {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TASK_ID", id)
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

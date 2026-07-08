package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestStopHook_BlocksWhenActiveUnreported: a worker whose task is still `active` (it never
// reported a terminal/blocking status) is blocked from going idle and told to report.
func TestStopHook_BlocksWhenActiveUnreported(t *testing.T) {
	newWorkerDB(t, db.StatusActive)
	var out bytes.Buffer
	if err := cmdStopHook(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `"decision":"block"`) {
		t.Fatalf("active+unreported worker should be blocked, got: %q", s)
	}
	if !strings.Contains(s, "ttorch report done") {
		t.Fatalf("block reason should name the report commands, got: %q", s)
	}
}

// TestStopHook_AllowsWhenReported: once the worker has reported (status left `active`), the
// hook stands down — the work is visible to the advance machinery.
func TestStopHook_AllowsWhenReported(t *testing.T) {
	for _, status := range []string{db.StatusDone, db.StatusBlocked, db.StatusNeedsInput} {
		t.Run(status, func(t *testing.T) {
			newWorkerDB(t, status)
			var out bytes.Buffer
			if err := cmdStopHook(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("status %q should allow the stop (no output), got: %q", status, out.String())
			}
		})
	}
}

// TestStopHook_NoLoopWhenAlreadyActive: stop_hook_active=true means this stop already follows
// a prior block; re-blocking would loop, so the hook stands down even for an active task.
func TestStopHook_NoLoopWhenAlreadyActive(t *testing.T) {
	newWorkerDB(t, db.StatusActive)
	var out bytes.Buffer
	if err := cmdStopHook(strings.NewReader(`{"stop_hook_active":true}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("stop_hook_active must prevent a re-block, got: %q", out.String())
	}
}

// TestStopHook_OptOut: TTORCH_NO_STOP_REPORT disables the hook entirely.
func TestStopHook_OptOut(t *testing.T) {
	newWorkerDB(t, db.StatusActive)
	t.Setenv("TTORCH_NO_STOP_REPORT", "1")
	var out bytes.Buffer
	if err := cmdStopHook(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("opt-out must make the hook a no-op, got: %q", out.String())
	}
}

// TestStopHook_FailOpenNotAWorker: with no worker identity (manager/user session — which never
// gets the hook anyway, but belt-and-suspenders), resolution fails and the stop is allowed.
func TestStopHook_FailOpenNotAWorker(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Setenv("TTORCH_DB", "")
	t.Chdir(t.TempDir()) // no .ttorch/task up the tree
	var out bytes.Buffer
	if err := cmdStopHook(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a non-worker context must allow the stop, got: %q", out.String())
	}
}

// Guard: the block fires for the CANONICAL active status only after a real transition back to
// active (report active) too — a worker that heartbeats active is still unreported-terminal.
func TestStopHook_ActiveAfterHeartbeatStillBlocks(t *testing.T) {
	dbPath, id := newWorkerDB(t, db.StatusActive)
	s := reopen(t, dbPath)
	if _, err := s.ReportStatus(context.Background(), id, db.StatusActive, "worker:"+id, ""); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdStopHook(strings.NewReader(`{}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"decision":"block"`) {
		t.Fatalf("a heartbeat-active worker is still unreported and must block, got: %q", out.String())
	}
}

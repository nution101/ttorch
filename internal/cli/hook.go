package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

const hookUsage = `usage: ttorch hook <turn-started|turn-ended|session-ended>   (run by a harness hook; reads the hook payload on stdin)`

// cmdHook is the entry point for the harness lifecycle hooks installed on every worker
// session (harness.WriteWorkerSettings, harness.HooksFor). `ttorch hook <event>` records the
// event in the calling worker's hook record (paths.HookRecordFile), which the watcher and
// `ttorch status` weigh beside the pane text (livestate.Reconcile).
//
// A missing or unknown event name is a usage error. Past that it is fail-open: with no
// worker identity, a task id that is not a plain file name, or a failed write it records
// nothing and returns nil, and a missing record reads as no hook signal. It never writes to
// stdout, because Claude Code adds a UserPromptSubmit hook's stdout to the worker's context.
// It drains in, the hook payload, without parsing it, so the harness is never left writing
// a large prompt into a pipe nobody reads.
//
// Claude Code waits for each of these hooks before it moves on, so a worker's records land
// in the order its events happened.
func cmdHook(args []string, in io.Reader) error {
	if len(args) != 1 {
		return errors.New(hookUsage)
	}
	ev, ok := livestate.ParseEvent(args[0])
	if !ok {
		return fmt.Errorf("hook: unknown event %q\n%s", args[0], hookUsage)
	}
	defer func() { _, _ = io.Copy(io.Discard, in) }()
	// The worker's own identity, as `ttorch report` resolves it ($TTORCH_TASK_ID, then the
	// worktree's .ttorch/task). None means this is not a spawned worker.
	taskID, _ := callerIdentity()
	if !plainTaskID(taskID) {
		return nil
	}
	_ = livestate.WriteRecord(paths.Default().HookRecordFile(taskID), livestate.Record{
		Event: ev, TaskID: taskID, At: time.Now().UTC(),
	})
	return nil
}

// plainTaskID reports whether id can name the task's state directory as a single path
// element, so a record is never written outside it.
func plainTaskID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
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
// worker identity, identity sources that name different tasks (hookIdentity), a task id
// that is not a plain file name, a session running in a review workspace
// (inReviewWorkspace), or a failed write it records nothing and returns nil, and a missing
// record reads as no hook signal. It never writes to
// stdout, because Claude Code adds a UserPromptSubmit hook's stdout to the worker's context.
// It drains in, the hook payload, without parsing it, so the harness is never left writing
// a large prompt into a pipe nobody reads.
//
// A turn-started record also leaves a trace on the event spine (recordTurnStarted): the
// first writer each minute, as that writer describes itself.
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
	if inReviewWorkspace() {
		return nil
	}
	taskID, fileDB, via, ok := hookIdentity()
	if !ok || !plainTaskID(taskID) {
		return nil
	}
	if err := livestate.WriteRecord(paths.Default().HookRecordFile(taskID), livestate.Record{
		Event: ev, TaskID: taskID, At: time.Now().UTC(),
	}); err != nil {
		return nil
	}
	if ev == livestate.TurnStarted {
		recordTurnStarted(taskID, fileDB, via)
	}
	return nil
}

// recordTurnStarted appends the task's hook_turn_started event (db.AppendHookTurnStarted),
// which is non-actionable and written at most once a minute per task, so a chatty session
// cannot flood the events table. A later writer inside the same minute leaves nothing. The
// payload is this writer's own account, unverified: how the hook resolved its task (via),
// its CLAUDE_PROJECT_DIR (the cwd when that is unset), and the hook process's parent pid.
// Claude Code runs a command hook through a shell, so that pid is usually the short-lived
// shell's, and the harness's own only when the shell exec'd ttorch in its place. Reading
// further up the process tree would not settle which, so the field records only this.
//
// It is best-effort like the rest of the hook. The DB is resolved as `ttorch report`
// resolves it, a DB that does not exist yet is never created, and any error is dropped.
func recordTurnStarted(taskID, fileDB, via string) {
	path := resolveDBPath(fileDB)
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return
	}
	store, err := db.Open(path)
	if err != nil {
		return
	}
	defer store.Close()
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	payload := fmt.Sprintf("via=%s dir=%q ppid=%d", via, dir, os.Getppid())
	_, _ = store.AppendHookTurnStarted(context.Background(), taskID, payload)
}

// hookIdentity resolves the task a hook writes for, the DB path recorded in its task file
// ("" when no task file was found), and which source named it first: "env" or "task-file".
// ok is false when there is none (not a spawned worker) or when the sources disagree.
//
// The sources are $TTORCH_TASK_ID, the .ttorch/task found walking up from the cwd, and the
// one found walking up from CLAUDE_PROJECT_DIR (where the session started). Every source
// that resolves must name the same task. `ttorch report` takes $TTORCH_TASK_ID over the
// task file outright (callerIdentity), but a subprocess that inherited a worker's env and
// runs in another task's worktree would then write that worker's record, and a record that
// reads busy holds the other task's stall ladder quiet. So the hook writes nothing instead:
// not when the env and a task file disagree, not when only the env is set and the cwd is
// inside a different task's worktree, and not when the cwd and the project dir lie in
// different worktrees.
//
// This stops accidents, and the turn-started trace (recordTurnStarted) notes the first
// writer each minute as it describes itself; it is not a security boundary. Any process running as the worker's user can
// still write hook.json directly.
func hookIdentity() (taskID, fileDB, via string, ok bool) {
	env := strings.TrimSpace(os.Getenv("TTORCH_TASK_ID"))
	cwdID, cwdDB := findTaskFile()
	var projectID, projectDB string
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
		projectID, projectDB = findTaskFileFrom(dir)
	}
	for _, id := range []string{env, cwdID, projectID} {
		switch {
		case id == "":
		case taskID == "":
			taskID = id
		case id != taskID:
			return "", "", "", false
		}
	}
	if taskID == "" {
		return "", "", "", false
	}
	fileDB = cwdDB
	if fileDB == "" {
		fileDB = projectDB
	}
	via = "task-file"
	if env != "" {
		via = "env"
	}
	return taskID, fileDB, via, true
}

// inReviewWorkspace reports whether this hook's session started inside the review workspaces
// (paths.ReviewWorkspaceDir). The gate's reviewer sessions run there with the same worker
// settings, so they carry the lifecycle hooks, but they have no task of their own. A reviewer
// that inherited the worker's TTORCH_TASK_ID, or cd'd into the worker's worktree and so found
// its .ttorch/task, would otherwise overwrite that worker's record.
//
// Claude Code sets CLAUDE_PROJECT_DIR to the directory the session started in and leaves it
// there when the session cds, so it is what this checks. The process's cwd is the fallback
// when it is unset. Both sides are compared with symlinks resolved, so a home reached
// through a link still matches a project dir given as its real path.
func inReviewWorkspace() bool {
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		dir = wd
	}
	root := filepath.Dir(paths.Default().ReviewWorkspaceDir("x")) // <home>/review-workspaces
	return within(resolved(root), resolved(dir))
}

// within reports whether dir is root or lies under it.
func within(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolved is p with symlinks resolved, or p cleaned when that fails (p does not exist).
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// plainTaskID reports whether id can name the task's state directory as a single path
// element, so a record is never written outside it.
func plainTaskID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

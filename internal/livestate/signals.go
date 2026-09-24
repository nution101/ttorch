package livestate

// The hook signal: a second view of a worker's liveness, from the harness's own lifecycle
// hooks rather than its screen. The pane heuristics in livestate.go match text the harness
// draws, so a change to that text degrades them silently. A harness that can run hooks
// tells ttorch when a turn starts and ends instead: `ttorch hook <event>` (internal/cli)
// writes a small per-task Record, and Reconcile weighs it against the pane in a fixed order.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Event is a harness lifecycle moment the hook signal records. The string is the name
// `ttorch hook` takes on its command line and the value stored in the record.
type Event string

const (
	// TurnStarted: a prompt was submitted and the harness began a turn.
	TurnStarted Event = "turn-started"
	// TurnEnded: the harness finished the turn and is waiting at its prompt.
	TurnEnded Event = "turn-ended"
	// SessionEnded: the harness session exited.
	SessionEnded Event = "session-ended"
)

// Events lists every Event in a stable order.
var Events = []Event{TurnStarted, TurnEnded, SessionEnded}

// ParseEvent maps a command-line event name to its Event.
func ParseEvent(s string) (Event, bool) {
	for _, e := range Events {
		if string(e) == s {
			return e, true
		}
	}
	return "", false
}

// StartedMaxAge is how long a turn-started record is believed without a turn-ended after
// it. A harness that dies mid-turn, or a turn the user interrupts, never records its end;
// past this age the record stops holding the worker busy and the pane decides again. It
// bounds how much later such a worker is surfaced than the pane alone would surface it, at
// the cost of the hook adding nothing to a turn that runs longer.
const StartedMaxAge = 30 * time.Minute

// maxRecordBytes caps what ReadRecord will parse. A real record is well under 200 bytes;
// anything larger is not one.
const maxRecordBytes = 4096

// Record is the per-task file the lifecycle hooks write: the latest event, the task it
// belongs to, and when it happened. Each event replaces the record whole.
type Record struct {
	Event  Event     `json:"event"`
	TaskID string    `json:"task_id"`
	At     time.Time `json:"at"`
}

// WriteRecord writes r to path atomically: into a temporary file in the same directory,
// then renamed over path, so a reader sees the old record or the new one and never a
// partial write. It creates the directory if needed.
func WriteRecord(path string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hook-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has moved it
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ReadRecord reads the record at path for taskID. ok is false, meaning no hook signal,
// when the file is missing, unreadable, oversized or not valid JSON, when it names an
// unknown event or carries no timestamp, or when it belongs to a different task. A bad
// record never reads as busy or idle; the caller falls back to the pane.
//
// The watcher calls this for every worker on every sweep, so whatever sits at path must not
// be able to stall it or fill its memory. A symlink there is refused rather than followed
// (O_NOFOLLOW), the open never waits for a FIFO's writer (O_NONBLOCK), anything but a regular
// file is refused on the open descriptor before a byte is read, and at most one byte past
// maxRecordBytes is read.
func ReadRecord(path, taskID string) (Record, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Record{}, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return Record{}, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil || len(b) > maxRecordBytes {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, false
	}
	if _, known := ParseEvent(string(r.Event)); !known || r.At.IsZero() || taskID == "" || r.TaskID != taskID {
		return Record{}, false
	}
	return r, true
}

// State is a worker's reconciled live state.
type State int

const (
	// StateUnknown: neither signal says busy or idle, e.g. no hook record and a pane
	// showing neither a busy marker nor the input prompt. Callers that only need "is it
	// mid-turn" read it as not busy, as they read such a pane before.
	StateUnknown State = iota
	// StateBusy: the worker is mid-turn.
	StateBusy
	// StateIdle: the worker's turn has ended.
	StateIdle
)

func (s State) String() string {
	switch s {
	case StateBusy:
		return "busy"
	case StateIdle:
		return "idle"
	}
	return "unknown"
}

// paneState is the pane heuristic the reconciler falls back to: today's classification.
func paneState(pane string) State {
	if Busy(pane) {
		return StateBusy
	}
	if Idle(pane) {
		return StateIdle
	}
	return StateUnknown
}

// Reconcile combines a task's hook record with a capture of its pane. rec and ok are what
// ReadRecord returned, or harness.ReadHookRecord, which also returns ok false for a harness
// without lifecycle hooks (ok false means no hook signal); now is the time the record's age
// is measured against. The precedence is fixed, first matching row wins:
//
//	| # | hook record                         | pane                 | state          |
//	|---|-------------------------------------|----------------------|----------------|
//	| 1 | any                                 | busy text (Busy)     | busy           |
//	| 2 | any                                 | stall text (Stalled) | pane heuristic |
//	| 3 | none: missing, corrupt, another     | any                  | pane heuristic |
//	|   | task's, or a harness without hooks  |                      |                |
//	| 4 | turn-started, under StartedMaxAge   | any                  | busy           |
//	| 5 | turn-started, older or future-dated | any                  | pane heuristic |
//	| 6 | turn-ended                          | any                  | idle           |
//	| 7 | session-ended                       | any                  | pane heuristic |
//
// The pane heuristic is today's classification, unchanged: busy when Busy(pane), idle when
// Idle(pane), unknown otherwise. With no hook signal every pane reads exactly as it did
// before the record existed.
//
// Busy text overrides the record (row 1) because the hooks can miss the start of a turn:
// when the worker Stop hook blocks a stop, the turn carries on without a new prompt, so the
// record says turn-ended while the worker works. Stall text overrides it (row 2) because a
// turn that dies on an API error does not fire Stop. Claude Code records that ending through
// StopFailure where the release has it; where it does not, or the harness does not report the
// stall as an API error, a turn-started record would hold the stalled worker busy and keep it
// from the watcher's stall auto-resume. A harness killed mid-turn, or a turn the user
// interrupts, records no end at all, so a turn-started record is believed only for
// StartedMaxAge (row 5). A session end clears the
// record (row 7): the harness exited, and whatever runs in the window since is for the pane
// to show.
func Reconcile(rec Record, ok bool, pane string, now time.Time) State {
	fromPane := paneState(pane)
	if !ok || fromPane == StateBusy || Stalled(pane) {
		return fromPane
	}
	switch rec.Event {
	case TurnStarted:
		if !rec.At.After(now) && now.Sub(rec.At) < StartedMaxAge {
			return StateBusy
		}
	case TurnEnded:
		return StateIdle
	}
	return fromPane
}

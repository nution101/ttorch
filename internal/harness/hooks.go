package harness

import (
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

// LifecycleHooks is the per-harness plugin point for the hook liveness signal
// (livestate.Reconcile): which lifecycle events a harness's own hooks can report, and which
// of its hook events deliver each one. Each of those hooks runs `ttorch hook <event>`
// (LifecycleHookCommand), which records the event for the worker's task. A harness that
// reports no events gives no hook signal, and its workers' liveness stays the pane
// heuristic alone.
type LifecycleHooks interface {
	// Events lists the lifecycle events the harness can report.
	Events() []livestate.Event
	// NativeEvents names the harness's own hook events that report ev, as its hook
	// configuration spells them. It is empty for an event the harness cannot report.
	NativeEvents(ev livestate.Event) []string
}

// HooksFor returns kind's lifecycle hooks. Only Claude Code has any; every other harness
// reports none.
func HooksFor(kind string) LifecycleHooks {
	if kind == "claude" {
		return claudeHooks{}
	}
	return noHooks{}
}

// claudeHooks maps the lifecycle events onto Claude Code's hook events. A turn that ends
// normally fires Stop; one that ends on an API error fires StopFailure instead, so both
// record turn-ended. A Claude Code release older than StopFailure skips that one entry and
// keeps the rest of the settings file, so on it an API-error ending goes unrecorded and the
// reconciler's stall and age rules cover it.
type claudeHooks struct{}

func (claudeHooks) Events() []livestate.Event {
	return []livestate.Event{livestate.TurnStarted, livestate.TurnEnded, livestate.SessionEnded}
}

func (claudeHooks) NativeEvents(ev livestate.Event) []string {
	switch ev {
	case livestate.TurnStarted:
		return []string{"UserPromptSubmit"}
	case livestate.TurnEnded:
		return []string{"Stop", "StopFailure"}
	case livestate.SessionEnded:
		return []string{"SessionEnd"}
	}
	return nil
}

// noHooks is every harness without lifecycle hooks.
type noHooks struct{}

func (noHooks) Events() []livestate.Event             { return nil }
func (noHooks) NativeEvents(livestate.Event) []string { return nil }

// reportsTurns reports whether h can report both edges of a turn. A record is only worth
// reading when it can: with starts but no ends, a turn-started record would hold a worker
// busy until it aged out; with ends but no starts, a turn-ended record would outlive the
// next turn.
func reportsTurns(h LifecycleHooks) bool {
	var started, ended bool
	for _, ev := range h.Events() {
		started = started || ev == livestate.TurnStarted
		ended = ended || ev == livestate.TurnEnded
	}
	return started && ended
}

// ReadHookRecord returns a task's hook record for livestate.Reconcile. ok is false, meaning
// no hook signal, when kind's hooks cannot report both edges of a turn, or when the record
// is missing, corrupt, or another task's (livestate.ReadRecord).
func ReadHookRecord(kind, path, taskID string) (livestate.Record, bool) {
	if !reportsTurns(HooksFor(kind)) {
		return livestate.Record{}, false
	}
	return livestate.ReadRecord(path, taskID)
}

// LifecycleHookCommand is the command a harness hook runs to record ev: `ttorch hook <ev>` by
// absolute path, so it resolves whatever the hook process's PATH is. It ends in `|| true` so
// it never exits non-zero. Claude Code reads exit 2 from a UserPromptSubmit hook as "discard
// the prompt" and from a Stop hook as "keep going", and an installed ttorch older than the
// settings file answers the unknown `hook` verb with exit 2. Without the guard, a worker
// running against such a binary could take no prompt and never finish a turn.
func LifecycleHookCommand(ev livestate.Event) string {
	return shq(paths.Default().Binary()) + " hook " + string(ev) + " || true"
}

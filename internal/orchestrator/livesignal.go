package orchestrator

import (
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
)

// liveState is TaskState's reading of a worker whose window is live: its harness's hook
// record reconciled with the pane capture (livestate.Reconcile), the same reading the
// watcher's liveness sweep makes, so `ttorch status` and the events the manager receives
// agree about whether a worker is mid-turn. With no hook signal it is DeriveState(true,
// pane).
func liveState(p paths.Paths, t db.Task, pane string, now time.Time) string {
	rec, ok := harness.ReadHookRecord(t.Harness, p.HookRecordFile(t.ID), t.ID)
	if livestate.Reconcile(rec, ok, pane, now) == livestate.StateBusy {
		return "working"
	}
	return "idle"
}

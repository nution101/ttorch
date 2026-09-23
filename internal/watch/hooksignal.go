package watch

import (
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/livestate"
)

// busy reports whether a worker is mid-turn for the liveness sweep: its harness's hook
// record reconciled with the pane capture (livestate.Reconcile). With no hook signal (no
// record, a bad one, or a harness without lifecycle hooks) it is livestate.Busy(pane), the
// check the sweep made before the record existed.
func (w *Watcher) busy(t db.Task, pane string) bool {
	rec, ok := harness.ReadHookRecord(t.Harness, w.P.HookRecordFile(t.ID), t.ID)
	return livestate.Reconcile(rec, ok, pane, w.clock()) == livestate.StateBusy
}

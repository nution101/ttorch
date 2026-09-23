package scheduler

import (
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
)

// occupiedFootprints maps each repo to the footprints already "occupied" in snap: owned by a
// worker that is, or is about to be, on those files. A snapshot task occupies its footprint
// when it is EITHER status=active (a claim, with or without a window yet) OR tmux-live
// (snap.Live, a live window of any non-terminal status). The active arm catches a claim that
// has not yet materialized a window (invisible to a liveness-gated check: a prior tick's slow
// dispatch, a second scheduler instance, or this tick before its window comes up), and the
// snap.Live arm catches every live-windowed worker, answered in memory from the one snapshot
// instead of a per-pair `tmux list-windows` subprocess. cc sessions and empty footprints never
// occupy. RunOnce extends the result with each task it claims during the tick.
func occupiedFootprints(snap *orchestrator.LiveSnapshot) map[string][][]string {
	occupied := map[string][][]string{}
	for _, t := range snap.Tasks {
		if t.Kind == db.KindCC || len(t.Footprint) == 0 {
			continue
		}
		if t.Status == db.StatusActive || snap.Live(t) {
			occupied[t.Project] = append(occupied[t.Project], t.Footprint)
		}
	}
	return occupied
}

// DispatchOverlaps reports whether dispatching t now would put it on files the live fleet in
// snap already occupies, by the rule the dispatch pass applies (occupiedFootprints and
// footprintConflicts). The dispatch pass passes forceOverlap to the spawn only when this is
// true, so a disjoint dispatch keeps the spawn's fail-closed overlap check. It is exported for
// dispatches started outside the daemon (the board's dispatch action), which must take snap
// before claiming t so that t's own footprint is not counted against it. A task with no
// declared footprint never overlaps.
func DispatchOverlaps(snap *orchestrator.LiveSnapshot, t db.Task) bool {
	if len(t.Footprint) == 0 {
		return false
	}
	return footprintConflicts(t.Footprint, occupiedFootprints(snap)[t.Project])
}

package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

// Daemon observability (§roadmap H5). The scheduler was a black box: an idle daemon logged
// nothing, so a healthy idle daemon was indistinguishable from a wedged one, swallowed errors
// were invisible, and there was no heartbeat the manager's watchdog could read to tell a DAEMON
// stall from a MANAGER stall. This file gives the daemon observability with a single extra write
// per tick: runTick folds the tick's per-pass counts into a tickStats and calls recordTick, which
// upserts the durable scheduler_status row (last_tick_at + cumulative counters + last error) and,
// on a throttled cadence, emits a HEARTBEAT line to the daemon LOG (sc.Log — a file in the
// auto-started daemon, never the manager TTY). `ttorch scheduler status` reads the row back
// through StatusView. The heavy lifting lives here so scheduler.go's wiring stays minimal.

const (
	// heartbeatInterval throttles the HEARTBEAT log line: recordTick writes the durable status
	// row every tick, but emits a human-readable heartbeat to the daemon log only this often, so
	// an idle daemon proves liveness in the log roughly once a minute without spamming it across
	// the (much tighter) DefaultInterval tick cadence.
	heartbeatInterval = 60 * time.Second

	// StaleAfterDefault is the default last-tick age past which `ttorch scheduler status` reports
	// the daemon stalled (and exits non-zero). It is comfortably larger than heartbeatInterval and
	// the default tick cadence — a healthy daemon advances last_tick_at every tick — so a busy or
	// idle HEALTHY daemon is never falsely flagged; --stale-after tunes it for a non-default
	// --interval.
	StaleAfterDefault = 90 * time.Second
)

// tickStats accumulates one tick's per-pass outcome counts (and any swallowed pass errors) so
// runTick can fold the whole tick into a SINGLE durable status write. Counts are per-tick deltas;
// recordTick hands them to the store, which adds them to the cumulative row.
type tickStats struct {
	dispatched int
	landed     int
	gated      int
	recovered  int
	// deferred is the load-backpressure-deferred dispatch count. It is reserved for the sibling
	// load-backpressure pass (task H4) and stays 0 until that pass wires it through; the durable
	// column and the status output carry it now so the schema is complete when H4 lands.
	deferred int
	errors   int
	lastErr  string
}

// noteErr records a swallowed pass error so its occurrence becomes observable in the durable
// status row (the error counter + last_error) and in the daemon log — WITHOUT changing the tick's
// control flow. The pass error is still logged-and-swallowed exactly as before (the daemon
// survives a DB hiccup; C2's fail-closed handling is untouched); this only makes the occurrence
// countable. The latest error in a tick wins as last_error.
func (s *tickStats) noteErr(err error) {
	s.errors++
	s.lastErr = err.Error()
}

// recordTick folds one tick's outcome into the durable scheduler_status row and, on a throttled
// cadence, emits a HEARTBEAT line to the daemon LOG. It is the daemon's observability heartbeat:
// even a fully idle tick advances last_tick_at and tick_count, so `ttorch scheduler status` (and
// a watchdog) can tell a healthy idle daemon from a wedged one. The per-tick cost is ONE small
// upsert; the live queue/active gauges in the heartbeat are read only when a heartbeat is actually
// emitted (~once a minute), so an ordinary tick stays a single write with no extra reads. A
// status-store write failure is itself a swallowed error: it is logged to the daemon log and does
// not disturb the tick (the next tick re-derives), so observability can never break dispatch.
func (sc *Scheduler) recordTick(ctx context.Context, stats tickStats) {
	now := sc.clock()
	row, err := sc.Store.RecordSchedulerTick(ctx, db.SchedulerTick{
		At:         now,
		Dispatched: stats.dispatched,
		Landed:     stats.landed,
		Gated:      stats.gated,
		Recovered:  stats.recovered,
		Deferred:   stats.deferred,
		Errors:     stats.errors,
		LastError:  stats.lastErr,
	})
	if err != nil {
		sc.logf("status record error: %v", err)
		return
	}
	if !sc.heartbeatDue(now) {
		return
	}
	sc.lastHeartbeatAt = now
	// Live gauges (queue depth + active workers) — read only when a heartbeat is actually emitted,
	// so an ordinary tick incurs no extra reads beyond the one status upsert.
	pending, active := BoardGauges(ctx, sc.Store)
	sc.logf("heartbeat: tick=%d active=%s pending=%s dispatched=%d landed=%d gated=%d recovered=%d deferred=%d errors=%d",
		row.TickCount, gauge(active), gauge(pending),
		row.Dispatched, row.Landed, row.Gated, row.Recovered, row.Deferred, row.Errors)
}

// heartbeatDue reports whether enough wall-clock has elapsed since the last emitted heartbeat to
// log another. A zero lastHeartbeatAt (a fresh start, or the first tick after a daemon restart) is
// always due, so the daemon emits a startup heartbeat on its first tick.
func (sc *Scheduler) heartbeatDue(now time.Time) bool {
	return sc.lastHeartbeatAt.IsZero() || now.Sub(sc.lastHeartbeatAt) >= heartbeatInterval
}

// BoardGauges returns the current queue depth (ready pending tasks) and active worker count — the
// point-in-time fleet gauges shown in the daemon heartbeat and by `ttorch scheduler status`. They
// mirror the dispatch pass's ready/active filter (excluding ad-hoc cc) and are derived LIVE (never
// stored), so a stalled daemon's status still reflects the real board. A read error degrades that
// gauge to -1 (rendered "?") rather than failing the caller.
func BoardGauges(ctx context.Context, store *db.Store) (pending, active int) {
	return countByStatus(ctx, store, db.StatusPending), countByStatus(ctx, store, db.StatusActive)
}

// countByStatus counts non-cc tasks in the given status, returning -1 on a read error (an unknown
// gauge, never a fatal one — the heartbeat/status are best-effort observability).
func countByStatus(ctx context.Context, store *db.Store, status string) int {
	tasks, err := store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{status},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return -1
	}
	return len(tasks)
}

// gauge renders a live board gauge: a known count, or "?" when the read failed (-1). Keeping the
// "unknown" case explicit avoids printing a misleading "0".
func gauge(n int) string {
	if n < 0 {
		return "?"
	}
	return strconv.Itoa(n)
}

// StatusView is the assembled input `ttorch scheduler status` renders and judges staleness from:
// the durable row (and whether it exists yet), the live board gauges, whether the daemon's
// singleton lock is currently held (a live daemon process), the reference clock, and the staleness
// threshold. It is plain data so the rendering and stalled-detection are pure and unit-testable.
type StatusView struct {
	Row        db.SchedulerStatus
	HasRow     bool
	Pending    int
	Active     int
	LockHeld   bool
	Now        time.Time
	StaleAfter time.Duration
}

// Stalled is the watchdog signal: the daemon is NOT healthily ticking. It is true when the daemon
// has never ticked (no row) or its last tick is older than StaleAfter — which is exactly the case
// that distinguishes a wedged daemon (its singleton lock may still be held, yet last_tick_at is
// old) from a healthy idle one (last_tick_at fresh because every idle tick still records). `ttorch
// scheduler status` exits non-zero exactly when this is true.
func (v StatusView) Stalled() bool {
	if !v.HasRow {
		return true
	}
	return v.Now.Sub(v.Row.LastTickAt) > v.StaleAfter
}

// Render returns the human-readable status report — always printed (healthy or stalled) so the
// counters and last error are visible regardless of the exit code.
func (v StatusView) Render() string {
	var b strings.Builder
	b.WriteString("scheduler daemon status\n")

	// Liveness has two independent facets: the singleton lock says a daemon PROCESS is alive;
	// last_tick_at says it is actually TICKING. Both matter — a held lock with a stale tick is the
	// wedged-daemon case this whole record exists to surface.
	if v.LockHeld {
		b.WriteString("  daemon:       running (singleton lock held)\n")
	} else {
		b.WriteString("  daemon:       not running (no singleton lock holder)\n")
	}

	if !v.HasRow {
		b.WriteString("  last tick:    never (no tick recorded yet)\n")
		fmt.Fprintf(&b, "  queue:        %s pending, %s active\n", gauge(v.Pending), gauge(v.Active))
		b.WriteString("  STALLED:      daemon has not recorded a tick\n")
		return b.String()
	}

	age := roundDur(v.Now.Sub(v.Row.LastTickAt))
	fmt.Fprintf(&b, "  last tick:    %s ago (%s)\n", age, v.Row.LastTickAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  ticks:        %d\n", v.Row.TickCount)
	fmt.Fprintf(&b, "  queue:        %s pending, %s active\n", gauge(v.Pending), gauge(v.Active))
	fmt.Fprintf(&b, "  dispatched:   %d\n", v.Row.Dispatched)
	fmt.Fprintf(&b, "  landed:       %d\n", v.Row.Landed)
	fmt.Fprintf(&b, "  gated:        %d\n", v.Row.Gated)
	fmt.Fprintf(&b, "  recovered:    %d\n", v.Row.Recovered)
	fmt.Fprintf(&b, "  deferred:     %d\n", v.Row.Deferred)
	fmt.Fprintf(&b, "  errors:       %d\n", v.Row.Errors)
	if v.Row.LastError != "" {
		when := ""
		if !v.Row.LastErrorAt.IsZero() {
			when = fmt.Sprintf(" (%s ago)", roundDur(v.Now.Sub(v.Row.LastErrorAt)))
		}
		fmt.Fprintf(&b, "  last error:   %s%s\n", v.Row.LastError, when)
	} else {
		b.WriteString("  last error:   none\n")
	}
	if v.Stalled() {
		fmt.Fprintf(&b, "  STALLED:      last tick %s ago exceeds threshold %s\n", age, roundDur(v.StaleAfter))
	}
	return b.String()
}

// roundDur rounds a duration to the second for display, flooring a (clock-skew) negative to 0 so
// the report never shows a nonsensical "-3s ago".
func roundDur(d time.Duration) time.Duration {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second)
}

// API-stall recovery (the explicit complement to the H2 idle-nudge): the deterministic Go
// daemon recognizes a session whose harness turn DIED mid-stream with a recoverable API error
// — the pane shows `API Error: Response stalled mid-stream`, `Connection closed mid-response`,
// `stream disconnected`, a stream-watchdog timeout, and the like (livestate.APIStalled) — and
// is now sitting idle at the input prompt with work unfinished. Left alone, a worker strands
// its pool slot until the lease lapses and the MANAGER strands until a human notices. This pass
// waits out a short grace (~15s) and nudges the session ONCE per stall episode with "continue",
// exactly as a human would, for BOTH worker windows AND the manager window — bounded (it gives
// up after MaxStallNudges ineffective nudges, never spamming a truly-dead session) and
// observable (every nudge and give-up is logged).
//
// It is the COMPLEMENT to the idle-nudge pass, not a duplicate: the idle-nudge acts on a plain
// idle prompt (no error) after a minutes-long grace and explicitly SKIPS APIStalled panes; this
// pass acts only on an API-stalled prompt after a ~15s grace. The two therefore act on disjoint
// pane states, so a session is nudged by exactly one of them.
//
// Manager window: the manager has no task row, so the per-task Fleet.Peek/Send cannot reach it.
// The daemon — a SEPARATE process from the manager's harness — would read and nudge the manager
// pane through the mgrPeek/mgrSend seams (the tmux window named "manager" on the Manager's
// session, the SAME window orchestrator.StartManager and the watcher identify), sending ONLY
// when the pane is APIStalled so a healthy manager is never injected into.
//
// IMPORTANT — manager seams are UNWIRED in production pending a lead decision. Nudging the
// manager means a `tmux.SendLine` of "continue" INTO the manager window, which the increment-6
// security invariant TestNoInjectionIntoManagerSession (internal/orchestrator/inc6_test.go)
// forbids outright: after the supervisor's keystroke poke was retired, NO code may type into
// the manager session. This task asks to resume a STALLED manager (the lead's silent-wake
// constraint forbids writing only to a HEALTHY manager), which requires admitting a narrow,
// bounded exception to that invariant — a cross-cutting, security-relevant change in
// internal/orchestrator, outside this task's footprint, that should not ship through trusted
// auto-merge on a worker's judgement alone. So New() deliberately leaves mgrPeek/mgrSend nil
// (the manager half stays dormant in production); the logic below is complete and fully
// exercised through injected test seams, ready to activate the moment the invariant is evolved
// to permit this one resume nudge. The WORKER half is unaffected — worker nudges go through
// Fleet.Send to a WORKER window, which the invariant does not (and must not) touch.
//
// Bookkeeping is in-memory, per episode (stallSeen): the manager cannot use the durable
// event-spine claim the idle-nudge uses (ClaimIdleNudge requires a task row), so for uniformity
// BOTH halves use the same in-memory tracking. The cost is that stall nudges are not pooled with
// the watcher's own (slower, ~60s) stall auto-resume and do not survive a daemon restart; both
// only ever cost a small, bounded number of extra "continue" nudges to a session that is already
// wedged — benign. The episode is reset the instant a session is observed not-stalled, so a
// revived session always starts with a full budget.
package scheduler

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
)

// Stall-recovery defaults and env knobs. The grace is intentionally on the order of SECONDS
// (not the idle-nudge's minutes): an API-stall is an unambiguous mid-stream death, so a short
// wait is enough to be sure the turn really ended (and to let a still-settling frame clear)
// without making a human wait. MaxStallNudges is the restart-storm bound — after this many
// consecutive ineffective "continue"s the pass gives up and leaves the session for the manager.
const (
	defaultStallNudgeGrace = 15 * time.Second
	defaultMaxStallNudges  = 2

	// stallCaptureLines mirrors idleCaptureLines: enough trailing pane lines to hold the
	// harness's input box (so the caret is visible) plus the stall marker rendered above it.
	stallCaptureLines = 12

	// managerStallKey is the stallSeen key for the manager window. It starts with a NUL byte,
	// which a real task id can never contain, so it can never collide with a worker's key.
	managerStallKey = "\x00manager"

	// stallNudgeText is the single word typed into a stalled session to resume it — the same
	// resume a human (or the watcher's auto-resume, or the idle-nudge) sends.
	stallNudgeText = "continue"

	envStallNudgeGrace = "TTORCH_STALL_NUDGE_GRACE"
	envMaxStallNudges  = "TTORCH_MAX_STALL_NUDGES"
)

// stallObservation is one session's current API-stall episode: when the stall was first
// observed (the grace anchor), how many "continue" nudges it has received this episode, when
// the most recent nudge was sent (so consecutive nudges are spaced by at least the grace), and
// whether the budget is spent (the give-up has already been logged once). An episode lives from
// the first stalled observation until the session is next observed NOT stalled, at which point
// its entry is dropped and the budget replenishes.
type stallObservation struct {
	firstSeen   time.Time
	nudges      int
	lastNudgeAt time.Time
	capped      bool
}

// stallNudgeGraceFromEnv resolves the stall-recovery grace from TTORCH_STALL_NUDGE_GRACE (a Go
// duration like "15s"), falling back to defaultStallNudgeGrace when unset or unparseable.
func stallNudgeGraceFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv(envStallNudgeGrace)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultStallNudgeGrace
}

// maxStallNudgesFromEnv resolves the consecutive-nudge ceiling from TTORCH_MAX_STALL_NUDGES,
// falling back to defaultMaxStallNudges when unset or unparseable. A configured value of 0 (or
// negative) is honored as "disable the stall-recovery pass entirely" (the off-switch).
func maxStallNudgesFromEnv() int {
	if v := strings.TrimSpace(os.Getenv(envMaxStallNudges)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultMaxStallNudges
}

// RunStallRecoveryOnce performs one API-stall recovery pass and returns how many sessions it
// nudged. It inspects every alive worker (active, non-cc, live window, valid lease) AND the
// manager window; any whose pane is APIStalled past the grace, under the per-episode budget, is
// nudged once with "continue". It is a no-op (returns 0) when disabled (MaxStallNudges <= 0).
//
// A per-session pane-capture or send failure is non-fatal — that session is skipped — so one
// dead window never stalls recovering the rest; only a DB error (reading the active set) aborts
// the pass, which the next tick re-derives from scratch.
func (sc *Scheduler) RunStallRecoveryOnce(ctx context.Context) (int, error) {
	if sc.MaxStallNudges <= 0 {
		return 0, nil // stall-recovery disabled
	}
	if sc.stallSeen == nil {
		sc.stallSeen = make(map[string]stallObservation)
	}
	now := sc.clock()
	recovered := 0
	// observed collects every session key we could inspect this pass (stalled or not); the prune
	// at the end drops episodes for keys that vanished from observation entirely (a worker that
	// left the active set, or a manager window that went away), so stallSeen never grows unbounded.
	observed := make(map[string]bool)

	// --- Worker fleet: alive, autonomous workers only (cc/lead sessions are never auto-nudged) ---
	tasks, err := sc.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusActive},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			return recovered, err
		}
		// Window-less, or the manager window (handled below) — not a worker candidate.
		if t.Window == "" || t.Window == managerWindow {
			continue
		}
		// Valid lease only: an expired/absent lease is the reclaim pass's domain, not a nudge's.
		if t.LeaseExpiresAt == nil || !t.LeaseExpiresAt.After(now) {
			continue
		}
		pane, err := sc.Fleet.Peek(t.ID, stallCaptureLines)
		if err != nil {
			// Unobservable/gone — the window-gone reclaim path owns dead windows. End any episode.
			delete(sc.stallSeen, t.ID)
			continue
		}
		observed[t.ID] = true
		id := t.ID // capture for the send closure
		if sc.recoverStall(t.ID, pane, now, func() error { return sc.Fleet.Send(id, stallNudgeText) }, "worker "+t.ID) {
			recovered++
		}
	}
	if err := ctx.Err(); err != nil {
		return recovered, err
	}

	// --- Manager window: the daemon watches and nudges the manager that cannot nudge itself ---
	// Gated on BOTH seams being wired. In production they are deliberately nil (see the file
	// header: nudging the manager needs the increment-6 manager-injection invariant evolved
	// first), so this half is dormant; a test injects both to exercise it end to end.
	if sc.mgrPeek != nil && sc.mgrSend != nil {
		if pane, ok := sc.mgrPeek(stallCaptureLines); ok {
			observed[managerStallKey] = true
			if sc.recoverStall(managerStallKey, pane, now, func() error { return sc.mgrSend(stallNudgeText) }, "manager") {
				recovered++
			}
		} else {
			delete(sc.stallSeen, managerStallKey) // unobservable manager — end any episode
		}
	}

	// Prune episodes for sessions not observed this pass (left the active set / window gone).
	for k := range sc.stallSeen {
		if !observed[k] {
			delete(sc.stallSeen, k)
		}
	}
	return recovered, nil
}

// recoverStall evaluates ONE session's captured pane and, when it is API-stalled past the grace
// and under the per-episode nudge budget, sends "continue" via send(). It owns the in-memory
// episode bookkeeping for key, and returns whether it sent a nudge. label is used only in the
// log lines ("worker w1" / "manager").
//
// The decision is deliberately conservative:
//
//   - if the pane is NOT APIStalled (healthy, busy, cleanly idle, or a non-stall error), the
//     episode is ENDED and nothing is sent — this is the silent-wake guarantee: a non-stalled
//     session (especially the manager) is never injected into;
//   - the FIRST nudge waits until the stall has persisted at least the grace since it was first
//     observed, which doubles as the stability gate (a single transient stalled frame is cleared
//     on the next not-stalled tick, long before the grace elapses, so it never nudges);
//   - consecutive nudges are spaced by at least the grace (timed from the later of firstSeen and
//     the last nudge);
//   - after MaxStallNudges ineffective nudges the pass gives up (logged once) and stands down.
//
// The nudge is counted against the budget BEFORE the send (record-then-send), so a send that
// fails leaves the budget decremented rather than risking a re-nudge storm on a wedged pane —
// the same safety the idle-nudge path uses. Under-nudging beats over-nudging a dead session.
func (sc *Scheduler) recoverStall(key, pane string, now time.Time, send func() error, label string) bool {
	if !livestate.APIStalled(pane) {
		delete(sc.stallSeen, key) // not stalled — end the episode; never inject into a live session
		return false
	}
	obs, ok := sc.stallSeen[key]
	if !ok {
		obs = stallObservation{firstSeen: now} // first observation of this stall episode
	}
	defer func() { sc.stallSeen[key] = obs }()

	if obs.capped {
		return false // budget already spent and the give-up already logged this episode
	}
	if obs.nudges >= sc.MaxStallNudges {
		obs.capped = true
		sc.logf("stall recovery: %s nudged %dx, no progress — leaving for manager", label, obs.nudges)
		return false
	}
	// Grace gate: require the stall to have persisted >= grace since it first appeared, and
	// consecutive nudges to be spaced by >= grace (anchored at the later of firstSeen / last nudge).
	anchor := obs.firstSeen
	if obs.lastNudgeAt.After(anchor) {
		anchor = obs.lastNudgeAt
	}
	if now.Sub(anchor) < sc.stallGrace() {
		return false
	}
	obs.nudges++
	obs.lastNudgeAt = now
	if err := send(); err != nil {
		sc.logf("stall recovery: %s nudge send failed (counted; not re-nudged this episode): %v", label, err)
		return false
	}
	sc.logf("recovered API-stalled %s (stalled %s)", label, now.Sub(obs.firstSeen).Round(time.Second))
	return true
}

// stallGrace is the configured stall-recovery grace, with a floor of the package default so a
// hand-built Scheduler that sets only MaxStallNudges still uses a sane grace.
func (sc *Scheduler) stallGrace() time.Duration {
	if sc.StallNudgeGrace > 0 {
		return sc.StallNudgeGrace
	}
	return defaultStallNudgeGrace
}

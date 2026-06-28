package watch

// The watchdog is the manager's own liveness net — the counterpart to the worker
// liveness net `ttorch watch` already runs (§4.4). `ttorch watch` recovers a *worker*
// whose model stream stalled; nothing recovers the *manager* whose own LLM turn stalls
// on an API error, which silently halts the whole orchestration. The watchdog closes
// that gap from OUTSIDE the manager session (launchd/cron, or `ttorch watchdog
// --interval`): it detects a manager that is stalled WHILE actionable work waits and
// re-pokes it to resume.
//
// The re-poke uses ONLY the safe out-of-band channel the event watcher already uses —
// it appends one actionable DB event. An armed `ttorch watch` surfaces that event and
// exits, and the harness re-invokes the manager through its own background-task-
// completion channel (§B.1). The watchdog NEVER writes to the manager console, never
// adds to its terminal history, and never injects a keystroke; the only safe lever an
// external process has on a `claude` session is a row in the DB.
//
// Idle-awareness is structural, not a separate timer: a check that finds nothing
// actionable, or a manager that acted recently, or a pending event that will wake the
// manager on its own, simply stands down. So the watchdog can be polled cheaply on a
// short interval and costs nothing while the team is genuinely idle — it acts only when
// the manager appears stalled and work is waiting. Section citations (§…) refer to
// docs/design/sqlite-event-architecture.md.

import (
	"context"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// defaultStall is how long the manager must go without any DB-visible action before the
// watchdog treats it as stalled. It is generous on purpose: a manager idling in its
// event loop (a watch armed, a worker mid-build) legitimately writes nothing for long
// stretches, so the threshold sits well above a normal turn cadence to avoid a spurious
// poke. The other gates (a pending event, a live watcher, no outstanding work) suppress
// most false positives before the timer is even consulted.
const defaultStall = 5 * time.Minute

// managerEntityID is the entity_id the manager singleton's events carry (the manager
// row is id=1; the watchdog's event is manager-scoped, not task-scoped).
const managerEntityID = "manager"

// Watchdog performs one idle-aware, timer-gated manager-liveness check against a Store.
// The seam fields are swapped out in tests; NewWatchdog wires the production defaults.
type Watchdog struct {
	Store *db.Store
	P     paths.Paths

	// Stall is the no-action threshold (0 ⇒ defaultStall).
	Stall time.Duration

	// Seams (nil ⇒ the production default).
	now        func() time.Time
	watchArmed func() bool // a live `ttorch watch` holds the singleton (manager is in the loop)
}

// WatchdogResult reports what one Check decided, for the CLI/log only — never for the
// manager. Poked is true exactly when a manager_stalled event was appended.
type WatchdogResult struct {
	Poked  bool
	Reason string
}

// NewWatchdog builds a Watchdog with production seams. store and p are required.
func NewWatchdog(store *db.Store, p paths.Paths) *Watchdog {
	wd := &Watchdog{Store: store, P: p, Stall: defaultStall}
	wd.now = time.Now
	wd.watchArmed = func() bool { return watchHolderAlive(p.WatchPIDFile()) }
	return wd
}

func (wd *Watchdog) clock() time.Time { return wd.now() }

func (wd *Watchdog) stall() time.Duration {
	if wd.Stall <= 0 {
		return defaultStall
	}
	return wd.Stall
}

// Check runs one liveness check and, if the manager is stalled with outstanding work
// that nothing else will wake it for, appends a single actionable manager_stalled
// event. It returns the decision (for the CLI/log); it never errors merely because the
// manager is fine. The gates, in order, are what make it idle-aware and non-spammy:
//
//  1. No manager record yet → nothing to watch.
//  2. awaiting_lead set → the manager is deliberately silent for the lead (§4.6); never
//     poke (that would pull it off a pending decision — the cardinal sin).
//  3. An unconsumed actionable event exists (max actionable id > watermark) → the normal
//     watch channel will wake the manager (now if armed, or the instant it re-arms). This
//     is ALSO the debounce: the event this method appends makes the gate true on the next
//     run, so pokes never stack while one is pending.
//  4. No outstanding work → idle; stand down. "Outstanding work" is a task the manager
//     itself must clear (done/blocked/needs_input), or — only when no watcher is in the
//     loop — an active worker that nothing is supervising. An active worker WITH a live
//     watcher self-heals through the §4.4 liveness net, so it is not the watchdog's job.
//  5. The manager acted within the stall window → not stalled yet.
//
// Only past all five does it synthesize the wake.
func (wd *Watchdog) Check(ctx context.Context) (WatchdogResult, error) {
	m, ok, err := wd.Store.GetManager(ctx)
	if err != nil {
		return WatchdogResult{}, err
	}
	if !ok {
		return WatchdogResult{Reason: "no manager session recorded"}, nil
	}
	if m.AwaitingLead {
		return WatchdogResult{Reason: "manager is awaiting the lead — staying silent"}, nil
	}

	maxEvt, err := wd.Store.MaxActionableEventID(ctx)
	if err != nil {
		return WatchdogResult{}, err
	}
	if maxEvt > m.WatchWatermark {
		return WatchdogResult{Reason: "a pending actionable event will wake the manager"}, nil
	}

	outstanding, reason, err := wd.hasOutstandingWork(ctx)
	if err != nil {
		return WatchdogResult{}, err
	}
	if !outstanding {
		return WatchdogResult{Reason: reason}, nil
	}

	if wd.clock().Sub(m.UpdatedAt) < wd.stall() {
		return WatchdogResult{Reason: "manager acted recently"}, nil
	}

	if _, err := wd.Store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeManager,
		EntityID:   managerEntityID,
		Type:       db.EventManagerStalled,
		Actor:      db.ActorSystem,
		Actionable: true,
	}); err != nil {
		return WatchdogResult{}, err
	}
	return WatchdogResult{Poked: true, Reason: reason + " — manager stalled, re-poked"}, nil
}

// hasOutstandingWork reports whether the manager has work it must act on right now, and
// a human-readable reason either way. Manager-owned terminal states (done/blocked/
// needs_input) always count: a worker reaches them once and emits a single actionable
// event, so once the manager has consumed that event (watermark caught up, gate 3 above)
// nothing re-surfaces the task — only the manager landing/answering/unblocking it clears
// it. An active worker counts ONLY when no live watcher is in the loop: a watched active
// worker self-heals through the §4.4 liveness net, so re-poking for it would be spurious;
// an UNWATCHED active worker (the manager's turn died without re-arming a watch) has
// nothing watching it, so it is genuine outstanding supervision. cc tasks are ad-hoc and
// excluded, mirroring the watch liveness scope.
func (wd *Watchdog) hasOutstandingWork(ctx context.Context) (bool, string, error) {
	managerOwned, err := wd.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusDone, db.StatusBlocked, db.StatusNeedsInput},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return false, "", err
	}
	if len(managerOwned) > 0 {
		return true, "outstanding done/blocked/needs_input task(s) await the manager", nil
	}

	active, err := wd.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusActive},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return false, "", err
	}
	if len(active) == 0 {
		return false, "no actionable work — idle", nil
	}
	if wd.watchArmed() {
		return false, "active work in flight and a live watcher is in the loop", nil
	}
	return true, "active worker(s) in flight with no watcher in the loop", nil
}

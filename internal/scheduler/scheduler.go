// Package scheduler is ttorch's deterministic dispatch daemon (roadmap item A, phase 1):
// a persistent Go loop that drains the SQLite task board and dispatches ready backlog
// WITHOUT depending on the LLM manager remembering to do it each turn. Each tick it
// re-derives ready pending tasks from the DB, atomically claims the ones it can prove safe
// to run in parallel (a declared footprint that is disjoint from every live and
// already-claimed worker, within free worktree-pool capacity), and dispatches them through
// the SAME spawn path the manager uses. It is the permanent fix for disjoint ready work
// sitting idle while the manager is mid-turn or stalled — parallelism becomes deterministic
// code, not a rule the manager must remember.
//
// Phase 1 DISPATCHES ready backlog; phase 2a additionally LANDS work that has already passed
// the trust gate; phase 3 additionally SUPERVISES the fleet — it reclaims workers that have
// VERIFIABLY died (a tmux window the watcher confirmed gone, or an expired lease — never
// pane-output inference) and re-dispatches them within a bounded retry ceiling, poison-pilling
// a task that exceeds the ceiling to the terminal 'failed' status with an actionable event for
// the lead rather than restarting it forever. With the land pass enabled, each tick also finds
// done tasks that already carry a passing durable verdict and lands them through the manager's
// EXISTING land pipeline (the same code `ttorch land` runs), so a green, gated task merges
// without the LLM manager doing it by hand — the core throughput/anti-stall win. The three
// passes are independent: a tick runs whichever are enabled, dispatch defaults on
// (`ttorch scheduler`) while land (`--land`) and supervise (`--supervise`) are opt-in.
// Autonomous GATING (spawning the reviewers, recording the verdict) remains the LLM manager's
// job — the LAND pass deliberately lands only work that ALREADY carries a passing verdict, so
// recording it stays a deliberate, human-or-LLM gate. The LAND pass NEVER lands ungated work;
// the SUPERVISE pass reclaims only a worker that has shown no sign of life (no re-dispatch and no
// heartbeat) since its window was flagged gone, or whose lease genuinely lapsed, and never
// restarts a task past its retry ceiling. The daemon now AUTO-STARTS by default from the manager
// session (Manager.StartManager, config-gated by TTORCH_SCHEDULER_AUTOSTART, singleton via
// `scheduler --singleton`), with dispatch+land+supervise enabled, so a normal `ttorch` session
// drives the board autonomously; the manual `ttorch scheduler` subcommand stays available for
// running it by hand or with a different pass mix.
//
// Concurrency model: the scheduler is self-consistent and safe to run as multiple
// instances against one DB — the atomic dispatch claim (db.ClaimTask: BEGIN IMMEDIATE +
// status re-check) makes double-dispatch impossible, the atomic land claim (db.ClaimForLand,
// the same BEGIN IMMEDIATE pattern) makes double-land impossible, and the overlap check
// consults active claims (not just live tmux windows) so two instances never dispatch
// overlapping work. (The land path's own gate — a fresh, commit-pinned passing verdict plus
// a single-use approval consumed at the fast-forward — is the second, authoritative guard.)
// The one
// boundary in this phase: a HUMAN/LLM manager running `ttorch spawn` for the exact id the
// scheduler has just claimed (active, window not yet up) won't see that claim in its own
// liveness-gated overlap gate; the worst case is a window-name collision that fails one of
// the two spawns (never a silent double-dispatch). The manager↔scheduler co-running model is
// now the DEFAULT: the auto-started daemon drives dispatch/land/recovery while the LLM manager
// plans, gates, and answers decisions, coexisting purely through the DB's atomic claims. A
// manager-driven `ttorch spawn` and the daemon's dispatch can still collide on the exact same
// id only in the brief claimed-but-window-not-yet-up window above (a loud spawn failure, never a
// silent double-dispatch); in normal operation the manager leaves routine dispatch to the daemon
// and spawns directly only for work the daemon will not pick up.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/worktree"
)

// DefaultInterval is the tick cadence when none is configured: re-derive ready backlog and
// dispatch every 5s. Generous enough that a tick is cheap and the DB is barely touched when
// nothing is ready, tight enough that freshly-added disjoint backlog dispatches promptly.
const DefaultInterval = 5 * time.Second

// Idle-nudge defaults (the alive-but-idle recovery pass, §roadmap H2). The grace period is
// deliberately on the order of minutes: a worker that merely paused between sub-steps, or is
// thinking with no busy indicator yet, must NEVER be nudged — only one whose turn has been
// idle at the prompt for longer than any plausible brief pause. The max-nudge count is a
// small restart-storm bound: after this many consecutive nudges fail to revive a worker, the
// pass falls silent and leaves it for the manager or lease expiry, never spamming a wedged
// pane. Both are overridable via TTORCH_IDLE_NUDGE_GRACE (a Go duration) and
// TTORCH_MAX_IDLE_NUDGES; a max of 0 (or negative) DISABLES the pass entirely.
const (
	defaultIdleNudgeGrace = 3 * time.Minute
	defaultMaxIdleNudges  = 2

	// defaultIdleConfirmations requires a worker to be observed idle at the same pane for two
	// consecutive ticks before the first nudge — a single transient not-busy frame never fires.
	defaultIdleConfirmations = 2

	// idleCaptureLines is how many trailing pane lines the idle-nudge pass reads — enough to
	// contain the harness's input box (so the prompt caret is visible) plus any busy/stall
	// marker above it. It mirrors the watcher's small capture window.
	idleCaptureLines = 12

	// managerWindow is the tmux window the manager session runs in; the idle-nudge pass never
	// inspects or injects into it (it is not part of the autonomous worker fleet).
	managerWindow = "manager"

	// idleNudgeText is the message typed into an idle worker's pane — the same single word a
	// manager would `ttorch send` to resume a stalled turn.
	idleNudgeText = "continue"

	envIdleNudgeGrace = "TTORCH_IDLE_NUDGE_GRACE"
	envMaxIdleNudges  = "TTORCH_MAX_IDLE_NUDGES"
)

// Fleet is the orchestrator surface the scheduler drives. *orchestrator.Manager satisfies
// it; tests substitute a fake so the selection/claim logic runs without tmux or a real
// worktree pool. Every method is one the manager already exposes — the scheduler reuses
// the manager's spawn, overlap gate, and live-fleet view rather than reimplementing them.
type Fleet interface {
	// Status returns the live (non-terminal) tracked tasks — the occupancy source of truth
	// for worktree capacity (mirroring Manager.inUseWorktrees) and the live set the overlap
	// gate consults.
	Status() ([]db.Task, error)
	// CheckOverlap reports which LIVE workers a proposed footprint conflicts with in repo —
	// the same deterministic gate the manager's spawn uses. It returns an error when the board
	// cannot be read; the scheduler treats that as "cannot prove disjoint" and refuses to
	// dispatch the task this tick (fail closed), never as "no conflict".
	CheckOverlap(repo string, proposed []string) ([]orchestrator.Conflict, error)
	// SpawnWithFootprint dispatches a worker for an already-claimed task via the manager's
	// spawn path (worktree + tmux window + harness launch).
	SpawnWithFootprint(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool) (db.Task, error)
	// LandSet lands the given already-gated tasks through the manager's concurrent land
	// pipeline (rebase onto the current default, re-validate, carry the verdict+approval over
	// a clean rebase, per-repo-locked fast-forward, teardown), returning one result per task
	// in order. It is the SAME path `ttorch land` drives — the land pass reuses the merge gate
	// rather than reimplementing it, so requireVerdict and every approval/verdict/validate
	// check apply identically.
	LandSet(ctx context.Context, taskIDs []string, requireVerdict bool) []orchestrator.LandResult
	// Peek returns the last n lines of a worker's tmux pane (the SAME capture `ttorch peek`
	// reads). The idle-nudge pass uses it to observe whether an alive worker's turn has ended
	// at the input prompt. It errors when the task or its window is gone, which the caller
	// treats as "not observable this tick" (the window-gone reclaim path owns a dead window).
	Peek(taskID string, lines int) (string, error)
	// Send types a line into a worker's pane — the SAME mechanism `ttorch send` uses. The
	// idle-nudge pass calls it with "continue" to resume an alive-but-idle worker. It refuses
	// (errors) when the worker has no live window, so a nudge is never injected into a dead one.
	Send(taskID, text string) error
	// Gateable reports whether repo is a daemon-gate candidate (trusted delivery mode, where a
	// recorded pass auto-authorizes the merge). The gate pass uses it to skip claiming done
	// tasks in repos where a daemon-recorded verdict would not advance delivery.
	Gateable(repo string) bool
	// GateOnce drives one tick of the daemon gate for an already-claimed candidate task: it
	// runs the trust prep, dispatches the size-scaled reviewers, and — once every per-dimension
	// report is in and pinned to the prepped head — aggregates and, on an all-pass, records the
	// durable verdict through the UNCHANGED trust-record path (the same one a manager runs).
	// It records NOTHING on a block, missing/mismatched report, or stall — it surfaces an
	// actionable gate_blocked event for the manager to adjudicate. It is idempotent across ticks
	// and a daemon restart (the review dir + reviewer windows are its source of truth), so a
	// reviewer is never double-dispatched and a verdict never double-recorded. See
	// orchestrator.GateOnce.
	GateOnce(taskID string) (orchestrator.GateOutcome, error)
}

// Scheduler is the dispatch loop. Store is the task board (ready selection + atomic claim),
// Fleet the orchestrator it dispatches through, and Pool the worktree pool whose free
// capacity bounds each tick. Log receives the daemon's own diagnostic lines — its stdout or
// a log file, NEVER the manager pane (no TTY injection); a nil Log silences them.
type Scheduler struct {
	Store    *db.Store
	Fleet    Fleet
	Pool     worktree.Pool
	Interval time.Duration
	Log      io.Writer

	// Dispatch, Land, and Supervise select which passes each tick runs, independently.
	// Dispatch (the phase-1 behavior) claims and dispatches ready pending backlog; Land claims
	// and lands done tasks that already carry a passing verdict; Supervise (phase 3) reclaims
	// workers that have verifiably died — a confirmed-gone tmux window or an expired lease —
	// re-dispatching them within a bounded retry ceiling and poison-pilling the ones that
	// exceed it. New() turns Dispatch on (today's default); Land and Supervise are OFF unless
	// explicitly enabled (the autonomous-land / autonomous-recovery opt-ins) so there is no
	// behavior change unless the scheduler is run with them turned on. RunOnce/RunLandOnce/
	// RunSuperviseOnce are unconditional primitives — these toggles gate only which the loop
	// (runTick) drives.
	Dispatch  bool
	Land      bool
	Supervise bool

	// Gate selects the daemon GATE pass (roadmap A1): for a done task in a trusted repo with no
	// passing verdict, run the trust prep, dispatch the size-scaled adversarial reviewers, and —
	// once their reports are in — aggregate and record an all-pass verdict through the UNCHANGED
	// trust-record path, so the LLM manager is no longer on the steady-state critical path for
	// the mechanical prep→dispatch→aggregate→record step. It FAILS CLOSED: a blocking finding, a
	// prep refusal, or a stalled reviewer is never recorded — it surfaces a gate_blocked event
	// for the manager to adjudicate. OFF by default (a bare struct and New() both leave it
	// false), so existing manager-driven gating is unchanged unless the daemon is run with
	// --gate; RunGateOnce is an unconditional primitive that this toggle only gates the loop on.
	Gate bool

	// IdleNudgeGrace and MaxIdleNudges configure the alive-but-idle recovery pass that runs
	// inside Supervise (§roadmap H2). An alive worker (live window, valid lease, status
	// 'active') whose pane has sat idle at the prompt longer than IdleNudgeGrace is nudged
	// once with "continue"; MaxIdleNudges bounds how many consecutive nudges an unrevived
	// worker receives before the pass stands down. Both are zero by default on a bare struct
	// (so the pass is OFF unless explicitly configured — existing tests and any hand-built
	// Scheduler see no new behavior); New() populates them from the env-or-default. A
	// MaxIdleNudges <= 0 disables the pass.
	IdleNudgeGrace time.Duration
	MaxIdleNudges  int

	// IdleConfirmations is how many CONSECUTIVE ticks a worker must be observed idle at the
	// SAME pane before the first nudge — the stability gate that keeps a single transient
	// not-busy capture (e.g. a mid-compaction frame of a working worker) from ever triggering
	// a send. <1 is treated as 1 (nudge on first idle observation). New() sets it to 2.
	IdleConfirmations int

	// Backpressure governor (roadmap H4) — a machine-aware throttle layered ON TOP of the hard
	// worktree-pool cap and the disjoint-footprint invariant; it only ever dispatches FEWER
	// workers, never more, so it cannot weaken any correctness gate. MaxActiveWorkers caps how
	// many heavy workers may run concurrently across the fleet (counted as live 'active' tasks);
	// at the cap a dispatch tick spawns nothing more and LOGS the deferral. LoadCeiling, when > 0,
	// additionally defers a tick whose 1-minute system load average exceeds it (fail OPEN — an
	// unreadable loadavg never wedges dispatch; only the always-enforceable MaxActiveWorkers cap
	// gates such a tick). MaxLandConcurrency bounds how many gated tasks the LAND pass hands to
	// the concurrent land pipeline per tick, so a burst of gated work cannot launch many heavy
	// validate suites at once. All three are zero on a bare struct (governor OFF — existing
	// hand-built Schedulers and tests see no new behavior); New() populates them from the
	// env-or-default. A value <= 0 DISABLES that knob. See governor.go for the full knob docs.
	MaxActiveWorkers   int
	LoadCeiling        float64
	MaxLandConcurrency int

	// loadAvg reads the host's 1-minute load average for the LoadCeiling check; nil means
	// systemLoadAvg (the real darwin/linux reader). A test injects a fake to drive the ceiling
	// deterministically. It returns ok=false when load is unreadable, which loadDefersDispatch
	// treats as "no limit" (fail open).
	loadAvg func() (float64, bool)

	// idleSeen tracks, per task, the last idle pane hash observed and how many consecutive
	// ticks it has held — the in-memory backing for the IdleConfirmations stability gate. It is
	// touched only from the (single-threaded) tick loop, so it needs no lock; it is best-effort
	// across a daemon restart (a restart just costs one extra confirmation tick). nil until the
	// first idle-nudge pass lazily initializes it.
	idleSeen map[string]idleObservation

	// now returns the current time; nil means time.Now. It is the single clock the idle-nudge
	// pass reads so a test can drive grace deterministically without sleeping.
	now func() time.Time
}

// idleObservation is one task's idle-stability state: the last idle pane hash seen and how
// many consecutive ticks it has been observed unchanged.
type idleObservation struct {
	hash  string
	count int
}

// New builds the production Scheduler from a Manager: it dispatches through the manager's
// spawn path, reads live occupancy and overlap from it, and sizes capacity from its
// worktree pool. log receives the dispatch/diagnostic lines (the daemon's own stdout).
func New(m *orchestrator.Manager, interval time.Duration, log io.Writer) *Scheduler {
	return &Scheduler{
		Store:    m.Store,
		Fleet:    m,
		Pool:     m.Pool,
		Interval: interval,
		Log:      log,
		Dispatch: true, // dispatch is the default pass; the CLI may toggle it and enable Land

		IdleNudgeGrace:    idleNudgeGraceFromEnv(),
		MaxIdleNudges:     maxIdleNudgesFromEnv(),
		IdleConfirmations: defaultIdleConfirmations,

		// Backpressure governor (H4): default-on machine-load throttle (env-overridable). The
		// active-worker cap is sized from the worktree pool; the land cap and load ceiling take
		// their package defaults. See governor.go.
		MaxActiveWorkers:   maxActiveWorkersFromEnv(m.Pool.Max),
		LoadCeiling:        loadCeilingFromEnv(),
		MaxLandConcurrency: maxLandConcurrencyFromEnv(),
	}
}

// idleNudgeGraceFromEnv resolves the idle-nudge grace period from TTORCH_IDLE_NUDGE_GRACE
// (a Go duration like "5m"), falling back to defaultIdleNudgeGrace when unset or unparseable.
func idleNudgeGraceFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv(envIdleNudgeGrace)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultIdleNudgeGrace
}

// maxIdleNudgesFromEnv resolves the consecutive-nudge ceiling from TTORCH_MAX_IDLE_NUDGES,
// falling back to defaultMaxIdleNudges when unset or unparseable. A configured value of 0
// (or negative) is honored as "disable the idle-nudge pass".
func maxIdleNudgesFromEnv() int {
	if v := strings.TrimSpace(os.Getenv(envMaxIdleNudges)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultMaxIdleNudges
}

// Run drives RunOnce on a ticker until ctx is cancelled, then returns ctx.Err(). The first
// tick fires immediately (so an existing backlog drains without waiting a full interval),
// then every Interval. A tick's hard error (a DB failure) is logged and swallowed so a
// transient hiccup never kills the daemon — the next tick re-derives from scratch; only
// context cancellation ends the loop.
func (sc *Scheduler) Run(ctx context.Context) error {
	interval := sc.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	sc.runTick(ctx) // immediate first tick
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			sc.runTick(ctx)
		}
	}
}

// runTick runs one tick of each enabled pass (supervise, then dispatch, then gate, then land)
// and logs the outcome. The passes are independent: a transient error in one is logged and swallowed
// so the daemon survives a DB hiccup and still runs the others (the next tick re-derives from
// scratch). A cancellation error is left for Run's select to surface (not logged as a
// failure), and a cancellation between passes halts the rest of the tick promptly.
//
// Supervise runs FIRST so the tasks it reclaims to pending — and the worktree slots a dead
// worker frees — are visible to THIS tick's dispatch pass, which re-dispatches them as
// ordinary ready backlog. Recovery is thus reclaim (supervise) + re-dispatch (dispatch): a
// dispatch-disabled run still reclaims and poison-pills, it just defers the restart to a tick
// with dispatch on.
func (sc *Scheduler) runTick(ctx context.Context) {
	if sc.Supervise {
		if n, err := sc.RunSuperviseOnce(ctx); err != nil {
			if ctx.Err() == nil {
				sc.logf("supervise tick error: %v", err)
			}
		} else if n > 0 {
			sc.logf("tick reclaimed %d task(s) for retry", n)
		}
		if ctx.Err() == nil {
			// Idle-nudge runs after reclaim: a worker the reclaim pass just moved off 'active'
			// (a dead window/expired lease) is no longer a nudge candidate, so the two never
			// act on the same task in one tick.
			if n, err := sc.RunNudgeIdleOnce(ctx); err != nil {
				if ctx.Err() == nil {
					sc.logf("idle-nudge tick error: %v", err)
				}
			} else if n > 0 {
				sc.logf("tick nudged %d idle worker(s)", n)
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	if sc.Dispatch {
		if n, err := sc.RunOnce(ctx); err != nil {
			if ctx.Err() == nil {
				sc.logf("tick error: %v", err)
			}
		} else if n > 0 {
			sc.logf("tick dispatched %d task(s)", n)
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Gate runs AFTER dispatch and BEFORE land: dispatch surfaces new work, gate produces the
	// verdict, land consumes it — so a task whose reviewers' reports are all in this tick can be
	// recorded by the gate pass and then landed by the land pass in the SAME tick.
	if sc.Gate {
		if n, err := sc.RunGateOnce(ctx); err != nil {
			if ctx.Err() == nil {
				sc.logf("gate tick error: %v", err)
			}
		} else if n > 0 {
			sc.logf("tick gated %d task(s)", n)
		}
	}
	if ctx.Err() != nil {
		return
	}
	if sc.Land {
		if n, err := sc.RunLandOnce(ctx); err != nil {
			if ctx.Err() == nil {
				sc.logf("land tick error: %v", err)
			}
		} else if n > 0 {
			sc.logf("tick landed %d task(s)", n)
		}
	}
}

// RunOnce performs one scheduling tick and returns how many tasks it dispatched. It
// re-derives ready backlog from the DB and, for each ready task in deterministic
// (created-order) sequence, dispatches it when ALL hold:
//
//   - it declares a footprint (an undeclared footprint cannot be proven disjoint from a
//     concurrently-dispatched sibling, so the scheduler leaves it for the manager);
//   - it has a stored brief (has_brief): with none, the autonomous spawn would strand the
//     worker on the "wait for ttorch send" stub that no daemon send will satisfy, so a
//     briefless task is left for the manager — the same skip-don't-fail treatment as an
//     undeclared footprint;
//   - its footprint is disjoint from every LIVE worker (Fleet.CheckOverlap) AND from every
//     task already claimed THIS tick (which has no live window yet, so CheckOverlap cannot
//     see it);
//   - its repo has free worktree-pool capacity, counting tasks already claimed this tick.
//
// Tasks that overlap or lack capacity are SKIPPED (left pending for a future tick), never
// failed. Each selected task is claimed atomically (Store.ClaimTask: a BEGIN IMMEDIATE tx
// re-checking status='pending') before the heavy dispatch, so two concurrent ticks/instances
// can never double-dispatch — the loser of the claim simply skips. It is a no-op (returns
// 0) when there is no ready work or no free capacity.
func (sc *Scheduler) RunOnce(ctx context.Context) (int, error) {
	ready, err := sc.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusPending},
		ExcludeKind: []string{db.KindCC}, // cc sessions are ad-hoc, lead-driven — never auto-dispatched
	})
	if err != nil {
		return 0, err
	}
	if len(ready) == 0 {
		return 0, nil // nothing ready — the common idle-tick fast path
	}

	live, err := sc.Fleet.Status()
	if err != nil {
		return 0, err
	}

	// Backpressure governor (H4): a machine-load throttle layered ON TOP of the pool cap and
	// the disjoint-footprint invariant below — it only ever dispatches FEWER workers, never
	// more. Checked BEFORE building the occupancy maps so a deferred tick is a cheap fast path.
	// The load ceiling fails OPEN (an unreadable loadavg never defers); the max-active cap is
	// always enforceable. Both leave ready tasks pending (never failed) and LOG the deferral so
	// backpressure is never a silent stall. See governor.go.
	if over, load := sc.loadDefersDispatch(); over {
		sc.logf("dispatch deferred: load %.2f >= ceiling %.2f (%d ready task(s) left pending)", load, sc.LoadCeiling, len(ready))
		return 0, nil
	}
	budget, active := sc.dispatchBudget(live)
	if budget <= 0 {
		sc.logf("dispatch deferred: %d active >= max-active %d (%d ready task(s) left pending)", active, sc.MaxActiveWorkers, len(ready))
		return 0, nil
	}

	// Per-repo free worktree capacity at tick start. In-use accounting mirrors
	// freeSlotsByRepo / Manager.inUseWorktrees: a live task occupies a slot for as long as
	// it HOLDS a worktree (even after its window is gone, before teardown releases it).
	inUse := map[string][]string{}
	for _, t := range live {
		if t.Worktree != "" {
			inUse[t.Project] = append(inUse[t.Project], t.Worktree)
		}
	}
	free := map[string]int{}
	freeOf := func(repo string) int {
		if _, ok := free[repo]; !ok {
			free[repo] = sc.Pool.FreeSlots(inUse[repo])
		}
		return free[repo]
	}

	// Per-repo footprints already "occupied" — owned by a worker that is, or is about to be,
	// on those files. Seeded from every ACTIVE task in the live snapshot (claimed/dispatched
	// workers), then extended with each task claimed THIS tick. This closes the gap that
	// Fleet.CheckOverlap alone leaves: CheckOverlap is liveness-gated (a tmux window must
	// exist), so a task claimed-but-not-yet-spawned — by a prior tick whose dispatch is slow,
	// by a second scheduler instance, or by this tick before its window comes up — is
	// invisible to it. Checking active-task footprints from the snapshot makes the daemon
	// refuse to dispatch overlapping work onto a claim that has not yet materialized a window.
	occupied := map[string][][]string{}
	for _, t := range live {
		if t.Status == db.StatusActive && t.Kind != db.KindCC && len(t.Footprint) > 0 {
			occupied[t.Project] = append(occupied[t.Project], t.Footprint)
		}
	}

	dispatched := 0
	govCapHit := false
	for _, t := range ready {
		select {
		case <-ctx.Done():
			return dispatched, ctx.Err()
		default:
		}
		repo := t.Project
		if repo == "" {
			sc.logf("skip %s: no project repo on the task", t.ID)
			continue
		}
		// The scheduler auto-dispatches only work whose disjointness it can VERIFY. An
		// undeclared footprint can't be proven disjoint from a sibling dispatched the same
		// tick, so it is left for the manager (which has judgment) rather than risked.
		if len(t.Footprint) == 0 {
			sc.logf("skip %s: no declared footprint (left for the manager)", t.ID)
			continue
		}
		// The scheduler auto-dispatches only work whose readiness it can VERIFY. With no
		// stored brief, the autonomous spawn would launch the worker on the "wait for
		// ttorch send" stub — but no `ttorch send` is coming on the daemon path, so the
		// worker would hold a pool slot and go silent until its lease lapsed. Leave a
		// briefless task for the manager (mirroring the footprint skip), exactly as the
		// has_brief column records (set by `task add --brief` / `spawn --brief`).
		if !t.HasBrief {
			sc.logf("skip %s: no stored brief (left for the manager)", t.ID)
			continue
		}
		if freeOf(repo) <= 0 {
			continue // no capacity in this repo right now — try the next ready task
		}
		conflict, err := sc.overlaps(repo, t.Footprint, occupied[repo])
		if err != nil {
			// Fail closed: the overlap gate could not read the board, so disjointness is
			// unproven. Skip this task THIS tick (leave it pending for a future tick) rather
			// than read the read-failure as "no conflict" and dispatch onto possibly-shared
			// files. A whole-board read failure surfaces earlier via Fleet.Status (which aborts
			// the tick); this guards the per-task overlap read that can fail independently.
			sc.logf("skip %s: cannot verify overlap (board read failed): %v", t.ID, err)
			continue
		}
		if conflict {
			continue // overlaps a live or already-claimed worker — skip, leave pending
		}
		// Governor cap (H4): this task is otherwise dispatchable (footprint declared, brief
		// stored, capacity free, disjoint), but the machine-load throttle's per-tick budget is
		// spent. Stop dispatching this tick and leave the rest pending — dispatching FEWER than
		// capacity allows, never more. The deferral is logged after the loop.
		if dispatched >= budget {
			govCapHit = true
			break
		}

		owner := "worker:" + t.ID
		claimed, won, err := sc.Store.ClaimTask(ctx, t.ID, owner)
		if err != nil {
			return dispatched, err
		}
		if !won {
			continue // lost the claim race, or it moved off pending — skip
		}

		scout := claimed.Kind == db.KindScout
		// Dispatch through the manager's spawn path with an empty rawCmd (a harness launch). The
		// worker's initial prompt is the brief stored for this task (by `ttorch task add
		// --brief/--brief-file`), which the spawn reads from the task's brief file — so an
		// autonomously-dispatched worker starts on its FULL brief, not the stub that waits for a
		// manager `ttorch send`. The has_brief gate above guarantees a stored brief here; the
		// autonomous spawn refuses (rather than stub) if it is ever somehow missing.
		if _, err := sc.Fleet.SpawnWithFootprint(claimed.ID, repo, scout, "", claimed.Footprint, false); err != nil {
			// Dispatch failed after the claim. Revert so the task is not a phantom (active +
			// lease, no window): back to pending with the lease cleared. The guarded revert
			// never clobbers a concurrent change; if it declines or errors, the lease still
			// makes the task reclaimable as a backstop.
			sc.logf("dispatch of %s failed: %v", claimed.ID, err)
			if _, rerr := sc.Store.ReleaseClaim(ctx, claimed.ID, owner); rerr != nil {
				sc.logf("could not release claim on %s after failed dispatch: %v", claimed.ID, rerr)
			}
			continue
		}

		// Won and dispatched: it now holds a worktree slot and its footprint is in play for
		// the rest of this tick.
		free[repo]--
		occupied[repo] = append(occupied[repo], claimed.Footprint)
		dispatched++
		sc.logf("dispatched %s to %s (footprint: %s)", claimed.ID, repo, strings.Join(claimed.Footprint, ", "))
	}
	if govCapHit {
		// Backpressure was applied this tick: we filled the max-active budget and left the
		// remaining ready, dispatchable tasks pending so the next tick (or a worker finishing)
		// can pick them up. Logged so the throttle is observable, never a silent stall.
		sc.logf("dispatch deferred: reached max-active %d (%d already active + %d dispatched this tick); remaining ready task(s) left pending", sc.MaxActiveWorkers, active, dispatched)
	}
	return dispatched, nil
}

// RunLandOnce performs one land pass and returns how many tasks it landed. It re-derives the
// tasks awaiting landing from the DB (status done, excluding ad-hoc cc) and, for each that
// already carries a PASSING durable verdict, atomically claims it (Store.ClaimForLand) and
// hands the won set to the manager's EXISTING land pipeline (Fleet.LandSet) — the same code
// `ttorch land` runs: rebase onto the current default, re-validate, carry the verdict+approval
// forward across a clean rebase (or re-gate when the content changed), fast-forward under the
// per-repo lock, and tear down.
//
// It NEVER lands ungated work. Two independent guards uphold that:
//
//   - the candidate pre-filter requires a passing verdict ROW to even attempt a task (a missing
//     or blocking verdict is skipped here — never claimed, never handed to LandSet); and
//   - LandSet's own trust gate (a fresh, COMMIT-PINNED passing verdict plus a valid approval,
//     consumed only at the fast-forward) remains the authority that decides WHETHER each task
//     merges. The pass reuses that gate — it does not weaken, bypass, or reimplement it — and
//     passes requireVerdict=true so the verdict is required regardless of the repo's mode.
//
// So a task whose verdict is missing or blocking is skipped, and one whose verdict is stale and
// not carryable is refused by LandSet, leaving the task safely done and un-landed; either way
// the claim is released for a later retry. The verdict's FRESHNESS (still covering the merged
// commit, or carrying across a clean rebase) is deliberately left to LandSet — pre-filtering on
// reviewed_sha here would forfeit exactly the stale-base disjoint tasks LandSet can carry and
// land cleanly.
//
// Concurrency: each task is claimed atomically before it is landed, so two concurrent
// ticks/instances can never both land the same task — the loser of the claim simply skips.
// LandSet bounds its own fan-out and serializes the fast-forward per repo, so landing is
// capacity-bounded and per-repo-serialized exactly as a hand-run `ttorch land --all` is. It is
// a no-op (returns 0) when nothing awaits landing or nothing carries a passing verdict.
func (sc *Scheduler) RunLandOnce(ctx context.Context) (int, error) {
	done, err := sc.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusDone},
		ExcludeKind: []string{db.KindCC}, // cc sessions are ad-hoc, lead-driven — never auto-landed
	})
	if err != nil {
		return 0, err
	}
	if len(done) == 0 {
		return 0, nil // nothing awaiting landing — the common idle-tick fast path
	}

	// Claim each gated candidate atomically, then hand the won set to LandSet as one batch so
	// its concurrent pipeline (per-repo locks, bounded fan-out) lands them as efficiently as
	// `ttorch land --all`. owners records the claim owner per task so a release targets exactly
	// the claim it took. release uses context.Background() deliberately: a claim must always be
	// freed even when the tick's own ctx is the reason we are unwinding (a SIGTERM cancel), so a
	// half-claimed batch never strands done tasks until the lease lapses.
	var ids []string
	landDeferred := false
	owners := map[string]string{}
	release := func(id string) {
		if owner := owners[id]; owner != "" {
			if _, rerr := sc.Store.ReleaseLandClaim(context.Background(), id, owner); rerr != nil {
				sc.logf("could not release land claim on %s: %v", id, rerr)
			}
		}
	}
	releaseAll := func() {
		for _, id := range ids {
			release(id)
		}
	}
	for _, t := range done {
		select {
		case <-ctx.Done():
			releaseAll() // free this tick's claims so a partial batch never strands on cancel
			return 0, ctx.Err()
		default:
		}
		// Fail safe: only land work that already carries a PASSING verdict. Gating stays the
		// authority that produces the verdict; this pass automates only the land once one
		// exists. A missing or blocking verdict is never landed — and never even claimed.
		v, ok, err := sc.Store.GetVerdict(ctx, t.ID)
		if err != nil {
			releaseAll()
			return 0, err
		}
		if !ok || v.Overall != review.Pass {
			continue
		}
		// Bounded land fan-out (H4): cap how many gated tasks this tick hands to the concurrent
		// land pipeline, so a burst of gated work cannot launch many heavy (offloaded) validate
		// suites at once. The remaining gated tasks land on later ticks (the board is re-derived
		// each tick). A cap of <= 0 disables the bound (today's behavior — claim the whole set).
		if sc.landCapReached(len(ids)) {
			landDeferred = true
			break
		}
		owner := "lander:" + t.ID
		won, err := sc.Store.ClaimForLand(ctx, t.ID, owner)
		if err != nil {
			releaseAll()
			return 0, err
		}
		if !won {
			continue // another tick/instance is landing it, or it moved off done — skip
		}
		ids = append(ids, t.ID)
		owners[t.ID] = owner
	}
	if landDeferred {
		// Backpressure was applied: we capped this tick's land fan-out and left the remaining
		// gated tasks for a later tick. Logged so the bound is observable, never a silent stall.
		sc.logf("land deferred: capped at %d task(s) this tick; remaining gated work left for a later tick", sc.MaxLandConcurrency)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// requireVerdict=true forces the verdict gate regardless of the repo's delivery mode, so a
	// local/validated repo can never auto-land ungated through this pass; in trusted mode it is
	// implied. The authoritative gate (verdict freshness/commit-pin + approval) lives inside
	// LandSet/MergeLocal and is never bypassed here.
	landed := 0
	for _, r := range sc.Fleet.LandSet(ctx, ids, true) {
		if r.Err != nil {
			// Did not land (a stale/uncarryable verdict, a rebase conflict, a refused merge, or
			// a lost fast-forward race): leave it safely done and release the claim so a later
			// tick can retry once the obstruction clears.
			sc.logf("land of %s did not complete: %v", r.TaskID, r.Err)
			release(r.TaskID)
			continue
		}
		landed++
		sc.logf("landed %s to %s", r.TaskID, r.Repo)
	}
	return landed, nil
}

// RunSuperviseOnce performs one supervision pass and returns how many dead workers' tasks it
// reclaimed to pending (the re-dispatchable count). It AUTO-RECOVERS workers that have
// VERIFIABLY died, by two independent signals — never pane-output inference:
//
//   - WINDOW GONE (fast path, Store.ReclaimWindowGone): a worker whose tmux window the
//     watcher confirmed absent (a crash). Recovered promptly, without waiting out the lease.
//   - LEASE EXPIRED (backstop, Store.ReclaimExpiredLeases): a worker that has not extended its
//     lease within the lease window — the universal signal that needs no watcher running.
//
// Both reclaim through the SAME bounded transition: a task under its retry ceiling goes back
// to pending (retry_count++), so the dispatch pass re-dispatches it onto a fresh, clean
// worktree via the manager's spawn path; a task that has EXCEEDED the ceiling — including one
// that repeatedly kills its worker, since each death consumes a retry — is poison-pilled to
// the terminal 'failed' status with an actionable event for the lead, and is NOT re-dispatched.
// The retry ceiling is the restart-storm bound (mirroring Sidekiq's max-retries ⇒ dead set):
// a flapping task can only burn its bounded retries before it goes terminal, never loop forever.
//
// It does not reclaim a worker that has shown a SIGN OF LIFE. Each reclaim's verifiable signal
// IS a liveness signal — a confirmed-gone window with no later sign of life (no re-dispatch,
// no heartbeat), or a lapsed lease a heartbeat would have extended — re-checked under the
// per-task write lock, so a worker still heartbeating, re-dispatched, or resumed-and-reporting
// since the scan is left untouched (see Store.ReclaimWindowGone for the sign-of-life anchor and
// the one residual `ttorch resume` co-running boundary). And the pass does not itself
// re-dispatch: it only moves a dead worker's task to pending or failed, leaving the actual
// restart to the dispatch pass (which acts on pending tasks only) — so a still-active worker is
// never re-spawned. Reclaiming is idempotent and safe across instances (the atomic per-task
// claims in the db layer), so two ticks/instances can never double-reclaim. It is a no-op
// (returns 0) when nothing has died.
func (sc *Scheduler) RunSuperviseOnce(ctx context.Context) (int, error) {
	// Window-gone first: a confirmed-gone window is the promptest death signal, so recover it
	// without waiting out the (much longer) lease.
	gone, err := sc.Store.ReclaimWindowGone(ctx)
	if err != nil {
		return 0, err
	}
	reclaimed := sc.logReclaims(gone, "window gone")
	if err := ctx.Err(); err != nil {
		return reclaimed, err
	}
	// Lease-expiry backstop: catches a worker that died without its window vanishing (a hung
	// process), or any run with no watcher emitting window_gone events.
	expired, err := sc.Store.ReclaimExpiredLeases(ctx)
	if err != nil {
		return reclaimed, err
	}
	reclaimed += sc.logReclaims(expired, "lease expired")
	return reclaimed, nil
}

// logReclaims logs each reclaim outcome — a retry reclaim or a terminal poison-pill — and
// returns how many were reclaimed to pending (re-dispatchable). Poison-pilled tasks are
// terminal and excluded from the count (they are not re-dispatched).
func (sc *Scheduler) logReclaims(outcomes []db.ReclaimOutcome, cause string) int {
	reclaimed := 0
	for _, oc := range outcomes {
		if oc.Failed {
			sc.logf("poison-pilled %s to failed (%s; retries exhausted)", oc.TaskID, cause)
			continue
		}
		sc.logf("reclaimed %s for retry %d (%s)", oc.TaskID, oc.RetryCount, cause)
		reclaimed++
	}
	return reclaimed
}

// RunNudgeIdleOnce performs one idle-nudge pass and returns how many alive-but-idle workers
// it nudged. It is the alive-but-idle complement to RunSuperviseOnce's verifiable-death
// recovery (§roadmap H2): a worker can be perfectly alive — its tmux window present, its lease
// valid — yet sit IDLE at the input prompt because its harness turn simply ended at an empty
// prompt (a finished sub-step waiting for input that, in the autonomous path, no human will
// type). Such a worker holds its pool slot and lease for the FULL lease window while making no
// progress. This pass nudges it ONCE per idle episode with "continue" — the same resume a
// manager would `ttorch send` by hand — so the autonomous fleet self-heals instead of
// stranding the slot until the lease finally lapses.
//
// It is deliberately conservative; a worker is nudged ONLY when ALL hold (see nudgeIfIdle):
//
//   - it is status 'active' — so a worker that reported blocked/needs_input/done (a decision
//     the MANAGER owns) is out of scope, never nudged — and non-cc (the lead-driven sessions
//     are not the scheduler's to drive), and not the manager window;
//   - it holds a VALID lease (an expired/absent lease is the reclaim pass's job, not a nudge's)
//     and has reported progress at least once (a last_progress_at to time the idle stretch);
//   - its pane is observable AND idle at the prompt (livestate.Idle: NOT busy AND showing the
//     input caret), and NOT showing a non-recoverable API error — a working, half-rendered,
//     crashed-to-shell, or rate-limited/auth-failed pane is never injected into;
//   - it has been observed idle at the SAME pane for IdleConfirmations consecutive ticks (the
//     stability gate), so a single transient not-busy frame of a working worker never fires;
//   - it has been idle past IdleNudgeGrace (timed from the later of last_progress_at and the
//     last nudge, so a brief inter-step pause and a just-nudged worker are both spared);
//   - this exact idle screen has not already been nudged (pane hash differs from the last
//     idle_nudged) and the consecutive-nudge budget (MaxIdleNudges) is not spent.
//
// The decision-and-record step is ATOMIC: Store.ClaimIdleNudge re-derives the predicate and
// records the idle_nudged event inside one BEGIN IMMEDIATE transaction (mirroring ClaimTask /
// the reclaim paths), re-reading status + lease under the write lock. That is what makes
// "once per episode" and the bounded budget hold across a daemon restart AND across two
// instances against one DB, and what closes the race where a worker reports
// blocked/needs_input/done between this pass's active-task snapshot and the send. The pass is
// a no-op (returns 0) when disabled (MaxIdleNudges <= 0), when nothing is active, or when
// nothing is idle. A per-task pane-capture or send failure is non-fatal (that worker is
// skipped); only a DB error aborts the pass, which the next tick re-derives from scratch.
func (sc *Scheduler) RunNudgeIdleOnce(ctx context.Context) (int, error) {
	if sc.MaxIdleNudges <= 0 {
		return 0, nil // idle-nudge disabled
	}
	tasks, err := sc.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusActive},
		ExcludeKind: []string{db.KindCC}, // cc sessions are lead-driven — never auto-nudged
	})
	if err != nil {
		return 0, err
	}
	if sc.idleSeen == nil {
		sc.idleSeen = make(map[string]idleObservation)
	}
	now := sc.clock()
	nudged := 0
	active := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		active[t.ID] = true
		if err := ctx.Err(); err != nil {
			return nudged, err
		}
		ok, err := sc.nudgeIfIdle(ctx, t, now)
		if err != nil {
			return nudged, err
		}
		if ok {
			nudged++
		}
	}
	// Prune stability state for tasks that have left the active set (done/failed/reclaimed), so
	// the in-memory map never grows without bound across a long-running daemon.
	for id := range sc.idleSeen {
		if !active[id] {
			delete(sc.idleSeen, id)
		}
	}
	return nudged, nil
}

// nudgeIfIdle evaluates ONE active task and nudges it when it is alive-but-idle past the grace
// period (see RunNudgeIdleOnce for the full predicate). It returns whether it sent a nudge. A
// DB read/write error is returned (it aborts the pass, like the reclaim paths); a pane-capture
// or send failure is non-fatal — that worker is skipped (a failed send is logged) so one dead
// window never stalls nudging the rest of the fleet.
//
// The pane-shape checks (idle/error/stability) run here, from the live capture; the
// status/lease/grace/budget decision and the record run under a write lock in
// Store.ClaimIdleNudge, so the act of nudging is atomic and a worker that changed status since
// the snapshot is dropped.
func (sc *Scheduler) nudgeIfIdle(ctx context.Context, t db.Task, now time.Time) (bool, error) {
	// Never the manager window or a window-less task — only the autonomous worker fleet.
	if t.Window == "" || t.Window == managerWindow {
		return false, nil
	}
	// Valid lease only: an expired/absent lease is the reclaim pass's domain, not a nudge's.
	// (ClaimIdleNudge re-checks this under the lock; this is the cheap pre-filter.)
	if t.LeaseExpiresAt == nil || !t.LeaseExpiresAt.After(now) {
		return false, nil
	}
	// Need a progress anchor to time the idle stretch from. A worker that has never reported is
	// also almost certainly still ramping up (busy), so deferring it costs nothing.
	if t.LastProgressAt == nil {
		return false, nil
	}
	pane, err := sc.Fleet.Peek(t.ID, idleCaptureLines)
	if err != nil {
		sc.forgetIdle(t.ID) // unobservable — reset stability so a later idle run starts fresh
		return false, nil   // window gone/unreadable — reclaim owns dead windows
	}
	if !livestate.Idle(pane) || nonRecoverableError(pane) {
		// Busy/working, not at the input prompt, or showing a non-recoverable API error
		// (rate-limit/auth) that "continue" cannot fix — never inject, and reset the stability
		// count so a transient idle frame must re-stabilize before a nudge.
		sc.forgetIdle(t.ID)
		return false, nil
	}
	h := hashPane(pane)
	// Stability gate: require the SAME idle screen across IdleConfirmations consecutive ticks
	// before acting, so a single transient not-busy capture of a working worker never nudges.
	if !sc.idleConfirmed(t.ID, h) {
		return false, nil
	}
	// Atomic claim: ClaimIdleNudge re-reads status + lease under a BEGIN IMMEDIATE write lock,
	// re-derives the grace/budget/already-nudged decision, and records the event — so two
	// instances never double-nudge and a worker that changed status since the snapshot is
	// dropped. The event is recorded BEFORE the send (record-then-send); a send that then fails
	// leaves the worker un-nudged for this screen rather than risking a re-nudge storm.
	decision, count, err := sc.Store.ClaimIdleNudge(ctx, t.ID, h, now, sc.IdleNudgeGrace, sc.MaxIdleNudges)
	if err != nil {
		return false, err
	}
	switch decision {
	case db.IdleNudgeGiveUp:
		sc.logf("idle worker %s: nudged %dx, no progress — leaving for manager", t.ID, count)
		return false, nil
	case db.IdleNudgeSend:
		if err := sc.Fleet.Send(t.ID, idleNudgeText); err != nil {
			sc.logf("idle worker %s: nudge send failed (recorded; not re-nudged this screen): %v", t.ID, err)
			return false, nil
		}
		sc.logf("nudged idle worker %s (idle %s)", t.ID, now.Sub(*t.LastProgressAt).Round(time.Second))
		return true, nil
	default:
		return false, nil
	}
}

// idleConfirmed records one observation that task id is idle at pane hash h, and reports
// whether it has now been observed idle at the SAME hash for IdleConfirmations consecutive
// ticks (the stability gate). A changed hash restarts the count at 1.
func (sc *Scheduler) idleConfirmed(id, h string) bool {
	obs := sc.idleSeen[id]
	if obs.hash == h {
		obs.count++
	} else {
		obs = idleObservation{hash: h, count: 1}
	}
	sc.idleSeen[id] = obs
	return obs.count >= sc.idleConfirmations()
}

// forgetIdle clears a task's idle-stability state — called when it is observed not-idle (or
// unobservable) so the next idle stretch must re-stabilize before a nudge.
func (sc *Scheduler) forgetIdle(id string) {
	delete(sc.idleSeen, id)
}

// idleConfirmations is the consecutive-observation threshold, with a floor of 1 (so a bare
// struct that sets only grace/max still nudges on the first idle observation).
func (sc *Scheduler) idleConfirmations() int {
	if sc.IdleConfirmations < 1 {
		return 1
	}
	return sc.IdleConfirmations
}

// nonRecoverableError reports whether an idle pane shows an API error that a "continue" nudge
// cannot fix — a rate limit, auth failure, or the like. It deliberately EXCLUDES the harness's
// self-recoverable mid-stream stalls (livestate.Stalled), which "continue" DOES fix, so the
// scheduler's exclusion matches the watcher's auto-resume inclusion: the two agree on what is
// safely resumable. A worker showing a non-recoverable error is left for the manager.
func nonRecoverableError(pane string) bool {
	return strings.Contains(strings.ToLower(pane), "api error") && !livestate.Stalled(pane)
}

// clock returns the scheduler's current time (time.Now unless a test injected sc.now).
func (sc *Scheduler) clock() time.Time {
	if sc.now != nil {
		return sc.now()
	}
	return time.Now()
}

// hashPane returns a stable hex digest of a captured pane, used to recognize whether an idle
// worker's screen has changed since its last nudge. The digest is hex (never the word
// "capped"), so it can never collide with db.IdleNudgeCappedMarker.
func hashPane(pane string) string {
	sum := sha256.Sum256([]byte(pane))
	return hex.EncodeToString(sum[:])
}

// overlaps reports whether footprint conflicts with any LIVE worker (Fleet.CheckOverlap,
// the manager's gate, which catches live-windowed workers of any status) or with any
// already-occupied footprint (active claims, with or without a window yet, plus tasks
// claimed earlier this tick). The occupied set is checked with the same pure predicate the
// manager's overlap core uses (state.FootprintOverlap), so the daemon's disjointness
// matches the manager's exactly. Callers only reach this with a non-empty footprint.
//
// It returns the CheckOverlap read error unchanged (rather than collapsing it to false):
// a board-read failure means disjointness is UNPROVEN, and the caller must fail closed
// (skip the task) rather than treat an empty conflict list as "safe to dispatch".
func (sc *Scheduler) overlaps(repo string, footprint []string, occupied [][]string) (bool, error) {
	conflicts, err := sc.Fleet.CheckOverlap(repo, footprint)
	if err != nil {
		return false, err
	}
	if len(conflicts) > 0 {
		return true, nil
	}
	for _, fp := range occupied {
		if len(state.FootprintOverlap(footprint, fp)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (sc *Scheduler) logf(format string, args ...any) {
	if sc.Log == nil {
		return
	}
	fmt.Fprintf(sc.Log, "scheduler: "+format+"\n", args...)
}

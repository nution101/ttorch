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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/worktree"
)

// DefaultInterval is the tick cadence when none is configured: re-derive ready backlog and
// dispatch every 5s. Generous enough that a tick is cheap and the DB is barely touched when
// nothing is ready, tight enough that freshly-added disjoint backlog dispatches promptly.
const DefaultInterval = 5 * time.Second

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
	// the same deterministic gate the manager's spawn uses.
	CheckOverlap(repo string, proposed []string) []orchestrator.Conflict
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
	}
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

// runTick runs one tick of each enabled pass (supervise, then dispatch, then land) and logs
// the outcome. The passes are independent: a transient error in one is logged and swallowed
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
		if freeOf(repo) <= 0 {
			continue // no capacity in this repo right now — try the next ready task
		}
		if sc.overlaps(repo, t.Footprint, occupied[repo]) {
			continue // overlaps a live or already-claimed worker — skip, leave pending
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
		// manager `ttorch send`. A task with no stored brief falls back to the stub, unchanged.
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

// overlaps reports whether footprint conflicts with any LIVE worker (Fleet.CheckOverlap,
// the manager's gate, which catches live-windowed workers of any status) or with any
// already-occupied footprint (active claims, with or without a window yet, plus tasks
// claimed earlier this tick). The occupied set is checked with the same pure predicate the
// manager's overlap core uses (state.FootprintOverlap), so the daemon's disjointness
// matches the manager's exactly. Callers only reach this with a non-empty footprint.
func (sc *Scheduler) overlaps(repo string, footprint []string, occupied [][]string) bool {
	if len(sc.Fleet.CheckOverlap(repo, footprint)) > 0 {
		return true
	}
	for _, fp := range occupied {
		if len(state.FootprintOverlap(footprint, fp)) > 0 {
			return true
		}
	}
	return false
}

func (sc *Scheduler) logf(format string, args ...any) {
	if sc.Log == nil {
		return
	}
	fmt.Fprintf(sc.Log, "scheduler: "+format+"\n", args...)
}

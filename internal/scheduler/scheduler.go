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
// This phase DISPATCHES only. Autonomous gating/merging and a supervisor are deliberate
// follow-on phases. The daemon is OPT-IN: nothing starts it automatically (see
// `ttorch scheduler`); existing manager/worker behavior is unchanged unless it is run.
//
// Concurrency model: the scheduler is self-consistent and safe to run as multiple
// instances against one DB — the atomic claim (db.ClaimTask: BEGIN IMMEDIATE + status
// re-check) makes double-claim impossible, and the overlap check consults active claims
// (not just live tmux windows) so two instances never dispatch overlapping work. The one
// boundary in this phase: a HUMAN/LLM manager running `ttorch spawn` for the exact id the
// scheduler has just claimed (active, window not yet up) won't see that claim in its own
// liveness-gated overlap gate; the worst case is a window-name collision that fails one of
// the two spawns (never a silent double-dispatch). Full manager↔scheduler co-running
// coordination — including auto-starting the scheduler from the manager — is a later,
// deliberate phase; until then, run the scheduler as the dispatcher OR drive spawns from
// the manager, not both onto the same ids.
package scheduler

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
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

// runTick runs one tick and logs the outcome. A cancellation error is left for Run's select
// to surface (not logged as a failure); any other error is logged and swallowed so the
// daemon survives a transient DB hiccup.
func (sc *Scheduler) runTick(ctx context.Context) {
	n, err := sc.RunOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			sc.logf("tick error: %v", err)
		}
		return
	}
	if n > 0 {
		sc.logf("tick dispatched %d task(s)", n)
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

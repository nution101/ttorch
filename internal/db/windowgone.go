package db

import (
	"context"
	"database/sql"
	"time"
)

// ReclaimWindowGone is the verifiable window-gone recovery primitive (§roadmap A phase 3) —
// the FAST-PATH complement to the slower lease-expiry sweep (ReclaimExpiredLeases). It finds
// every active, non-cc task whose CURRENT incarnation's tmux window has been confirmed gone
// and applies the SAME bounded retry/poison-pill transition (applyReclaimTransitionTx):
// retry_count < max_retries ⇒ back to pending (retry++, a worker_reclaimed event);
// retry_count >= max_retries ⇒ the terminal 'failed' status (poison-pill, a task_failed
// event). It NEVER re-dispatches itself — it only reclaims; the dispatch pass re-dispatches
// the reclaimed-pending task.
//
// "Window gone" is NOT pane-output inference: the signal is a durable window_gone EVENT,
// recorded by the watcher's liveness poll (internal/watch) on a window it VERIFIED absent
// (tmux.WindowExistsErr distinguishes a genuinely-gone window from a transient read hiccup,
// so a hiccup never reads as gone). The event is consumed only when it is the task's LATEST
// SIGN OF LIFE OR DEATH — newer (higher events.id) than every later 'spawned' event (a
// re-dispatch) AND every later worker-authored event (a heartbeat: the worker's own
// report/stage, actor worker:<id>). So the supervisor reclaims a worker only when it has
// shown NO sign of life since its window was flagged gone — see hasFreshWindowGoneTx. A
// window_gone left over from a prior incarnation that was re-dispatched (a newer 'spawned'),
// OR a worker that has since reported progress (a fresh dispatch, or a `ttorch resume` the
// worker has resumed reporting from), therefore sorts before that later sign of life and is
// ignored. The id-ordering anchor (not a timestamp) is immune to clock skew.
//
// BOUNDARY: a worker rebuilt IN PLACE by `ttorch resume` (orchestrator.restore) that has not
// yet reported since the resume leaves no DB sign of life — restore writes no event — so for
// that brief window a stale window_gone still reads as current and the supervisor could
// reclaim the now-live worker. Running --supervise alongside manual resumes of the SAME tasks
// is the manager↔scheduler co-running boundary deferred to a later phase (see the scheduler
// package doc); a follow-on tracks having the resume path refresh this anchor.
//
// Concurrency / idempotency: each per-task transaction RE-VERIFIES under the write lock that
// the task is STILL active AND the window_gone signal is STILL current (no newer sign of
// life). A re-dispatch or a heartbeat that landed between the candidate scan and the lock —
// by a second instance, or by this tick's own dispatch pass — appends a newer 'spawned' /
// worker event and so cancels the reclaim (changed=false). Two concurrent ticks/instances
// therefore can never both reclaim a task, and never reclaim a worker that has since shown a
// sign of life. It returns one ReclaimOutcome per task it actually transitioned (a no-op
// returns none).
func (s *Store) ReclaimWindowGone(ctx context.Context) ([]ReclaimOutcome, error) {
	now := s.now()
	// Pre-filter to genuine candidates in one read so the (write-locking) per-task
	// transactions run only for tasks that actually carry a current-incarnation window_gone.
	// EXISTS over events, anchored on the latest sign-of-life id, mirrors hasFreshWindowGoneTx
	// EXACTLY (the two must agree). Drain + close the rows BEFORE any per-task write
	// transaction — under SetMaxOpenConns(1) the open rows hold the only connection a
	// transaction would need.
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id FROM tasks t
		 WHERE t.status = ? AND t.kind != ?
		   AND EXISTS (
		       SELECT 1 FROM events e
		        WHERE e.entity_type = 'task' AND e.entity_id = t.id AND e.type = ?
		          AND e.id > COALESCE((SELECT MAX(sp.id) FROM events sp
		                                WHERE sp.entity_type = 'task' AND sp.entity_id = t.id
		                                  AND (sp.type = ? OR sp.actor GLOB 'worker:*')), 0))
		 ORDER BY t.id`,
		StatusActive, KindCC, EventWindowGone, EventSpawned)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []ReclaimOutcome
	for _, id := range ids {
		oc, changed, err := s.reclaimWindowGoneTask(ctx, id, now)
		if err != nil {
			return out, err
		}
		if changed {
			out = append(out, oc)
		}
	}
	return out, nil
}

// reclaimWindowGoneTask transitions one confirmed-window-gone task in a single short
// transaction. It re-reads the task under the write lock and is a no-op (changed=false)
// unless the task is STILL active AND its window_gone signal is STILL current — the
// verifiable guard against a concurrent re-dispatch (which appends a newer 'spawned' event)
// or any status move since the candidate scan.
func (s *Store) reclaimWindowGoneTask(ctx context.Context, id string, now time.Time) (ReclaimOutcome, bool, error) {
	var oc ReclaimOutcome
	changed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		var retry, maxRetries int
		err := tx.QueryRowContext(ctx,
			`SELECT status, retry_count, max_retries FROM tasks WHERE id = ?`, id).
			Scan(&status, &retry, &maxRetries)
		if err == sql.ErrNoRows {
			return nil // vanished between scan and lock — nothing to do
		}
		if err != nil {
			return err
		}
		// Verifiable guard: only an active task is reclaimable. A reclaim or status move
		// since the scan (it is now pending/done/failed/…) cancels this one.
		if status != StatusActive {
			return nil
		}
		// And the window_gone must still be the worker's LATEST sign of life: a re-dispatch (a
		// newer 'spawned' event) OR a heartbeat (a newer worker-authored event) landing since
		// the scan means the worker is live again and must NOT be reclaimed.
		fresh, err := hasFreshWindowGoneTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		oc, err = applyReclaimTransitionTx(ctx, tx, now, id, retry, maxRetries, EventWorkerReclaimed, "window gone (worker crashed)")
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	return oc, changed, err
}

// hasFreshWindowGoneTx reports whether taskID carries a window_gone event that is its LATEST
// sign of life or death — one with a higher events.id than every later 'spawned' event (a
// re-dispatch) AND every later worker-authored event (a heartbeat: actor worker:<id>), or
// any window_gone when there is no such sign at all (COALESCE(...,0)). Equivalently: the
// worker has shown NO sign of life since the window was flagged gone. The id-ordering anchor
// (not a timestamp) is clock-skew-proof. Tracking worker heartbeats — not just 'spawned' —
// is what keeps a worker that has resumed reporting (e.g. after a `ttorch resume` that
// rebuilt its window, or after a re-dispatch whose best-effort 'spawned' append was dropped)
// from being reclaimed as dead. It takes a queryer so the per-task reclaim re-checks it
// inside its own write transaction.
func hasFreshWindowGoneTx(ctx context.Context, q queryer, taskID string) (bool, error) {
	var exists int64
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM events e
			 WHERE e.entity_type = 'task' AND e.entity_id = ? AND e.type = ?
			   AND e.id > COALESCE((SELECT MAX(sp.id) FROM events sp
			                         WHERE sp.entity_type = 'task' AND sp.entity_id = ?
			                           AND (sp.type = ? OR sp.actor GLOB 'worker:*')), 0))`,
		taskID, EventWindowGone, taskID, EventSpawned).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists != 0, nil
}

package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DefaultLeaseDuration is how far in the future a granted or extended lease expires. It
// must comfortably exceed the longest plausible gap between a worker's progress
// heartbeats — and a Claude worker can spend many minutes inside a single step (deep
// reasoning, a long `make test`, a slow tool call) without emitting a report/stage/note.
// This mirrors Celery's "visibility timeout must exceed your longest task" rule: set it
// too short and a still-working worker's lease lapses and its task is wrongly reclaimed;
// generous is the safe direction, because the cost of a too-long lease is only that a
// genuinely dead worker's task waits a little longer before reclaim. Two hours leaves a
// wide margin over any single worker step while still bounding how long a dead worker can
// hold a task.
const DefaultLeaseDuration = 2 * time.Hour

// DefaultMaxRetries bounds how many times a task may be reclaimed after a lease expiry
// before it is poison-pilled to the terminal StatusFailed instead of re-dispatched
// (bounded retry — never an infinite loop). It must match the max_retries column default
// in migration 0003 so a row created without an explicit ceiling and a row inserted by
// the migration agree.
const DefaultMaxRetries = 3

// leaseExtendTx pushes a HELD lease's expiry to now+DefaultLeaseDuration. It is a no-op
// for a task that holds no lease (empty lease_owner), so the worker progress path
// (ReportStatus/SetStage) can call it unconditionally. It takes a queryer so those paths
// run it inside their own transaction — the heartbeat then commits atomically with the
// progress write. It deliberately does not touch updated_at (the caller's own UPDATE
// already advanced it) and does not require a row (the caller already confirmed the task).
func leaseExtendTx(ctx context.Context, q queryer, now time.Time, taskID string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE tasks
		    SET lease_expires_at = CASE WHEN lease_owner != '' THEN ? ELSE lease_expires_at END
		  WHERE id = ?`,
		formatTime(now.Add(DefaultLeaseDuration)), taskID)
	return err
}

// GrantLease assigns owner the lease on a task and arms its expiry to
// now+DefaultLeaseDuration, incrementing attempt (a fresh dispatch). It is the primitive
// the dispatch path calls when a task becomes active/owned (see orchestrator.Spawn), so a
// live worker always holds a lease that the reclaim sweep can later judge. No event — it
// is dispatch bookkeeping, like SetTaskFields/SetLiveness; the spawn path emits the
// 'spawned' audit event.
func (s *Store) GrantLease(ctx context.Context, taskID, owner string) error {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		    SET lease_owner = ?, lease_expires_at = ?, attempt = attempt + 1, updated_at = ?
		  WHERE id = ?`,
		owner, formatTime(now.Add(DefaultLeaseDuration)), formatTime(now), taskID)
	if err != nil {
		return err
	}
	return requireRows(res, "task "+taskID)
}

// ExtendLease is the standalone heartbeat primitive: it pushes a held lease's expiry
// forward (no-op when no lease is held). The worker progress path extends the lease
// inline (atomically with its status/stage write); ExtendLease is the explicit
// heartbeat for any future caller that renews a lease without other state change.
func (s *Store) ExtendLease(ctx context.Context, taskID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := leaseExtendTx(ctx, tx, s.now(), taskID); err != nil {
			return err
		}
		return nil
	})
}

// ReclaimOutcome reports what ReclaimExpiredLeases did to one task.
type ReclaimOutcome struct {
	TaskID     string
	Failed     bool // true ⇒ poison-pilled to StatusFailed; false ⇒ returned to StatusPending for retry
	RetryCount int  // retry_count after the transition
}

// ReclaimExpiredLeases is the verifiable stall-recovery primitive (§roadmap 2). It finds
// every active task whose lease has GENUINELY expired (lease_expires_at < now) and, in a
// short per-task write transaction, transitions it:
//
//   - retry_count < max_retries ⇒ back to StatusPending (re-dispatchable), retry_count++,
//     lease cleared, with an actionable 'lease_expired' event; or
//   - retry_count >= max_retries ⇒ the terminal StatusFailed (poison-pill / bounded
//     retry — never re-dispatched forever), lease cleared, with an actionable
//     'task_failed' event.
//
// Reclaim is never inference: a lease expiry is ground truth that the worker is gone, and
// each per-task transaction RE-READS the lease under its write lock, reclaiming only if
// the lease is STILL expired — so a heartbeat that landed between the scan and the
// transaction (extending the lease) safely cancels the reclaim. Per-task transactions
// keep each write short (SQLite is single-writer and has no SKIP LOCKED). It returns one
// ReclaimOutcome per task it actually transitioned.
func (s *Store) ReclaimExpiredLeases(ctx context.Context) ([]ReclaimOutcome, error) {
	now := s.now()
	// Scan active leased tasks and decide expiry IN GO (expiry.Before(now)) — the SAME
	// comparison the per-task re-check uses, so the two layers can never disagree. A SQL
	// `lease_expires_at < ?` predicate would instead byte-compare the stored RFC3339Nano
	// strings, which diverges from Go's time comparison at whole-second boundaries:
	// RFC3339Nano trims trailing-zero fractions, so "…00Z" sorts AFTER "…00.5Z" ('Z' >
	// '.') and a genuinely-expired lease would be skipped until now crossed the next whole
	// second. Read the candidates fully (draining + closing the rows) before any per-task
	// write transaction — required under SetMaxOpenConns(1), where the open rows hold the
	// only connection the transaction needs.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, lease_expires_at FROM tasks
		  WHERE status = ? AND lease_expires_at IS NOT NULL
		  ORDER BY id`,
		StatusActive)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id, expires string
		if err := rows.Scan(&id, &expires); err != nil {
			rows.Close()
			return nil, err
		}
		expiry, err := parseTime(expires)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if expiry.Before(now) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []ReclaimOutcome
	for _, id := range ids {
		oc, changed, err := s.reclaimTask(ctx, id, now)
		if err != nil {
			return out, err
		}
		if changed {
			out = append(out, oc)
		}
	}
	return out, nil
}

// reclaimTask transitions one expired-lease task in a single short transaction. It
// re-reads the lease under the write lock and is a no-op (changed=false) unless the task
// is still active with a lease that is still expired — the verifiable guard against a
// concurrent heartbeat. It clears only the lease fields (lease_owner/lease_expires_at),
// leaving the assignment field (owner) and worktree/window for the re-dispatch path: the
// dedicated lease columns exist precisely so reclaim manipulates lease state alone.
func (s *Store) reclaimTask(ctx context.Context, id string, now time.Time) (ReclaimOutcome, bool, error) {
	var oc ReclaimOutcome
	changed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status, leaseOwner string
		var leaseExpires sql.NullString
		var retry, maxRetries int
		err := tx.QueryRowContext(ctx,
			`SELECT status, lease_owner, lease_expires_at, retry_count, max_retries FROM tasks WHERE id = ?`, id).
			Scan(&status, &leaseOwner, &leaseExpires, &retry, &maxRetries)
		if err == sql.ErrNoRows {
			return nil // vanished between scan and lock — nothing to do
		}
		if err != nil {
			return err
		}
		// Verifiable guard: reclaim only an active task whose lease is real and still
		// expired. A heartbeat that extended the lease (or any status move) since the scan
		// cancels the reclaim.
		if status != StatusActive || !leaseExpires.Valid {
			return nil
		}
		expiry, err := parseTime(leaseExpires.String)
		if err != nil {
			return err
		}
		if !expiry.Before(now) {
			return nil
		}
		oc, err = applyReclaimTransitionTx(ctx, tx, now, id, retry, maxRetries, EventLeaseExpired, "lease expired")
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	return oc, changed, err
}

// applyReclaimTransitionTx performs the bounded retry/poison-pill transition for a task the
// caller has ALREADY verified dead (still active, with the death signal — an expired lease
// or a confirmed-gone window — re-checked under THIS write lock). It is the shared core of
// the two recovery sweeps (lease-expiry: reclaimTask; window-gone: reclaimWindowGoneTask),
// so both bound retries and poison-pill identically:
//
//   - retry_count < max_retries ⇒ back to StatusPending (re-dispatchable), retry_count++,
//     lease cleared, with retryEvent (active → pending, actionable, system actor); or
//   - retry_count >= max_retries ⇒ the terminal StatusFailed (poison-pill — never
//     re-dispatched forever), lease cleared, with a task_failed event (active → failed).
//
// reason names the cause ("lease expired" / "window gone (worker crashed)") and is woven
// into both payloads, so the two paths share one transition with path-specific narration.
// It clears only the lease fields (lease_owner/lease_expires_at), leaving worktree/window
// for the re-dispatch path — matching reclaimTask's original contract.
func applyReclaimTransitionTx(ctx context.Context, tx *sql.Tx, now time.Time, id string, retry, maxRetries int, retryEvent, reason string) (ReclaimOutcome, error) {
	nowStr := formatTime(now)
	from := StatusActive
	if retry >= maxRetries {
		// Poison-pill: retries exhausted ⇒ terminal failed, surfaced actionably.
		to := StatusFailed
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, lease_owner = '', lease_expires_at = NULL, updated_at = ? WHERE id = ?`,
			to, nowStr, id); err != nil {
			return ReclaimOutcome{}, err
		}
		if _, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: id, Type: EventTaskFailed, Actor: ActorSystem,
			FromStatus: &from, ToStatus: &to, Actionable: true,
			Payload: fmt.Sprintf("%s; retries exhausted (%d/%d)", reason, retry, maxRetries),
		}); err != nil {
			return ReclaimOutcome{}, err
		}
		return ReclaimOutcome{TaskID: id, Failed: true, RetryCount: retry}, nil
	}
	// Reclaim for retry: back to the re-dispatchable backlog state, retry_count++.
	to := StatusPending
	newRetry := retry + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, retry_count = ?, lease_owner = '', lease_expires_at = NULL, updated_at = ? WHERE id = ?`,
		to, newRetry, nowStr, id); err != nil {
		return ReclaimOutcome{}, err
	}
	if _, err := appendEvent(ctx, tx, now, Event{
		EntityType: EntityTypeTask, EntityID: id, Type: retryEvent, Actor: ActorSystem,
		FromStatus: &from, ToStatus: &to, Actionable: true,
		Payload: fmt.Sprintf("%s; reclaimed for retry %d/%d", reason, newRetry, maxRetries),
	}); err != nil {
		return ReclaimOutcome{}, err
	}
	return ReclaimOutcome{TaskID: id, Failed: false, RetryCount: newRetry}, nil
}

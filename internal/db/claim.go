package db

import (
	"context"
	"database/sql"
	"fmt"
)

// ClaimTask is the scheduler's atomic dispatch-claim primitive (roadmap item A). In a
// single BEGIN IMMEDIATE write transaction it re-reads the task under the write lock and,
// ONLY if it is still StatusPending, flips it to StatusActive, assigns owner, and arms a
// lease (lease_owner=owner, lease_expires_at=now+DefaultLeaseDuration) — recording a
// non-actionable status_changed event attributed to the system actor. It returns
// (claimedTask, true, nil) when it won the claim, or (zero, false, nil) when the task was
// NOT claimable: it vanished, or another claimant/path already moved it off pending (the
// lost-the-race case).
//
// It deliberately does NOT bump attempt: the dispatch path that follows a claim
// (orchestrator spawn → GrantLease) increments attempt exactly once, so a claim+dispatch
// shows attempt=1 — the SAME count as the manager's own spawn-from-backlog path, which
// also lands its single increment in GrantLease. (GrantLease re-arms this lease then, an
// idempotent refresh of the same lease_owner/expiry.)
//
// This is the Solid-Queue claim pattern adapted to SQLite, which has no SKIP LOCKED.
// SetMaxOpenConns(1) plus _txlock=immediate serialize writers — in-process (one
// connection) and cross-process (WAL + busy_timeout + BEGIN IMMEDIATE) — and the status
// re-check inside the write lock guarantees two concurrent ticks/instances can never both
// claim the same task: the loser re-reads a non-pending status and backs off. The claim
// is deliberately CHEAP (one row write + one event) so the heavy dispatch (orchestrator
// spawn: worktree + tmux + harness) runs only for the winner and never races.
//
// Flipping to active and arming a lease means a claimant that dies between the claim and a
// completed dispatch leaves a reclaimable task (active + lease) that ReclaimExpiredLeases
// recovers on verifiable ground truth — not a stuck row. The event is non-actionable (a
// system actor moving into a non-actionable status), so claiming never wakes the
// watcher/manager.
func (s *Store) ClaimTask(ctx context.Context, taskID, owner string) (Task, bool, error) {
	now := s.now()
	var claimed Task
	won := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status)
		if err == sql.ErrNoRows {
			return nil // vanished between selection and claim — not claimable
		}
		if err != nil {
			return err
		}
		if status != StatusPending {
			return nil // already claimed/dispatched/moved — lost the race, back off
		}
		from := StatusPending
		to := StatusActive
		// Flip pending → active and arm the lease. attempt is left to the dispatch path's
		// GrantLease (so claim+dispatch counts as one attempt, matching the manager path).
		// The redundant `AND status = 'pending'` is belt-and-suspenders: we already confirmed
		// pending under this write lock, so it can only match — but it makes the claim
		// self-guarding.
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = ?, owner = ?, lease_owner = ?, lease_expires_at = ?, updated_at = ?
			  WHERE id = ? AND status = ?`,
			to, owner, owner, formatTime(now.Add(DefaultLeaseDuration)), formatTime(now), taskID, from); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: taskID, Type: EventStatusChanged, Actor: ActorSystem,
			FromStatus: &from, ToStatus: &to, Actionable: false, Payload: "claimed for dispatch",
		}); err != nil {
			return err
		}
		t, ok, err := getTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("task %s vanished after claim", taskID)
		}
		claimed = t
		won = true
		return nil
	})
	if err != nil {
		return Task{}, false, err
	}
	return claimed, won, nil
}

// ReleaseClaim reverts a claim that could not be dispatched: it returns the task to
// StatusPending and clears its lease, recording a non-actionable status_changed event
// (active → pending, system actor) — but ONLY if the task is still active and still held
// by owner. That guard is verifiable, like reclaimTask's: a worker that already reported,
// or a reclaim that already fired, has moved the row, and a blind revert would clobber it.
// It returns whether it reverted (false when the guard declined — not an error).
//
// It is the fast-path undo for a spawn that failed after ClaimTask, so a transient
// dispatch failure does not strand a phantom (active + lease, no window) for a full lease
// duration. If it does not run (e.g. the scheduler itself crashed between the failed spawn
// and this call), the lease ClaimTask armed still lets ReclaimExpiredLeases recover the
// task as a backstop — the revert is an optimization, the lease is the guarantee.
func (s *Store) ReleaseClaim(ctx context.Context, taskID, owner string) (bool, error) {
	now := s.now()
	released := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status, leaseOwner string
		err := tx.QueryRowContext(ctx,
			`SELECT status, lease_owner FROM tasks WHERE id = ?`, taskID).Scan(&status, &leaseOwner)
		if err == sql.ErrNoRows {
			return nil // vanished — nothing to revert
		}
		if err != nil {
			return err
		}
		if status != StatusActive || leaseOwner != owner {
			return nil // moved on since the claim — never clobber a concurrent change
		}
		from := StatusActive
		to := StatusPending
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, lease_owner = '', lease_expires_at = NULL, updated_at = ? WHERE id = ?`,
			to, formatTime(now), taskID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: taskID, Type: EventStatusChanged, Actor: ActorSystem,
			FromStatus: &from, ToStatus: &to, Actionable: false, Payload: "dispatch failed; claim released",
		}); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

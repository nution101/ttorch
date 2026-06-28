package db

import (
	"context"
	"database/sql"
)

// ClaimForLand is the scheduler's atomic LAND-claim primitive (roadmap item A, phase 2a):
// the land-pass analogue of ClaimTask. In a single BEGIN IMMEDIATE write transaction it
// re-reads the task under the write lock and claims it for landing — taking a lease in
// owner's name — ONLY if it is still StatusDone and not already held by a LIVE land claim
// (a lease already in owner's name whose expiry is still in the future). It returns
// (true, nil) when it won the claim, or (false, nil) when the task was NOT claimable: it
// vanished, it has moved off done (already landed/abandoned), or another tick/instance
// holds a live land claim on it (the lost-the-race case).
//
// Unlike ClaimTask it does NOT change the task's status: a task being landed stays
// StatusDone, so it stays invisible to the dispatch claim (which only takes pending) and to
// ReclaimExpiredLeases (which only reclaims active) — a half-landed task is therefore never
// wrongly re-dispatched or poison-pilled. The lease IS the claim marker: a done task's
// worker lease is dead weight (the worker has finished and nothing consumes a done task's
// lease), so overwriting it with the lander's lease is safe, and arming a fresh expiry means
// a lander that dies mid-land frees the task for a later tick once the lease lapses (the
// backstop; ReleaseLandClaim is the fast-path undo). No event is appended — like GrantLease
// this is lease bookkeeping, not a status change.
//
// Single-winner across concurrent ticks/instances rests on the same foundation as ClaimTask:
// SetMaxOpenConns(1) plus _txlock=immediate serialize writers, so the second claimant's
// transaction re-reads the lease AFTER the first commits and backs off. The owner is per-task
// (not per-instance) by design — the serialized re-read, not owner uniqueness, is what makes
// the claim exclusive.
func (s *Store) ClaimForLand(ctx context.Context, taskID, owner string) (bool, error) {
	now := s.now()
	won := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status, leaseOwner string
		var leaseExpires sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT status, lease_owner, lease_expires_at FROM tasks WHERE id = ?`, taskID).
			Scan(&status, &leaseOwner, &leaseExpires)
		if err == sql.ErrNoRows {
			return nil // vanished between selection and claim — not claimable
		}
		if err != nil {
			return err
		}
		if status != StatusDone {
			return nil // already landed/moved, or not awaiting landing — lost the race
		}
		// A live land claim already in flight (this owner, unexpired) means another tick or
		// instance is landing it right now; back off. An expired land claim is recoverable
		// (a lander died mid-land), so fall through and re-claim it. Expiry is decided in Go,
		// matching ReclaimExpiredLeases, to avoid RFC3339Nano string-compare skew.
		if leaseOwner == owner && leaseExpires.Valid {
			expiry, perr := parseTime(leaseExpires.String)
			if perr == nil && !expiry.Before(now) {
				return nil
			}
		}
		// The redundant `AND status = 'done'` is belt-and-suspenders: we confirmed done under
		// this write lock, so it can only match — but it makes the claim self-guarding.
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET lease_owner = ?, lease_expires_at = ?, updated_at = ?
			  WHERE id = ? AND status = ?`,
			owner, formatTime(now.Add(DefaultLeaseDuration)), formatTime(now), taskID, StatusDone); err != nil {
			return err
		}
		won = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// ReleaseLandClaim reverts a land claim that did not complete, clearing the lease so a later
// tick can re-claim the task immediately instead of waiting for the claim to lapse. It is the
// land-pass analogue of ReleaseClaim. It clears the lease ONLY if the task is still StatusDone
// and still held by owner (the verifiable guard): a task that has since landed (moved to
// delivered) or been re-claimed by another owner is left untouched, never clobbered. It
// returns whether it cleared (false when the guard declined — not an error). If it does not
// run (e.g. the lander crashed before it), the lease ClaimForLand armed is the backstop: it
// lapses on its own and the task becomes re-claimable.
func (s *Store) ReleaseLandClaim(ctx context.Context, taskID, owner string) (bool, error) {
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
		if status != StatusDone || leaseOwner != owner {
			return nil // landed, moved on, or not ours — never clobber a concurrent change
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET lease_owner = '', lease_expires_at = NULL, updated_at = ? WHERE id = ?`,
			formatTime(now), taskID); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

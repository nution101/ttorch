package db

import (
	"context"
	"database/sql"
	"time"
)

// IdleNudgeCappedMarker is the sentinel payload an idle_nudged event carries when it records
// the supervisor GIVING UP on an idle worker (the consecutive-nudge budget is spent) rather
// than an actual nudge. It lets the give-up be logged exactly ONCE per idle episode — the
// marker's presence suppresses any repeat — without minting a second event type, and is
// excluded from the nudge COUNT so the budget reflects only real nudges. A real nudge's
// payload is a hex pane hash, which can never equal this word, so the two never collide.
const IdleNudgeCappedMarker = "capped"

// IdleNudge summarizes the supervisor's idle-nudge bookkeeping for one task, derived purely
// from the event spine since the task's last SIGN OF LIFE — a re-dispatch ('spawned') or a
// worker heartbeat (an event whose actor is worker:<id>). That is the SAME freshness anchor
// ReclaimWindowGone/hasFreshWindowGoneTx use, so a worker that has reported any progress
// since its last nudge starts a CLEAN episode: Count resets to 0, Capped clears, and the
// bounded-nudge budget is replenished. Keeping the state on the event spine (rather than a
// per-process field) makes it correct across a daemon restart.
type IdleNudge struct {
	// Count is how many REAL nudges (this supervisor's idle_nudged plus the watcher's
	// auto_resumed) have landed since the last sign of life. The two recovery paths are
	// pooled so they can never COMBINE to over-nudge past the cap. The give-up marker is
	// excluded.
	Count int
	// Capped is true once the give-up marker has been recorded this episode: the budget is
	// spent and the give-up already logged, so the pass stays silent until the worker revives.
	Capped bool
	// LastHash is the pane hash recorded with the most recent REAL idle_nudged — the "we
	// already nudged THIS exact idle screen" guard that avoids re-nudging an unchanged pane.
	// It is "" when the most recent real nudge was the watcher's auto_resumed (whose payload
	// is a window, not a pane hash) or when there is none.
	LastHash string
	// LastAt is the timestamp of the most recent real nudge (nil when none), used as the grace
	// anchor so consecutive nudges are always spaced by at least the grace period.
	LastAt *time.Time
}

// IdleNudgeInfo reads the idle-nudge bookkeeping for taskID off the event spine. It is a pure
// read (no writes) used by tests and diagnostics; the live decision-and-record path is
// ClaimIdleNudge, which performs the SAME derivation under a write lock so two instances can
// never double-nudge.
func (s *Store) IdleNudgeInfo(ctx context.Context, taskID string) (IdleNudge, error) {
	return idleNudgeInfoTx(ctx, s.db, taskID)
}

// idleNudgeInfoTx derives the idle-nudge bookkeeping over any queryer (the Store's pool for
// the pure read, or an open write transaction for the atomic claim). The window of events
// considered is exactly those NEWER than the task's latest sign of life, so the count and the
// last-nudge anchor describe only the CURRENT idle episode. It fully drains and closes its
// rows before returning, so a caller holding a write transaction (single SQLite connection)
// can safely issue its next statement afterward.
func idleNudgeInfoTx(ctx context.Context, q queryer, taskID string) (IdleNudge, error) {
	var info IdleNudge
	rows, err := q.QueryContext(ctx, `
		SELECT type, payload, ts FROM events
		 WHERE entity_type = 'task' AND entity_id = ?
		   AND type IN (?, ?)
		   AND id > COALESCE((SELECT MAX(sp.id) FROM events sp
		                       WHERE sp.entity_type = 'task' AND sp.entity_id = ?
		                         AND (sp.type = ? OR sp.actor GLOB 'worker:*')), 0)
		 ORDER BY id`,
		taskID, EventIdleNudged, EventAutoResumed, taskID, EventSpawned)
	if err != nil {
		return info, err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, payload, ts string
		if err := rows.Scan(&typ, &payload, &ts); err != nil {
			return info, err
		}
		if typ == EventIdleNudged && payload == IdleNudgeCappedMarker {
			info.Capped = true
			continue // give-up marker: not a real nudge — excluded from the count and anchor
		}
		info.Count++
		t, err := parseTime(ts)
		if err != nil {
			return info, err
		}
		when := t
		info.LastAt = &when
		// The newest real nudge wins (ascending id): an idle_nudged carries the pane hash; the
		// watcher's auto_resumed does not, so clear the hash when it is the latest nudge.
		if typ == EventIdleNudged {
			info.LastHash = payload
		} else {
			info.LastHash = ""
		}
	}
	return info, rows.Err()
}

// IdleNudgeDecision is what ClaimIdleNudge resolved to for one task.
type IdleNudgeDecision int

const (
	// IdleNudgeSkip: the task is not (or no longer) a nudge candidate — status moved off
	// 'active', the lease lapsed, the grace has not elapsed, this exact screen was already
	// nudged, or the budget is already spent and the give-up logged. The caller does nothing.
	IdleNudgeSkip IdleNudgeDecision = iota
	// IdleNudgeSend: the caller WON the claim. The idle_nudged event is ALREADY recorded (so a
	// concurrent instance cannot also win), and the caller must now type "continue" into the
	// worker's pane. A send that fails leaves the recorded nudge in place — under-nudge beats
	// re-nudging the same wedged screen.
	IdleNudgeSend
	// IdleNudgeGiveUp: the budget is spent. The capped marker is ALREADY recorded (so the
	// give-up logs exactly once across instances), and the caller should log the give-up.
	IdleNudgeGiveUp
)

// ClaimIdleNudge atomically decides whether to nudge an alive-but-idle worker and, on a
// SEND/GIVE-UP decision, records the corresponding idle_nudged event — all inside one
// BEGIN IMMEDIATE write transaction, so it is the idle-nudge analogue of ClaimTask /
// reclaimWindowGoneTask: two scheduler instances (or two ticks) can never both nudge the same
// idle episode, and a worker that changed status since the caller's snapshot is not nudged.
//
// Under the write lock it RE-READS the task and requires, freshly: status STILL 'active' (a
// worker that has since reported blocked/needs_input/done — a manager decision — is dropped),
// a STILL-valid lease (an expired lease is the reclaim pass's domain), and a non-null
// last_progress_at. It then derives the episode's nudge bookkeeping (idleNudgeInfoTx) and:
//
//   - if the budget is spent (Count >= maxNudges) and not already capped ⇒ records the capped
//     marker and returns IdleNudgeGiveUp (the caller logs the give-up, once);
//   - else if this exact paneHash was already nudged this episode ⇒ IdleNudgeSkip;
//   - else if the grace has not elapsed (timed from the later of last_progress_at and the last
//     nudge) ⇒ IdleNudgeSkip;
//   - else records idle_nudged(paneHash) and returns IdleNudgeSend (the caller types
//     "continue").
//
// The event is recorded BEFORE the caller sends (record-then-send): committing the intent
// under the lock is what serializes instances; a send that later fails simply means one fewer
// nudge than budgeted, which is the safe direction. count is the episode's real-nudge count at
// decision time (for the give-up log line).
func (s *Store) ClaimIdleNudge(ctx context.Context, taskID, paneHash string, now time.Time, grace time.Duration, maxNudges int) (decision IdleNudgeDecision, count int, err error) {
	decision = IdleNudgeSkip
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		var leaseExpires, lastProgress sql.NullString
		row := tx.QueryRowContext(ctx,
			`SELECT status, lease_expires_at, last_progress_at FROM tasks WHERE id = ?`, taskID)
		switch err := row.Scan(&status, &leaseExpires, &lastProgress); err {
		case sql.ErrNoRows:
			return nil // vanished between snapshot and claim — nothing to do
		case nil:
		default:
			return err
		}
		// Fresh re-checks under the write lock — the guard against a status/lease change since
		// the caller's active-task snapshot (an in-flight needs_input/blocked/done report, or a
		// lapsed lease).
		if status != StatusActive {
			return nil
		}
		if !leaseExpires.Valid {
			return nil
		}
		le, perr := parseTime(leaseExpires.String)
		if perr != nil {
			return perr
		}
		if !le.After(now) {
			return nil // lease lapsed — the reclaim pass owns it
		}
		if !lastProgress.Valid {
			return nil // no progress anchor to time the idle stretch from
		}
		lp, perr := parseTime(lastProgress.String)
		if perr != nil {
			return perr
		}

		info, ierr := idleNudgeInfoTx(ctx, tx, taskID)
		if ierr != nil {
			return ierr
		}
		count = info.Count
		if info.Capped {
			return nil // already gave up this episode
		}
		if info.Count >= maxNudges {
			// Budget spent: record the give-up marker once, atomically, then report GiveUp.
			if _, aerr := appendEvent(ctx, tx, s.now(), Event{
				EntityType: EntityTypeTask, EntityID: taskID, Type: EventIdleNudged,
				Actor: ActorSystem, Actionable: false, Payload: IdleNudgeCappedMarker,
			}); aerr != nil {
				return aerr
			}
			decision = IdleNudgeGiveUp
			return nil
		}
		if info.LastHash != "" && info.LastHash == paneHash {
			return nil // this exact idle screen was already nudged
		}
		last := lp
		if info.LastAt != nil && info.LastAt.After(last) {
			last = *info.LastAt
		}
		if now.Sub(last) < grace {
			return nil // not idle long enough yet
		}
		// Win: record the nudge under the lock, then the caller sends.
		if _, aerr := appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypeTask, EntityID: taskID, Type: EventIdleNudged,
			Actor: ActorSystem, Actionable: false, Payload: paneHash,
		}); aerr != nil {
			return aerr
		}
		decision = IdleNudgeSend
		return nil
	})
	return decision, count, err
}

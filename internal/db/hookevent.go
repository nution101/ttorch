package db

import (
	"context"
	"database/sql"
	"time"
)

// EventHookTurnStarted is the trace a worker's turn-started lifecycle hook (`ttorch hook
// turn-started`) leaves on the event spine when it writes the task's hook record. That record
// holds the stall ladder quiet while it reads busy. The trace is rate-limited
// (HookEventInterval), so it records only the first writer in each interval, and its payload
// is that writer's description of itself: how the hook resolved its task, the project dir
// from the writer's own CLAUDE_PROJECT_DIR, and the hook process's parent pid (the shell
// the harness ran the hook through, or the harness itself when that shell exec'd ttorch).
// Nothing in it is verified. actor=system, non-actionable, and deliberately not a worker:
// actor, so it never reads as a sign of life to the stall clock (StallInfo).
const EventHookTurnStarted = "hook_turn_started"

// HookEventInterval is the least time between two EventHookTurnStarted rows for one task, so
// a chatty session cannot flood the events table.
const HookEventInterval = time.Minute

// AppendHookTurnStarted records an EventHookTurnStarted row for taskID unless the task is
// unknown or already has one within HookEventInterval of now. It reports whether a row was
// written. The check and the insert share one immediate transaction, so two hooks racing for
// the same task write at most one row.
func (s *Store) AppendHookTurnStarted(ctx context.Context, taskID, payload string) (bool, error) {
	now := s.now()
	wrote := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var known int64
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id = ?)`, taskID).Scan(&known); err != nil {
			return err
		}
		if known == 0 {
			return nil
		}
		// RFC3339Nano drops trailing zeros, so stored timestamps do not compare as strings;
		// read the latest one and compare it as a time.
		var ts string
		err := tx.QueryRowContext(ctx, `
			SELECT ts FROM events
			 WHERE entity_type = 'task' AND entity_id = ? AND type = ?
			 ORDER BY id DESC LIMIT 1`, taskID, EventHookTurnStarted).Scan(&ts)
		switch err {
		case nil:
			last, err := parseTime(ts)
			if err != nil {
				return err
			}
			if now.Sub(last) < HookEventInterval {
				return nil
			}
		case sql.ErrNoRows:
		default:
			return err
		}
		if _, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: taskID, Type: EventHookTurnStarted,
			Actor: ActorSystem, Payload: payload,
		}); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

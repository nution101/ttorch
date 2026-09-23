package db

import (
	"context"
	"database/sql"
	"time"
)

// Stall-escalation event types. The watcher's liveness poll (internal/watch) keeps a
// per-worker "no progress observed" clock and raises a silent worker on a ladder; both
// live on the event spine so the ladder survives the watcher exiting and re-arming,
// which it does every time it surfaces a batch.
const (
	// EventStallClock restarts a task's stall clock: the watcher observed progress (the
	// pane changed, the worker reported, a commit landed) at ts. payload is the hash of
	// the idle pane the clock now runs on, or "" while the pane is busy (the clock is
	// stopped). actor=system, non-actionable.
	EventStallClock = "stall_clock"
	// EventStalled is one step of the stall ladder: the task has shown no progress since
	// its latest stall_clock. payload is JSON (level, raise, idle, window). actor=system,
	// actionable.
	EventStalled = "stalled"
)

// StallState is a task's stall-clock bookkeeping, derived from the event spine.
type StallState struct {
	// HasClock is false when the watcher has never recorded a stall_clock for the task.
	HasClock bool
	// ClockID, ClockAt and ClockPane describe the latest stall_clock: its events.id, its
	// timestamp (when progress was last observed) and its payload.
	ClockID   int64
	ClockAt   time.Time
	ClockPane string
	// SignOfLifeID is the id of the task's latest spawned, status_changed or
	// worker-authored event (0 when none). One newer than ClockID is progress the clock
	// has not yet absorbed. Ids, not timestamps, order it against the clock, so a
	// status change made on another process's clock still reads as newer.
	SignOfLifeID int64
	// Raises are the timestamps of the stalled events recorded since the latest
	// stall_clock, oldest first. Their count is the task's position on the ladder.
	Raises []time.Time
}

// StallInfo reads the stall-clock bookkeeping for taskID. It is a pure read.
func (s *Store) StallInfo(ctx context.Context, taskID string) (StallState, error) {
	var st StallState
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, ts, payload FROM events
		 WHERE entity_type = 'task' AND entity_id = ? AND type = ?
		 ORDER BY id DESC LIMIT 1`,
		taskID, EventStallClock).Scan(&st.ClockID, &ts, &st.ClockPane)
	switch err {
	case nil:
		st.HasClock = true
		if st.ClockAt, err = parseTime(ts); err != nil {
			return st, err
		}
	case sql.ErrNoRows:
	default:
		return st, err
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(id), 0) FROM events
		 WHERE entity_type = 'task' AND entity_id = ?
		   AND (type IN (?, ?) OR actor GLOB 'worker:*')`,
		taskID, EventSpawned, EventStatusChanged).Scan(&st.SignOfLifeID); err != nil {
		return st, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ts FROM events
		 WHERE entity_type = 'task' AND entity_id = ? AND type = ? AND id > ?
		 ORDER BY id`,
		taskID, EventStalled, st.ClockID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&ts); err != nil {
			return st, err
		}
		t, err := parseTime(ts)
		if err != nil {
			return st, err
		}
		st.Raises = append(st.Raises, t)
	}
	return st, rows.Err()
}

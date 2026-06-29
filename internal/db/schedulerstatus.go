package db

import (
	"context"
	"database/sql"
	"time"
)

// SchedulerTick is one tick's per-pass outcome — the deltas RecordSchedulerTick folds into the
// cumulative scheduler_status row. The counts are THIS tick's increments (0 for a pass that did
// nothing, is disabled, or is not run); At is the tick's wall-clock time (the new last_tick_at);
// LastError is the most recent swallowed pass error THIS tick, "" when the tick had none.
type SchedulerTick struct {
	At         time.Time
	Dispatched int
	Landed     int
	Gated      int
	Recovered  int
	Deferred   int
	Errors     int
	LastError  string
}

const schedulerStatusColumns = `last_tick_at, tick_count, dispatched, landed, gated, recovered, deferred, errors, last_error, last_error_at, updated_at`

func scanSchedulerStatus(sc rowScanner) (SchedulerStatus, error) {
	var (
		st                       SchedulerStatus
		lastTick, lastErrAt, upd string
	)
	if err := sc.Scan(&lastTick, &st.TickCount, &st.Dispatched, &st.Landed, &st.Gated,
		&st.Recovered, &st.Deferred, &st.Errors, &st.LastError, &lastErrAt, &upd); err != nil {
		return SchedulerStatus{}, err
	}
	var err error
	if st.LastTickAt, err = parseTime(lastTick); err != nil {
		return SchedulerStatus{}, err
	}
	// last_error_at is '' until the first error is recorded; leave LastErrorAt as the zero time.
	if lastErrAt != "" {
		if st.LastErrorAt, err = parseTime(lastErrAt); err != nil {
			return SchedulerStatus{}, err
		}
	}
	if st.UpdatedAt, err = parseTime(upd); err != nil {
		return SchedulerStatus{}, err
	}
	return st, nil
}

// RecordSchedulerTick folds one tick's outcome into the singleton scheduler_status row and
// returns the updated row. It is a SINGLE upsert: it inserts the row on the daemon's first tick
// or — when it already exists — advances last_tick_at, increments tick_count, and ADDS each
// pass's delta to the cumulative counter. last_error/last_error_at are overwritten only when this
// tick actually had an error (tick.LastError != ""); an error-free tick PRESERVES the previously
// recorded last error, so the "most recent error" survives any number of clean ticks afterwards.
// The row id is pinned to 1 by the table CHECK, so this is exactly one small write per tick on
// the single-writer DB.
func (s *Store) RecordSchedulerTick(ctx context.Context, tick SchedulerTick) (SchedulerStatus, error) {
	now := formatTime(s.now())
	at := formatTime(tick.At)
	errAt := ""
	if tick.LastError != "" {
		errAt = at
	}
	var out SchedulerStatus
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scheduler_status (id, `+schedulerStatusColumns+`)
			VALUES (1, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				last_tick_at  = excluded.last_tick_at,
				tick_count    = scheduler_status.tick_count + 1,
				dispatched    = scheduler_status.dispatched + excluded.dispatched,
				landed        = scheduler_status.landed     + excluded.landed,
				gated         = scheduler_status.gated      + excluded.gated,
				recovered     = scheduler_status.recovered  + excluded.recovered,
				deferred      = scheduler_status.deferred   + excluded.deferred,
				errors        = scheduler_status.errors     + excluded.errors,
				last_error    = CASE WHEN excluded.last_error != '' THEN excluded.last_error    ELSE scheduler_status.last_error    END,
				last_error_at = CASE WHEN excluded.last_error != '' THEN excluded.last_error_at ELSE scheduler_status.last_error_at END,
				updated_at    = excluded.updated_at`,
			at, tick.Dispatched, tick.Landed, tick.Gated, tick.Recovered,
			tick.Deferred, tick.Errors, tick.LastError, errAt, now); err != nil {
			return err
		}
		var scanErr error
		out, scanErr = scanSchedulerStatus(tx.QueryRowContext(ctx,
			`SELECT `+schedulerStatusColumns+` FROM scheduler_status WHERE id = 1`))
		return scanErr
	})
	if err != nil {
		return SchedulerStatus{}, err
	}
	return out, nil
}

// GetSchedulerStatus returns the singleton scheduler-status row. The bool reports whether the
// daemon has ever ticked: the row is absent until the first RecordSchedulerTick, which `ttorch
// scheduler status` renders as "no tick recorded yet". A missing row is not an error.
func (s *Store) GetSchedulerStatus(ctx context.Context) (SchedulerStatus, bool, error) {
	st, err := scanSchedulerStatus(s.db.QueryRowContext(ctx,
		`SELECT `+schedulerStatusColumns+` FROM scheduler_status WHERE id = 1`))
	if err == sql.ErrNoRows {
		return SchedulerStatus{}, false, nil
	}
	if err != nil {
		return SchedulerStatus{}, false, err
	}
	return st, true, nil
}

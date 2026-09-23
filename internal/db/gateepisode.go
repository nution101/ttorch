package db

import (
	"context"
	"database/sql"
	"time"
)

// GateEpisode is the daemon gate's per-task record of one review episode: the commit it
// gates, the reviewer set it decided at the boundary, what each dimension's launches have
// cost, and when it opened.
//
// It lives here rather than beside the review inputs because every gate decision depends on
// it and the worker can write that directory. A parseable record naming no dimensions erased
// the memory that a security reviewer had been dispatched, which let an already-pinned
// critical finding fall out of the fold and minted an approval over it. Zeroing the start
// time held the stall bound open; making the file unwritable stopped the bound accumulating.
// Each shape needed its own check, which is why the record moved instead.
//
// Dims and Attempts are JSON and opaque here, the same way a verdict's findings are: the
// store owns durability and ownership, the orchestrator owns their meaning.
type GateEpisode struct {
	TaskID            string
	Head              string
	Dims              string // JSON array of dimension names
	Attempts          string // JSON object, dimension -> launch attempts charged
	StartedAt         time.Time
	DispatchedAt      time.Time // zero until a dispatch is charged
	Outcome           string    // "" in flight | recorded | blocked
	LastDispatchError string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const gateEpisodeColumns = `task_id, head, dims, attempts, started_at, dispatched_at, outcome, last_dispatch_error, created_at, updated_at`

func scanGateEpisode(sc rowScanner) (GateEpisode, error) {
	var (
		e                       GateEpisode
		startedAt, dispatchedAt string
		createdAt, updatedAt    string
	)
	if err := sc.Scan(&e.TaskID, &e.Head, &e.Dims, &e.Attempts, &startedAt, &dispatchedAt,
		&e.Outcome, &e.LastDispatchError, &createdAt, &updatedAt); err != nil {
		return GateEpisode{}, err
	}
	var err error
	if e.StartedAt, err = parseTime(startedAt); err != nil {
		return GateEpisode{}, err
	}
	// dispatched_at is '' until a dispatch is charged, which is not a parse failure.
	if dispatchedAt != "" {
		if e.DispatchedAt, err = parseTime(dispatchedAt); err != nil {
			return GateEpisode{}, err
		}
	}
	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return GateEpisode{}, err
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return GateEpisode{}, err
	}
	return e, nil
}

// SaveGateEpisode inserts or replaces a task's episode row, preserving the original
// created_at so the row's first-opened time survives the per-tick rewrites.
func (s *Store) SaveGateEpisode(ctx context.Context, e GateEpisode) error {
	if e.Dims == "" {
		e.Dims = "[]"
	}
	if e.Attempts == "" {
		e.Attempts = "{}"
	}
	dispatchedAt := ""
	if !e.DispatchedAt.IsZero() {
		dispatchedAt = formatTime(e.DispatchedAt)
	}
	ts := formatTime(s.now())
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO gate_episodes (`+gateEpisodeColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET
				head                = excluded.head,
				dims                = excluded.dims,
				attempts            = excluded.attempts,
				started_at          = excluded.started_at,
				dispatched_at       = excluded.dispatched_at,
				outcome             = excluded.outcome,
				last_dispatch_error = excluded.last_dispatch_error,
				updated_at          = excluded.updated_at`,
			e.TaskID, e.Head, e.Dims, e.Attempts, formatTime(e.StartedAt), dispatchedAt,
			e.Outcome, e.LastDispatchError, ts, ts)
		return err
	})
}

// GetGateEpisode returns a task's episode. The bool reports existence; an error is a real
// store failure and callers must NOT treat it as "no episode in flight", which is the
// reading that let a lost record pass a verdict.
func (s *Store) GetGateEpisode(ctx context.Context, taskID string) (GateEpisode, bool, error) {
	e, err := scanGateEpisode(s.db.QueryRowContext(ctx,
		`SELECT `+gateEpisodeColumns+` FROM gate_episodes WHERE task_id = ?`, taskID))
	if err == sql.ErrNoRows {
		return GateEpisode{}, false, nil
	}
	if err != nil {
		return GateEpisode{}, false, err
	}
	return e, true, nil
}

// DeleteGateEpisode removes a task's episode row. Idempotent.
func (s *Store) DeleteGateEpisode(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM gate_episodes WHERE task_id = ?`, taskID)
	return err
}

// GateEpisodeOpenedAt returns when the daemon gate first opened an episode for head in the
// task's current run of that head: the earliest gate_episode_opened event for head that is
// newer than the task's latest such event for any OTHER head. A worker that moves A to B and
// back to A therefore starts A's clock again rather than inheriting the first A episode's,
// while re-opening the same head after its episode row was deleted keeps the first opening.
//
// Earliest is by id, not by ts. Ids are assigned in commit order (§1.4); ts is RFC3339Nano,
// which trims trailing zeros, so its strings do not sort as times.
//
// Nothing in this package updates or deletes an events row. That is a property of this code,
// not of the file: a process running as the same user can delete the marker with the sqlite3
// CLI, which is why the gate treats it as one anchor among several rather than the clock.
func (s *Store) GateEpisodeOpenedAt(ctx context.Context, taskID, head string) (time.Time, bool, error) {
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT ts FROM events
		WHERE entity_type = 'task' AND entity_id = ? AND type = ? AND payload = ?
		  AND id > COALESCE((
			SELECT MAX(id) FROM events
			WHERE entity_type = 'task' AND entity_id = ? AND type = ? AND payload <> ?), 0)
		ORDER BY id LIMIT 1`,
		taskID, EventGateEpisodeOpened, head, taskID, EventGateEpisodeOpened, head).Scan(&ts)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := parseTime(ts)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

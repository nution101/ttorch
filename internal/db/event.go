package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// requireRows turns a 0-rows-affected write into a not-found error.
func requireRows(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s not found", what)
	}
	return nil
}

const eventColumns = `id, ts, entity_type, entity_id, type, actor, from_status, to_status, actionable, payload`

func scanEvent(sc rowScanner) (Event, error) {
	var (
		e         Event
		ts        string
		from, to  sql.NullString
		actionInt int64
	)
	if err := sc.Scan(&e.ID, &ts, &e.EntityType, &e.EntityID, &e.Type, &e.Actor, &from, &to, &actionInt, &e.Payload); err != nil {
		return Event{}, err
	}
	var err error
	if e.TS, err = parseTime(ts); err != nil {
		return Event{}, err
	}
	if from.Valid {
		v := from.String
		e.FromStatus = &v
	}
	if to.Valid {
		v := to.String
		e.ToStatus = &v
	}
	e.Actionable = actionInt != 0
	return e, nil
}

// appendEvent inserts one events row and returns its new id. It takes a queryer so
// composite operations pass their *sql.Tx (§2.3). A zero e.TS defaults to now.
func appendEvent(ctx context.Context, q queryer, now time.Time, e Event) (int64, error) {
	ts := e.TS
	if ts.IsZero() {
		ts = now
	}
	actionable := 0
	if e.Actionable {
		actionable = 1
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO events (ts, entity_type, entity_id, type, actor, from_status, to_status, actionable, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(ts), e.EntityType, e.EntityID, e.Type, e.Actor,
		nullStr(e.FromStatus), nullStr(e.ToStatus), actionable, e.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AppendEvent inserts one events row and returns its new id.
func (s *Store) AppendEvent(ctx context.Context, e Event) (int64, error) {
	return appendEvent(ctx, s.db, s.now(), e)
}

// EventsSince returns events with id > sinceID in id order (== commit order under
// IMMEDIATE writers, §1.4). When onlyActionable is set it returns only rows that
// should wake the manager: actionable=1, and — defensively per §1.3/§4.2 —
// excluding any status_changed row whose actor is not a worker (so a manager's own
// lifecycle action can never self-trigger the watcher even if mislabeled).
func (s *Store) EventsSince(ctx context.Context, sinceID int64, onlyActionable bool) ([]Event, error) {
	query := `SELECT ` + eventColumns + ` FROM events WHERE id > ?`
	if onlyActionable {
		// GLOB (not LIKE) so the actor match is case-sensitive and prefix-only,
		// exactly mirroring isWorkerActor (HasPrefix "worker:").
		query += ` AND actionable = 1 AND NOT (type = 'status_changed' AND actor NOT GLOB 'worker:*')`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxActionableEventID returns the highest actionable events.id, or 0 when none
// exist (COALESCE over an empty/NULL aggregate, §2.2).
func (s *Store) MaxActionableEventID(ctx context.Context) (int64, error) {
	var max int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE actionable = 1`).Scan(&max)
	return max, err
}

const noteColumns = `id, ts, task_id, author, body`

func scanNote(sc rowScanner) (Note, error) {
	var (
		n   Note
		ts  string
		tid sql.NullString
	)
	if err := sc.Scan(&n.ID, &ts, &tid, &n.Author, &n.Body); err != nil {
		return Note{}, err
	}
	var err error
	if n.TS, err = parseTime(ts); err != nil {
		return Note{}, err
	}
	if tid.Valid {
		v := tid.String
		n.TaskID = &v
	}
	return n, nil
}

// addNote inserts one notes row. It takes a queryer so a composite operation can
// pass its *sql.Tx (§2.3). An empty taskID stores NULL.
func addNote(ctx context.Context, q queryer, now time.Time, taskID, author, body string) error {
	var tid any
	if taskID != "" {
		tid = taskID
	}
	_, err := q.ExecContext(ctx, `INSERT INTO notes (ts, task_id, author, body) VALUES (?, ?, ?, ?)`,
		formatTime(now), tid, author, body)
	return err
}

// AddNote records freeform activity (never actionable, §3.1).
func (s *Store) AddNote(ctx context.Context, taskID, author, body string) error {
	return addNote(ctx, s.db, s.now(), taskID, author, body)
}

// Timeline returns a task's history: its task-scoped events merged with its notes,
// ordered by timestamp (§2.2). On a tie it preserves insertion order — all events
// (in id order) then all notes (in id order).
func (s *Store) Timeline(ctx context.Context, taskID string) ([]TimelineItem, error) {
	events, err := s.collectEvents(ctx,
		`SELECT `+eventColumns+` FROM events WHERE entity_type = 'task' AND entity_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	notes, err := s.collectNotes(ctx,
		`SELECT `+noteColumns+` FROM notes WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	items := make([]TimelineItem, 0, len(events)+len(notes))
	for i := range events {
		e := events[i]
		items = append(items, TimelineItem{TS: e.TS, Kind: "event", Event: &e})
	}
	for i := range notes {
		n := notes[i]
		items = append(items, TimelineItem{TS: n.TS, Kind: "note", Note: &n})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].TS.Before(items[j].TS) })
	return items, nil
}

// collectEvents reads every event row for a query, fully draining and closing the
// result set before returning (required under SetMaxOpenConns(1) so a later query
// has a free connection).
func (s *Store) collectEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) collectNotes(ctx context.Context, query string, args ...any) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

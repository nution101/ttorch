package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// taskSelect joins projects so every returned Task carries the repo path in its
// Project field (= state.Task.Project). project_id is NOT NULL with FK RESTRICT,
// so an INNER JOIN never drops a task.
const taskSelect = `SELECT
	t.id, t.project_id, t.epic_id, t.phase_id, t.parent_task_id, t.created_by,
	t.title, t.kind, t.status, t.stage, t.owner,
	t.window, t.worktree, t.harness, t.session_id, t.pr, t.gate_passed, t.approved_by,
	t.reviewed_sha, t.footprint, t.last_pane_hash, t.idle_sweeps,
	t.created_at, t.updated_at, t.last_progress_at,
	t.lease_owner, t.lease_expires_at, t.retry_count, t.max_retries, t.attempt,
	t.effort,
	p.repo_path
	FROM tasks t JOIN projects p ON p.id = t.project_id`

func scanTask(sc rowScanner) (Task, error) {
	var (
		t                Task
		epicID, phaseID  sql.NullInt64
		parentID         sql.NullString
		gate             int64
		footprint        string
		createdAt, updAt string
		lastProgress     sql.NullString
		leaseExpires     sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.ProjectID, &epicID, &phaseID, &parentID, &t.CreatedBy,
		&t.Title, &t.Kind, &t.Status, &t.Stage, &t.Owner,
		&t.Window, &t.Worktree, &t.Harness, &t.SessionID, &t.PR, &gate, &t.ApprovedBy,
		&t.ReviewedSHA, &footprint, &t.LastPaneHash, &t.IdleSweeps,
		&createdAt, &updAt, &lastProgress,
		&t.LeaseOwner, &leaseExpires, &t.RetryCount, &t.MaxRetries, &t.Attempt,
		&t.Effort,
		&t.Project); err != nil {
		return Task{}, err
	}
	t.GatePassed = gate != 0
	if epicID.Valid {
		v := epicID.Int64
		t.EpicID = &v
	}
	if phaseID.Valid {
		v := phaseID.Int64
		t.PhaseID = &v
	}
	if parentID.Valid {
		v := parentID.String
		t.ParentTaskID = &v
	}
	var err error
	if t.Footprint, err = unmarshalFootprint(footprint); err != nil {
		return Task{}, err
	}
	if t.Created, err = parseTime(createdAt); err != nil {
		return Task{}, err
	}
	if t.UpdatedAt, err = parseTime(updAt); err != nil {
		return Task{}, err
	}
	if lastProgress.Valid {
		lp, err := parseTime(lastProgress.String)
		if err != nil {
			return Task{}, err
		}
		t.LastProgressAt = &lp
	}
	if leaseExpires.Valid {
		le, err := parseTime(leaseExpires.String)
		if err != nil {
			return Task{}, err
		}
		t.LeaseExpiresAt = &le
	}
	return t, nil
}

// getTask loads one task (with its joined repo path). Takes a queryer so it works
// inside or outside a transaction.
func getTask(ctx context.Context, q queryer, id string) (Task, bool, error) {
	t, err := scanTask(q.QueryRowContext(ctx, taskSelect+` WHERE t.id = ?`, id))
	if err == sql.ErrNoRows {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// GetTask loads one task by id. The bool reports existence.
func (s *Store) GetTask(ctx context.Context, id string) (Task, bool, error) {
	return getTask(ctx, s.db, id)
}

// applyTaskDefaults fills the defaults a fresh task needs so callers may omit them
// (mirroring the schema defaults).
func (s *Store) applyTaskDefaults(t *Task) {
	now := s.now()
	if t.Created.IsZero() {
		t.Created = now
	}
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusPending
	}
	if t.Kind == "" {
		t.Kind = KindShip
	}
	if t.CreatedBy == "" {
		t.CreatedBy = ActorManager
	}
	if t.MaxRetries == 0 {
		t.MaxRetries = DefaultMaxRetries
	}
}

// insertTaskTx inserts a fully-specified task row.
func (s *Store) insertTaskTx(ctx context.Context, tx *sql.Tx, t Task) error {
	fp, err := marshalFootprint(t.Footprint)
	if err != nil {
		return err
	}
	gate := 0
	if t.GatePassed {
		gate = 1
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (
			id, project_id, epic_id, phase_id, parent_task_id, created_by,
			title, kind, status, stage, owner,
			window, worktree, harness, session_id, pr, gate_passed, approved_by, reviewed_sha, footprint,
			last_pane_hash, idle_sweeps, created_at, updated_at, last_progress_at,
			lease_owner, lease_expires_at, retry_count, max_retries, attempt, effort)
		VALUES (?, ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?, ?, ?,  ?)`,
		t.ID, t.ProjectID, nullInt(t.EpicID), nullInt(t.PhaseID), nullStr(t.ParentTaskID), t.CreatedBy,
		t.Title, t.Kind, t.Status, t.Stage, t.Owner,
		t.Window, t.Worktree, t.Harness, t.SessionID, t.PR, gate, t.ApprovedBy, t.ReviewedSHA, fp,
		t.LastPaneHash, t.IdleSweeps, formatTime(t.Created), formatTime(t.UpdatedAt), nullTime(t.LastProgressAt),
		t.LeaseOwner, nullTime(t.LeaseExpiresAt), t.RetryCount, t.MaxRetries, t.Attempt, t.Effort)
	return err
}

// CreateTask inserts a new task and records a 'created' event in one transaction
// (§2.2). Returns the canonical stored row.
func (s *Store) CreateTask(ctx context.Context, t Task, actor string) (Task, error) {
	s.applyTaskDefaults(&t)
	var out Task
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.insertTaskTx(ctx, tx, t); err != nil {
			return err
		}
		to := t.Status
		if _, err := appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypeTask, EntityID: t.ID, Type: EventCreated, Actor: actor, ToStatus: &to,
		}); err != nil {
			return err
		}
		var ok bool
		var err error
		if out, ok, err = getTask(ctx, tx, t.ID); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("task %s vanished after insert", t.ID)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// CreateFollowOn atomically files a follow-on (child) task: it inserts the row and
// appends BOTH its 'created' event and the typed 'follow_on_created' event in ONE
// transaction (§1.4), so an observer never sees the child without its provenance and
// a crash leaves neither (the §1.4 fix tracked from inc2, where internal/db was out
// of scope and the CLI did this as two separate transactions). Both events are
// non-actionable — a follow-on is backlog the manager surfaces on its next re-derive,
// never an interrupt (§3.1/§9). title is recorded on the follow_on_created payload.
// Returns the canonical stored child row.
func (s *Store) CreateFollowOn(ctx context.Context, child Task, actor, title string) (Task, error) {
	s.applyTaskDefaults(&child)
	var out Task
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.insertTaskTx(ctx, tx, child); err != nil {
			return err
		}
		to := child.Status
		if _, err := appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypeTask, EntityID: child.ID, Type: EventCreated, Actor: actor, ToStatus: &to,
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypeTask, EntityID: child.ID, Type: EventFollowOnCreated, Actor: actor, Payload: title,
		}); err != nil {
			return err
		}
		var ok bool
		var err error
		if out, ok, err = getTask(ctx, tx, child.ID); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("follow-on %s vanished after insert", child.ID)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// UpsertTask inserts the task if absent (== CreateTask: insert + 'created' event),
// or, if its id already exists, syncs the runtime/coupling fields onto the existing
// row WITHOUT changing lifecycle state or emitting an event (§2.2/§3.4). This is
// the SetTaskFields half of the §3.4 spawn-from-backlog flow; the caller drives the
// status transition with ReportStatus. created_at, created_by, kind, status, stage,
// the delivery fields, and liveness bookkeeping are preserved on the update path.
func (s *Store) UpsertTask(ctx context.Context, t Task, actor string) (Task, error) {
	var out Task
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, exists, err := getTask(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if !exists {
			s.applyTaskDefaults(&t)
			if err := s.insertTaskTx(ctx, tx, t); err != nil {
				return err
			}
			to := t.Status
			if _, err := appendEvent(ctx, tx, s.now(), Event{
				EntityType: EntityTypeTask, EntityID: t.ID, Type: EventCreated, Actor: actor, ToStatus: &to,
			}); err != nil {
				return err
			}
		} else {
			fp, err := marshalFootprint(t.Footprint)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks SET
					window = ?, worktree = ?, harness = ?, session_id = ?, pr = ?,
					owner = ?, title = ?, epic_id = ?, phase_id = ?, footprint = ?, effort = ?, updated_at = ?
				WHERE id = ?`,
				t.Window, t.Worktree, t.Harness, t.SessionID, t.PR,
				t.Owner, t.Title, nullInt(t.EpicID), nullInt(t.PhaseID), fp, t.Effort, formatTime(s.now()),
				t.ID); err != nil {
				return err
			}
		}
		var ok bool
		if out, ok, err = getTask(ctx, tx, t.ID); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("task %s vanished after upsert", t.ID)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// placeholders returns "?,?,…?" for an IN/NOT IN clause. n comes from a slice
// length, never from caller text, so it cannot inject SQL.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// ListTasks returns tasks matching filter, oldest first (subsuming state.List()
// semantics when the filter is empty).
func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	query := taskSelect
	var where []string
	var args []any
	if len(filter.Status) > 0 {
		where = append(where, `t.status IN (`+placeholders(len(filter.Status))+`)`)
		for _, st := range filter.Status {
			args = append(args, st)
		}
	}
	if filter.ProjectID != 0 {
		where = append(where, `t.project_id = ?`)
		args = append(args, filter.ProjectID)
	}
	if filter.EpicID != 0 {
		where = append(where, `t.epic_id = ?`)
		args = append(args, filter.EpicID)
	}
	if filter.Owner != "" {
		where = append(where, `t.owner = ?`)
		args = append(args, filter.Owner)
	}
	if filter.ParentID != "" {
		where = append(where, `t.parent_task_id = ?`)
		args = append(args, filter.ParentID)
	}
	if len(filter.ExcludeKind) > 0 {
		where = append(where, `t.kind NOT IN (`+placeholders(len(filter.ExcludeKind))+`)`)
		for _, k := range filter.ExcludeKind {
			args = append(args, k)
		}
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY t.created_at ASC, t.id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListChildren returns the follow-on/child tasks of a parent, oldest first.
func (s *Store) ListChildren(ctx context.Context, parentID string) ([]Task, error) {
	return s.ListTasks(ctx, TaskFilter{ParentID: parentID})
}

// taskFieldsAssignments turns a partial TaskFields into parallel `col = ?` clauses
// and their bind args (no updated_at, no WHERE — callers append those). Column names
// are fixed literals, never caller text, so they cannot inject SQL. It is shared by
// SetTaskFields (s.db) and RecordTransition (a *sql.Tx) so both build the SET list
// identically (§2.3).
func taskFieldsAssignments(f TaskFields) ([]string, []any, error) {
	var set []string
	var args []any
	add := func(col string, v any) { set = append(set, col+" = ?"); args = append(args, v) }
	if f.Window != nil {
		add("window", *f.Window)
	}
	if f.Worktree != nil {
		add("worktree", *f.Worktree)
	}
	if f.Harness != nil {
		add("harness", *f.Harness)
	}
	if f.SessionID != nil {
		add("session_id", *f.SessionID)
	}
	if f.PR != nil {
		add("pr", *f.PR)
	}
	if f.Owner != nil {
		add("owner", *f.Owner)
	}
	if f.Title != nil {
		add("title", *f.Title)
	}
	if f.Kind != nil {
		add("kind", *f.Kind)
	}
	if f.EpicID != nil {
		add("epic_id", *f.EpicID)
	}
	if f.PhaseID != nil {
		add("phase_id", *f.PhaseID)
	}
	if f.Footprint != nil {
		fp, err := marshalFootprint(*f.Footprint)
		if err != nil {
			return nil, nil, err
		}
		add("footprint", fp)
	}
	if f.Effort != nil {
		add("effort", *f.Effort)
	}
	return set, args, nil
}

// SetTaskFields applies a partial update of a task's runtime/coupling fields. It
// writes no event (§2.2); updated_at always advances.
func (s *Store) SetTaskFields(ctx context.Context, id string, f TaskFields) error {
	set, args, err := taskFieldsAssignments(f)
	if err != nil {
		return err
	}
	set = append(set, "updated_at = ?")
	args = append(args, formatTime(s.now()), id)
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	return requireRows(res, "task "+id)
}

// RecordTransition applies a manager-authored lifecycle change in one transaction
// (§1.4/§2.3): it optionally moves the task to newStatus (when non-empty), applies
// any partial TaskFields, and appends ONE typed event (eventType) attributed to
// actor — all atomically, so an observer never sees the change without its event and
// a crash leaves neither. When the status changes, the event records from/to status.
//
// The event is ALWAYS non-actionable. These are manager-authored lifecycle events
// (spawned/validated/.../delivered/merged/promoted/pr_armed/torn_down, §1.3) that
// must NEVER self-trigger the watcher; a worker-actor transition that may wake the
// manager goes through ReportStatus instead. This hard-coded actionable=0 is the
// enforcement point for the §1.3 actionability invariant on these verbs. Returns the
// appended event.
func (s *Store) RecordTransition(ctx context.Context, id, newStatus string, fields TaskFields, eventType, actor, payload string) (Event, error) {
	now := s.now()
	storedTS, _ := parseTime(formatTime(now))
	var ev Event
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Read the current status: it both confirms the task exists (an UPDATE of a
		// missing id would silently affect 0 rows) and supplies the event's from-status.
		var from string
		err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&from)
		if err == sql.ErrNoRows {
			return fmt.Errorf("task %s not found", id)
		}
		if err != nil {
			return err
		}
		set, args, err := taskFieldsAssignments(fields)
		if err != nil {
			return err
		}
		if newStatus != "" {
			// Prepend status to both lists so the SET clause and its args stay aligned.
			set = append([]string{"status = ?"}, set...)
			args = append([]any{newStatus}, args...)
		}
		set = append(set, "updated_at = ?")
		args = append(args, formatTime(now), id)
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...); err != nil {
			return err
		}
		var fromPtr, toPtr *string
		if newStatus != "" {
			f, to := from, newStatus
			fromPtr, toPtr = &f, &to
		}
		eid, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: id, Type: eventType, Actor: actor,
			FromStatus: fromPtr, ToStatus: toPtr, Actionable: false, Payload: payload,
		})
		if err != nil {
			return err
		}
		ev = Event{
			ID: eid, TS: storedTS, EntityType: EntityTypeTask, EntityID: id, Type: eventType,
			Actor: actor, FromStatus: fromPtr, ToStatus: toPtr, Actionable: false, Payload: payload,
		}
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

// ReportStatus sets a task's status, touches last_progress_at, and appends a
// status_changed event — all in one transaction so an observer never sees the
// status change without its event (§1.4). A worker actor transitioning into
// {needs_input, blocked, done} makes the event actionable (§1.3). A non-empty msg
// is stored on the event payload (so the watcher can surface it, §4.3) AND written
// as a note in the same transaction (§3.1). Returns the appended event.
func (s *Store) ReportStatus(ctx context.Context, id, status, actor, msg string) (Event, error) {
	now := s.now()
	storedTS, _ := parseTime(formatTime(now))
	var ev Event
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var from string
		err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&from)
		if err == sql.ErrNoRows {
			return fmt.Errorf("task %s not found", id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, last_progress_at = ?, updated_at = ? WHERE id = ?`,
			status, formatTime(now), formatTime(now), id); err != nil {
			return err
		}
		// Progress is a heartbeat: extend the owning worker's lease (no-op if no lease is
		// held). Same transaction as the status write, so the lease never drifts from the
		// progress it is vouching for (§roadmap 2).
		if err := leaseExtendTx(ctx, tx, now, id); err != nil {
			return err
		}
		actionable := isWorkerActor(actor) && isActionableStatus(status)
		eid, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: id, Type: EventStatusChanged,
			Actor: actor, FromStatus: &from, ToStatus: &status, Actionable: actionable, Payload: msg,
		})
		if err != nil {
			return err
		}
		if msg != "" {
			if err := addNote(ctx, tx, now, id, actor, msg); err != nil {
				return err
			}
		}
		fromCopy, toCopy := from, status
		ev = Event{
			ID: eid, TS: storedTS, EntityType: EntityTypeTask, EntityID: id, Type: EventStatusChanged,
			Actor: actor, FromStatus: &fromCopy, ToStatus: &toCopy, Actionable: actionable, Payload: msg,
		}
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

// SetStage sets a task's free-text progress stage, touches last_progress_at, and
// appends a non-actionable stage_changed event in one transaction (§3.1).
func (s *Store) SetStage(ctx context.Context, id, stage, actor string) (Event, error) {
	now := s.now()
	storedTS, _ := parseTime(formatTime(now))
	var ev Event
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET stage = ?, last_progress_at = ?, updated_at = ? WHERE id = ?`,
			stage, formatTime(now), formatTime(now), id)
		if err != nil {
			return err
		}
		if err := requireRows(res, "task "+id); err != nil {
			return err
		}
		// A stage update is worker progress too — extend the lease (heartbeat, §roadmap 2).
		if err := leaseExtendTx(ctx, tx, now, id); err != nil {
			return err
		}
		eid, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: id, Type: EventStageChanged, Actor: actor, Payload: stage,
		})
		if err != nil {
			return err
		}
		ev = Event{ID: eid, TS: storedTS, EntityType: EntityTypeTask, EntityID: id, Type: EventStageChanged, Actor: actor, Payload: stage}
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

// RecordDelivery writes a task's delivery provenance (gate/approval/sha), optionally
// upserts the durable verdict row, and appends a manager-authored event
// (review_recorded by default) — all in one transaction. The event is always
// non-actionable (§1.3). When d.Verdict is non-nil the verdict row is replaced in the
// SAME transaction as the summary update, so the flattened summary columns and the
// authoritative verdict row can never drift apart (a crash leaves both old or both new).
func (s *Store) RecordDelivery(ctx context.Context, id string, d Delivery) error {
	now := s.now()
	eventType := d.EventType
	if eventType == "" {
		eventType = EventReviewRecorded
	}
	actor := d.Actor
	if actor == "" {
		actor = ActorManager
	}
	gate := 0
	if d.GatePassed {
		gate = 1
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET gate_passed = ?, approved_by = ?, reviewed_sha = ?, updated_at = ? WHERE id = ?`,
			gate, d.ApprovedBy, d.ReviewedSHA, formatTime(now), id)
		if err != nil {
			return err
		}
		if err := requireRows(res, "task "+id); err != nil {
			return err
		}
		if d.Verdict != nil {
			if err := upsertVerdictTx(ctx, tx, now, *d.Verdict); err != nil {
				return err
			}
		}
		_, err = appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: id, Type: eventType, Actor: actor, Payload: d.Payload,
		})
		return err
	})
}

// SetLiveness persists the watcher's stale-detection bookkeeping (§4.4). No event.
func (s *Store) SetLiveness(ctx context.Context, id, paneHash string, idleSweeps int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET last_pane_hash = ?, idle_sweeps = ?, updated_at = ? WHERE id = ?`,
		paneHash, idleSweeps, formatTime(s.now()), id)
	if err != nil {
		return err
	}
	return requireRows(res, "task "+id)
}

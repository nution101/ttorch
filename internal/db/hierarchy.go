package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

const projectColumns = `id, repo_path, name, delivery_mode, status, owner, created_at, updated_at`

func scanProject(sc rowScanner) (Project, error) {
	var p Project
	var createdAt, updatedAt string
	if err := sc.Scan(&p.ID, &p.RepoPath, &p.Name, &p.DeliveryMode, &p.Status, &p.Owner, &createdAt, &updatedAt); err != nil {
		return Project{}, err
	}
	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return Project{}, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Project{}, err
	}
	return p, nil
}

// UpsertProject inserts a project by its UNIQUE repo_path, or updates the existing
// one, and returns the row. A non-empty name updates the cached name; an empty
// name leaves the stored name untouched.
func (s *Store) UpsertProject(ctx context.Context, repoPath, name string) (Project, error) {
	now := formatTime(s.now())
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO projects (repo_path, name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo_path) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE projects.name END,
			updated_at = excluded.updated_at
		RETURNING `+projectColumns, repoPath, name, now, now)
	return scanProject(row)
}

// GetProjectByRepo loads a project by repo_path. The bool reports existence.
func (s *Store) GetProjectByRepo(ctx context.Context, repoPath string) (Project, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE repo_path = ?`, repoPath)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	return p, true, nil
}

// ListProjects returns all projects, oldest first.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProjectMode updates the cached delivery_mode for display (§0.3/§3.4). This is
// a DISPLAY CACHE ONLY — the merge/land gates always resolve the authoritative
// mode from AGENTS.md, never from this column.
func (s *Store) SetProjectMode(ctx context.Context, id int64, mode string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET delivery_mode = ?, updated_at = ? WHERE id = ?`,
		mode, formatTime(s.now()), id)
	if err != nil {
		return err
	}
	return requireRows(res, fmt.Sprintf("project %d", id))
}

const epicColumns = `id, project_id, title, description, status, owner, position, created_at, updated_at`

func scanEpic(sc rowScanner) (Epic, error) {
	var e Epic
	var createdAt, updatedAt string
	if err := sc.Scan(&e.ID, &e.ProjectID, &e.Title, &e.Description, &e.Status, &e.Owner, &e.Position, &createdAt, &updatedAt); err != nil {
		return Epic{}, err
	}
	var err error
	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return Epic{}, err
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Epic{}, err
	}
	return e, nil
}

// CreateEpic inserts an epic under a project and records a 'created' event in the
// same transaction (audit spine, §0.1/§1.3; actionable=0).
func (s *Store) CreateEpic(ctx context.Context, projectID int64, title, desc string) (Epic, error) {
	now := formatTime(s.now())
	var epic Epic
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO epics (project_id, title, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			RETURNING `+epicColumns, projectID, title, desc, now, now)
		var err error
		if epic, err = scanEpic(row); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypeEpic,
			EntityID:   strconv.FormatInt(epic.ID, 10),
			Type:       EventCreated,
			Actor:      ActorManager,
		})
		return err
	})
	if err != nil {
		return Epic{}, err
	}
	return epic, nil
}

const phaseColumns = `id, epic_id, title, description, status, owner, position, created_at, updated_at`

func scanPhase(sc rowScanner) (Phase, error) {
	var p Phase
	var createdAt, updatedAt string
	if err := sc.Scan(&p.ID, &p.EpicID, &p.Title, &p.Description, &p.Status, &p.Owner, &p.Position, &createdAt, &updatedAt); err != nil {
		return Phase{}, err
	}
	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return Phase{}, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Phase{}, err
	}
	return p, nil
}

// CreatePhase inserts a phase under an epic and records a 'created' event in the
// same transaction (actionable=0).
func (s *Store) CreatePhase(ctx context.Context, epicID int64, title, desc string) (Phase, error) {
	now := formatTime(s.now())
	var phase Phase
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO phases (epic_id, title, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			RETURNING `+phaseColumns, epicID, title, desc, now, now)
		var err error
		if phase, err = scanPhase(row); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, s.now(), Event{
			EntityType: EntityTypePhase,
			EntityID:   strconv.FormatInt(phase.ID, 10),
			Type:       EventCreated,
			Actor:      ActorManager,
		})
		return err
	})
	if err != nil {
		return Phase{}, err
	}
	return phase, nil
}

// entityTable maps an EntityKind to its (table, entity_type) via a fixed switch —
// never string-concatenated from caller input, so it cannot inject SQL.
func entityTable(kind EntityKind) (table, entityType string, err error) {
	switch kind {
	case EntityProject:
		return "projects", EntityTypeProject, nil
	case EntityEpic:
		return "epics", EntityTypeEpic, nil
	case EntityPhase:
		return "phases", EntityTypePhase, nil
	default:
		return "", "", fmt.Errorf("unknown entity kind %q", kind)
	}
}

// SetEntityStatus updates a project/epic/phase status and records a
// status_changed event (actionable=0 — hierarchy status changes never wake the
// manager, §1.3). The new status is validated by the table's CHECK constraint.
func (s *Store) SetEntityStatus(ctx context.Context, kind EntityKind, id int64, status, actor string) error {
	table, entityType, err := entityTable(kind)
	if err != nil {
		return err
	}
	now := formatTime(s.now())
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var from string
		// table is a trusted constant from entityTable, not caller input.
		err := tx.QueryRowContext(ctx, `SELECT status FROM `+table+` WHERE id = ?`, id).Scan(&from)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%s %d not found", entityType, id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET status = ?, updated_at = ? WHERE id = ?`, status, now, id); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, s.now(), Event{
			EntityType: entityType,
			EntityID:   strconv.FormatInt(id, 10),
			Type:       EventStatusChanged,
			Actor:      actor,
			FromStatus: &from,
			ToStatus:   &status,
		})
		return err
	})
}

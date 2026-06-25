package db

import (
	"context"
	"database/sql"
)

// GetManager loads the singleton manager record (id=1). The bool reports whether
// the record exists; when absent it returns (zero, false, nil), mirroring the old
// state.LoadManager contract.
func (s *Store) GetManager(ctx context.Context) (Manager, bool, error) {
	var (
		m         Manager
		awaiting  int64
		updatedAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT dir, session_id, watch_watermark, awaiting_lead, updated_at FROM manager WHERE id = 1`,
	).Scan(&m.Dir, &m.SessionID, &m.WatchWatermark, &awaiting, &updatedAt)
	if err == sql.ErrNoRows {
		return Manager{}, false, nil
	}
	if err != nil {
		return Manager{}, false, err
	}
	m.AwaitingLead = awaiting != 0
	if m.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Manager{}, false, err
	}
	return m, true, nil
}

// SetManager upserts the manager identity (dir + session_id). It deliberately does
// NOT write WatchWatermark or AwaitingLead — those are owned by SetWatermark /
// SetAwaitingLead and are preserved across SetManager calls (a fresh insert leaves
// them at their 0 defaults). This replaces state.SaveManager (§2.4).
func (s *Store) SetManager(ctx context.Context, m Manager) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO manager (id, dir, session_id, updated_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			dir = excluded.dir, session_id = excluded.session_id, updated_at = excluded.updated_at`,
		m.Dir, m.SessionID, formatTime(s.now()))
	return err
}

// SetWatermark records the last actionable events.id the manager has consumed. It
// upserts so it is robust even before SetManager has run.
func (s *Store) SetWatermark(ctx context.Context, eventID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO manager (id, watch_watermark, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			watch_watermark = excluded.watch_watermark, updated_at = excluded.updated_at`,
		eventID, formatTime(s.now()))
	return err
}

// SetAwaitingLead sets the awaiting-lead backstop flag (§4.6). It upserts so it is
// robust even before SetManager has run.
func (s *Store) SetAwaitingLead(ctx context.Context, awaiting bool) error {
	v := 0
	if awaiting {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO manager (id, awaiting_lead, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			awaiting_lead = excluded.awaiting_lead, updated_at = excluded.updated_at`,
		v, formatTime(s.now()))
	return err
}

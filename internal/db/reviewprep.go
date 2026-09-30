package db

import (
	"context"
	"database/sql"
)

const reviewPrepColumns = `task_id, head, base_sha, created_at, updated_at`

// SaveReviewPrep records, for taskID, the commit a trust prep staged (head) and the commit it
// staged the reviewers' diff against (base), replacing the previous prep's row. The record
// step reads it back to pin the verdict to the base the reviewers were shown.
func (s *Store) SaveReviewPrep(ctx context.Context, taskID, head, base string) error {
	ts := formatTime(s.now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_preps (`+reviewPrepColumns+`)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			head       = excluded.head,
			base_sha   = excluded.base_sha,
			updated_at = excluded.updated_at`,
		taskID, head, base, ts, ts)
	return err
}

// GetReviewPrep returns taskID's latest prep record. The bool reports existence.
func (s *Store) GetReviewPrep(ctx context.Context, taskID string) (ReviewPrep, bool, error) {
	var (
		p                    ReviewPrep
		createdAt, updatedAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT `+reviewPrepColumns+` FROM review_preps WHERE task_id = ?`, taskID,
	).Scan(&p.TaskID, &p.Head, &p.BaseSHA, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return ReviewPrep{}, false, nil
	}
	if err != nil {
		return ReviewPrep{}, false, err
	}
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return ReviewPrep{}, false, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return ReviewPrep{}, false, err
	}
	return p, true, nil
}

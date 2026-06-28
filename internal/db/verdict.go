package db

import (
	"context"
	"database/sql"
	"time"
)

const verdictColumns = `task_id, overall, reviewed_sha, diff_id, findings, approved_by, approval_sha, created_at, updated_at`

func scanVerdict(sc rowScanner) (Verdict, error) {
	var (
		v                Verdict
		createdAt, updAt string
	)
	if err := sc.Scan(&v.TaskID, &v.Overall, &v.ReviewedSHA, &v.DiffID, &v.Findings,
		&v.ApprovedBy, &v.ApprovalSHA, &createdAt, &updAt); err != nil {
		return Verdict{}, err
	}
	var err error
	if v.CreatedAt, err = parseTime(createdAt); err != nil {
		return Verdict{}, err
	}
	if v.UpdatedAt, err = parseTime(updAt); err != nil {
		return Verdict{}, err
	}
	return v, nil
}

// upsertVerdictTx replaces task v.TaskID's verdict row in place: it inserts a new row
// or, when one already exists, overwrites every mutable column while PRESERVING the
// original created_at (so the row's first-recorded time survives a re-record or a
// carry-forward re-pin). updated_at always advances to now. It takes a queryer so a
// composite operation (RecordDelivery) can run it inside its own transaction, keeping
// the verdict row and the task's summary columns atomically consistent (§2.3).
func upsertVerdictTx(ctx context.Context, q queryer, now time.Time, v Verdict) error {
	if v.Findings == "" {
		v.Findings = "[]"
	}
	ts := formatTime(now)
	_, err := q.ExecContext(ctx, `
		INSERT INTO verdicts (`+verdictColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			overall      = excluded.overall,
			reviewed_sha = excluded.reviewed_sha,
			diff_id      = excluded.diff_id,
			findings     = excluded.findings,
			approved_by  = excluded.approved_by,
			approval_sha = excluded.approval_sha,
			updated_at   = excluded.updated_at`,
		v.TaskID, v.Overall, v.ReviewedSHA, v.DiffID, v.Findings,
		v.ApprovedBy, v.ApprovalSHA, ts, ts)
	return err
}

// SaveVerdict inserts or replaces the verdict row for a task (see upsertVerdictTx). It
// is the standalone writer used where there is no task-summary change to bundle — the
// land carry-forward re-pin. TrustRecord/Approve instead pass the verdict through
// RecordDelivery so the summary and the verdict commit in one transaction.
func (s *Store) SaveVerdict(ctx context.Context, v Verdict) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return upsertVerdictTx(ctx, tx, s.now(), v)
	})
}

// GetVerdict returns the current verdict for a task. The bool reports existence; the
// merge gate treats a missing row as fail-closed (no valid verdict). There is no TTL:
// a verdict is fresh iff its commit/content pins still match the worker head being
// merged, so the gate's freshness check is the caller's reviewed_sha (and DiffID)
// comparison, never the row's age.
func (s *Store) GetVerdict(ctx context.Context, taskID string) (Verdict, bool, error) {
	v, err := scanVerdict(s.db.QueryRowContext(ctx,
		`SELECT `+verdictColumns+` FROM verdicts WHERE task_id = ?`, taskID))
	if err == sql.ErrNoRows {
		return Verdict{}, false, nil
	}
	if err != nil {
		return Verdict{}, false, err
	}
	return v, true, nil
}

// DeleteVerdict removes a task's verdict row — the consume step a gated merge runs once
// the verdict has authorized the fast-forward, so the same verdict can never authorize
// a second merge. It is idempotent: deleting an absent row is not an error.
func (s *Store) DeleteVerdict(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM verdicts WHERE task_id = ?`, taskID)
	return err
}

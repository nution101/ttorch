package db

import (
	"context"
	"database/sql"
	"errors"
)

// ClaimMarker takes a one-time claim recorded on the event spine. In a single BEGIN IMMEDIATE
// write transaction it reads the newest event on task taskID whose type is claimType or
// releaseType and whose payload is key, and appends a claimType event with that payload (by
// actor, non-actionable) only if there is none or the newest one is a release. It returns
// (true, nil) when this call appended the claim and (false, nil) when the key is already held.
//
// Writers serialize on SQLite's write lock in-process and across processes, the foundation
// ClaimTask and ClaimForLand rest on, and the read happens under that lock, so of any number
// of concurrent claimants on one DB exactly one wins. A winner that then fails to do the
// claimed work appends a releaseType event with the same key, which makes the key claimable
// again; nothing is ever deleted, so the spine stays append-only. A winner that dies before
// releasing leaves the key held.
func (s *Store) ClaimMarker(ctx context.Context, taskID, claimType, releaseType, key, actor string) (bool, error) {
	if claimType == "" || releaseType == "" || claimType == releaseType {
		return false, errors.New("ClaimMarker: claim and release types must be distinct and non-empty")
	}
	now := s.now()
	won := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var newest string
		err := tx.QueryRowContext(ctx, `
			SELECT type FROM events
			 WHERE entity_type = 'task' AND entity_id = ? AND type IN (?, ?) AND payload = ?
			 ORDER BY id DESC LIMIT 1`,
			taskID, claimType, releaseType, key).Scan(&newest)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && newest == claimType {
			return nil // held
		}
		if _, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: taskID, Type: claimType, Actor: actor, Payload: key,
		}); err != nil {
			return err
		}
		won = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

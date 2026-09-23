package db

import (
	"context"
	"database/sql"
)

// Consumed is what one ConsumeActionable call took: the watermark the read started
// from, the actionable rows above it (id ascending), and the watermark afterwards.
// When nothing was unread, Events is empty and Watermark == Since.
type Consumed struct {
	Since     int64
	Events    []Event
	Watermark int64
}

// ConsumeActionable is the manager inbox's claim primitive. In one BEGIN IMMEDIATE
// transaction it reads manager.watch_watermark, takes every actionable event above
// max(floor, watermark), and advances the watermark to the highest id it took. Two
// consumers racing for the same updates (`ttorch inbox` and an armed `ttorch watch`)
// are serialized by the write lock, so exactly one of them gets each row and the other
// reads an already-advanced watermark and takes nothing. The watermark only ever moves
// forward here, which is what makes a repeated read idempotent: a second call with no
// new events returns an empty Consumed and writes nothing.
//
// floor lets a caller that already holds a newer watermark than the stored one (a
// watcher that captured it at arm time) refuse to re-take anything at or below it.
func (s *Store) ConsumeActionable(ctx context.Context, floor int64) (Consumed, error) {
	var out Consumed
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var wm int64
		err := tx.QueryRowContext(ctx, `SELECT watch_watermark FROM manager WHERE id = 1`).Scan(&wm)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		since := wm
		if floor > since {
			since = floor
		}
		out = Consumed{Since: since, Watermark: since}
		rows, err := tx.QueryContext(ctx,
			`SELECT `+eventColumns+` FROM events WHERE id > ?`+actionableFilter+` ORDER BY id ASC`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out.Events = append(out.Events, e)
			if e.ID > out.Watermark {
				out.Watermark = e.ID
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if out.Watermark <= wm {
			return nil // nothing new above the stored watermark: leave the row untouched
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO manager (id, watch_watermark, updated_at) VALUES (1, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				watch_watermark = excluded.watch_watermark, updated_at = excluded.updated_at`,
			out.Watermark, formatTime(s.now()))
		return err
	})
	if err != nil {
		return Consumed{}, err
	}
	return out, nil
}

// LatestEvent returns the most recent event of eventType recorded against one entity,
// and whether any exists. The scheduler's watch loop reads its own last wake record
// this way, so its coalescing survives a restart.
func (s *Store) LatestEvent(ctx context.Context, entityType, entityID, eventType string) (Event, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM events
		  WHERE entity_type = ? AND entity_id = ? AND type = ?
		  ORDER BY id DESC LIMIT 1`, entityType, entityID, eventType)
	e, err := scanEvent(row)
	if err == sql.ErrNoRows {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return e, true, nil
}

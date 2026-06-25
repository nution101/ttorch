// Package db is ttorch's SQLite-backed state store: the single, durable,
// transactional source of truth for orchestration state (projects → epics →
// phases → tasks), an append-only events/audit spine that doubles as the
// watcher's signal, and a notes/activity log. It absorbs the persistence that
// internal/state held as JSON files; the pure footprint-overlap helpers stay in
// internal/state.
//
// The driver is modernc.org/sqlite — pure Go, cgo-free — so the binary stays a
// single statically-linked cross-compiled artifact (CGO_ENABLED=0). See the design
// record in docs/design/sqlite-event-architecture.md; section citations (§…) in
// this package refer to it.
package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Store owns a single SQLite connection pool. now is injectable so tests can use
// a deterministic clock.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// dsn builds the connection string. The PRAGMAs and _txlock=immediate are
// load-bearing (§2.1/§1.4):
//   - journal_mode(WAL): many concurrent readers + one writer, readers never block
//     the writer — several workers, the manager, and the watcher all touch the DB.
//   - busy_timeout(5000): a writer that hits a held write lock retries for up to 5s.
//   - foreign_keys(on): FK constraints are enforced (RESTRICT/SET NULL/CASCADE).
//   - synchronous(NORMAL): durable under WAL, faster than FULL.
//   - _txlock=immediate: makes database/sql begin every tx with BEGIN IMMEDIATE,
//     grabbing the write lock up front. This avoids SQLITE_BUSY_SNAPSHOT (517) —
//     which busy_timeout does NOT retry — when two read-then-write txns race, and
//     guarantees events.id order == commit order (serialized immediate writers).
func dsn(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
}

// Open opens (creating if absent) the SQLite database at path, applies the DSN
// PRAGMAs, runs any pending migrations, and returns a ready Store.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	sdb, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	// One writer connection removes in-process self-contention; cross-process
	// concurrency is handled by WAL + busy_timeout. This mandates the §2.3
	// single-connection transaction discipline (helpers take the *sql.Tx).
	sdb.SetMaxOpenConns(1)
	s := &Store{db: sdb, now: time.Now}
	if err := s.Migrate(context.Background()); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// withTx runs fn inside a single transaction. Because the DSN sets
// _txlock=immediate, BeginTx issues BEGIN IMMEDIATE (§1.4).
//
// MANDATORY (§2.3): inside fn, every statement must use the passed *sql.Tx, never
// s.db — with SetMaxOpenConns(1) the tx holds the only connection, so any s.db
// call would block until the context deadline and deadlock the process. Compose
// multi-statement operations as ONE withTx that passes tx to the tx-scoped
// helpers (appendEvent(tx,…), addNote(tx,…), …).
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

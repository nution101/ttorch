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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// guardRealHomeUnderTest fails closed when a `go test` run resolves a DB or state
// path to the user's real ~/.ttorch home. It is a data-loss backstop: Open creates
// state.db there and db.ImportLegacy RENAMES the live state/ directory away, so a
// test that forgot to point TTORCH_HOME at a temp dir would silently take down the
// user's running session. Production (testing.Testing() == false) is never affected
// and migrates normally; a test that genuinely needs the store must set TTORCH_HOME
// (and TTORCH_DB) to a temp dir. The package-level TestMain in the orchestrator/cli
// test packages already guarantees this; this guard catches any path that slips
// past it.
func guardRealHomeUnderTest(path string) error {
	if !testing.Testing() {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	real := filepath.Join(home, ".ttorch")
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	if abs == real || strings.HasPrefix(abs, real+string(os.PathSeparator)) {
		return fmt.Errorf("db: refusing to touch the real ttorch home %q under `go test`; point TTORCH_HOME (and TTORCH_DB) at a temp dir in the test", abs)
	}
	return nil
}

// Open opens (creating if absent) the SQLite database at path, applies the DSN
// PRAGMAs, runs any pending migrations, and returns a ready Store.
func Open(path string) (*Store, error) {
	if err := guardRealHomeUnderTest(path); err != nil {
		return nil, err
	}
	// The DB is a finance-grade audit store: keep its directory and files private
	// (0700 dir / 0600 file) so the audit trail is never group/world-readable. Only a
	// directory WE create is tightened — a pre-existing home is left exactly as the
	// user/installer configured it.
	if dir := filepath.Dir(path); dir != "" {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			// Defeat a permissive umask on the leaf dir we just created.
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, err
			}
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
	// Migrate has created the db file (and, under WAL, its -wal/-shm sidecars), but so
	// far only schema DDL has been written. Restrict it to 0600 BEFORE any task/event
	// data lands. The main file must succeed (an unsecured audit store is a finding);
	// the sidecars carry the same data, so tighten them too — best-effort, since they
	// may not exist at this instant.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("db: securing %s: %w", path, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
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

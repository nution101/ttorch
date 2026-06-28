package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one reversible schema step. SQL lives in migrations/NNNN_<name>.up.sql
// and .down.sql; a shipped migration is never edited (new schema = a new pair).
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// migrations returns the embedded migrations ordered by version.
func migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	byVersion := map[int]*Migration{}
	for _, e := range entries {
		name := e.Name()
		var dir string
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			dir = "up"
		case strings.HasSuffix(name, ".down.sql"):
			dir = "down"
		default:
			continue
		}
		// Filename: NNNN_<name>.{up,down}.sql
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".up.sql"), ".down.sql")
		idx := strings.IndexByte(base, '_')
		if idx <= 0 {
			return nil, fmt.Errorf("migration %q: expected NNNN_<name>.{up,down}.sql", name)
		}
		version, err := strconv.Atoi(base[:idx])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: base[idx+1:]}
			byVersion[version] = m
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		if dir == "up" {
			m.Up = string(b)
		} else {
			m.Down = string(b)
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" || m.Down == "" {
			return nil, fmt.Errorf("migration %04d_%s: missing up or down half", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// schemaVersion returns the current schema version, bootstrapping the
// table-absent case (§1.5): a fresh DB has no schema_migrations table, so a bare
// SELECT MAX(version) would error with "no such table". We first probe
// sqlite_master and treat 0 matching tables as version 0, then read
// COALESCE(MAX(version),0).
func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var present int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`,
	).Scan(&present)
	if err != nil {
		return 0, err
	}
	if present == 0 {
		return 0, nil
	}
	var version int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_migrations`,
	).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// migrationRebuildMarker, present in a migration's SQL, tells the runner to apply that
// migration with foreign-key ENFORCEMENT disabled and to re-validate integrity with
// foreign_key_check afterward (see applyMigration). It is required for any migration that
// REBUILDS a table — SQLite cannot ALTER a CHECK constraint in place, so changing one
// means create-new / copy / drop-old / rename — whose DROP would otherwise fire ON DELETE
// CASCADE on dependent tables and silently destroy their rows. It is a SQL comment, so it
// is inert to SQLite itself.
const migrationRebuildMarker = "-- ttorch:rebuild-table"

func rebuildsTable(sql string) bool { return strings.Contains(sql, migrationRebuildMarker) }

// applyMigration runs one migration body + ledger mutation (fn) in a single transaction.
//
// A normal migration just runs in s.withTx. A table-rebuild migration (rebuild=true)
// cannot run with foreign keys enforced — SQLite fires ON DELETE CASCADE on the rebuilt
// table's children during the DROP and destroys their rows — yet FK enforcement is a
// no-op inside a transaction. So the whole step is pinned to a SINGLE dedicated
// connection (s.db.Conn) where foreign_keys is disabled BEFORE the transaction and
// re-enabled after; pinning guarantees the pragma governs the same connection the rebuild
// runs on, never a fresh pool connection the DSN would silently re-open with
// foreign_keys(on). Referential integrity is re-validated with foreign_key_check while
// enforcement is still off, so a rebuild that orphaned a row fails loudly. (SQLite
// "Making Other Kinds Of Table Schema Changes", steps 1/9/12.)
func (s *Store) applyMigration(ctx context.Context, rebuild bool, fn func(*sql.Tx) error) error {
	if !rebuild {
		return s.withTx(ctx, fn)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	runErr := func() error {
		tx, err := conn.BeginTx(ctx, nil) // BEGIN IMMEDIATE (DSN _txlock=immediate)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}()
	if runErr == nil {
		runErr = foreignKeyCheck(ctx, conn) // integrity gate, enforcement still off
	}
	// Re-enable enforcement no matter how the step exited; a store left with foreign keys
	// disabled would corrupt every later write, so a restore failure on an otherwise-clean
	// step is itself fatal.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil && runErr == nil {
		runErr = fmt.Errorf("re-enabling foreign_keys after migration: %w", err)
	}
	return runErr
}

// foreignKeyCheck runs PRAGMA foreign_key_check and returns an error naming the first
// referential-integrity violations, if any. PRAGMA foreign_key_check verifies regardless
// of whether enforcement is currently enabled, so it is the integrity gate after a
// table-rebuild migration that ran with enforcement disabled.
func foreignKeyCheck(ctx context.Context, q queryer) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		// columns: table, rowid, referred-table (parent), fk-index id
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		violations = append(violations, fmt.Sprintf("%s(rowid=%d) → %s", table.String, rowid.Int64, parent.String))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("foreign_key_check failed: %s", strings.Join(violations, "; "))
	}
	return nil
}

// Migrate applies every pending migration in order, each inside its own
// transaction together with the schema_migrations ledger row, so a crash leaves
// the schema and the ledger consistent.
func (s *Store) Migrate(ctx context.Context) error {
	current, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	all, err := migrations()
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.Version <= current {
			continue
		}
		applied := formatTime(s.now())
		m := m
		err := s.applyMigration(ctx, rebuildsTable(m.Up), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.Up); err != nil {
				return fmt.Errorf("migration %04d_%s up: %w", m.Version, m.Name, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.Version, m.Name, applied,
			); err != nil {
				return fmt.Errorf("migration %04d_%s ledger: %w", m.Version, m.Name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// MigrateDown rolls the schema back to toVersion, applying each migration's Down
// in reverse order. Within each step the ledger row is deleted BEFORE the Down
// SQL runs, so a Down that drops schema_migrations itself (the base migration's
// child-first teardown, §1.5) does not race the delete.
//
// "Reversible" means schema-reversible, not data-reversible: this drops tables and
// destroys data. The data-rollback path is the preserved state.migrated/ dir (§2.5).
func (s *Store) MigrateDown(ctx context.Context, toVersion int) error {
	current, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	all, err := migrations()
	if err != nil {
		return err
	}
	for i := len(all) - 1; i >= 0; i-- {
		m := all[i]
		if m.Version <= toVersion || m.Version > current {
			continue
		}
		err := s.applyMigration(ctx, rebuildsTable(m.Down), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM schema_migrations WHERE version = ?`, m.Version,
			); err != nil {
				return fmt.Errorf("migration %04d_%s ledger delete: %w", m.Version, m.Name, err)
			}
			if _, err := tx.ExecContext(ctx, m.Down); err != nil {
				return fmt.Errorf("migration %04d_%s down: %w", m.Version, m.Name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

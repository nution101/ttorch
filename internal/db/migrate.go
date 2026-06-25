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
		err := s.withTx(ctx, func(tx *sql.Tx) error {
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
		err := s.withTx(ctx, func(tx *sql.Tx) error {
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

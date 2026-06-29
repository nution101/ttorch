package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestFreshDBBootstrapsToV0 proves the §1.5 sqlite_master bootstrap: on a fresh DB
// the schema_migrations table is absent, so a naked SELECT MAX(version) errors;
// schemaVersion must probe sqlite_master and return 0 instead.
func TestFreshDBBootstrapsToV0(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")
	sdb, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	s := &Store{db: sdb, now: time.Now}

	v, err := s.schemaVersion(ctx)
	if err != nil {
		t.Fatalf("schemaVersion on fresh DB: %v", err)
	}
	if v != 0 {
		t.Errorf("fresh DB version = %d, want 0", v)
	}

	// Document WHY the probe is required: the naked query errors on a fresh DB.
	var x sql.NullInt64
	if err := sdb.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&x); err == nil {
		t.Error("expected 'no such table' on fresh DB (justifies the sqlite_master probe)")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err = s.schemaVersion(ctx); err != nil || v != 5 {
		t.Errorf("after Migrate: version=%d err=%v, want 5/nil", v, err)
	}
}

// TestMigrateUpDownUp exercises a full migrate up → down → up cycle with
// foreign_keys ON, proving the child-first DROP order in 0001_initial.down.sql does
// not trip an FK constraint.
func TestMigrateUpDownUp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // Open() already migrated up

	if v, err := s.schemaVersion(ctx); err != nil || v != 5 {
		t.Fatalf("after Open: version=%d err=%v, want 5", v, err)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys=%d err=%v, want 1 (so child-first drop is actually required)", fk, err)
	}
	for _, tbl := range []string{"projects", "epics", "phases", "tasks", "events", "notes", "manager", "verdicts", "schema_migrations"} {
		if !tableExists(t, s, tbl) {
			t.Fatalf("table %q missing after up", tbl)
		}
	}

	if err := s.MigrateDown(ctx, 0); err != nil {
		t.Fatalf("MigrateDown(0): %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 0 {
		t.Fatalf("after down: version=%d err=%v, want 0", v, err)
	}
	for _, tbl := range []string{"tasks", "events", "schema_migrations"} {
		if tableExists(t, s, tbl) {
			t.Errorf("table %q still present after down", tbl)
		}
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	if v, err := s.schemaVersion(ctx); err != nil || v != 5 {
		t.Fatalf("after re-up: version=%d err=%v, want 5", v, err)
	}
	if !tableExists(t, s, "tasks") {
		t.Error("tasks missing after re-up")
	}
	if !tableExists(t, s, "verdicts") {
		t.Error("verdicts missing after re-up")
	}
}

// TestMigrateIdempotent confirms a second Migrate on an up-to-date DB is a no-op.
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 5 {
		t.Errorf("schema_migrations rows = %d, want 5 (no duplicate ledger row)", rows)
	}
}

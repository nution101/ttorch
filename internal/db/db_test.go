package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock is a deterministic, manually-advanced clock for tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestStore opens a Store backed by a fresh temp-file DB.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestStoreClock opens a Store wired to a deterministic clock (set after the
// migrations run, so data timestamps are reproducible).
func newTestStoreClock(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	s := newTestStore(t)
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.now = c.now
	return s, c
}

func TestOpenAppliesPragmas(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var jm string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(jm) != "wal" {
		t.Errorf("journal_mode = %q, want wal", jm)
	}
	var bt int
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&bt); err != nil {
		t.Fatal(err)
	}
	if bt != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", bt)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var sync int
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 { // NORMAL
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
}

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

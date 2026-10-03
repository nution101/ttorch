package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fileState is what a read-only open must leave alone: a file's size, mtime and contents.
type fileState struct {
	exists bool
	size   int64
	mtime  int64
	sum    [32]byte
}

func stateOf(t *testing.T, path string) fileState {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fileState{}
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileState{exists: true, size: fi.Size(), mtime: fi.ModTime().UnixNano(), sum: sha256.Sum256(b)}
}

// TestOpenReadOnlyChangesNothing proves a read-only open reads what a writer committed, refuses
// a write, and leaves the main file and the WAL as it found them, both with no writer open and
// with one holding uncheckpointed frames in the WAL.
func TestOpenReadOnlyChangesNothing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.UpsertProject(ctx, "/repo", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	check := func(label string) {
		t.Helper()
		before, wal := stateOf(t, path), stateOf(t, path+"-wal")
		r, err := OpenReadOnly(path)
		if err != nil {
			t.Fatalf("%s: OpenReadOnly: %v", label, err)
		}
		projects, err := r.ListProjects(ctx)
		if err != nil || len(projects) == 0 {
			t.Fatalf("%s: a read-only store must read what a writer committed: %d projects, err %v", label, len(projects), err)
		}
		if _, err := r.UpsertProject(ctx, "/other", "other"); err == nil {
			t.Errorf("%s: a write through a read-only store was accepted", label)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, path); got != before {
			t.Errorf("%s: the main file changed: before %+v, after %+v", label, before, got)
		}
		// SQLite creates an empty WAL for a reader when there is none; it must never write a
		// frame into one.
		if got := stateOf(t, path+"-wal"); wal.exists && got != wal || !wal.exists && got.size != 0 {
			t.Errorf("%s: the WAL changed: before %+v, after %+v", label, wal, got)
		}
	}
	check("no writer open")

	w, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.UpsertProject(ctx, "/second", "second"); err != nil {
		t.Fatal(err)
	}
	if stateOf(t, path+"-wal").size == 0 {
		t.Fatal("setup: the writer's frame should still be in the WAL")
	}
	check("writer open")
}

// TestOpenReadOnlyRefusesWhatItCannotRead proves a read-only open never creates a store and
// never migrates one: a missing file stays missing, and a store whose schema is behind this
// binary is refused and left at its old version.
func TestOpenReadOnlyRefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.db")
	if _, err := OpenReadOnly(missing); err == nil {
		t.Fatal("a missing store was opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("a read-only open created the store: %v", err)
	}

	path := filepath.Join(dir, "behind.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	all, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := all[len(all)-1].Version
	if err := w.MigrateDown(ctx, latest-1); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, path)

	_, err = OpenReadOnly(path)
	var behind *SchemaBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("a store behind this binary's schema must be refused with SchemaBehindError, got %v", err)
	}
	if behind.Have != latest-1 || behind.Want != latest {
		t.Errorf("SchemaBehindError = have %d want %d, expected have %d want %d", behind.Have, behind.Want, latest-1, latest)
	}
	if got := stateOf(t, path); got != before {
		t.Errorf("the refused open changed the main file: before %+v, after %+v", before, got)
	}
	if got := stateOf(t, path+"-wal"); got.size != 0 {
		t.Errorf("the refused open wrote to the WAL: %+v", got)
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != latest-1 {
		t.Errorf("schema version after the refused open = %d, want %d (untouched)", version, latest-1)
	}
}

// TestGetCoordinator reads the identity row migration 0011 mints, through a read-only open as
// well as a writable one.
func TestGetCoordinator(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := w.GetCoordinator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.CoordID) != 32 || c.Role != CoordinatorRoot || c.Name != "" || c.ParentID != "" || c.CreatedAt.IsZero() {
		t.Errorf("coordinator = %+v, want a 32-hex id, role root, no name or parent, a creation time", c)
	}
	if _, err := w.db.ExecContext(ctx, `UPDATE coordinator SET role = 'peer', name = 'b', parent_id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.GetCoordinator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.CoordID != c.CoordID || got.Role != CoordinatorPeer || got.Name != "b" || got.ParentID != "p1" {
		t.Errorf("coordinator after provisioning = %+v", got)
	}
}

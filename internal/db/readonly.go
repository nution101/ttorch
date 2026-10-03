package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// SchemaBehindError is OpenReadOnly refusing a store whose schema is older than this binary's
// migrations. A read-only open cannot migrate it, and reading it as it is would meet tables and
// columns that do not exist yet.
type SchemaBehindError struct {
	Have, Want int
}

func (e *SchemaBehindError) Error() string {
	return fmt.Sprintf("the state store is at schema %d and this binary needs %d; a read-only open does not migrate, so run any ttorch command on this machine to bring it up to date", e.Have, e.Want)
}

// readOnlyDSN is dsn's read-only counterpart. mode=ro opens the file read-only, so SQLite
// refuses any write and never creates the file; query_only refuses a write at the statement
// level as well. busy_timeout matches the writer's. There is no journal_mode pragma: setting it
// is a write, and the file already records WAL.
func readOnlyDSN(path string) string {
	return "file:" + path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
}

// OpenReadOnly opens an existing state store for reading only. Unlike Open it never creates the
// file, runs no migration and cannot write a row, so a caller that must not change the store (the
// peer control channel's read verbs) can read it. SQLite does create an empty -wal and the -shm
// index beside the file when they are missing, because a WAL reader needs them; it writes no
// frame and never changes the main file, and the sidecars take the main file's 0600 mode.
//
// A store whose schema is behind this binary's migrations is refused with *SchemaBehindError.
// A store ahead of it is read as Open would read it: Open ignores versions it does not know, and
// every query here names its columns.
func OpenReadOnly(path string) (*Store, error) {
	if err := guardRealHomeUnderTest(path); err != nil {
		return nil, err
	}
	// sqlite reports a missing file only once a statement runs; say so plainly instead.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("db: opening %s read-only: %w", path, err)
	}
	sdb, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	s := &Store{db: sdb, now: time.Now}
	have, err := s.schemaVersion(context.Background())
	if err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("db: reading the schema version of %s: %w", path, err)
	}
	all, err := migrations()
	if err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if want := all[len(all)-1].Version; have < want {
		_ = sdb.Close()
		return nil, &SchemaBehindError{Have: have, Want: want}
	}
	return s, nil
}

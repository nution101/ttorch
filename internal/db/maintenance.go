package db

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Verify runs PRAGMA integrity_check and returns an error if the database is not
// "ok". CLI wiring (`ttorch db verify`) lands in a later increment; the method is
// added now per the lead's approval.
func (s *Store) Verify(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("integrity_check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// quoteIdent double-quotes a SQL identifier (table name) read from our own schema.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// sqlLiteral renders a scanned column value as a SQL literal for the dump.
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return "x'" + fmt.Sprintf("%x", x) + "'"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprintf("%v", x), "'", "''") + "'"
	}
}

// schemaObject is one CREATE statement from sqlite_master.
type schemaObject struct{ name, sql string }

// Export writes a schema+data dump (a self-contained .sql script that recreates the
// database) to w. CLI wiring (`ttorch db export`) lands in a later increment; the
// method is added now per the lead's approval.
//
// Table/index identifiers come from our own sqlite_master (not external input);
// every emitted row value is rendered as an escaped SQL literal via sqlLiteral.
func (s *Store) Export(ctx context.Context, w io.Writer) error {
	tables, err := s.schemaObjects(ctx, "table")
	if err != nil {
		return err
	}
	indexes, err := s.schemaObjects(ctx, "index")
	if err != nil {
		return err
	}
	bw := &errWriter{w: w}
	bw.printf("PRAGMA foreign_keys=OFF;\n")
	bw.printf("BEGIN TRANSACTION;\n")
	for _, t := range tables {
		bw.printf("%s;\n", t.sql)
		if err := s.dumpTableData(ctx, bw, t.name); err != nil {
			return err
		}
	}
	for _, idx := range indexes {
		bw.printf("%s;\n", idx.sql)
	}
	bw.printf("COMMIT;\n")
	return bw.err
}

// schemaObjects reads the CREATE statements of the given type (table|index) from
// sqlite_master, skipping internal sqlite_* objects and auto-indexes (NULL sql).
func (s *Store) schemaObjects(ctx context.Context, objType string) ([]schemaObject, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY name`,
		objType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.name, &o.sql); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// dumpTableData writes an INSERT statement for every row of table, ordered by
// rowid for a deterministic dump.
func (s *Store) dumpTableData(ctx context.Context, bw *errWriter, table string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM `+quoteIdent(table)+` ORDER BY rowid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		lits := make([]string, len(cells))
		for i, c := range cells {
			lits[i] = sqlLiteral(c)
		}
		bw.printf("INSERT INTO %s VALUES(%s);\n", quoteIdent(table), strings.Join(lits, ","))
	}
	return rows.Err()
}

// errWriter accumulates the first write error so the dump loop stays readable.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

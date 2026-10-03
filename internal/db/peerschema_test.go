package db

import (
	"context"
	"testing"
)

// TestMigration0012AddsPeerTables proves 0012 creates the parent's registry of peers, their
// repositories and what it delegated to them, enforces the status and kind CHECKs and the
// references to peers, and that its down half drops all three.
func TestMigration0012AddsPeerTables(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tables := []string{"peers", "peer_repos", "peer_delegations"}
	for _, tbl := range tables {
		if !tableExists(t, s, tbl) {
			t.Fatalf("0012 must create %s", tbl)
		}
	}
	peer := func(name, status string) error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO peers (name, control_dest, approve_dest, control_key, status, created_at, updated_at)
			 VALUES (?, 'b@host', 'b@host', '/k', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, name, status)
		return err
	}
	for _, st := range []string{"provisioning", "live", "unreachable", "retired"} {
		if err := peer("p-"+st, st); err != nil {
			t.Errorf("status %s was refused: %v", st, err)
		}
	}
	if err := peer("p-bad", "down"); err == nil {
		t.Error("an unknown peer status was accepted")
	}
	if err := peer("p-live", "live"); err == nil {
		t.Error("a second peer with the same name was accepted")
	}
	var proto, cursor, fails, downs int
	var version, summary, lastErr string
	var lastOK *string
	if err := s.db.QueryRowContext(ctx,
		`SELECT protocol, version, escalation_cursor, summary, consecutive_failures, down_attempts, last_ok_at, last_error
		 FROM peers WHERE name = 'p-live'`,
	).Scan(&proto, &version, &cursor, &summary, &fails, &downs, &lastOK, &lastErr); err != nil {
		t.Fatal(err)
	}
	if proto != 0 || version != "" || cursor != 0 || summary != "{}" || fails != 0 || downs != 0 || lastOK != nil || lastErr != "" {
		t.Errorf("peer defaults = %d %q %d %q %d %d %v %q", proto, version, cursor, summary, fails, downs, lastOK, lastErr)
	}

	repo := func(peer, path string) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO peer_repos (peer, remote_path, origin_url) VALUES (?, ?, 'git@x:y.git')`, peer, path)
		return err
	}
	if err := repo("p-live", "/r"); err != nil {
		t.Fatal(err)
	}
	if err := repo("p-live", "/r"); err == nil {
		t.Error("the same repo path twice for one peer was accepted")
	}
	if err := repo("nobody", "/r"); err == nil {
		t.Error("a repo for an unregistered peer was accepted")
	}

	delegate := func(id, peer, kind string) error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO peer_delegations (request_id, peer, kind, created_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z')`, id, peer, kind)
		return err
	}
	if err := delegate("r1", "p-live", "task"); err != nil {
		t.Fatal(err)
	}
	if err := delegate("r2", "p-live", "goal"); err != nil {
		t.Fatal(err)
	}
	if err := delegate("r1", "p-live", "goal"); err == nil {
		t.Error("a request id delegated twice was accepted")
	}
	if err := delegate("r3", "p-live", "answer"); err == nil {
		t.Error("an unknown delegation kind was accepted")
	}
	if err := delegate("r4", "nobody", "task"); err == nil {
		t.Error("a delegation to an unregistered peer was accepted")
	}

	if err := s.MigrateDown(ctx, 11); err != nil {
		t.Fatalf("MigrateDown(11): %v", err)
	}
	for _, tbl := range tables {
		if tableExists(t, s, tbl) {
			t.Errorf("0012 down left %s behind", tbl)
		}
	}
	if !tableExists(t, s, "coordinator") {
		t.Error("0012 down dropped 0011's coordinator table")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	for _, tbl := range tables {
		if !tableExists(t, s, tbl) {
			t.Errorf("re-up did not recreate %s", tbl)
		}
	}
}

// TestMigration0013AddsTheRaisedLedger proves 0013 adds peer_open_escalations, keyed by peer and
// escalation id and referencing peers, and the two episode flags on peers, defaulting to off;
// and that its down half removes them and leaves 0012's tables.
func TestMigration0013AddsTheRaisedLedger(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if !tableExists(t, s, "peer_open_escalations") {
		t.Fatal("0013 must create peer_open_escalations")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO peers (name, control_dest, approve_dest, control_key, status, created_at, updated_at)
		 VALUES ('p', 'b@host', 'b@host', '/k', 'live', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var reset, bad int
	if err := s.db.QueryRowContext(ctx, `SELECT cursor_reset_open, bad_ids_open FROM peers WHERE name = 'p'`).Scan(&reset, &bad); err != nil || reset != 0 || bad != 0 {
		t.Errorf("episode flags = %d %d, %v; want 0 0", reset, bad, err)
	}
	row := func(peer string, id int64) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO peer_open_escalations (peer, escalation_id, created_at, event_id) VALUES (?, ?, 't', 1)`, peer, id)
		return err
	}
	if err := row("p", 1); err != nil {
		t.Fatal(err)
	}
	if err := row("p", 1); err == nil {
		t.Error("one escalation id twice for one peer was accepted")
	}
	if err := row("nobody", 1); err == nil {
		t.Error("a ledger row for an unregistered peer was accepted")
	}
	if err := s.MigrateDown(ctx, 12); err != nil {
		t.Fatalf("MigrateDown(12): %v", err)
	}
	if tableExists(t, s, "peer_open_escalations") || !tableExists(t, s, "peers") {
		t.Error("0013 down must drop the ledger and keep peers")
	}
	if _, err := s.db.ExecContext(ctx, `SELECT cursor_reset_open FROM peers`); err == nil {
		t.Error("0013 down left cursor_reset_open on peers")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
}

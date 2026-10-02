package db

import (
	"context"
	"testing"
	"time"
)

// TestMigration0011AddsCoordinatorTables proves 0011 creates the three coordinator tables,
// mints exactly one root coordinator row with a random id, enforces the escalation CHECKs and
// one approval escalation per task and done episode, and that its down half drops all three.
func TestMigration0011AddsCoordinatorTables(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, tbl := range []string{"coordinator", "escalations", "peer_requests"} {
		if !tableExists(t, s, tbl) {
			t.Fatalf("0011 must create %s", tbl)
		}
	}

	var (
		n                      int
		coordID, role, created string
		name, parent           string
	)
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM coordinator`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("coordinator rows = %d err=%v, want exactly 1", n, err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT coord_id, role, name, parent_id, created_at FROM coordinator WHERE id = 1`,
	).Scan(&coordID, &role, &name, &parent, &created); err != nil {
		t.Fatal(err)
	}
	if len(coordID) != 32 || role != "root" || name != "" || parent != "" {
		t.Errorf("coordinator row = id %q role %q name %q parent %q, want a 32-hex id, root, no name or parent", coordID, role, name, parent)
	}
	if _, err := time.Parse(time.RFC3339Nano, created); err != nil {
		t.Errorf("coordinator created_at %q does not parse the way the store reads stamps: %v", created, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO coordinator (id, coord_id, role, created_at) VALUES (2, 'x', 'root', '2026-01-01T00:00:00Z')`,
	); err == nil {
		t.Error("a second coordinator row was accepted")
	}

	mkPendingTask(t, s, "schema-t", nil)
	insert := func(task any, kind, status string, source, episode any) error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO escalations (task_id, kind, body, status, source_event_id, episode_event_id, created_at)
			 VALUES (?, ?, 'b', ?, ?, ?, '2026-01-01T00:00:00Z')`,
			task, kind, status, source, episode)
		return err
	}
	if err := insert(nil, "question", "open", nil, nil); err != nil {
		t.Fatalf("a valid escalation was refused: %v", err)
	}
	if err := insert(nil, "question", "open", nil, nil); err != nil {
		t.Fatalf("two escalations with no source or episode must both fit: %v", err)
	}
	if err := insert(nil, "nonsense", "open", nil, nil); err == nil {
		t.Error("an unknown escalation kind was accepted")
	}
	if err := insert(nil, "question", "pending", nil, nil); err == nil {
		t.Error("an unknown escalation status was accepted")
	}
	// One approval escalation per task per done episode; the same approval_required event may
	// back one in each episode.
	if err := insert("schema-t", "approval", "resolved", 7, 0); err != nil {
		t.Fatal(err)
	}
	if err := insert("schema-t", "approval", "open", 7, 12); err != nil {
		t.Fatalf("the same event in a second episode was refused: %v", err)
	}
	if err := insert("schema-t", "approval", "open", 7, 12); err == nil {
		t.Error("a second approval escalation for the same task and episode was accepted")
	}
	if err := insert("schema-t", "approval", "open", nil, nil); err != nil {
		t.Fatalf("a hand-raised approval (no episode) was refused: %v", err)
	}

	// Migrating down past 0011 and up again mints a fresh coordinator id.
	if err := s.MigrateDown(ctx, 10); err != nil {
		t.Fatalf("MigrateDown(10): %v", err)
	}
	for _, tbl := range []string{"coordinator", "escalations", "peer_requests"} {
		if tableExists(t, s, tbl) {
			t.Errorf("0011 down left %s behind", tbl)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	var again string
	if err := s.db.QueryRowContext(ctx, `SELECT coord_id FROM coordinator WHERE id = 1`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again == coordID {
		t.Errorf("coord_id %q repeated across two migrations; it must be random", again)
	}
}

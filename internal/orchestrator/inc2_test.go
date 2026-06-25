package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
)

// TestCCID_Format pins the widened cc id shape cc-HHMMSS-<4hex> (§3.4).
func TestCCID_Format(t *testing.T) {
	if id := ccID(); !regexp.MustCompile(`^cc-\d{6}-[0-9a-f]{4}$`).MatchString(id) {
		t.Fatalf("ccID() = %q, want cc-HHMMSS-<4hex>", id)
	}
}

// TestCCID_DistinctInSameSecond proves the 4hex suffix disambiguates two cc sessions
// opened in the same wall-clock second (identical HHMMSS prefix), so they never
// collide on the tasks TEXT PRIMARY KEY (§3.4). Overriding ccSuffix with a counter
// makes the test deterministic while still exercising ccID's composition.
func TestCCID_DistinctInSameSecond(t *testing.T) {
	orig := ccSuffix
	defer func() { ccSuffix = orig }()
	n := 0
	ccSuffix = func() string { n++; return fmt.Sprintf("%04x", n) }
	if a, b := ccID(), ccID(); a == b {
		t.Fatalf("two cc ids in the same second collided: %q == %q", a, b)
	}
}

// TestCCSuffix_RealIsFourHex pins the real (random) suffix to four lowercase hex
// digits, so the format holds without relying on the test override.
func TestCCSuffix_RealIsFourHex(t *testing.T) {
	if s := ccSuffix(); !regexp.MustCompile(`^[0-9a-f]{4}$`).MatchString(s) {
		t.Fatalf("ccSuffix() = %q, want 4 hex digits", s)
	}
}

// TestSpawn_BacklogRowUpsertsToActive proves a spawn over a pre-existing pending
// backlog row UPDATEs it (no TEXT-PRIMARY-KEY collision) and flips it to active with
// a non-actionable transition, syncing the runtime fields — the §3.4 spawn-from-
// backlog path that `task add` then `spawn <id>` relies on.
func TestSpawn_BacklogRowUpsertsToActive(t *testing.T) {
	m, repo := deliveryHarness(t, "backlog")
	ctx := context.Background()

	// Seed a pending backlog row for the id, as `task add` will once it exists. It
	// has no window, so it is never a live-overlap candidate.
	proj, err := m.Store.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.CreateTask(ctx, db.Task{
		ID: "bk1", ProjectID: proj.ID, Status: db.StatusPending, Title: "queued",
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}

	task, err := m.SpawnWithFootprint("bk1", repo, false, "sleep 30", []string{"internal/x"}, false)
	if err != nil {
		t.Fatalf("spawn over a backlog row must not collide: %v", err)
	}
	if task.Status != db.StatusActive {
		t.Fatalf("status = %q, want active", task.Status)
	}
	if task.Window != "wk-bk1" || task.Worktree == "" {
		t.Fatalf("runtime fields not synced: window=%q worktree=%q", task.Window, task.Worktree)
	}
	if got := fmt.Sprint(task.Footprint); got != "[internal/x]" {
		t.Fatalf("footprint = %s, want [internal/x]", got)
	}

	// Exactly one row for the id (UPDATE, not a duplicate INSERT).
	all, _ := m.Store.ListTasks(ctx, db.TaskFilter{})
	count := 0
	for _, x := range all {
		if x.ID == "bk1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one row for bk1 after spawn, got %d", count)
	}

	// Spawning is manager-authored: it must wake no watcher.
	if actionable, _ := m.Store.EventsSince(ctx, 0, true); len(actionable) != 0 {
		t.Fatalf("spawn must write no actionable event, got %+v", actionable)
	}

	_, _ = m.Teardown("bk1", true)
}

// TestSpawn_WritesWorkerTaskFile proves a spawned worker's worktree carries the
// git-excluded .ttorch/task identity file naming its task and DB (§3.1).
func TestSpawn_WritesWorkerTaskFile(t *testing.T) {
	m, repo := deliveryHarness(t, "idfile")
	task, err := m.SpawnWithFootprint("id1", repo, false, "sleep 30", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Teardown("id1", true)
	id, dbPath, ok := harness.ReadWorkerTaskFile(task.Worktree)
	if !ok || id != "id1" {
		t.Fatalf(".ttorch/task task id = (%q,%v), want (id1,true)", id, ok)
	}
	if dbPath != m.P.StateDB() {
		t.Fatalf(".ttorch/task db = %q, want %q", dbPath, m.P.StateDB())
	}
}

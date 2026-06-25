package db

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLegacyTask writes a <id>.meta.json record into dir, matching the old
// state.Task on-disk shape.
func writeLegacyTask(t *testing.T, dir string, lt legacyTask) {
	t.Helper()
	b, err := json.MarshalIndent(lt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lt.ID+legacyTaskSuffix), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyManager(t *testing.T, dir string, lm legacyManager) {
	t.Helper()
	b, err := json.MarshalIndent(lm, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyManagerFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// createdEventActor returns the actor of the 'created' event for taskID, or "" if
// there is none.
func createdEventActor(t *testing.T, s *Store, taskID string) string {
	t.Helper()
	events, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == EventCreated && e.EntityType == EntityTypeTask && e.EntityID == taskID {
			return e.Actor
		}
	}
	return ""
}

func TestImportLegacy_ImportsRowsAndCreatedEvents(t *testing.T) {
	s, _ := newTestStoreClock(t)
	ctx := context.Background()

	home := t.TempDir()
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	writeLegacyTask(t, stateDir, legacyTask{
		ID: "live-one", Window: "wk-live-one", Worktree: "/wt/live", Project: "/repo/a",
		Harness: "claude", Kind: KindShip, Created: created, SessionID: "sid-1",
		Footprint: []string{"internal/cli", "internal/db"},
	})
	writeLegacyTask(t, stateDir, legacyTask{
		ID: "gone-one", Window: "wk-gone-one", Worktree: "/wt/gone", Project: "/repo/a",
		Harness: "claude", Kind: KindScout, Created: created.Add(time.Minute),
		PR: "https://example/pr/9", GatePassed: true, ApprovedBy: "human", ReviewedSHA: "abcdef",
	})
	writeLegacyManager(t, stateDir, legacyManager{Dir: "/repo/a", SessionID: "mgr-sid"})

	// Only "wk-live-one" is live: it must import as active; the absent one torn_down.
	liveWindows := map[string]bool{"wk-live-one": true}
	n, err := ImportLegacy(ctx, s, stateDir, func(w string) bool { return liveWindows[w] })
	if err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d tasks, want 2", n)
	}

	// Rows present with carried fields intact.
	live, ok, err := s.GetTask(ctx, "live-one")
	if err != nil || !ok {
		t.Fatalf("GetTask(live-one): ok=%v err=%v", ok, err)
	}
	if live.Status != StatusActive {
		t.Errorf("live task status = %q, want active (window is live)", live.Status)
	}
	if live.Kind != KindShip || live.Window != "wk-live-one" || live.Worktree != "/wt/live" {
		t.Errorf("live task carried fields wrong: %+v", live)
	}
	if live.Project != "/repo/a" {
		t.Errorf("live task project = %q, want /repo/a", live.Project)
	}
	if live.SessionID != "sid-1" {
		t.Errorf("live task session id = %q, want sid-1", live.SessionID)
	}
	if len(live.Footprint) != 2 || live.Footprint[0] != "internal/cli" || live.Footprint[1] != "internal/db" {
		t.Errorf("live task footprint = %v, want [internal/cli internal/db]", live.Footprint)
	}
	if !live.Created.Equal(created) {
		t.Errorf("live task created = %s, want %s", live.Created, created)
	}

	gone, ok, err := s.GetTask(ctx, "gone-one")
	if err != nil || !ok {
		t.Fatalf("GetTask(gone-one): ok=%v err=%v", ok, err)
	}
	if gone.Status != StatusTornDown {
		t.Errorf("absent-window task status = %q, want torn_down", gone.Status)
	}
	if gone.Kind != KindScout || gone.PR != "https://example/pr/9" || !gone.GatePassed ||
		gone.ApprovedBy != "human" || gone.ReviewedSHA != "abcdef" {
		t.Errorf("gone task delivery/kind fields wrong: %+v", gone)
	}

	// Both tasks share the one project (same repo path → one UpsertProject).
	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].RepoPath != "/repo/a" {
		t.Fatalf("projects = %+v, want one for /repo/a", projects)
	}

	// A created event, attributed to the import (actor=system), exists for each task.
	if a := createdEventActor(t, s, "live-one"); a != ActorSystem {
		t.Errorf("created-event actor for live-one = %q, want system", a)
	}
	if a := createdEventActor(t, s, "gone-one"); a != ActorSystem {
		t.Errorf("created-event actor for gone-one = %q, want system", a)
	}

	// Manager record imported.
	mgr, ok, err := s.GetManager(ctx)
	if err != nil || !ok {
		t.Fatalf("GetManager: ok=%v err=%v", ok, err)
	}
	if mgr.Dir != "/repo/a" || mgr.SessionID != "mgr-sid" {
		t.Errorf("manager = %+v, want dir=/repo/a session=mgr-sid", mgr)
	}
	// No actionable events were created by the import, so the watermark seeds to 0.
	if mgr.WatchWatermark != 0 {
		t.Errorf("watermark = %d, want 0 (import creates no actionable events)", mgr.WatchWatermark)
	}

	// The legacy dir is preserved (renamed), never deleted.
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("legacy state dir should have been renamed away, stat err = %v", err)
	}
	migrated := filepath.Join(home, migratedDirName)
	if _, err := os.Stat(filepath.Join(migrated, "live-one"+legacyTaskSuffix)); err != nil {
		t.Errorf("migrated dir should preserve the legacy records: %v", err)
	}
}

func TestImportLegacy_Idempotent(t *testing.T) {
	s, _ := newTestStoreClock(t)
	ctx := context.Background()

	home := t.TempDir()
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacyTask(t, stateDir, legacyTask{
		ID: "t1", Window: "wk-t1", Project: "/repo/a", Kind: KindShip,
		Created: time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC),
	})

	first, err := ImportLegacy(ctx, s, stateDir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("first ImportLegacy: %v", err)
	}
	if first != 1 {
		t.Fatalf("first import count = %d, want 1", first)
	}

	tasksAfterFirst, err := s.ListTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	eventsAfterFirst, err := s.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run: the legacy dir is gone (renamed), so this is a clean no-op.
	second, err := ImportLegacy(ctx, s, stateDir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("second ImportLegacy: %v", err)
	}
	if second != 0 {
		t.Fatalf("second import count = %d, want 0 (idempotent no-op)", second)
	}
	tasksAfterSecond, err := s.ListTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	eventsAfterSecond, err := s.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksAfterSecond) != len(tasksAfterFirst) {
		t.Errorf("task count changed on re-import: %d → %d", len(tasksAfterFirst), len(tasksAfterSecond))
	}
	if len(eventsAfterSecond) != len(eventsAfterFirst) {
		t.Errorf("event count changed on re-import: %d → %d", len(eventsAfterFirst), len(eventsAfterSecond))
	}
}

func TestImportLegacy_SkipsNonPristineDB(t *testing.T) {
	s, _ := newTestStoreClock(t)
	ctx := context.Background()

	// Pre-populate the DB so it is not pristine.
	proj, err := s.UpsertProject(ctx, "/repo/existing", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, Task{ID: "pre", ProjectID: proj.ID, Status: StatusActive, Kind: KindShip}, ActorManager); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacyTask(t, stateDir, legacyTask{ID: "legacy", Window: "wk-legacy", Project: "/repo/a", Kind: KindShip})

	n, err := ImportLegacy(ctx, s, stateDir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n != 0 {
		t.Fatalf("import count = %d, want 0 (DB not pristine)", n)
	}
	if _, ok, _ := s.GetTask(ctx, "legacy"); ok {
		t.Error("legacy task must not be imported into a non-pristine DB")
	}
	// A skipped import must not touch the legacy dir.
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("skipped import must leave the legacy dir intact: %v", err)
	}
}

// TestOpenAndImportRefuseRealHomeUnderTest proves the data-loss backstop: under
// `go test`, db.Open and ImportLegacy refuse any path under the user's real
// ~/.ttorch (here simulated by overriding HOME), so a test that forgot to point
// TTORCH_HOME at a temp dir can never create state.db in — or rename away — the
// live session's state. A path outside that home still works.
func TestOpenAndImportRefuseRealHomeUnderTest(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	realStateDB := filepath.Join(fakeHome, ".ttorch", "state.db")
	if _, err := Open(realStateDB); err == nil {
		t.Fatalf("Open must refuse %q (the real ttorch home) under go test", realStateDB)
	}
	realStateDir := filepath.Join(fakeHome, ".ttorch", "state")
	if _, err := ImportLegacy(context.Background(), nil, realStateDir, nil); err == nil {
		t.Fatalf("ImportLegacy must refuse %q (the real ttorch home) under go test", realStateDir)
	}
	// Critically, the guard fired before any filesystem mutation.
	if _, err := os.Stat(filepath.Join(fakeHome, ".ttorch")); !os.IsNotExist(err) {
		t.Fatalf("the guard must not have created the real home dir, stat err = %v", err)
	}
	// A path outside the real home opens fine (TMPDIR-based, not under fakeHome).
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open of a temp path should succeed: %v", err)
	}
	_ = s.Close()
}

func TestImportLegacy_NoStateDir(t *testing.T) {
	s := newTestStore(t)
	n, err := ImportLegacy(context.Background(), s, filepath.Join(t.TempDir(), "does-not-exist"), nil)
	if err != nil {
		t.Fatalf("ImportLegacy on a missing dir should be a no-op, got %v", err)
	}
	if n != 0 {
		t.Fatalf("import count = %d, want 0", n)
	}
}

package orchestrator

import (
	"context"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestSpawnWithEffort_PersistsExplicit proves an explicit --effort is resolved, used,
// and persisted on the task row so a later resume can restore it.
func TestSpawnWithEffort_PersistsExplicit(t *testing.T) {
	m, repo := deliveryHarness(t, "effort-explicit")
	ctx := context.Background()

	task, err := m.SpawnWithEffort("e1", repo, false, "sleep 30", nil, false, "medium", "")
	if err != nil {
		t.Fatalf("SpawnWithEffort: %v", err)
	}
	if task.Effort != "medium" {
		t.Fatalf("returned effort = %q, want medium", task.Effort)
	}
	// Re-read from the DB: this is exactly what restore() threads into the resume command.
	got, ok, err := m.Store.GetTask(ctx, "e1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Effort != "medium" {
		t.Fatalf("persisted effort = %q, want medium", got.Effort)
	}
	_, _ = m.Teardown("e1", true)
}

// TestSpawnEffortDefaults proves the kind defaults when no --effort and no TTORCH_EFFORT:
// a scout defaults to high, a ship to ultracode.
func TestSpawnEffortDefaults(t *testing.T) {
	m, repo := deliveryHarness(t, "effort-defaults")
	t.Setenv("TTORCH_EFFORT", "") // pin the env so only the kind default applies

	scout, err := m.SpawnWithFootprint("s1", repo, true, "sleep 30", nil, false)
	if err != nil {
		t.Fatalf("spawn scout: %v", err)
	}
	if scout.Kind != db.KindScout || scout.Effort != "high" {
		t.Fatalf("scout effort = %q (kind %q), want high/scout", scout.Effort, scout.Kind)
	}
	_, _ = m.Teardown("s1", true)

	ship, err := m.SpawnWithFootprint("p1", repo, false, "sleep 30", nil, false)
	if err != nil {
		t.Fatalf("spawn ship: %v", err)
	}
	if ship.Effort != "ultracode" {
		t.Fatalf("ship effort = %q, want ultracode", ship.Effort)
	}
	_, _ = m.Teardown("p1", true)
}

// TestSpawnEffortEnvFallback proves TTORCH_EFFORT is the fallback when no --effort is
// passed (and that it wins over the kind default).
func TestSpawnEffortEnvFallback(t *testing.T) {
	m, repo := deliveryHarness(t, "effort-env")
	t.Setenv("TTORCH_EFFORT", "low")

	ship, err := m.SpawnWithFootprint("p2", repo, false, "sleep 30", nil, false)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if ship.Effort != "low" {
		t.Fatalf("ship effort = %q, want low (TTORCH_EFFORT)", ship.Effort)
	}
	_, _ = m.Teardown("p2", true)
}

// TestSpawnWithModel_PersistsExplicit proves an explicit --model is resolved, used, and
// persisted on the task row so a later resume restores it (mirrors the effort test).
func TestSpawnWithModel_PersistsExplicit(t *testing.T) {
	m, repo := deliveryHarness(t, "model-explicit")
	ctx := context.Background()

	task, err := m.SpawnWithEffort("m1", repo, false, "sleep 30", nil, false, "", "haiku")
	if err != nil {
		t.Fatalf("SpawnWithEffort: %v", err)
	}
	if task.Model != "haiku" {
		t.Fatalf("returned model = %q, want haiku", task.Model)
	}
	// Re-read from the DB: this is exactly what restore() threads into the resume command.
	got, ok, err := m.Store.GetTask(ctx, "m1")
	if err != nil || !ok {
		t.Fatalf("GetTask: ok=%v err=%v", ok, err)
	}
	if got.Model != "haiku" {
		t.Fatalf("persisted model = %q, want haiku", got.Model)
	}
	_, _ = m.Teardown("m1", true)
}

// TestSpawnModelDefaultsUnset proves an unset model (no --model, no TTORCH_MODEL) resolves
// to "" — claude keeps its own default — so the model dial is opt-in and non-breaking.
func TestSpawnModelDefaultsUnset(t *testing.T) {
	m, repo := deliveryHarness(t, "model-unset")
	t.Setenv("TTORCH_MODEL", "") // pin: no env default

	ship, err := m.SpawnWithFootprint("mu1", repo, false, "sleep 30", nil, false)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if ship.Model != "" {
		t.Fatalf("ship model = %q, want empty (unset)", ship.Model)
	}
	_, _ = m.Teardown("mu1", true)
}

// TestSpawnModelEnvFallback proves TTORCH_MODEL is the fallback when no --model is passed.
func TestSpawnModelEnvFallback(t *testing.T) {
	m, repo := deliveryHarness(t, "model-env")
	t.Setenv("TTORCH_MODEL", "sonnet")

	ship, err := m.SpawnWithFootprint("me1", repo, false, "sleep 30", nil, false)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if ship.Model != "sonnet" {
		t.Fatalf("ship model = %q, want sonnet (TTORCH_MODEL)", ship.Model)
	}
	_, _ = m.Teardown("me1", true)
}

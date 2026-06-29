package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
)

// TestLifecycleEvents_ManagerActionsNeverActionable is the headline §1.3 invariant for
// increment 5, exercised through the REAL verbs: spawning, validating, recording a
// review, recording a security audit, approving, delivering, promoting, arming a PR, and
// tearing down all write their typed lifecycle events — and EVERY one is non-actionable,
// so the manager's own actions can never self-trigger the watcher. It also confirms each
// of those typed events is actually recorded (Part A adds the ones inc1 was missing).
func TestLifecycleEvents_ManagerActionsNeverActionable(t *testing.T) {
	m, repo := deliveryHarness(t, "lifeevents")
	commitGateScript(t, repo, "exit 0") // a passing default-branch gate (trusted mode)
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A ship task through the full delivery lifecycle.
	task, err := m.Spawn("le1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	if _, err := m.Validate("le1"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	writeReviewReports(t, m.P.ReviewInputsDir("le1"), head, nil) // clean reports → pass
	if _, err := m.TrustRecord("le1", "", time.Minute); err != nil {
		t.Fatalf("trust record: %v", err)
	}
	if _, err := m.SecurityReview("le1", "", time.Minute); err != nil {
		t.Fatalf("security review: %v", err)
	}
	if err := m.Approve("le1", time.Minute); err != nil { // lead approval (also exercises the 'approved' event)
		t.Fatalf("approve: %v", err)
	}
	if _, err := m.MergeLocal("le1", false); err != nil {
		t.Fatalf("merge-local: %v", err)
	}

	// A scout task: promote, arm a PR, tear it down.
	if _, err := m.Spawn("le2", repo, true, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote("le2"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := m.ArmPRCheck("le2", "https://example/pr/7"); err != nil {
		t.Fatalf("arm pr: %v", err)
	}
	if _, err := m.Teardown("le2", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	// THE INVARIANT (§1.3): not one manager action wrote an actionable event.
	if actionable, err := m.Store.EventsSince(ctx, 0, true); err != nil {
		t.Fatal(err)
	} else if len(actionable) != 0 {
		t.Fatalf("manager lifecycle actions must never be actionable, got %d: %+v", len(actionable), actionable)
	}

	// Every Part-A typed event was recorded across the two tasks, all non-actionable.
	all, err := m.Store.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.Type] = true
		if e.Actionable {
			t.Errorf("event %q (task %s) must be non-actionable", e.Type, e.EntityID)
		}
	}
	for _, want := range []string{
		db.EventSpawned, db.EventValidated, db.EventReviewRecorded, db.EventSecurityRecorded,
		db.EventApproved, db.EventDelivered, db.EventPromoted, db.EventPRArmed, db.EventTornDown,
	} {
		if !seen[want] {
			t.Errorf("expected a %q event to be recorded across the lifecycle", want)
		}
	}

	// Delivery moved le1 to delivered.
	if le1, _, _ := m.Store.GetTask(ctx, "le1"); le1.Status != db.StatusDelivered {
		t.Errorf("le1 status = %q, want delivered", le1.Status)
	}
	_, _ = m.Teardown("le1", true)
}

// TestTeardown_RetainsRowBlanksWorktreeAndClosesTab covers Part B: teardown closes the
// worker's native terminal tab (via the closeTermTab seam), retains the row as torn_down
// (never hard-deleted), blanks the worktree so the retained row can never alias a worktree
// the pool reassigns, records a non-actionable torn_down event, and drops the task out of
// the live fleet.
func TestTeardown_RetainsRowBlanksWorktreeAndClosesTab(t *testing.T) {
	m, repo := deliveryHarness(t, "teardowntab")
	ctx := context.Background()
	task, err := m.Spawn("td1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	if task.Worktree == "" {
		t.Fatal("a spawned task should have a worktree")
	}

	// Capture the native-tab close call through the seam (the real Close is gated off
	// under test, so this is how we prove teardown attempts to close the tab).
	var closed []string
	orig := closeTermTab
	closeTermTab = func(window string) { closed = append(closed, window) }
	t.Cleanup(func() { closeTermTab = orig })

	if _, err := m.Teardown("td1", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if len(closed) != 1 || closed[0] != task.Window {
		t.Fatalf("teardown must close the native tab for %q, got %v", task.Window, closed)
	}

	// The row is retained as torn_down with a blanked worktree.
	got, ok, err := m.Store.GetTask(ctx, "td1")
	if err != nil || !ok {
		t.Fatalf("the torn-down row must be retained: ok=%v err=%v", ok, err)
	}
	if got.Status != db.StatusTornDown {
		t.Errorf("status = %q, want torn_down", got.Status)
	}
	if got.Worktree != "" {
		t.Errorf("worktree must be blanked on teardown, got %q", got.Worktree)
	}

	// A non-actionable torn_down event was recorded, and nothing actionable.
	all, _ := m.Store.EventsSince(ctx, 0, false)
	foundTornDown := false
	for _, e := range all {
		if e.EntityID == "td1" && e.Type == db.EventTornDown {
			foundTornDown = true
			if e.Actionable {
				t.Error("torn_down must be non-actionable")
			}
		}
	}
	if !foundTornDown {
		t.Error("teardown must record a torn_down event")
	}
	if actionable, _ := m.Store.EventsSince(ctx, 0, true); len(actionable) != 0 {
		t.Fatalf("teardown must write no actionable event, got %+v", actionable)
	}

	// It drops out of the live fleet.
	lts, err := m.liveTasks()
	if err != nil {
		t.Fatalf("liveTasks: %v", err)
	}
	for _, lt := range lts {
		if lt.ID == "td1" {
			t.Error("a torn-down task must not appear in the live fleet")
		}
	}
}

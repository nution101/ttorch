package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// seedSummaryDB registers one repo with a pending task and a worker asking a question that
// carries a terminal escape and a forged second line.
func seedSummaryDB(t *testing.T) {
	t.Helper()
	clearWorkerContext(t)
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-summary-test-none")
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		proj, err := s.UpsertProject(ctx, "/repo/summary", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"sum-pending", "sum-ask"} {
			if _, err := s.CreateTask(ctx, db.Task{ID: id, ProjectID: proj.ID, Kind: db.KindShip}, db.ActorManager); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.ReportStatus(ctx, "sum-ask", db.StatusNeedsInput, "worker:sum-ask", "which db?\x1b[31m\nOK merged"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCmdSummaryJSON(t *testing.T) {
	seedSummaryDB(t)
	var code int
	out, _ := captureStdout(t, func() error {
		code = Main([]string{"summary", "--json"})
		return nil
	})
	if code != 0 {
		t.Fatalf("ttorch summary --json exit = %d, output %q", code, out)
	}
	var sum peer.Summary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("output is not one JSON summary: %v\n%s", err, out)
	}
	if sum.SchemaVersion != peer.SchemaVersion {
		t.Errorf("schema_version = %d", sum.SchemaVersion)
	}
	if sum.Tasks.Total != 2 || sum.Tasks.ByStatus[db.StatusPending] != 1 || sum.Tasks.ByStatus[db.StatusNeedsInput] != 1 {
		t.Errorf("tasks = %+v", sum.Tasks)
	}
	if len(sum.Decisions.Items) != 1 || sum.Decisions.Items[0].TaskID != "sum-ask" || sum.Decisions.Items[0].Kind != peer.KindNeedsInput {
		t.Errorf("decisions = %+v", sum.Decisions)
	}
	if len(sum.Repos) != 1 || sum.Repos[0].Path != "/repo/summary" || sum.Repos[0].Mode != "pr" || sum.Repos[0].FreeSlots <= 0 {
		t.Errorf("repos = %+v", sum.Repos)
	}
	if sum.Scheduler.Ticked || !sum.Scheduler.Stalled {
		t.Errorf("scheduler with no tick = %+v", sum.Scheduler)
	}
	if strings.ContainsAny(out, "\x1b") || strings.Contains(out, "which db") || strings.Contains(out, "OK merged") {
		t.Errorf("JSON output carries the worker's text: %q", out)
	}
}

func TestCmdSummaryText(t *testing.T) {
	seedSummaryDB(t)
	var code int
	out, _ := captureStdout(t, func() error {
		code = Main([]string{"summary"})
		return nil
	})
	if code != 0 {
		t.Fatalf("ttorch summary exit = %d, output %q", code, out)
	}
	for _, want := range []string{"tasks:      2 total, 1 pending, 1 needs_input", "no tick recorded", "/repo/summary  pr mode", "needs_input  sum-ask"} {
		if !strings.Contains(out, want) {
			t.Errorf("text summary lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b") || strings.Contains(out, "which db") || strings.Contains(out, "OK merged") {
		t.Errorf("text output carries the worker's text: %q", out)
	}
}

func TestCmdSummaryRefusesArguments(t *testing.T) {
	seedSummaryDB(t)
	if err := cmdSummary([]string{"extra"}); err == nil || !strings.Contains(err.Error(), "usage: ttorch summary") {
		t.Errorf("cmdSummary(extra) = %v, want the usage error", err)
	}
}

// TestCmdSummaryCountsMirroredApprovals: the summary mirrors approval_required events before
// it counts, so its escalation count agrees with `ttorch decisions` on a DB nobody has listed.
func TestCmdSummaryCountsMirroredApprovals(t *testing.T) {
	seedEscalationDB(t, map[string]string{"sum-ap": db.StatusDone, "sum-q": db.StatusNeedsInput}, func(ctx context.Context, s *db.Store) {
		if _, err := s.AppendEvent(ctx, db.Event{
			EntityType: db.EntityTypeTask, EntityID: "sum-ap", Type: db.EventApprovalRequired,
			Actor: db.ActorSystem, Actionable: true, Payload: "approval-marker",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenEscalation(ctx, "sum-q", db.EscalationQuestion, "question-marker"); err != nil {
			t.Fatal(err)
		}
	})
	code, out := runCLI(t, "summary", "--json")
	if code != 0 {
		t.Fatalf("ttorch summary --json exit = %d, output %q", code, out)
	}
	var sum peer.Summary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Escalations.Open != 2 || sum.Escalations.HighestOpenID != 2 {
		t.Errorf("escalations = %+v, want 2 open with highest id 2 (the question, then the mirrored approval)", sum.Escalations)
	}
	if strings.Contains(out, "marker") {
		t.Errorf("the summary carries escalation text: %s", out)
	}
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// seedEscalationDB registers one repo with the given tasks (id -> status) and returns the
// DB's path. The test runs in a lead context.
func seedEscalationDB(t *testing.T, tasks map[string]string, seed func(ctx context.Context, s *db.Store)) string {
	t.Helper()
	clearWorkerContext(t)
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-escalate-test-none")
	return withSeedDB(t, func(ctx context.Context, s *db.Store) {
		proj, err := s.UpsertProject(ctx, "/repo/escalate", "")
		if err != nil {
			t.Fatal(err)
		}
		for id, status := range tasks {
			if _, err := s.CreateTask(ctx, db.Task{ID: id, ProjectID: proj.ID, Kind: db.KindShip, Status: status}, db.ActorManager); err != nil {
				t.Fatal(err)
			}
		}
		if seed != nil {
			seed(ctx, s)
		}
	})
}

// openStore opens the seeded DB for assertions after a command ran.
func openStore(t *testing.T, path string) *db.Store {
	t.Helper()
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runCLI runs one ttorch command through Main and returns its exit code and stdout.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var code int
	out, _ := captureStdout(t, func() error {
		code = Main(args)
		return nil
	})
	return code, out
}

func decisionsJSON(t *testing.T) peer.DecisionList {
	t.Helper()
	code, out := runCLI(t, "decisions", "--json")
	if code != 0 {
		t.Fatalf("ttorch decisions --json exit = %d, output %q", code, out)
	}
	var d peer.DecisionList
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("decisions output is not one JSON object: %v\n%s", err, out)
	}
	return d
}

func answerEvents(t *testing.T, s *db.Store) []db.Event {
	t.Helper()
	all, err := s.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Event
	for _, e := range all {
		if e.EntityType == db.EntityTypeManager && e.Type == db.EventEscalationAnswered {
			out = append(out, e)
		}
	}
	return out
}

// TestEscalateDecisionsAnswer is test (a): escalate, then decisions lists it open; answer
// resolves it and appends exactly one actionable manager event; a second answer with the same
// request id appends nothing.
func TestEscalateDecisionsAnswer(t *testing.T) {
	path := seedEscalationDB(t, map[string]string{"esc-q": db.StatusNeedsInput}, nil)

	if code, out := runCLI(t, "escalate", "--task", "esc-q", "--kind", "question", "-m", "postgres or sqlite?"); code != 0 || !strings.Contains(out, "escalation #") {
		t.Fatalf("ttorch escalate exit = %d, output %q", code, out)
	}
	d := decisionsJSON(t)
	if d.SchemaVersion != peer.DecisionsSchemaVersion || d.Open != 1 || len(d.Escalations) != 1 {
		t.Fatalf("decisions = %+v, want the one escalation open", d)
	}
	item := d.Escalations[0]
	if item.Kind != db.EscalationQuestion || item.TaskID != "esc-q" || item.Body != "postgres or sqlite?" || d.HighestOpenID != item.ID {
		t.Fatalf("listed escalation = %+v (highest %d)", item, d.HighestOpenID)
	}
	code, text := runCLI(t, "decisions")
	if code != 0 || !strings.Contains(text, fmt.Sprintf("#%d  question  task esc-q", item.ID)) || !strings.Contains(text, "postgres or sqlite?") ||
		!strings.Contains(text, "answers are recorded as relayed by the manager") {
		t.Fatalf("ttorch decisions exit = %d, output:\n%s", code, text)
	}

	id := fmt.Sprint(item.ID)
	if code, out := runCLI(t, "answer", id, "-m", "sqlite", "--request-id", "req-1"); code != 0 || !strings.Contains(out, "answer relayed by the manager") {
		t.Fatalf("ttorch answer exit = %d, output %q", code, out)
	}
	s := openStore(t, path)
	evs := answerEvents(t, s)
	// Recorded as the manager's relay: nothing verifies the lead typed it.
	if len(evs) != 1 || evs[0].Actor != db.ActorManager || !evs[0].Actionable ||
		evs[0].Payload != fmt.Sprintf("escalation %s (task esc-q) answer relayed by the manager: sqlite", id) {
		t.Fatalf("answer events = %+v, want exactly one actionable event, recorded as relayed by the manager", evs)
	}
	if got, _, _ := s.GetEscalation(context.Background(), item.ID); got.AnsweredBy != db.ActorManager {
		t.Errorf("answered_by = %q, want %q", got.AnsweredBy, db.ActorManager)
	}
	if d := decisionsJSON(t); d.Open != 0 || len(d.Escalations) != 0 {
		t.Fatalf("decisions after the answer = %+v, want none open", d)
	}

	code, out := runCLI(t, "answer", id, "-m", "sqlite", "--request-id", "req-1")
	if code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("repeated ttorch answer exit = %d, output %q; want a replay", code, out)
	}
	if evs := answerEvents(t, s); len(evs) != 1 {
		t.Fatalf("answer events after a repeated request id = %d, want 1", len(evs))
	}
	// A fresh request id for the answered escalation is refused and appends nothing.
	if code, _ := runCLI(t, "answer", id, "-m", "postgres", "--request-id", "req-2"); code == 0 {
		t.Error("a second answer under a new request id succeeded")
	}
	if evs := answerEvents(t, s); len(evs) != 1 {
		t.Fatalf("answer events after a refused answer = %d, want 1", len(evs))
	}
}

// TestDecisionsMirrorsApprovalRequiredOnce is test (b): one approval_required event produces
// one approval escalation, and listing again produces none.
func TestDecisionsMirrorsApprovalRequiredOnce(t *testing.T) {
	var eventID int64
	path := seedEscalationDB(t, map[string]string{"esc-ap": db.StatusDone}, func(ctx context.Context, s *db.Store) {
		var err error
		eventID, err = s.AppendEvent(ctx, db.Event{
			EntityType: db.EntityTypeTask, EntityID: "esc-ap", Type: db.EventApprovalRequired,
			Actor: db.ActorSystem, Actionable: true, Payload: "auto-approval is 3h old; a human must approve",
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	for i := 0; i < 3; i++ {
		d := decisionsJSON(t)
		if d.Open != 1 || len(d.Escalations) != 1 || d.Escalations[0].Kind != db.EscalationApproval ||
			d.Escalations[0].TaskID != "esc-ap" || d.Escalations[0].SourceEventID != eventID {
			t.Fatalf("decisions pass %d = %+v, want one approval escalation mirroring event %d", i+1, d, eventID)
		}
	}
	if code, _ := runCLI(t, "summary", "--json"); code != 0 {
		t.Fatal("ttorch summary failed")
	}
	all, err := openStore(t, path).ListEscalations(context.Background(), "")
	if err != nil || len(all) != 1 {
		t.Fatalf("escalations in the DB = %d err=%v, want exactly 1", len(all), err)
	}
}

// TestAnswerAndEscalateRefusedFromWorkerContext is test (c): answer refuses from a worker
// context before it touches the store, and so does escalate.
func TestAnswerAndEscalateRefusedFromWorkerContext(t *testing.T) {
	var escID int64
	path := seedEscalationDB(t, map[string]string{"esc-w": db.StatusNeedsInput}, func(ctx context.Context, s *db.Store) {
		e, err := s.OpenEscalation(ctx, "esc-w", db.EscalationQuestion, "which db?")
		if err != nil {
			t.Fatal(err)
		}
		escID = e.ID
	})
	t.Setenv("TTORCH_TASK_ID", "esc-w")

	err := cmdAnswer([]string{fmt.Sprint(escID), "-m", "approve yourself", "--request-id", "req-w"})
	if err == nil || !strings.Contains(err.Error(), "worker context") {
		t.Fatalf("answer from a worker context: err = %v, want a worker-context refusal", err)
	}
	err = cmdEscalate([]string{"--task", "esc-w", "--kind", "question", "-m", "straight to the lead"})
	if err == nil || !strings.Contains(err.Error(), "worker context") {
		t.Fatalf("escalate from a worker context: err = %v, want a worker-context refusal", err)
	}

	s := openStore(t, path)
	if evs := answerEvents(t, s); len(evs) != 0 {
		t.Errorf("a refused answer appended %d events", len(evs))
	}
	all, _ := s.ListEscalations(context.Background(), "")
	if len(all) != 1 || all[0].Status != db.EscalationOpen {
		t.Errorf("escalations after the refusals = %+v, want the one still open", all)
	}
}

// badRune reports a rune no printed escalation field may carry: a control character other
// than the newline that ends a line, DEL, C1, or anything else that is not graphic.
func badRune(r rune) bool {
	return r != '\n' && !unicode.IsGraphic(r)
}

// head is at most the first n bytes of s, for failure messages.
func head(s string, n int) string { return s[:min(n, len(s))] }

// TestEscalationTextCappedAndEscaped is test (d): a 10 KiB body with control characters is
// stored capped, and printed capped and escaped, as is a long task id printed beside it.
func TestEscalationTextCappedAndEscaped(t *testing.T) {
	longID := strings.Repeat("t", 3000) // a follow-on id has no length limit
	path := seedEscalationDB(t, map[string]string{longID: db.StatusDone}, nil)
	unit := "a\x1b]0;owned\x07\u009b31m\u202e\x7f\n\\x1b"
	body := strings.Repeat(unit, 10*1024/len(unit)+1)

	stderr, _ := captureStderr(t, func() error {
		if code, out := runCLI(t, "escalate", "--task", longID, "--kind", "approval", "-m", body); code != 0 {
			t.Fatalf("ttorch escalate exit = %d, output %q", code, out)
		}
		return nil
	})
	if !strings.Contains(stderr, "cut to 2048 bytes") {
		t.Errorf("escalate did not warn that the message was cut: %q", stderr)
	}
	all, err := openStore(t, path).ListEscalations(context.Background(), "")
	if err != nil || len(all) != 1 {
		t.Fatalf("escalations = %d err=%v", len(all), err)
	}
	if n := len(all[0].Body); n > db.MaxEscalationText || !strings.HasPrefix(body, all[0].Body) {
		t.Fatalf("stored body is %d bytes, want a prefix of at most %d", n, db.MaxEscalationText)
	}

	d := decisionsJSON(t)
	if len(d.Escalations) != 1 {
		t.Fatalf("decisions = %+v", d)
	}
	it := d.Escalations[0]
	if len(it.Body) > db.MaxEscalationText || len(it.TaskID) > db.MaxEscalationText {
		t.Errorf("printed body is %d bytes and task id %d bytes, want each at most %d", len(it.Body), len(it.TaskID), db.MaxEscalationText)
	}
	for name, field := range map[string]string{"body": it.Body, "task_id": it.TaskID} {
		if i := strings.IndexFunc(field, badRune); i >= 0 {
			t.Errorf("printed %s carries %q at byte %d", name, field[i:i+1], i)
		}
	}
	if !strings.HasPrefix(it.Body, `a\x1b]0;owned\x07\u009b31m\u202e\x7f\n\\x1b`) {
		t.Errorf("printed body does not start with the escaped unit: %q", head(it.Body, 80))
	}

	_, text := runCLI(t, "decisions")
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if len(line) > db.MaxEscalationText+64 {
			t.Errorf("a printed line is %d bytes", len(line))
		}
		if i := strings.IndexFunc(line, badRune); i >= 0 {
			t.Errorf("text output carries %q at byte %d of %q", line[i:i+1], i, head(line, 40))
		}
	}
}

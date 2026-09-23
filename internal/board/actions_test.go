package board

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

func taskDone(id string) db.Task {
	return db.Task{ID: id, Window: "wk-" + id, Worktree: "/wt/" + id, Status: db.StatusDone}
}

func decode(t *testing.T, r response) actionResponse {
	t.Helper()
	var out actionResponse
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("action response is not JSON (%d): %q", r.code, r.body)
	}
	return out
}

func answerForm(task string, question int64, text string) url.Values {
	return url.Values{"task": {task}, "question": {strconv.FormatInt(question, 10)}, "answer": {text}}
}

func TestAnswerReachesTheWorkerOnce(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "rebase onto main or release?")

	first := decode(t, h.action("/api/answer", answerForm("t1", q, "main, please\r\n")))
	if !first.OK || !first.Done {
		t.Fatalf("first answer: %+v", first)
	}
	second := decode(t, h.action("/api/answer", answerForm("t1", q, "main, please")))
	if !second.OK || second.Done {
		t.Fatalf("second answer should do nothing: %+v", second)
	}
	if len(h.fleet.sends) != 1 {
		t.Fatalf("sends = %+v, want exactly one", h.fleet.sends)
	}
	if got := h.fleet.sends[0]; got.task != "t1" || got.text != "main, please" {
		t.Fatalf("sent %+v, want the answer with the browser's CRLF trimmed", got)
	}

	// Once answered, the page says so and offers no second form.
	if body := h.sections().body; !strings.Contains(body, "Answered from the board") || strings.Contains(body, `name="question"`) {
		t.Fatalf("answered question still offers a form:\n%s", body)
	}
}

func TestConcurrentAnswersSendOnce(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.srv.Answer(context.Background(), "t1", q, "main")
		}()
	}
	wg.Wait()
	if n := h.fleet.sendCount(); n != 1 {
		t.Fatalf("8 concurrent submits sent %d times, want 1", n)
	}
}

func TestAnswerIsNotResentByARestartedBoard(t *testing.T) {
	store := newStore(t)
	h := newHarnessOn(t, store, &fakeFleet{})
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	if res := decode(t, h.action("/api/answer", answerForm("t1", q, "main"))); !res.Done {
		t.Fatalf("first answer: %+v", res)
	}

	// A second board over the same DB has no memory of the first; the marker event is what
	// stops it.
	fresh := &fakeFleet{}
	h2 := newHarnessOn(t, store, fresh)
	if res := decode(t, h2.action("/api/answer", answerForm("t1", q, "main"))); res.Done {
		t.Fatalf("restarted board re-sent an answered question: %+v", res)
	}
	if n := fresh.sendCount(); n != 0 {
		t.Fatalf("restarted board sent %d times, want 0", n)
	}
}

func TestAnswerToAStaleQuestionIsNotSent(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	old := h.ask("t1", "first question")
	if _, err := h.store.ReportStatus(context.Background(), "t1", db.StatusActive, "worker:t1", ""); err != nil {
		t.Fatal(err)
	}
	newer := h.ask("t1", "second question")

	if res := decode(t, h.action("/api/answer", answerForm("t1", old, "answer to the first"))); res.Done {
		t.Fatalf("answer to a superseded question was sent: %+v", res)
	}
	if res := decode(t, h.action("/api/answer", answerForm("t1", newer, "answer to the second"))); !res.Done {
		t.Fatalf("answer to the current question: %+v", res)
	}
	if len(h.fleet.sends) != 1 || h.fleet.sends[0].text != "answer to the second" {
		t.Fatalf("sends = %+v", h.fleet.sends)
	}
}

func TestAnswerToATaskNoLongerWaitingIsNotSent(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	if _, err := h.store.ReportStatus(context.Background(), "t1", db.StatusActive, "worker:t1", ""); err != nil {
		t.Fatal(err)
	}
	if res := decode(t, h.action("/api/answer", answerForm("t1", q, "main"))); res.Done || !res.OK {
		t.Fatalf("answer to a working task: %+v", res)
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("sends = %d, want 0", n)
	}
}

func TestEmptyAnswerIsRefused(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	r := h.action("/api/answer", answerForm("t1", q, " \r\n\n"))
	if r.code != http.StatusBadRequest || decode(t, r).OK {
		t.Fatalf("empty answer: %d %q", r.code, r.body)
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("sends = %d, want 0", n)
	}
}

func TestFailedSendCanBeRetried(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	h.fleet.sendErr = errors.New("pane is in copy-mode")
	if r := h.action("/api/answer", answerForm("t1", q, "main")); decode(t, r).OK {
		t.Fatalf("a failed send reported success: %q", r.body)
	}
	h.fleet.sendErr = nil
	if res := decode(t, h.action("/api/answer", answerForm("t1", q, "main"))); !res.Done {
		t.Fatalf("retry after a failed send: %+v", res)
	}
	if n := h.fleet.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func TestDispatchClaimsOnce(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "b1", Title: "backlog", HasBrief: true, Footprint: []string{"a.go"}})
	if res := decode(t, h.action("/api/dispatch", url.Values{"task": {"b1"}})); !res.Done {
		t.Fatalf("dispatch: %+v", res)
	}
	if res := decode(t, h.action("/api/dispatch", url.Values{"task": {"b1"}})); res.Done || !res.OK {
		t.Fatalf("second dispatch should do nothing: %+v", res)
	}
	if len(h.fleet.spawns) != 1 {
		t.Fatalf("spawns = %v, want one", h.fleet.spawns)
	}
	got, _, _ := h.store.GetTask(context.Background(), "b1")
	if got.Status != db.StatusActive || got.Owner != "worker:b1" {
		t.Fatalf("after dispatch: status %s owner %s", got.Status, got.Owner)
	}
}

func TestDispatchRefusesATaskWithoutABrief(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "b1"})
	r := h.action("/api/dispatch", url.Values{"task": {"b1"}})
	if r.code != http.StatusBadRequest || decode(t, r).OK {
		t.Fatalf("briefless dispatch: %d %q", r.code, r.body)
	}
	if len(h.fleet.spawns) != 0 {
		t.Fatalf("spawned a briefless task: %v", h.fleet.spawns)
	}
	if got, _, _ := h.store.GetTask(context.Background(), "b1"); got.Status != db.StatusPending {
		t.Fatalf("briefless task moved to %s", got.Status)
	}
}

func TestFailedDispatchReturnsTheTaskToTheBacklog(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "b1", HasBrief: true})
	h.fleet.spawnErr = errors.New("tmux is required")
	if res := decode(t, h.action("/api/dispatch", url.Values{"task": {"b1"}})); res.OK {
		t.Fatalf("failed dispatch reported success: %+v", res)
	}
	if got, _, _ := h.store.GetTask(context.Background(), "b1"); got.Status != db.StatusPending {
		t.Fatalf("after a failed dispatch the task is %s, want pending", got.Status)
	}
	h.fleet.spawnErr = nil
	if res := decode(t, h.action("/api/dispatch", url.Values{"task": {"b1"}})); !res.Done {
		t.Fatalf("retry after a failed dispatch: %+v", res)
	}
}

func TestGatePrepRunsOncePerEscalation(t *testing.T) {
	h := newHarness(t)
	h.addTask(taskDone("g1"))
	ev := h.escalate("g1", `sha=abc123 adversarial review blocked: "high" ["security"] "token logged"`)
	form := url.Values{"task": {"g1"}, "event": {strconv.FormatInt(ev, 10)}}

	res := decode(t, h.action("/api/gate-prep", form))
	if !res.Done || !strings.Contains(res.Message, "ttorch trust record g1") {
		t.Fatalf("gate prep: %+v", res)
	}
	if res := decode(t, h.action("/api/gate-prep", form)); res.Done || !res.OK {
		t.Fatalf("second gate prep should do nothing: %+v", res)
	}
	if len(h.fleet.preps) != 1 {
		t.Fatalf("preps = %v, want one", h.fleet.preps)
	}
	// The gate claim is released afterwards, so the daemon can gate the task again.
	if got, _, _ := h.store.GetTask(context.Background(), "g1"); got.LeaseOwner != "" {
		t.Fatalf("gate claim left held by %q", got.LeaseOwner)
	}

	// A newer escalation is a new decision.
	ev2 := h.escalate("g1", "sha=def456 gate prep refused: worktree not clean")
	if res := decode(t, h.action("/api/gate-prep", url.Values{"task": {"g1"}, "event": {strconv.FormatInt(ev2, 10)}})); !res.Done {
		t.Fatalf("gate prep for a newer escalation: %+v", res)
	}
	// And the old one is superseded.
	if res := decode(t, h.action("/api/gate-prep", form)); res.Done {
		t.Fatalf("gate prep for a superseded escalation ran: %+v", res)
	}
	if len(h.fleet.preps) != 2 {
		t.Fatalf("preps = %v, want two", h.fleet.preps)
	}
}

func TestGatePrepDoesNothingOnceAVerdictIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.addTask(taskDone("g1"))
	ev := h.escalate("g1", "sha=abc123 reviewer stalled")
	if _, err := h.store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: "g1", Type: db.EventReviewRecorded, Actor: db.ActorManager,
	}); err != nil {
		t.Fatal(err)
	}
	if res := decode(t, h.action("/api/gate-prep", url.Values{"task": {"g1"}, "event": {strconv.FormatInt(ev, 10)}})); res.Done {
		t.Fatalf("gate prep ran over an adjudicated escalation: %+v", res)
	}
	if len(h.fleet.preps) != 0 {
		t.Fatalf("preps = %v", h.fleet.preps)
	}
	if body := h.sections().body; strings.Contains(body, "reviewer stalled") {
		t.Fatalf("an adjudicated escalation is still listed:\n%s", body)
	}
}

func TestGatePrepWaitsForTheDaemonGate(t *testing.T) {
	h := newHarness(t)
	h.addTask(taskDone("g1"))
	ev := h.escalate("g1", "sha=abc123 reviewer stalled")
	if won, err := h.store.ClaimForLand(context.Background(), "g1", "gater:g1"); err != nil || !won {
		t.Fatalf("could not take the gate claim: %v %v", won, err)
	}
	if res := decode(t, h.action("/api/gate-prep", url.Values{"task": {"g1"}, "event": {strconv.FormatInt(ev, 10)}})); res.Done {
		t.Fatalf("gate prep ran while the daemon held the gate claim: %+v", res)
	}
	if len(h.fleet.preps) != 0 {
		t.Fatalf("preps = %v", h.fleet.preps)
	}
}

func TestFailedGatePrepCanBeRetried(t *testing.T) {
	h := newHarness(t)
	h.addTask(taskDone("g1"))
	ev := h.escalate("g1", "sha=abc123 gate prep refused: stale base")
	form := url.Values{"task": {"g1"}, "event": {strconv.FormatInt(ev, 10)}}
	h.fleet.prepErr = errors.New("the branch is 2 commit(s) behind main")
	if res := decode(t, h.action("/api/gate-prep", form)); res.OK {
		t.Fatalf("a refused prep reported success: %+v", res)
	}
	h.fleet.prepErr = nil
	if res := decode(t, h.action("/api/gate-prep", form)); !res.Done {
		t.Fatalf("retry after a refused prep: %+v", res)
	}
}

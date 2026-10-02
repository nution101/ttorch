package db

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// managerEvents returns the actionable events addressed to the manager, as the inbox would
// take them.
func managerEvents(t *testing.T, s *Store) []Event {
	t.Helper()
	all, err := s.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range all {
		if e.EntityType == EntityTypeManager {
			out = append(out, e)
		}
	}
	return out
}

func openEscalations(t *testing.T, s *Store) []Escalation {
	t.Helper()
	open, err := s.ListEscalations(context.Background(), EscalationOpen)
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// TestEscalationAnswerAppendsOneManagerEvent is test (a) at the store: an escalation is listed
// open, an answer resolves it and appends exactly one actionable manager event, and a repeat of
// the same request id replays the stored result and appends nothing.
func TestEscalationAnswerAppendsOneManagerEvent(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkPendingTask(t, s, "q1", nil)

	esc, err := s.OpenEscalation(ctx, "q1", EscalationQuestion, "postgres or sqlite?")
	if err != nil {
		t.Fatal(err)
	}
	if esc.ID == 0 || esc.Status != EscalationOpen || esc.TaskID != "q1" || esc.Kind != EscalationQuestion || !esc.CreatedAt.Equal(clk.t) {
		t.Fatalf("opened escalation = %+v", esc)
	}
	if open := openEscalations(t, s); len(open) != 1 || open[0].ID != esc.ID || open[0].Body != "postgres or sqlite?" {
		t.Fatalf("open escalations = %+v, want the one just raised", open)
	}
	if got := managerEvents(t, s); len(got) != 0 {
		t.Fatalf("raising an escalation woke the manager: %+v", got)
	}

	clk.advance(time.Minute)
	res, err := s.AnswerEscalation(ctx, esc.ID, "req-1", "sqlite", ActorManager)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Escalation.Status != EscalationAnswered || res.Escalation.Answer != "sqlite" ||
		res.Escalation.AnswerRequestID != "req-1" || res.Escalation.AnsweredBy != ActorManager ||
		!res.Escalation.ResolvedAt.Equal(clk.t) || res.EventID == 0 {
		t.Fatalf("answer = %+v", res)
	}
	if open := openEscalations(t, s); len(open) != 0 {
		t.Fatalf("an answered escalation is still open: %+v", open)
	}
	evs := managerEvents(t, s)
	if len(evs) != 1 {
		t.Fatalf("manager events after one answer = %d, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	// The answer is recorded as the manager's relay, never as the lead: nothing verifies who
	// typed it.
	if ev.ID != res.EventID || ev.Type != EventEscalationAnswered || ev.Actor != ActorManager || ev.EntityID != "manager" ||
		ev.Payload != "escalation 1 (task q1) answer relayed by the manager: sqlite" {
		t.Errorf("answer event = %+v", ev)
	}

	// The same request again, even with different text, replays and appends nothing.
	clk.advance(time.Minute)
	again, err := s.AnswerEscalation(ctx, esc.ID, "req-1", "postgres after all", ActorManager)
	if err != nil {
		t.Fatalf("a repeated request id must replay, not fail: %v", err)
	}
	if !again.Replayed || again.Escalation.Answer != "sqlite" || again.EventID != res.EventID {
		t.Errorf("replayed answer = %+v, want the stored result", again)
	}
	if got := managerEvents(t, s); len(got) != 1 {
		t.Errorf("manager events after a repeated request = %d, want 1", len(got))
	}
	stored, ok, err := s.GetEscalation(ctx, esc.ID)
	if err != nil || !ok || stored.Answer != "sqlite" {
		t.Errorf("stored escalation after replay = %+v ok=%v err=%v", stored, ok, err)
	}
}

// TestAnswerEscalationRefusals covers the answers that must change nothing: a new request id
// for an escalation already answered, a request id reused for another escalation, an unknown
// escalation, an empty answer and a malformed request id.
func TestAnswerEscalationRefusals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "r1", nil)
	a, err := s.OpenEscalation(ctx, "r1", EscalationQuestion, "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.OpenEscalation(ctx, "r1", EscalationQuestion, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerEscalation(ctx, a.ID, "req-a", "yes", ActorManager); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		id        int64
		requestID string
		answer    string
		want      string
	}{
		{"answered under another request", a.ID, "req-other", "no", "not open"},
		{"request id reused for another escalation", b.ID, "req-a", "no", "already answered escalation"},
		{"unknown escalation", 999, "req-x", "no", "not found"},
		{"empty answer", b.ID, "req-y", "  ", "empty"},
		{"empty request id", b.ID, "", "no", "request id"},
		{"request id with a newline", b.ID, "req\nforged", "no", "request id"},
		{"overlong request id", b.ID, strings.Repeat("r", MaxRequestIDLen+1), "no", "request id"},
	}
	for _, c := range cases {
		if _, err := s.AnswerEscalation(ctx, c.id, c.requestID, c.answer, ActorManager); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
	for _, actor := range []string{ActorLead, ""} {
		if _, err := s.AnswerEscalation(ctx, b.ID, "req-lead", "no", actor); err == nil || !strings.Contains(err.Error(), "relayed") {
			t.Errorf("an answer recorded as %q: err = %v, want a refusal", actor, err)
		}
	}
	if got := managerEvents(t, s); len(got) != 1 {
		t.Errorf("manager events = %d, want only the first answer's", len(got))
	}
	if open := openEscalations(t, s); len(open) != 1 || open[0].ID != b.ID {
		t.Errorf("open escalations = %+v, want only the second", open)
	}
}

// TestOpenEscalationRefusals: an unknown kind or task, an empty body.
func TestOpenEscalationRefusals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "o1", nil)
	if _, err := s.OpenEscalation(ctx, "o1", "nonsense", "b"); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := s.OpenEscalation(ctx, "missing", EscalationQuestion, "b"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown task: err = %v", err)
	}
	if _, err := s.OpenEscalation(ctx, "o1", EscalationQuestion, " \n "); err == nil {
		t.Error("an empty body was accepted")
	}
}

// TestSyncApprovalEscalationsMirrorsOnce is test (b) at the store: one approval_required event
// produces one approval escalation, and syncing again, or a repeat of the gate's refusal (which
// appends no second event), produces none. A mirrored approval is resolved once its task leaves
// done, and an event whose task is not done opens nothing.
func TestSyncApprovalEscalationsMirrorsOnce(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkDoneTask(t, s, "ap1", false)
	mkPendingTask(t, s, "ap2", nil)

	raisedAt := clk.t
	evID, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "ap1", Type: EventApprovalRequired,
		Actor: ActorSystem, Actionable: true, Payload: "auto-approval is 3h old\x1b[2J",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "ap2", Type: EventApprovalRequired, Actor: ActorSystem, Actionable: true,
	}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)

	res, err := s.SyncApprovalEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Opened != 1 || res.Resolved != 0 {
		t.Fatalf("first sync = %+v, want one opened (ap1) and nothing for ap2, which is not done", res)
	}
	open := openEscalations(t, s)
	if len(open) != 1 {
		t.Fatalf("open escalations = %+v, want one", open)
	}
	esc := open[0]
	if esc.Kind != EscalationApproval || esc.TaskID != "ap1" || esc.SourceEventID != evID ||
		esc.Body != "auto-approval is 3h old\x1b[2J" || !esc.CreatedAt.Equal(raisedAt) {
		t.Errorf("mirrored escalation = %+v", esc)
	}
	all, err := s.ListEscalations(ctx, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("all escalations = %d err=%v, want 1", len(all), err)
	}

	for i := 0; i < 2; i++ {
		res, err := s.SyncApprovalEscalations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Opened != 0 || res.Resolved != 0 {
			t.Errorf("repeat sync %d = %+v, want nothing", i+1, res)
		}
	}
	if all, _ := s.ListEscalations(ctx, ""); len(all) != 1 {
		t.Errorf("escalations after repeat syncs = %d, want 1", len(all))
	}

	// The task lands: the approval is no longer waiting on anyone.
	if _, err := s.ReportStatus(ctx, "ap1", StatusDelivered, ActorManager, ""); err != nil {
		t.Fatal(err)
	}
	if res, err := s.SyncApprovalEscalations(ctx); err != nil || res.Opened != 0 || res.Resolved != 1 {
		t.Errorf("sync after the task left done = %+v err=%v, want one resolved", res, err)
	}
	if open := openEscalations(t, s); len(open) != 0 {
		t.Errorf("open escalations after the task left done = %+v", open)
	}
	got, _, _ := s.GetEscalation(ctx, esc.ID)
	if got.Status != EscalationResolved || got.ResolvedAt.IsZero() {
		t.Errorf("resolved escalation = %+v", got)
	}
}

// TestEscalationOpenCounts: the summary's two numbers.
func TestEscalationOpenCounts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if n, hi, err := s.EscalationOpenCounts(ctx); err != nil || n != 0 || hi != 0 {
		t.Fatalf("empty: %d %d %v", n, hi, err)
	}
	mkPendingTask(t, s, "c1", nil)
	var ids []int64
	for _, b := range []string{"a", "b", "c"} {
		e, err := s.OpenEscalation(ctx, "c1", EscalationQuestion, b)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	if _, err := s.AnswerEscalation(ctx, ids[2], "req-c", "done", ActorManager); err != nil {
		t.Fatal(err)
	}
	if n, hi, err := s.EscalationOpenCounts(ctx); err != nil || n != 2 || hi != ids[1] {
		t.Errorf("counts = %d open, highest %d, err %v; want 2 open, highest %d", n, hi, err, ids[1])
	}
}

// TestEscalationTextIsCapped is test (d) at the store: a 10 KiB body full of control
// characters is stored capped at MaxEscalationText bytes, as valid UTF-8 cut on a rune
// boundary. Storage keeps the raw runes; every renderer escapes them. The answer and the
// manager event's payload are capped the same way.
func TestEscalationTextIsCapped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkPendingTask(t, s, "big", nil)
	unit := "\u00e9\x1b[31m\u009b\u202e\x7f\n" // 2+5+2+3+1+1 bytes (U+009B is C1 CSI, U+202E is RLO)
	body := strings.Repeat(unit, 10*1024/len(unit)+1)
	if len(body) < 10*1024 {
		t.Fatalf("fixture is %d bytes", len(body))
	}
	esc, err := s.OpenEscalation(ctx, "big", EscalationQuestion, body)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.GetEscalation(ctx, esc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Body) > MaxEscalationText || len(stored.Body) < MaxEscalationText-4 || !utf8.ValidString(stored.Body) || !strings.HasPrefix(body, stored.Body) {
		t.Errorf("stored body is %d bytes (valid %v, prefix %v), want a valid prefix within 4 bytes under %d",
			len(stored.Body), utf8.ValidString(stored.Body), strings.HasPrefix(body, stored.Body), MaxEscalationText)
	}
	res, err := s.AnswerEscalation(ctx, esc.ID, "req-big", body, ActorManager)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Escalation.Answer); n > MaxEscalationText || !utf8.ValidString(res.Escalation.Answer) {
		t.Errorf("stored answer is %d bytes", n)
	}
	evs := managerEvents(t, s)
	if len(evs) != 1 || len(evs[0].Payload) > MaxEscalationText || !utf8.ValidString(evs[0].Payload) {
		t.Errorf("answer event payload = %d bytes", len(evs[0].Payload))
	}
}

func TestCapText(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"short", 10, "short", false},
		{"exact", 5, "exact", false},
		{"abcdef", 4, "abcd", true},
		{"aé", 2, "a", true},         // é is two bytes; never split it
		{"a\xffb", 10, "a�b", false}, // invalid UTF-8 is replaced before measuring
		{"日本語", 7, "日本", true},
	}
	for _, c := range cases {
		got, cut := CapText(c.in, c.n)
		if got != c.want || cut != c.cut {
			t.Errorf("CapText(%q, %d) = %q, %v; want %q, %v", c.in, c.n, got, cut, c.want, c.cut)
		}
	}
}

// TestApprovalEscalationFollowsDoneEpisodes: an approval escalation follows the task's state,
// not the single approval_required event. The gate appends that event once per task, so a task
// that goes done -> blocked -> done would otherwise sit done and unapproved with no open
// escalation. Each done episode gets at most one, keyed by the status event that opened the
// episode; an answer does not bring it back within the same episode, and a human approval
// ends the requirement.
func TestApprovalEscalationFollowsDoneEpisodes(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStoreClock(t)
	mkDoneTask(t, s, "ep", false)
	worker := "worker:ep"
	if _, err := s.AppendEvent(ctx, Event{
		EntityType: EntityTypeTask, EntityID: "ep", Type: EventApprovalRequired,
		Actor: ActorSystem, Actionable: true, Payload: "auto-approval is 3h old",
	}); err != nil {
		t.Fatal(err)
	}
	sync := func(wantOpened, wantResolved int) {
		t.Helper()
		res, err := s.SyncApprovalEscalations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Opened != wantOpened || res.Resolved != wantResolved {
			t.Fatalf("sync = %+v, want %d opened, %d resolved", res, wantOpened, wantResolved)
		}
	}
	report := func(status string) int64 {
		t.Helper()
		clk.advance(time.Minute)
		ev, err := s.ReportStatus(ctx, "ep", status, worker, "")
		if err != nil {
			t.Fatal(err)
		}
		return ev.ID
	}
	onlyOpen := func(wantEpisode int64) Escalation {
		t.Helper()
		open := openEscalations(t, s)
		if len(open) != 1 || open[0].Kind != EscalationApproval || open[0].EpisodeEventID == nil || *open[0].EpisodeEventID != wantEpisode || open[0].Body != "auto-approval is 3h old" {
			t.Fatalf("open escalations = %+v, want one approval for episode %d", open, wantEpisode)
		}
		return open[0]
	}

	// The task was created done, so its created event (to_status done) opens the first episode.
	var created int64
	for _, e := range taskEvents(t, s, "ep") {
		if e.Type == EventCreated && e.ToStatus != nil && *e.ToStatus == StatusDone {
			created = e.ID
		}
	}
	if created == 0 {
		t.Fatal("the fixture's created event does not carry to_status done")
	}
	sync(1, 0)
	first := onlyOpen(created)

	report(StatusBlocked)
	sync(0, 1)
	if open := openEscalations(t, s); len(open) != 0 {
		t.Fatalf("open while blocked = %+v", open)
	}

	doneAgain := report(StatusDone)
	sync(1, 0)
	second := onlyOpen(doneAgain)
	if second.ID == first.ID || !second.CreatedAt.Equal(clk.t) {
		t.Errorf("second episode's escalation = %+v, want a new row dated when the task became done again", second)
	}
	sync(0, 0)

	// Answered within the episode: not reopened by the next sync.
	if _, err := s.AnswerEscalation(ctx, second.ID, "req-ep", "rework the migration first", ActorManager); err != nil {
		t.Fatal(err)
	}
	sync(0, 0)

	// The next episode opens a new one. A round trip the sync never sees after that leaves it
	// as the only one, since the task is done and an approval is already open.
	report(StatusBlocked)
	third := report(StatusDone)
	sync(1, 0)
	got := onlyOpen(third)
	report(StatusBlocked)
	report(StatusDone)
	sync(0, 0)
	if again := onlyOpen(third); again.ID != got.ID {
		t.Errorf("an unseen round trip replaced the open escalation: %+v -> %+v", got, again)
	}

	// The lead approves: the requirement is met, the open escalation resolves, and no later done
	// episode reopens it.
	if _, err := s.AppendEvent(ctx, Event{EntityType: EntityTypeTask, EntityID: "ep", Type: EventApproved, Actor: ActorLead}); err != nil {
		t.Fatal(err)
	}
	sync(0, 1)
	report(StatusBlocked)
	report(StatusDone)
	sync(0, 0)
	if open := openEscalations(t, s); len(open) != 0 {
		t.Errorf("open after a human approval = %+v", open)
	}
}

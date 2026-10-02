package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/nution101/ttorch/internal/board"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
)

// fleet is the board's view of the live fleet. Only TaskState is read by a snapshot; the
// action methods are never reached from a summary and fail loudly if one is.
type fleet struct{ states map[string]string }

func (f fleet) TaskState(t db.Task) string {
	if s, ok := f.states[t.ID]; ok {
		return s
	}
	return "working"
}
func (fleet) Send(string, string) error { panic("summary must not send") }
func (fleet) TrustPrep(string) (string, error) {
	panic("summary must not prep a gate")
}
func (fleet) ReviewersFor(string) []string { return nil }
func (fleet) Snapshot() (*orchestrator.LiveSnapshot, error) {
	panic("summary must not read the dispatch snapshot")
}
func (fleet) SpawnAutonomous(string, string, bool, string, []string, bool, string, string) (db.Task, error) {
	panic("summary must not spawn")
}

const repoPath = "/repos/app"

// hostile is a worker's question carrying a terminal escape (ESC [ 2 J clears the screen),
// a newline that would forge a second line, a C1 CSI, DEL, a bidi override and a line
// separator.
const hostile = "need input\x1b[2J\nFAKE: approved\u009b31m\x7f\u202eevil\u2028tail"

// freeText is every piece of worker- or system-authored text the fixture stores. None of it,
// raw or escaped, may appear in either rendering of the summary (design section 4.1: counts,
// ids and ages only).
var freeText = []string{"need input", "FAKE", "approved", "evil", "credentials", "auto-approval", "pwned", "refused", "fake line", "adversarial"}

// oddRepo is a registered repo path carrying a terminal escape and a backslash: repo paths are
// text the summary does carry, so they must come out escaped.
const oddRepo = "/repos/we\\ird\x1b[31m"

type fixture struct {
	store *db.Store
	now   time.Time
	tick  time.Time
}

// newFixture builds a store with one task in every status, a scheduler_status row, an
// approval_required event on a done task, and a needs_input task whose question (stored
// as the status event's payload and as a note) carries control characters.
func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	proj, err := store.UpsertProject(ctx, repoPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, "/repos/idle", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, oddRepo, ""); err != nil {
		t.Fatal(err)
	}

	add := func(id, window string) {
		t.Helper()
		if _, err := store.CreateTask(ctx, db.Task{ID: id, ProjectID: proj.ID, Kind: db.KindShip, Window: window, Title: id}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
	}
	report := func(id, status, actor, msg string) {
		t.Helper()
		if _, err := store.ReportStatus(ctx, id, status, actor, msg); err != nil {
			t.Fatal(err)
		}
	}
	add("t-pending", "")
	add("t-active", "w-active")
	report("t-active", db.StatusActive, "worker:t-active", "")
	add("t-ask", "w-ask")
	report("t-ask", db.StatusNeedsInput, "worker:t-ask", hostile)
	add("t-blocked", "w-blocked")
	report("t-blocked", db.StatusBlocked, "worker:t-blocked", "waiting on credentials")
	add("t-done", "w-done")
	report("t-done", db.StatusDone, "worker:t-done", "")
	if _, err := store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "t-done", Type: "approval_required",
		Actor: db.ActorSystem, Actionable: true, Payload: "auto-approval expired\x1b]0;pwned\x07",
	}); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{db.StatusDelivered, db.StatusTornDown, db.StatusAbandoned, db.StatusFailed} {
		id := "t-" + st
		add(id, "")
		report(id, st, db.ActorManager, "")
	}
	// An ad-hoc lead session is not fleet work and must not be counted.
	if _, err := store.CreateTask(ctx, db.Task{ID: "cc-1", ProjectID: proj.ID, Kind: db.KindCC, Window: "cc-1"}, db.ActorManager); err != nil {
		t.Fatal(err)
	}

	if err := store.SetManager(ctx, db.Manager{Dir: "/home/lead", SessionID: "sid"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAwaitingLead(ctx, true); err != nil {
		t.Fatal(err)
	}
	mgr, ok, err := store.GetManager(ctx)
	if err != nil || !ok {
		t.Fatalf("GetManager: ok=%v err=%v", ok, err)
	}
	// The manager row is stamped with the store's wall clock, so the reference clock is
	// placed 90s after it and the scheduler's last tick 42s before the reference clock.
	now := mgr.UpdatedAt.Add(90 * time.Second)
	tick := now.Add(-42 * time.Second)
	if _, err := store.RecordSchedulerTick(ctx, db.SchedulerTick{At: tick.Add(-time.Minute), Errors: 2, LastError: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSchedulerTick(ctx, db.SchedulerTick{At: tick, Errors: 1, LastError: "land: refused\nfake line"}); err != nil {
		t.Fatal(err)
	}

	return fixture{store: store, now: now, tick: tick}
}

func (f fixture) sources(t *testing.T) Sources {
	t.Helper()
	b, err := board.New(board.Config{
		Store: f.store,
		Fleet: fleet{states: map[string]string{"t-blocked": "idle", "t-done": "gone"}},
		Mode:  func(string) string { return "trusted" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return Sources{
		Store: f.store,
		Board: b,
		Mode: func(repo string) string {
			if repo == repoPath {
				return "trusted"
			}
			return "pr"
		},
		FreeSlots:        func(repo string) int { return map[string]int{repoPath: 2, "/repos/idle": 4}[repo] },
		ManagerWindow:    func() bool { return true },
		SchedulerRunning: func() bool { return true },
		StaleAfter:       time.Hour,
	}
}

func TestBuildSummary(t *testing.T) {
	f := newFixture(t)
	now := f.now
	sum, err := Build(context.Background(), f.sources(t), now)
	if err != nil {
		t.Fatal(err)
	}

	if sum.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", sum.SchemaVersion)
	}
	if !sum.GeneratedAt.Equal(now) {
		t.Errorf("generated_at = %v, want %v", sum.GeneratedAt, now)
	}

	wantCounts := map[string]int{
		db.StatusPending: 1, db.StatusActive: 1, db.StatusNeedsInput: 1, db.StatusBlocked: 1,
		db.StatusDone: 1, db.StatusDelivered: 1, db.StatusTornDown: 1, db.StatusAbandoned: 1,
		db.StatusFailed: 1,
	}
	if !reflect.DeepEqual(sum.Tasks.ByStatus, wantCounts) {
		t.Errorf("tasks.by_status = %v, want %v", sum.Tasks.ByStatus, wantCounts)
	}
	if sum.Tasks.Total != 9 {
		t.Errorf("tasks.total = %d, want 9 (the cc session is not counted)", sum.Tasks.Total)
	}

	// Workers are the board's: windowed tasks in a live status (active, ask, blocked, done).
	wantWorkers := map[string]int{"working": 2, "idle": 1, "gone": 1}
	if !reflect.DeepEqual(sum.Workers.ByState, wantWorkers) || sum.Workers.Total != 4 {
		t.Errorf("workers = %+v, want total 4 by_state %v", sum.Workers, wantWorkers)
	}

	d := sum.Decisions
	if d.Pending != 3 || len(d.Items) != 3 {
		t.Fatalf("decisions = %+v, want 3 pending items (two questions, one approval)", d)
	}
	byTask := map[string]Decision{}
	for _, it := range d.Items {
		byTask[it.TaskID] = it
	}
	ask, blocked, ap := byTask["t-ask"], byTask["t-blocked"], byTask["t-done"]
	if ask.Kind != KindNeedsInput || ask.Project != "app" || ask.EventID == 0 || ask.AgeSeconds == nil {
		t.Errorf("needs_input decision = %+v", ask)
	}
	if blocked.Kind != KindBlocked || blocked.EventID == 0 || blocked.AgeSeconds == nil {
		t.Errorf("blocked decision = %+v", blocked)
	}
	// The board reads no time or event for an approval, so it has neither.
	if ap.Kind != KindApproval || ap.Project != "app" || ap.EventID != 0 || ap.AgeSeconds != nil {
		t.Errorf("approval decision = %+v", ap)
	}
	if want := max(ask.EventID, blocked.EventID); d.NewestEventID != want {
		t.Errorf("newest_event_id = %d, want %d", d.NewestEventID, want)
	}

	sc := sum.Scheduler
	if !sc.Running || !sc.Ticked || sc.Stalled || sc.Ticks != 2 || sc.Errors != 3 {
		t.Errorf("scheduler = %+v", sc)
	}
	if sc.LastTickAgeSeconds == nil || *sc.LastTickAgeSeconds != 42 {
		t.Errorf("last_tick_age_seconds = %v, want 42", sc.LastTickAgeSeconds)
	}
	if sc.LastErrorAgeSeconds == nil || *sc.LastErrorAgeSeconds != 42 {
		t.Errorf("last_error_age_seconds = %v, want 42", sc.LastErrorAgeSeconds)
	}

	m := sum.Manager
	if !m.Registered || !m.WindowPresent || !m.AwaitingLead {
		t.Errorf("manager = %+v", m)
	}
	if m.LastActionAgeSeconds == nil || *m.LastActionAgeSeconds != 90 {
		t.Errorf("manager last_action_age_seconds = %v, want 90", m.LastActionAgeSeconds)
	}

	wantRepos := []Repo{
		{Path: repoPath, Name: "app", Mode: "trusted", FreeSlots: 2},
		{Path: "/repos/idle", Name: "idle", Mode: "pr", FreeSlots: 4},
		{Path: `/repos/we\\ird\x1b[31m`, Name: `we\\ird\x1b[31m`, Mode: "pr"},
	}
	if !reflect.DeepEqual(sum.Repos, wantRepos) {
		t.Errorf("repos = %+v, want %+v", sum.Repos, wantRepos)
	}
}

// TestSummaryAges covers the health ages at their edges: a stale tick is reported stalled
// with its true age, a clock that runs behind the stored stamps floors to zero rather than
// going negative, and a coordinator with no tick and no manager row reports nulls.
func TestSummaryAges(t *testing.T) {
	f := newFixture(t)
	src := f.sources(t)

	stale, err := Build(context.Background(), src, f.tick.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Scheduler.Stalled || *stale.Scheduler.LastTickAgeSeconds != 7200 {
		t.Errorf("stale scheduler = %+v", stale.Scheduler)
	}

	behind, err := Build(context.Background(), src, f.tick.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if *behind.Scheduler.LastTickAgeSeconds != 0 {
		t.Errorf("age with a clock behind the tick = %d, want 0", *behind.Scheduler.LastTickAgeSeconds)
	}

	empty, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	b, err := board.New(board.Config{Store: empty, Fleet: fleet{}})
	if err != nil {
		t.Fatal(err)
	}
	src = Sources{Store: empty, Board: b}
	sum, err := Build(context.Background(), src, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Scheduler.Ticked || !sum.Scheduler.Stalled || sum.Scheduler.LastTickAgeSeconds != nil || sum.Scheduler.LastErrorAgeSeconds != nil {
		t.Errorf("empty scheduler = %+v", sum.Scheduler)
	}
	if sum.Manager.Registered || sum.Manager.LastActionAgeSeconds != nil {
		t.Errorf("empty manager = %+v", sum.Manager)
	}
	if sum.Tasks.Total != 0 || len(sum.Tasks.ByStatus) != 9 {
		t.Errorf("empty tasks = %+v, want every status present at zero", sum.Tasks)
	}
	out, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repos":[]`, `"items":[]`, `"newest_event_id":0`, `"last_tick_age_seconds":null`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("empty summary JSON lacks %s: %s", want, out)
		}
	}
}

// TestSummaryOutputIsTerminalSafe holds that neither rendering puts a control or invisible
// rune on the terminal, and that neither carries any of the free text the fixture stored,
// including an escalated gate's reason and findings.
func TestSummaryOutputIsTerminalSafe(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: "t-done", Type: db.EventGateBlocked,
		Actor: db.ActorSystem, Actionable: true,
		Payload: "sha=abc123 adversarial review blocked: \"high\" \"evil\x1b[2J\"",
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := Build(context.Background(), f.sources(t), f.now)
	if err != nil {
		t.Fatal(err)
	}
	var js, text bytes.Buffer
	if err := sum.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if err := sum.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"json": js.String(), "text": text.String()} {
		for _, r := range out {
			if r != '\n' && (unicode.IsControl(r) || !unicode.IsGraphic(r)) {
				t.Errorf("%s output carries %U", name, r)
			}
		}
		for _, ft := range freeText {
			if strings.Contains(out, ft) {
				t.Errorf("%s output carries free text %q:\n%s", name, ft, out)
			}
		}
		for _, id := range []string{"t-ask", "t-blocked", "t-done", KindGate, KindApproval} {
			if !strings.Contains(out, id) {
				t.Errorf("%s output lacks %q:\n%s", name, id, out)
			}
		}
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":       "plain text",
		"tab\there":        `tab\there`,
		"cr\rlf\n":         `cr\rlf\n`,
		"esc\x1b[0m":       `esc\x1b[0m`,
		"del\x7f":          `del\x7f`,
		"nel\u0085":        `nel\u0085`,
		"csi\u009b":        `csi\u009b`,
		"rlo\u202e":        `rlo\u202e`,
		"ls\u2028ps\u2029": `ls\u2028ps\u2029`,
		"zwj\u200d":        `zwj\u200d`,
		"bad\xffutf8":      "bad�utf8",
		"wide 日本 ü":        "wide 日本 ü",
		`back\slash "q"`:   `back\\slash "q"`,
		// Typed text that looks like an escape must not read as one.
		`typed \x1b`:       `typed \\x1b`,
		"real \x1b":        `real \x1b`,
		"plane1\U000E0001": `plane1\U000e0001`,
	} {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSummarySchema pins the JSON schema: its version and every field name at every level.
// A change here is a protocol change, and the version moves with it.
func TestSummarySchema(t *testing.T) {
	if SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d; a schema change must update this test", SchemaVersion)
	}
	f := newFixture(t)
	src := f.sources(t)
	if _, err := f.store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: "t-done", Type: db.EventGateBlocked,
		Actor: db.ActorSystem, Actionable: true,
		Payload: `sha=abc123 adversarial review blocked: "high" "x"; "low" "y"`,
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := Build(context.Background(), src, f.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"":                  {"decisions", "generated_at", "manager", "repos", "scheduler", "schema_version", "tasks", "workers"},
		"tasks":             {"by_status", "total"},
		"workers":           {"by_state", "total"},
		"decisions":         {"items", "newest_event_id", "pending"},
		"decisions.items[]": {"age_seconds", "event_id", "kind", "project", "task_id"},
		"scheduler":         {"daemon_running", "errors", "has_ticked", "last_error_age_seconds", "last_tick_age_seconds", "stalled", "tick_count"},
		"manager":           {"awaiting_lead", "last_action_age_seconds", "registered", "window_present"},
		"repos[]":           {"free_slots", "mode", "name", "path"},
	}
	for path, fields := range want {
		obj := lookup(t, doc, path)
		var got []string
		for k := range obj {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, fields) {
			t.Errorf("%q fields = %v, want %v", path, got, fields)
		}
	}
	if v, _ := doc["schema_version"].(float64); v != 1 {
		t.Errorf("schema_version in JSON = %v", doc["schema_version"])
	}
	if _, err := time.Parse(time.RFC3339, doc["generated_at"].(string)); err != nil {
		t.Errorf("generated_at is not RFC 3339: %v", err)
	}
}

// lookup walks a dotted path through decoded JSON; a "[]" suffix takes the first element.
func lookup(t *testing.T, doc map[string]any, path string) map[string]any {
	t.Helper()
	cur := doc
	if path == "" {
		return cur
	}
	for _, part := range strings.Split(path, ".") {
		name, list := strings.CutSuffix(part, "[]")
		v := cur[name]
		if list {
			arr, ok := v.([]any)
			if !ok || len(arr) == 0 {
				t.Fatalf("%s: no element to inspect in %v", path, v)
			}
			v = arr[0]
		}
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %v is not an object", path, v)
		}
		cur = obj
	}
	return cur
}

package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/nution101/ttorch/internal/db"
)

// TestServeVerbList pins the exact verb list. Adding a verb widens what the parent's control
// key can do on a peer, so it must be a visible change to this test as well as to serve.go,
// which the gate covers. None of these may ever appear: approve, merge-local, land, trust,
// spawn, send, peek, teardown.
func TestServeVerbList(t *testing.T) {
	want := []string{"version", "summary", "decisions", "task-add", "goal", "answer", "ensure-up"}
	if got := Verbs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Verbs() = %q, want exactly %q", got, want)
	}
	for _, v := range verbs {
		if v.serve == nil {
			t.Errorf("verb %s has no handler", v.name)
		}
	}
}

// unwired is a Host whose every hook fails the test: a refused request must not reach one.
func unwired(t *testing.T) Host {
	fail := func(what string) { t.Helper(); t.Errorf("a refused request reached %s", what) }
	return Host{
		ReadStore:      func() (*db.Store, error) { fail("ReadStore"); return nil, errors.New("unwired") },
		Store:          func() (*db.Store, error) { fail("Store"); return nil, errors.New("unwired") },
		SummarySources: func(*db.Store) (Sources, error) { fail("SummarySources"); return Sources{}, errors.New("unwired") },
		AddTask: func(context.Context, *db.Store, TaskAdd) (db.TaskAddResult, string, error) {
			fail("AddTask")
			return db.TaskAddResult{}, "", errors.New("unwired")
		},
		EnsureUp: func(context.Context) (EnsureUpResult, error) {
			fail("EnsureUp")
			return EnsureUpResult{}, errors.New("unwired")
		},
	}
}

// serveCall runs Serve and decodes the one response it must write.
func serveCall(t *testing.T, h Host, command, body string) (int, Response, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Serve(context.Background(), command, strings.NewReader(body), &out, &errOut, "", h)
	return code, decodeResponse(t, out.Bytes()), errOut.String()
}

func decodeResponse(t *testing.T, raw []byte) Response {
	t.Helper()
	if bytes.Count(raw, []byte("\n")) != 1 || !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("the response must be one line of JSON, got %q", raw)
	}
	var r Response
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("the response is not a Response: %v\n%s", err, raw)
	}
	if r.Protocol != ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", r.Protocol, ProtocolVersion)
	}
	return r
}

func wantRefusal(t *testing.T, label string, code int, r Response, stderr, wantCode string) {
	t.Helper()
	if code != 1 || r.OK || r.Error == nil || r.Error.Code != wantCode || r.Result != nil {
		t.Errorf("%s: exit %d, response %+v; want exit 1 refused with %s", label, code, r, wantCode)
		return
	}
	if !strings.Contains(stderr, wantCode) {
		t.Errorf("%s: stderr %q does not name %s", label, stderr, wantCode)
	}
}

// TestServeRefusesWhatIsNotAVerb proves the command string is never more than a lookup: every
// non-verb, every verb with arguments, and every shell construct is refused with a named code
// before the body is read or the store is opened.
func TestServeRefusesWhatIsNotAVerb(t *testing.T) {
	cases := []struct{ command, code string }{
		{"", CodeNoCommand},
		{"   \t ", CodeNoCommand},
		{"approve", CodeUnknownVerb},
		{"approve t1", CodeUnknownVerb},
		{"merge-local t1", CodeUnknownVerb},
		{"land", CodeUnknownVerb},
		{"trust prep t1", CodeUnknownVerb},
		{"review-diff t1", CodeUnknownVerb},
		{"spawn", CodeUnknownVerb},
		{"send t1 hello", CodeUnknownVerb},
		{"teardown t1", CodeUnknownVerb},
		{"ttorch approve t1", CodeUnknownVerb},
		{"sh -c 'ttorch approve t1'", CodeUnknownVerb},
		{"summary; touch /tmp/x", CodeUnknownVerb},
		{"summary&&id", CodeUnknownVerb},
		{"$(id)", CodeUnknownVerb},
		{"`id`", CodeUnknownVerb},
		{"SUMMARY", CodeUnknownVerb},
		{"summary\x00", CodeUnknownVerb},
		{"summary\x1b[2J", CodeUnknownVerb},
		{"summary extra", CodeBadCommand},
		{"summary ; touch /tmp/x", CodeBadCommand},
		{"summary\ntouch /tmp/x", CodeBadCommand},
		{"version --help", CodeBadCommand},
		{strings.Repeat("a", MaxCommand+1), CodeBadCommand},
	}
	for _, c := range cases {
		code, r, stderr := serveCall(t, unwired(t), c.command, `{}`)
		wantRefusal(t, c.command, code, r, stderr, c.code)
		if strings.ContainsAny(r.Error.Message, "\x00\x1b\n") || strings.ContainsAny(stderr[:len(stderr)-1], "\x00\x1b\n") {
			t.Errorf("%q: the refusal echoed a raw control character: %q / %q", c.command, r.Error.Message, stderr)
		}
	}
	_, r, _ := serveCall(t, unwired(t), "approve\x1b[2J", `{}`)
	if want := `"approve\x1b[2J" is not a verb`; !strings.Contains(r.Error.Message, want) {
		t.Errorf("the refusal message = %q, want it to quote the token once, as %s", r.Error.Message, want)
	}
	for _, v := range Verbs() {
		var out bytes.Buffer
		Serve(context.Background(), v, strings.NewReader(`{}`), &out, &bytes.Buffer{}, "", Host{})
		if r := decodeResponse(t, out.Bytes()); r.Error != nil && (r.Error.Code == CodeUnknownVerb || r.Error.Code == CodeBadCommand) {
			t.Errorf("verb %s was refused as a command: %+v", v, r.Error)
		}
	}
}

// TestServeRefusesAWorkerContext: the accident guard runs before the body is read.
func TestServeRefusesAWorkerContext(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Serve(context.Background(), VerbGoal, strings.NewReader(`{"request_id":"r1","text":"x"}`), &out, &errOut, "TTORCH_TASK_ID=t1", unwired(t))
	wantRefusal(t, "worker context", code, decodeResponse(t, out.Bytes()), errOut.String(), CodeWorkerContext)
}

// TestServeBodyIsOneBoundedObject proves the body is read up to MaxBody and no further, must be
// a single JSON object, and may carry no field the verb does not define.
func TestServeBodyIsOneBoundedObject(t *testing.T) {
	cases := []struct{ label, body, code string }{
		{"one byte over the cap", "{" + strings.Repeat(" ", MaxBody-1) + "}", CodeBodyTooLarge},
		{"2 MiB", strings.Repeat("x", 2<<20), CodeBodyTooLarge},
		{"an array", `[]`, CodeBadBody},
		{"null", `null`, CodeBadBody},
		{"a string", `"summary"`, CodeBadBody},
		{"two objects", `{}{}`, CodeBadBody},
		{"trailing text", `{} touch /tmp/x`, CodeBadBody},
		{"an unknown field", `{"command":"approve"}`, CodeBadBody},
		{"malformed", `{"since":`, CodeBadBody},
		{"a wrong type", `{"since":"1"}`, CodeBadBody},
	}
	for _, c := range cases {
		verb := VerbVersion
		if strings.Contains(c.body, "since") {
			verb = VerbDecisions
		}
		code, r, stderr := serveCall(t, unwired(t), verb, c.body)
		wantRefusal(t, c.label, code, r, stderr, c.code)
	}

	// Exactly at the cap is read; an empty body reads as {}.
	h := Host{ReadStore: func() (*db.Store, error) { return nil, errors.New("not here") }}
	for label, body := range map[string]string{"at the cap": "{" + strings.Repeat(" ", MaxBody-2) + "}", "empty": ""} {
		code, r, _ := serveCall(t, h, VerbVersion, body)
		if code != 1 || r.Error == nil || r.Error.Code != CodeUnavailable {
			t.Errorf("%s: want the body accepted and the missing store reported, got %+v", label, r.Error)
		}
	}
}

// TestServeRequestFields covers the fields each mutating verb checks before it opens anything.
func TestServeRequestFields(t *testing.T) {
	brief := `"brief":"# b"`
	cases := []struct{ label, verb, body string }{
		{"task-add without a request id", VerbTaskAdd, `{"task_id":"t1","repo":"/r",` + brief + `}`},
		{"task-add with a request id holding a space", VerbTaskAdd, `{"request_id":"a b","task_id":"t1","repo":"/r",` + brief + `}`},
		{"task-add without a task id", VerbTaskAdd, `{"request_id":"r1","repo":"/r",` + brief + `}`},
		{"task-add with a path for an id", VerbTaskAdd, `{"request_id":"r1","task_id":"../x","repo":"/r",` + brief + `}`},
		{"task-add with an overlong id", VerbTaskAdd, `{"request_id":"r1","task_id":"` + strings.Repeat("t", MaxTaskID+1) + `","repo":"/r",` + brief + `}`},
		{"task-add without a repo", VerbTaskAdd, `{"request_id":"r1","task_id":"t1",` + brief + `}`},
		{"task-add with a two-line title", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r","title":"a\nb",` + brief + `}`},
		{"task-add with a comma in a touch", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r","touches":["a,b"],` + brief + `}`},
		{"task-add with an empty touch", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r","touches":[""],` + brief + `}`},
		{"task-add without a brief", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r"}`},
		{"task-add with an unknown effort", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r","effort":"huge",` + brief + `}`},
		{"task-add with an unknown model", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r","model":"x y",` + brief + `}`},
		{"goal without a request id", VerbGoal, `{"text":"do it"}`},
		{"goal without text", VerbGoal, `{"request_id":"r1","text":"  "}`},
		{"goal over the cap", VerbGoal, `{"request_id":"r1","text":"` + strings.Repeat("g", db.MaxEscalationText+1) + `"}`},
		{"answer without an escalation", VerbAnswer, `{"request_id":"r1","text":"yes"}`},
		{"answer with a negative escalation", VerbAnswer, `{"request_id":"r1","escalation_id":-1,"text":"yes"}`},
		{"answer over the cap", VerbAnswer, `{"request_id":"r1","escalation_id":1,"text":"` + strings.Repeat("a", db.MaxEscalationText+1) + `"}`},
		{"decisions with a negative cursor", VerbDecisions, `{"since":-1}`},
	}
	for _, c := range cases {
		body := c.body
		if c.verb != VerbDecisions {
			body = withParent(body)
		}
		code, r, stderr := serveCall(t, unwired(t), c.verb, body)
		wantRefusal(t, c.label, code, r, stderr, CodeBadRequest)
	}
	// The parent's id is required, and must look like one: the fields above all carried one.
	for _, c := range []struct{ label, verb, body string }{
		{"task-add without a parent id", VerbTaskAdd, `{"request_id":"r1","task_id":"t1","repo":"/r",` + brief + `}`},
		{"goal without a parent id", VerbGoal, `{"request_id":"r1","text":"do it"}`},
		{"answer without a parent id", VerbAnswer, `{"request_id":"r1","escalation_id":1,"text":"yes"}`},
		{"task-add with a short parent id", VerbTaskAdd, withParentID(`{"request_id":"r1","task_id":"t1","repo":"/r",`+brief+`}`, "abc")},
		{"goal with an uppercase parent id", VerbGoal, withParentID(`{"request_id":"r1","text":"do it"}`, strings.ToUpper(testParent))},
		{"answer with a parent id holding a quote", VerbAnswer, withParentID(`{"request_id":"r1","escalation_id":1,"text":"yes"}`, `0123456789abcdef0123456789abcde\"`)},
	} {
		code, r, stderr := serveCall(t, unwired(t), c.verb, c.body)
		wantRefusal(t, c.label, code, r, stderr, CodeBadRequest)
	}
}

// assertWireSafe walks a decoded response and fails on any string, key or value, that holds a
// non-printing rune or is longer than MaxText.
func assertWireSafe(t *testing.T, label string, v any) int {
	t.Helper()
	var n int
	check := func(s string) {
		n++
		if len(s) > MaxText {
			t.Errorf("%s: a %d byte string reached the wire", label, len(s))
		}
		if i := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }); i >= 0 {
			t.Errorf("%s: a non-printing rune reached the wire: %q", label, s)
		}
	}
	switch x := v.(type) {
	case string:
		check(x)
	case []any:
		for _, e := range x {
			n += assertWireSafe(t, label, e)
		}
	case map[string]any:
		for k, e := range x {
			check(k)
			n += assertWireSafe(t, label, e)
		}
	}
	return n
}

// TestSanitizeCapsAndEscapesEveryString proves the last pass holds whatever a handler returns:
// a raw control character is escaped, an oversize string or key is cut, and a string a handler
// already escaped is left exactly as it was.
func TestSanitizeCapsAndEscapesEveryString(t *testing.T) {
	hostile := "a\x1b]0;pwned\x07\nline\u202eevil\u2028" + strings.Repeat("z", 3*MaxText)
	raw, err := encode(Response{Protocol: ProtocolVersion, OK: true, Result: map[string]any{
		"plain":            "fine",
		"escaped":          escape("a\x1b"),
		"hostile":          hostile,
		hostile:            []any{hostile, 7, true, nil},
		"nested":           map[string]any{"deep": []string{hostile}},
		"invalid utf-8":    "a\xffb",
		"exactly the cap":  strings.Repeat("c", MaxText),
		"one over the cap": strings.Repeat("c", MaxText+1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	if n := assertWireSafe(t, "sanitize", tree); n < 10 {
		t.Fatalf("checked only %d strings; the walk is not reaching the result", n)
	}
	res := tree["result"].(map[string]any)
	if res["plain"] != "fine" || res["escaped"] != `a\x1b` || res["exactly the cap"] != strings.Repeat("c", MaxText) {
		t.Errorf("sanitize changed a string that was already safe: %q, %q", res["plain"], res["escaped"])
	}
	if got := res["one over the cap"].(string); len(got) > MaxText || !strings.HasSuffix(got, truncated) {
		t.Errorf("an over-cap string = %d bytes, want cut to %d with the marker", len(got), MaxText)
	}
	if got := res["hostile"].(string); !strings.HasPrefix(got, `a\x1b]0;pwned\x07\nline\u202eevil\u2028`) {
		t.Errorf("hostile string came out as %q, want prefix %q", got[:60], `a\x1b]0;pwned\x07\nline\u202eevil\u2028`)
	}
}

// testParent is the coordinator id of the parent the served stores are provisioned under, and
// otherParent one that did not provision them.
const (
	testParent  = "0123456789abcdef0123456789abcdef"
	otherParent = "fedcba9876543210fedcba9876543210"
)

// withParent puts testParent's id in a request body, as the parent's client does.
func withParent(body string) string { return withParentID(body, testParent) }

func withParentID(body, id string) string {
	return strings.Replace(body, "{", `{"parent_id":"`+id+`",`, 1)
}

// servedStore is a Host over a real store in a temp dir, with the read-only open the CLI uses.
// The store is provisioned as a peer of testParent, named build.
func servedStore(t *testing.T) (Host, string) {
	t.Helper()
	return newServedStore(t, true)
}

// newServedStore is servedStore, with the store left a root (no parent) unless provision is set.
func newServedStore(t *testing.T, provision bool) (Host, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if provision {
		if _, err := s.ProvisionAsPeer(context.Background(), "build", testParent, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return Host{
		ReadStore: func() (*db.Store, error) { return db.OpenReadOnly(path) },
		Store:     func() (*db.Store, error) { return db.Open(path) },
		Version:   "v-test",
		Now:       func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) },
	}, path
}

func reopenStore(t *testing.T, path string) *db.Store {
	t.Helper()
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func resultOf(t *testing.T, r Response, v any) {
	t.Helper()
	if !r.OK || r.Error != nil {
		t.Fatalf("want success, got %+v", r.Error)
	}
	b, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("result does not decode as %T: %v\n%s", v, err, b)
	}
}

// TestServeVersionDecisionsGoalAnswer drives the store-backed verbs end to end against a real
// store: version reads the coordinator row; decisions lists what is open above the cursor and
// every open id; a goal and an answer each append one manager event recorded as the parent's,
// and a repeat of either request id replays without appending.
func TestServeVersionDecisionsGoalAnswer(t *testing.T) {
	ctx := context.Background()
	h, path := servedStore(t)

	code, r, _ := serveCall(t, h, VerbVersion, "")
	var v VersionResult
	resultOf(t, r, &v)
	if code != 0 || v.Protocol != ProtocolVersion || v.Version != "v-test" ||
		v.Coordinator != (CoordinatorInfo{Name: "build", Role: db.CoordinatorPeer, ParentID: testParent}) {
		t.Errorf("version = %+v", v)
	}

	s := reopenStore(t, path)
	proj, err := s.UpsertProject(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "t1", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	first, err := s.OpenEscalation(ctx, "t1", db.EscalationQuestion, "which one?\x1b[2J")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.OpenEscalation(ctx, "t1", db.EscalationQuestion, "and then?")
	if err != nil {
		t.Fatal(err)
	}

	_, r, _ = serveCall(t, h, VerbDecisions, `{"since":`+itoa(first.ID)+`}`)
	var d DecisionsResult
	resultOf(t, r, &d)
	if d.Open != 2 || d.HighestOpenID != second.ID || !reflect.DeepEqual(d.OpenIDs, []int64{first.ID, second.ID}) ||
		len(d.Escalations) != 1 || d.Escalations[0].ID != second.ID {
		t.Errorf("decisions since %d = %+v", first.ID, d)
	}
	_, r, _ = serveCall(t, h, VerbDecisions, ``)
	resultOf(t, r, &d)
	if len(d.Escalations) != 2 || d.Escalations[0].Body != `which one?\x1b[2J` {
		t.Errorf("decisions since 0 = %+v", d)
	}

	managerEvents := func() []db.Event {
		all, err := s.EventsSince(ctx, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []db.Event
		for _, e := range all {
			if e.EntityType == db.EntityTypeManager {
				out = append(out, e)
			}
		}
		return out
	}

	_, r, _ = serveCall(t, h, VerbGoal, withParent(`{"request_id":"goal-1","text":"split the importer"}`))
	var g GoalResult
	resultOf(t, r, &g)
	_, r, _ = serveCall(t, h, VerbGoal, withParent(`{"request_id":"goal-1","text":"something else"}`))
	var g2 GoalResult
	resultOf(t, r, &g2)
	if g.Replayed || !g2.Replayed || g2.EventID != g.EventID {
		t.Errorf("goal then repeat = %+v, %+v", g, g2)
	}

	_, r, _ = serveCall(t, h, VerbAnswer, withParent(`{"request_id":"ans-1","escalation_id":`+itoa(first.ID)+`,"text":"the first"}`))
	var a AnswerResult
	resultOf(t, r, &a)
	if a.EscalationID != first.ID || a.Status != db.EscalationAnswered || a.AnsweredBy != db.ActorParent || a.TaskID != "t1" || a.Replayed {
		t.Errorf("answer = %+v", a)
	}
	_, r, _ = serveCall(t, h, VerbAnswer, withParent(`{"request_id":"ans-1","escalation_id":`+itoa(first.ID)+`,"text":"changed my mind"}`))
	var a2 AnswerResult
	resultOf(t, r, &a2)
	if !a2.Replayed || a2.EventID != a.EventID {
		t.Errorf("repeated answer = %+v", a2)
	}

	evs := managerEvents()
	if len(evs) != 2 || evs[0].Type != db.EventGoal || evs[1].Type != db.EventEscalationAnswered {
		t.Fatalf("manager events = %+v, want one goal and one answer", evs)
	}
	for _, e := range evs {
		if e.Actor != db.ActorParent || !e.Actionable {
			t.Errorf("event %d actor %q actionable %v, want the parent's and actionable", e.ID, e.Actor, e.Actionable)
		}
	}

	code, r, stderr := serveCall(t, h, VerbAnswer, withParent(`{"request_id":"ans-2","escalation_id":`+itoa(first.ID)+`,"text":"again"}`))
	wantRefusal(t, "answer to an answered escalation", code, r, stderr, CodeConflict)
	code, r, stderr = serveCall(t, h, VerbAnswer, withParent(`{"request_id":"ans-3","escalation_id":999,"text":"x"}`))
	wantRefusal(t, "answer to no escalation", code, r, stderr, CodeNotFound)
	code, r, stderr = serveCall(t, h, VerbGoal, withParent(`{"request_id":"ans-1","text":"x"}`))
	wantRefusal(t, "goal under an answer's request id", code, r, stderr, CodeConflict)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestServeRefusesAnotherParent: task-add, goal and answer name the parent they come from, and a
// peer refuses one from any coordinator but the parent that provisioned it, before the request
// ledger is read, so not even a replay of a stored result reaches another parent. A coordinator no
// parent provisioned refuses all three. Nothing is written either way. The read verbs do not check.
func TestServeRefusesAnotherParent(t *testing.T) {
	ctx := context.Background()
	h, path := servedStore(t)
	s := reopenStore(t, path)
	if _, err := s.UpsertProject(ctx, "/repo", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "t1", ProjectID: 1}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	esc, err := s.OpenEscalation(ctx, "t1", db.EscalationQuestion, "which one?")
	if err != nil {
		t.Fatal(err)
	}
	h.AddTask = func(ctx context.Context, store *db.Store, req TaskAdd) (db.TaskAddResult, string, error) {
		res, err := store.AddTask(ctx, db.TaskAdd{
			Task: db.Task{ID: req.TaskID, ProjectID: req.ProjectID}, Actor: db.ActorParent,
			RequestID: req.RequestID, Brief: req.Brief, WriteBrief: func(string) error { return nil },
		})
		return res, "", err
	}
	// One goal from the parent, so there is a stored result another parent could try to replay.
	_, r, _ := serveCall(t, h, VerbGoal, withParent(`{"request_id":"goal-1","text":"tidy"}`))
	var g GoalResult
	resultOf(t, r, &g)

	events := func() int {
		evs, err := s.EventsSince(ctx, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	before := events()
	mutating := []struct{ label, verb, body string }{
		{"task-add", VerbTaskAdd, `{"request_id":"add-1","task_id":"p-1","repo":"/repo","brief":"# b"}`},
		{"goal", VerbGoal, `{"request_id":"goal-2","text":"something else"}`},
		{"a replayed goal", VerbGoal, `{"request_id":"goal-1","text":"tidy"}`},
		{"answer", VerbAnswer, `{"request_id":"ans-1","escalation_id":` + itoa(esc.ID) + `,"text":"the first"}`},
	}
	for _, c := range mutating {
		code, r, stderr := serveCall(t, h, c.verb, withParentID(c.body, otherParent))
		wantRefusal(t, "another parent's "+c.label, code, r, stderr, CodeWrongParent)
		if r.Error != nil && (!strings.Contains(r.Error.Message, otherParent) || !strings.Contains(r.Error.Message, testParent)) {
			t.Errorf("another parent's %s: the refusal %q does not name both coordinators", c.label, r.Error.Message)
		}
	}
	if _, ok, _ := s.GetTask(ctx, "p-1"); ok {
		t.Error("another parent's task-add created a task")
	}
	if n := events(); n != before {
		t.Errorf("another parent's requests appended %d events", n-before)
	}
	if e, _, _ := s.GetEscalation(ctx, esc.ID); e.Status != db.EscalationOpen {
		t.Errorf("another parent's answer changed the escalation: %+v", e)
	}
	for _, verb := range []string{VerbVersion, VerbDecisions} {
		if _, r, _ := serveCall(t, h, verb, ``); !r.OK {
			t.Errorf("%s is a read and does not check the parent; got %+v", verb, r.Error)
		}
	}

	// A root coordinator, which no parent provisioned, has no parent to match.
	root, rootPath := newServedStore(t, false)
	rs := reopenStore(t, rootPath)
	if _, err := rs.UpsertProject(ctx, "/repo", "repo"); err != nil {
		t.Fatal(err)
	}
	root.AddTask = h.AddTask
	for _, c := range mutating {
		code, r, stderr := serveCall(t, root, c.verb, withParent(c.body))
		wantRefusal(t, "a root's "+c.label, code, r, stderr, CodeWrongParent)
	}
}

// TestServeTaskAddReplaysBeforeTheLint proves a repeated task-add is answered from the ledger
// without calling the add (so without linting again), that a request id reused for another task
// is a conflict, and that an add for an unregistered repo stops before the add.
func TestServeTaskAddReplaysBeforeTheLint(t *testing.T) {
	ctx := context.Background()
	h, path := servedStore(t)
	s := reopenStore(t, path)
	if _, err := s.UpsertProject(ctx, "/repo", "repo"); err != nil {
		t.Fatal(err)
	}
	adds := 0
	h.AddTask = func(ctx context.Context, store *db.Store, req TaskAdd) (db.TaskAddResult, string, error) {
		adds++
		res, err := store.AddTask(ctx, db.TaskAdd{
			Task:       db.Task{ID: req.TaskID, ProjectID: req.ProjectID, Footprint: req.Touches},
			Actor:      db.ActorParent,
			RequestID:  req.RequestID,
			Brief:      req.Brief,
			WriteBrief: func(string) error { return nil },
		})
		return res, "5 of 5 rules ran: passed\x1b[0m", err
	}
	body := `{"request_id":"add-1","task_id":"p-1","repo":"/repo","touches":["a.go"],"brief":"# b"}`
	_, r, _ := serveCall(t, h, VerbTaskAdd, withParent(body))
	var first TaskAddResult
	resultOf(t, r, &first)
	if first.TaskID != "p-1" || !first.HasBrief || first.Replayed || first.Lint != `5 of 5 rules ran: passed\x1b[0m` {
		t.Errorf("task-add = %+v", first)
	}
	_, r, _ = serveCall(t, h, VerbTaskAdd, withParent(body))
	var again TaskAddResult
	resultOf(t, r, &again)
	if !again.Replayed || again.EventID != first.EventID || again.Lint != "" || adds != 1 {
		t.Errorf("repeat = %+v after %d adds, want a replay with no second add", again, adds)
	}
	code, r, stderr := serveCall(t, h, VerbTaskAdd, withParent(`{"request_id":"add-1","task_id":"p-2","repo":"/repo","brief":"# b"}`))
	wantRefusal(t, "request id reused for another task", code, r, stderr, CodeConflict)
	code, r, stderr = serveCall(t, h, VerbTaskAdd, withParent(`{"request_id":"add-2","task_id":"p-1","repo":"/repo","brief":"# b"}`))
	wantRefusal(t, "task id taken", code, r, stderr, CodeConflict)
	code, r, stderr = serveCall(t, h, VerbTaskAdd, withParent(`{"request_id":"add-3","task_id":"p-3","repo":"/elsewhere","brief":"# b"}`))
	wantRefusal(t, "unregistered repo", code, r, stderr, CodeNotFound)
	if adds != 2 {
		t.Errorf("adds = %d, want 2 (the first, and the taken id the store refused)", adds)
	}
}

// TestServeEnsureUpEscapesWhatItReports: the restore notes carry error text from tmux and the
// filesystem, so they are escaped like everything else.
func TestServeEnsureUpEscapesWhatItReports(t *testing.T) {
	h := Host{EnsureUp: func(context.Context) (EnsureUpResult, error) {
		return EnsureUpResult{Restored: []string{"restored manager", "skipped t\x1b[1m (gone)"}, Scheduler: "started"}, nil
	}}
	_, r, _ := serveCall(t, h, VerbEnsureUp, `{}`)
	var e EnsureUpResult
	resultOf(t, r, &e)
	if e.Scheduler != "started" || len(e.Restored) != 2 || e.Restored[1] != `skipped t\x1b[1m (gone)` {
		t.Errorf("ensure-up = %+v", e)
	}
	h.EnsureUp = func(context.Context) (EnsureUpResult, error) {
		return EnsureUpResult{}, Refuse(CodeNoManager, "no manager session is recorded")
	}
	code, r, stderr := serveCall(t, h, VerbEnsureUp, `{}`)
	wantRefusal(t, "no manager", code, r, stderr, CodeNoManager)
}

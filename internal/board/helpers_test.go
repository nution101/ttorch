package board

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// fakeFleet records every call the board makes through the Fleet interface, so a test can
// count sends, spawns and preps without tmux or a worktree pool.
type fakeFleet struct {
	mu       sync.Mutex
	sends    []sentMsg
	sendErr  error
	spawns   []string
	spawnErr error
	preps    []string
	prepErr  error
	state    string
}

type sentMsg struct{ task, text string }

func (f *fakeFleet) TaskState(db.Task) string {
	if f.state == "" {
		return "working"
	}
	return f.state
}

func (f *fakeFleet) Send(taskID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sends = append(f.sends, sentMsg{taskID, text})
	return nil
}

func (f *fakeFleet) TrustPrep(taskID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepErr != nil {
		return "", f.prepErr
	}
	f.preps = append(f.preps, taskID)
	return "/tmp/review/" + taskID, nil
}

func (f *fakeFleet) ReviewersFor(string) []string { return []string{"correctness", "scope"} }

func (f *fakeFleet) SpawnAutonomous(taskID, _ string, _ bool, _ string, _ []string, _ bool, _, _ string) (db.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return db.Task{}, f.spawnErr
	}
	f.spawns = append(f.spawns, taskID)
	return db.Task{ID: taskID, Window: "wk-" + taskID}, nil
}

func (f *fakeFleet) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// harness is one running board over a fresh store.
type harness struct {
	t     *testing.T
	store *db.Store
	fleet *fakeFleet
	srv   *Server
	log   *syncBuffer
	base  string // "http://127.0.0.1:<port>"
	proj  db.Project
}

// syncBuffer is a goroutine-safe log sink: the HTTP server writes to it from its own
// goroutines while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newStore(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newHarness starts a board on a real loopback listener, so every test request crosses the
// same network path and handler chain a browser's would.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOn(t, newStore(t), &fakeFleet{})
}

func newHarnessOn(t *testing.T, store *db.Store, fleet *fakeFleet) *harness {
	t.Helper()
	log := &syncBuffer{}
	srv, err := New(Config{
		Store: store, Fleet: fleet, Log: log,
		Mode: func(string) string { return "trusted" },
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	proj, err := store.UpsertProject(context.Background(), "/repos/app", "")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: store, fleet: fleet, srv: srv, log: log, base: "http://" + srv.host, proj: proj}
}

// addTask inserts a task row in the harness project.
func (h *harness) addTask(task db.Task) db.Task {
	h.t.Helper()
	task.ProjectID = h.proj.ID
	out, err := h.store.CreateTask(context.Background(), task, db.ActorManager)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// ask moves a task into needs_input as its worker would, with msg as the question, and
// returns the status_changed event id that carries it.
func (h *harness) ask(id, msg string) int64 {
	h.t.Helper()
	ev, err := h.store.ReportStatus(context.Background(), id, db.StatusNeedsInput, "worker:"+id, msg)
	if err != nil {
		h.t.Fatal(err)
	}
	return ev.ID
}

// escalate appends a gate_blocked event on a task, as the daemon gate does.
func (h *harness) escalate(id, payload string) int64 {
	h.t.Helper()
	eid, err := h.store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: id, Type: db.EventGateBlocked,
		Actor: db.ActorSystem, Actionable: true, Payload: payload,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return eid
}

// request is one HTTP request with full control over the headers that the security checks
// read. An empty host means the board's own; token "" means none is sent.
type request struct {
	method, path string
	host         string
	origin       string
	headerToken  string
	form         url.Values
	contentType  string
	secFetchSite string
}

type response struct {
	code   int
	body   string
	header http.Header
}

func (h *harness) do(r request) response {
	h.t.Helper()
	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	req, err := http.NewRequest(r.method, h.base+r.path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if r.host != "" {
		req.Host = r.host
	}
	if r.form != nil {
		ct := r.contentType
		if ct == "" {
			ct = "application/x-www-form-urlencoded"
		}
		req.Header.Set("Content-Type", ct)
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.headerToken != "" {
		req.Header.Set(TokenHeader, r.headerToken)
	}
	if r.secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", r.secFetchSite)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, body: string(b), header: resp.Header}
}

// action posts a form to an action route the way the page does: same origin, token header.
func (h *harness) action(path string, form url.Values) response {
	h.t.Helper()
	return h.do(request{method: http.MethodPost, path: path, origin: h.base, headerToken: h.srv.token, form: form})
}

func (h *harness) page() response {
	h.t.Helper()
	return h.do(request{method: http.MethodGet, path: "/?token=" + h.srv.token})
}

func (h *harness) sections() response {
	h.t.Helper()
	return h.do(request{method: http.MethodGet, path: "/api/board", headerToken: h.srv.token})
}

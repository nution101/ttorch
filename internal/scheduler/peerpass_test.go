package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/nution101/ttorch/internal/db"
)

// fakePeer is one peer's control channel. Every call is counted; a blocked peer's Summary waits
// for its context, the way a hung ssh waits for the client's deadline.
type fakePeer struct {
	mu           sync.Mutex
	summary      PeerSummary
	summaryErr   error
	decisions    PeerDecisions
	decisionsErr error
	ensureUpErr  error
	block        bool
	calls        map[string]int
	sinces       []int64
	started      chan struct{} // closed when a blocked Summary starts waiting
}

func newFakePeer() *fakePeer {
	return &fakePeer{summary: PeerSummary{ManagerWindow: true, SchedulerRunning: true, JSON: `{"schema_version":2}`}, calls: map[string]int{}}
}

func (f *fakePeer) count(verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[verb]
}

func (f *fakePeer) set(fn func(f *fakePeer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakePeer) Summary(ctx context.Context) (PeerSummary, error) {
	f.mu.Lock()
	f.calls["summary"]++
	block, started := f.block, f.started
	f.started = nil // closed once, however many polls reach it
	sum, err := f.summary, f.summaryErr
	f.mu.Unlock()
	if block {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return PeerSummary{}, ctx.Err()
	}
	return sum, err
}

func (f *fakePeer) Decisions(ctx context.Context, since int64) (PeerDecisions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["decisions"]++
	f.sinces = append(f.sinces, since)
	var out PeerDecisions
	out.Open = f.decisions.Open
	for _, e := range f.decisions.Escalations {
		if e.ID > since {
			out.Escalations = append(out.Escalations, e)
		}
	}
	// A peer lists the open escalations above since; the pass asks for all of them.
	return out, f.decisionsErr
}

func (f *fakePeer) EnsureUp(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["ensure-up"]++
	return "restored manager; scheduler started", f.ensureUpErr
}

// fakePeers dials the fakes by peer name.
type fakePeers map[string]*fakePeer

func (fp fakePeers) dial(p db.Peer) PeerChannel { return fp[p.Name] }

// syncWriter is a log the peer pass's goroutines and the tick can write at once.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// registerLivePeer registers name as a peer whose control key has answered.
func registerLivePeer(t *testing.T, s *db.Store, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.RegisterPeer(ctx, db.Peer{Name: name, ControlDest: "ttorch@" + name, ApproveDest: "lead@" + name, ControlKey: "/keys/" + name}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPeerLive(ctx, name, 1, "v1"); err != nil {
		t.Fatal(err)
	}
}

func peerEventsOf(t *testing.T, s *db.Store, name string) []db.Event {
	t.Helper()
	all, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Event
	for _, e := range all {
		if e.EntityID == db.PeerEntityID(name) {
			out = append(out, e)
		}
	}
	return out
}

func eventsOfType(evs []db.Event, typ string) []db.Event {
	var out []db.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func getPeer(t *testing.T, s *db.Store, name string) db.Peer {
	t.Helper()
	p, ok, err := s.GetPeer(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPeer %s = %v, %v", name, ok, err)
	}
	return p
}

func peerScheduler(s *db.Store, peers fakePeers) *Scheduler {
	return &Scheduler{Store: s, Peers: peers.dial, PeerPoll: time.Minute, Log: &syncWriter{}}
}

// TestPeerPassRaisesEachEscalationOnce: an escalation open on a peer becomes exactly one
// actionable parent event, under entity system and the peer's id and actor, and the summary is
// cached on the peer's row. The next pass asks only for escalations above the cursor, and a new
// Scheduler on the same store (a restart) raises no duplicate. A later escalation raises one more.
func TestPeerPassRaisesEachEscalationOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	fp.decisions = PeerDecisions{Open: 1, Escalations: []PeerEscalation{{ID: 4, Kind: "question", TaskID: "t-q", Body: "which one?"}}}
	peers := fakePeers{"build": fp}

	sc := peerScheduler(s, peers)
	if n, err := sc.RunPeerPassOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunPeerPassOnce = %d, %v", n, err)
	}
	evs := peerEventsOf(t, s, "build")
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	e := evs[0]
	if e.Type != db.EventPeerEscalation || e.EntityType != db.EntityTypeSystem || e.EntityID != "peer:build" || e.Actor != "peer:build" || !e.Actionable {
		t.Errorf("event = %+v", e)
	}
	var payload struct {
		Peer         string `json:"peer"`
		EscalationID int64  `json:"escalation_id"`
		Kind         string `json:"kind"`
		TaskID       string `json:"task_id"`
		Open         int    `json:"open"`
		Body         string `json:"body"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		t.Fatalf("payload %q: %v", e.Payload, err)
	}
	if payload.Peer != "build" || payload.EscalationID != 4 || payload.Kind != "question" || payload.TaskID != "t-q" || payload.Open != 1 || payload.Body != "which one?" {
		t.Errorf("payload = %+v", payload)
	}
	if p := getPeer(t, s, "build"); p.EscalationCursor != 4 || p.Summary != `{"schema_version":2}` || p.LastOKAt.IsZero() {
		t.Errorf("peer after the pass = %+v", p)
	}

	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := peerScheduler(s, peers)
	if _, err := restarted.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := peerEventsOf(t, s, "build"); len(evs) != 1 {
		t.Errorf("after a second pass and a restart: events = %+v, want still one", evs)
	}
	if got := fp.sinces; len(got) != 3 || got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Errorf("decisions asked since %v, want [0 0 0]: the whole open list each poll", got)
	}

	fp.set(func(f *fakePeer) {
		f.decisions.Open = 2
		f.decisions.Escalations = append(f.decisions.Escalations, PeerEscalation{ID: 9, Kind: "approval", TaskID: "t-a", Body: "approve t-a"})
	})
	if _, err := restarted.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := peerEventsOf(t, s, "build"); len(evs) != 2 || !strings.Contains(evs[1].Payload, `"escalation_id":9`) {
		t.Errorf("after a second escalation: events = %+v", evs)
	}
	if fp.count("ensure-up") != 0 {
		t.Error("a healthy peer was sent ensure-up")
	}
}

// TestPeerPassEscapesAndCapsPeerText: escalation text is untrusted. Every control, format and
// bidi rune in it is escaped, the whole payload is at most 2 KiB, and it is still one JSON object.
func TestPeerPassEscapesAndCapsPeerText(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	hostile := "line one\x1b]0;pwned\x07\nline two \u202egnp.exe\u0085 " + strings.Repeat(`"<&\`, 3000)
	fp.decisions = PeerDecisions{Open: 1, Escalations: []PeerEscalation{
		{ID: 1, Kind: "question\x1b[2J", TaskID: "t\r" + strings.Repeat("x", 500), Body: hostile},
	}}
	sc := peerScheduler(s, fakePeers{"build": fp})
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	evs := peerEventsOf(t, s, "build")
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	payload := evs[0].Payload
	if len(payload) > db.MaxEscalationText {
		t.Errorf("payload is %d bytes, over %d", len(payload), db.MaxEscalationText)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, payload)
	}
	for k, v := range decoded {
		str, ok := v.(string)
		if !ok {
			continue
		}
		if i := strings.IndexFunc(str, func(r rune) bool { return !unicode.IsGraphic(r) }); i >= 0 {
			t.Errorf("payload field %s holds a raw non-printing rune at %d: %q", k, i, str)
		}
	}
	body, _ := decoded["body"].(string)
	if !strings.HasPrefix(body, `line one\x1b]0;pwned\a\nline two \u202egnp.exe\u0085 `) || !strings.HasSuffix(body, "…") {
		t.Errorf("body = %.120q…, want the escaped text, cut with a marker", body)
	}
	if task, _ := decoded["task_id"].(string); !strings.HasPrefix(task, `t\r`) || len(task) > 128 {
		t.Errorf("task_id = %q, want it escaped and at most 128 bytes", task)
	}
	if kind, _ := decoded["kind"].(string); kind != `question\x1b[2J` {
		t.Errorf("kind = %q", kind)
	}
}

// TestPeerPassUnreachableThenRecovered: three failed polls in a row mark the peer unreachable
// with one actionable event, and the polls after that raise nothing more. The error is escaped.
// The next poll that succeeds sets the peer live and raises one peer_recovered, not actionable.
func TestPeerPassUnreachableThenRecovered(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	fp.summaryErr = errors.New("ssh: connect to host build port 22: refused\x1b[31m")
	sc := peerScheduler(s, fakePeers{"build": fp})
	for i := 0; i < db.PeerUnreachableAfter+2; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
		want := db.PeerLive
		if i+1 >= db.PeerUnreachableAfter {
			want = db.PeerUnreachable
		}
		if p := getPeer(t, s, "build"); p.Status != want || p.ConsecutiveFailures != i+1 {
			t.Errorf("after failed poll %d: status %s, failures %d", i+1, p.Status, p.ConsecutiveFailures)
		}
	}
	p := getPeer(t, s, "build")
	if strings.ContainsRune(p.LastError, 0x1b) || !strings.Contains(p.LastError, `refused\x1b[31m`) {
		t.Errorf("last error = %q, want it escaped", p.LastError)
	}
	evs := peerEventsOf(t, s, "build")
	if len(evs) != 1 || evs[0].Type != db.EventPeerUnreachable || !evs[0].Actionable || evs[0].Actor != "peer:build" {
		t.Fatalf("events = %+v, want one actionable peer_unreachable", evs)
	}
	if !strings.Contains(evs[0].Payload, `refused\\x1b[31m`) || strings.ContainsRune(evs[0].Payload, 0x1b) {
		t.Errorf("peer_unreachable payload = %q, want the escaped error", evs[0].Payload)
	}
	if fp.count("decisions") != 0 || fp.count("ensure-up") != 0 {
		t.Error("a poll whose summary failed went on to call the peer again")
	}

	fp.set(func(f *fakePeer) { f.summaryErr = nil })
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p := getPeer(t, s, "build"); p.Status != db.PeerLive || p.ConsecutiveFailures != 0 || p.LastError != "" {
		t.Errorf("after recovery: %+v", p)
	}
	evs = peerEventsOf(t, s, "build")
	rec := eventsOfType(evs, db.EventPeerRecovered)
	if len(evs) != 2 || len(rec) != 1 || rec[0].Actionable {
		t.Errorf("events = %+v, want one non-actionable peer_recovered after the outage", evs)
	}

	// A failed decisions call fails the poll too.
	fp.set(func(f *fakePeer) { f.decisionsErr = errors.New("unavailable") })
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p := getPeer(t, s, "build"); p.ConsecutiveFailures != 1 || p.LastError != "unavailable" {
		t.Errorf("after a failed decisions call: %+v", p)
	}
}

// TestPeerPassBoundsEnsureUp: a peer whose summary shows no manager window gets ensure-up on
// each poll, at most three times, then one actionable peer_down event, and no more calls until a
// poll finds it healthy. A scheduler that is not running, or stalled, counts as down too.
func TestPeerPassBoundsEnsureUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	fp.summary.ManagerWindow = false
	fp.ensureUpErr = errors.New("no_manager: no manager session is recorded here")
	sc := peerScheduler(s, fakePeers{"build": fp})
	for i := 0; i < 6; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := fp.count("ensure-up"); n != db.PeerEnsureUpAttempts {
		t.Errorf("ensure-up called %d times, want %d", n, db.PeerEnsureUpAttempts)
	}
	downs := eventsOfType(peerEventsOf(t, s, "build"), db.EventPeerDown)
	if len(downs) != 1 || !downs[0].Actionable || downs[0].Actor != "peer:build" {
		t.Fatalf("peer_down events = %+v, want one actionable", downs)
	}
	if !strings.Contains(downs[0].Payload, `"manager_window":false`) || !strings.Contains(downs[0].Payload, `"ensure_up_calls":3`) {
		t.Errorf("peer_down payload = %q", downs[0].Payload)
	}
	if p := getPeer(t, s, "build"); p.Status != db.PeerLive {
		t.Errorf("a down peer that answers is still live, got %s", p.Status)
	}

	// Healthy again ends the episode; a stalled scheduler starts a new one.
	fp.set(func(f *fakePeer) { f.summary.ManagerWindow = true })
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fp.count("ensure-up"); n != db.PeerEnsureUpAttempts {
		t.Errorf("a healthy peer was sent ensure-up (%d calls)", n)
	}
	for _, mark := range []func(f *fakePeer){
		func(f *fakePeer) { f.summary.SchedulerStalled = true },
		func(f *fakePeer) { f.summary.SchedulerStalled, f.summary.SchedulerRunning = false, false },
	} {
		fp.set(mark)
		before := fp.count("ensure-up")
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if fp.count("ensure-up") != before+1 {
			t.Errorf("a peer with summary %+v was not sent ensure-up", fp.summary)
		}
	}
}

// TestPeerPassSlowPeerDoesNotDelayTheTick: the tick starts the peer pass and returns without
// waiting on it. A peer that never answers holds up neither the tick nor the other peers, is
// polled once however many ticks pass meanwhile, and is cut off at its deadline and recorded as
// a failed poll. Run does not return until the pass has stopped.
func TestPeerPassSlowPeerDoesNotDelayTheTick(t *testing.T) {
	s := newStore(t)
	registerLivePeer(t, s, "fast")
	registerLivePeer(t, s, "slow")
	fast, slow := newFakePeer(), newFakePeer()
	fast.decisions = PeerDecisions{Open: 1, Escalations: []PeerEscalation{{ID: 1, Kind: "question", Body: "q"}}}
	started := make(chan struct{})
	slow.block, slow.started = true, started
	sc := peerScheduler(s, fakePeers{"fast": fast, "slow": slow})
	sc.PeerPoll = time.Millisecond
	sc.PeerDeadline = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	sc.runTick(ctx)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("runTick took %s with a peer that never answers", took)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow peer was never polled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(peerEventsOf(t, s, "fast")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if evs := peerEventsOf(t, s, "fast"); len(evs) != 1 {
		t.Errorf("the fast peer's escalation, with the slow one still in flight: %+v", evs)
	}
	// More ticks while the slow peer is in flight poll the fast one again, never the slow one.
	for i := 0; i < 3; i++ {
		time.Sleep(5 * time.Millisecond)
		start := time.Now()
		sc.runTick(ctx)
		if took := time.Since(start); took > time.Second {
			t.Errorf("tick %d took %s", i, took)
		}
	}
	sc.waitPeerPass()
	if n := slow.count("summary"); n != 1 {
		t.Errorf("the slow peer was polled %d times while one poll was in flight", n)
	}
	if fast.count("summary") < 2 {
		t.Errorf("the fast peer was polled %d times; it should not wait for the slow one", fast.count("summary"))
	}
	if p := getPeer(t, s, "slow"); p.ConsecutiveFailures != 1 || !strings.Contains(p.LastError, "deadline") {
		t.Errorf("the slow peer after its deadline: failures %d, last error %q", p.ConsecutiveFailures, p.LastError)
	}

	// Run waits for an in-flight poll, and a poll cut off by shutdown is not the peer's failure:
	// nothing is recorded, and no attempt to record it is logged.
	restarted := make(chan struct{})
	slow.set(func(f *fakePeer) { f.started = restarted })
	sc.PeerDeadline = time.Hour
	log := &syncWriter{}
	sc.Log = log
	runCtx, stop := context.WithCancel(context.Background())
	sc.Interval = time.Hour
	done := make(chan error, 1)
	go func() { done <- sc.Run(runCtx) }()
	<-restarted
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if n := slow.count("summary"); n != 2 {
		t.Errorf("slow peer polled %d times, want 2", n)
	}
	if p := getPeer(t, s, "slow"); p.ConsecutiveFailures != 1 {
		t.Errorf("shutdown during a poll counted as a failure: %d", p.ConsecutiveFailures)
	}
	if strings.Contains(log.String(), "peer slow") {
		t.Errorf("shutdown during a poll was logged as the peer's: %q", log.String())
	}
}

// TestPeerPassCadence: ticks start a pass at most once per PeerPoll, a zero PeerPoll or no
// dialer leaves the pass off, and peers that are provisioning or retired are never dialed.
func TestPeerPassCadence(t *testing.T) {
	s := newStore(t)
	registerLivePeer(t, s, "build")
	registerLivePeer(t, s, "old")
	if _, err := s.RetirePeer(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterPeer(context.Background(), db.Peer{Name: "new", ControlDest: "d", ApproveDest: "d", ControlKey: "/k"}, false); err != nil {
		t.Fatal(err)
	}
	fp := newFakePeer()
	dialed := map[string]int{}
	var mu sync.Mutex
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sc := &Scheduler{Store: s, PeerPoll: 30 * time.Second, Log: &syncWriter{}, now: func() time.Time { return clock }}
	sc.Peers = func(p db.Peer) PeerChannel {
		mu.Lock()
		dialed[p.Name]++
		mu.Unlock()
		return fp
	}
	ctx := context.Background()
	tick := func() {
		sc.runTick(ctx)
		sc.waitPeerPass()
	}
	tick()
	clock = clock.Add(10 * time.Second)
	tick()
	if n := fp.count("summary"); n != 1 {
		t.Errorf("two ticks 10s apart polled %d times, want 1", n)
	}
	clock = clock.Add(25 * time.Second)
	tick()
	if n := fp.count("summary"); n != 2 {
		t.Errorf("a tick past the cadence polled %d times in all, want 2", n)
	}
	if dialed["old"] != 0 || dialed["new"] != 0 || dialed["build"] != 2 {
		t.Errorf("dialed = %v, want only the live peer", dialed)
	}

	sc.PeerPoll = 0
	clock = clock.Add(time.Hour)
	tick()
	sc.PeerPoll, sc.Peers = 30*time.Second, nil
	tick()
	if n := fp.count("summary"); n != 2 {
		t.Errorf("a pass with no cadence or no dialer polled (%d in all)", n)
	}
}

// TestPeerPollFromEnv: TTORCH_PEER_POLL is a Go duration; unset or unparseable means the
// default, and zero or less turns the pass off.
func TestPeerPollFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", DefaultPeerPoll}, {"5s", 5 * time.Second}, {"2m", 2 * time.Minute}, {"0", 0}, {"-1s", 0}, {"soon", DefaultPeerPoll},
	} {
		t.Setenv(envPeerPoll, c.env)
		if got := peerPollFromEnv(); got != c.want {
			t.Errorf("TTORCH_PEER_POLL=%q: %s, want %s", c.env, got, c.want)
		}
	}
	if DefaultPeerPoll != 30*time.Second {
		t.Errorf("DefaultPeerPoll = %s, want 30s", DefaultPeerPoll)
	}
}

// stamped is n escalations from id from, each created at its own time in a store identified by
// epoch, as the peer reports them.
func stamped(from int64, n int, epoch time.Time) []PeerEscalation {
	var out []PeerEscalation
	for id := from; id < from+int64(n); id++ {
		out = append(out, PeerEscalation{ID: id, Kind: "question", TaskID: "t", Body: fmt.Sprintf("question %d", id),
			CreatedAt: epoch.Add(time.Duration(id) * time.Minute)})
	}
	return out
}

// TestPeerPassDetectsAStoreThatWentBack: a peer whose store was recreated lists escalations
// numbered from 1 again. The pass raises one peer_cursor_reset, raises the new escalations, which
// the old cursor would have hidden, and later polls raise nothing twice. Each escalation's payload
// carries the peer's creation stamp.
func TestPeerPassDetectsAStoreThatWentBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fp.decisions = PeerDecisions{Open: 5, Escalations: stamped(1, 5, old)}
	sc := peerScheduler(s, fakePeers{"build": fp})
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p := getPeer(t, s, "build"); p.EscalationCursor != 5 {
		t.Fatalf("cursor = %d, want 5", p.EscalationCursor)
	}
	fresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fp.set(func(f *fakePeer) { f.decisions = PeerDecisions{Open: 2, Escalations: stamped(1, 2, fresh)} })
	for i := 0; i < 3; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	evs := peerEventsOf(t, s, "build")
	if n := len(eventsOfType(evs, db.EventPeerCursorReset)); n != 1 {
		t.Errorf("%d peer_cursor_reset events, want 1", n)
	}
	escs := eventsOfType(evs, db.EventPeerEscalation)
	if len(escs) != 7 {
		t.Fatalf("%d peer_escalation events, want 7: five from the old store, two from the new", len(escs))
	}
	var last db.PeerEscalationPayload
	if err := json.Unmarshal([]byte(escs[6].Payload), &last); err != nil || last.EscalationID != 2 || last.CreatedAt != fresh.Add(2*time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("last payload %q = %+v, %v", escs[6].Payload, last, err)
	}
	if p := getPeer(t, s, "build"); p.EscalationCursor != 2 {
		t.Errorf("cursor after the re-sync = %d, want 2", p.EscalationCursor)
	}
}

// TestPeerPassRefusesEscalationIDsOutOfRange: an escalation id of math.MaxInt64, or one far
// above the cursor, is not raised and does not move the cursor; it raises one peer_protocol_error,
// and an escalation in range after it is still raised.
func TestPeerPassRefusesEscalationIDsOutOfRange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	registerLivePeer(t, s, "build")
	fp := newFakePeer()
	epoch := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fp.decisions = PeerDecisions{Open: 2, Escalations: append(stamped(1, 1, epoch), PeerEscalation{ID: math.MaxInt64, Kind: "question", Body: "hide the rest", CreatedAt: epoch})}
	sc := peerScheduler(s, fakePeers{"build": fp})
	for i := 0; i < 2; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if p := getPeer(t, s, "build"); p.EscalationCursor != 1 {
		t.Errorf("cursor = %d, want 1: the MaxInt64 id must not move it", p.EscalationCursor)
	}
	fp.set(func(f *fakePeer) {
		f.decisions.Escalations = append(f.decisions.Escalations, stamped(2, 1, epoch)[0],
			PeerEscalation{ID: 1 + maxPeerEscalationJump + 1, Kind: "question", Body: "far", CreatedAt: epoch})
	})
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	evs := peerEventsOf(t, s, "build")
	if n := len(eventsOfType(evs, db.EventPeerProtocolError)); n != 1 {
		t.Errorf("%d peer_protocol_error events, want 1 for the episode", n)
	}
	escs := eventsOfType(evs, db.EventPeerEscalation)
	if len(escs) != 2 || !strings.Contains(escs[1].Payload, `"escalation_id":2`) {
		t.Errorf("escalation events = %+v, want 1 and 2 only", escs)
	}
	if p := getPeer(t, s, "build"); p.EscalationCursor != 2 {
		t.Errorf("cursor = %d, want 2", p.EscalationCursor)
	}
}

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
	"github.com/nution101/ttorch/internal/scheduler"
)

// The scheduler's peer pass, end to end: the pass wired to the control client the way `ttorch
// scheduler` wires it (dialPeer), run in this process against the parent's store, reaching a
// provisioned peer through the ssh shim, which runs `ttorch peer serve` against the peer's own
// TTORCH_HOME and TTORCH_DB.

// usePeerShim points this process's control client at the fixture's stand-in ssh, as
// applyPeerClientSeams does for a parent run as its own process, and restores it afterwards.
func usePeerShim(t *testing.T, f *peerFixture, timeout time.Duration) {
	t.Helper()
	ssh, call := peerSSH, peerCallTimeout
	peerSSH, peerCallTimeout = f.shim, timeout
	t.Cleanup(func() { peerSSH, peerCallTimeout = ssh, call })
}

// shimVerbs is the verb of each control call the shim logged since call from, in order.
func shimVerbs(t *testing.T, f *peerFixture, key string, from int) []string {
	t.Helper()
	var out []string
	for _, c := range f.calls(t)[from:] {
		verb := c.Args[len(c.Args)-1]
		if want := controlArgs(key, "ttorch@build-host", verb); !reflect.DeepEqual(c.Args, want) {
			t.Errorf("the pass ran ssh %q\nwant          %q", c.Args, want)
		}
		out = append(out, verb)
	}
	return out
}

func peerEventsIn(t *testing.T, s *db.Store, name string) []db.Event {
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

func countType(evs []db.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// TestPeerPassOverTheControlChannel: one escalation on the peer becomes exactly one actionable
// event in the parent's store, its text escaped, with the cursor and the summary on the peer's
// row. The pass talks to the peer only through the control key's pinned command line. A new
// Scheduler on the same store (a restart) raises no duplicate. The fixture's peer has no manager
// recorded and no scheduler, so it is down: it gets three ensure-up calls, each refused there, and
// then one peer_down event and no more calls.
func TestPeerPassOverTheControlChannel(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	usePeerShim(t, f, 30*time.Second)
	ps := f.peerStore(t)
	proj, err := ps.UpsertProject(ctx, "/srv/q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.CreateTask(ctx, db.Task{ID: "t-q", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	esc, err := ps.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "which one?\x1b]0;pwned\x07\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	parent := f.parentStore(t)
	p, _, _ := parent.GetPeer(ctx, "build")
	from := len(f.calls(t))

	sc := &scheduler.Scheduler{Store: parent, Peers: dialPeer}
	if n, err := sc.RunPeerPassOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunPeerPassOnce = %d, %v", n, err)
	}
	if got, want := shimVerbs(t, f, p.ControlKey, from), []string{"summary", "decisions", "ensure-up"}; !reflect.DeepEqual(got, want) {
		t.Errorf("first pass called %v, want %v", got, want)
	}
	evs := peerEventsIn(t, parent, "build")
	if countType(evs, db.EventPeerEscalation) != 1 {
		t.Fatalf("events = %+v, want one peer_escalation", evs)
	}
	e := evs[0]
	if e.Type != db.EventPeerEscalation || !e.Actionable || e.EntityType != db.EntityTypeSystem || e.EntityID != "peer:build" || e.Actor != "peer:build" {
		t.Errorf("event = %+v", e)
	}
	if strings.ContainsAny(e.Payload, "\x1b\x07\n") {
		t.Errorf("payload holds a raw control character: %q", e.Payload)
	}
	var payload struct {
		EscalationID int64  `json:"escalation_id"`
		Kind         string `json:"kind"`
		TaskID       string `json:"task_id"`
		Body         string `json:"body"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		t.Fatalf("payload %q: %v", e.Payload, err)
	}
	if payload.EscalationID != esc.ID || payload.Kind != db.EscalationQuestion || payload.TaskID != "t-q" || payload.Body != `which one?\x1b]0;pwned\x07\nsecond line` {
		t.Errorf("payload = %+v", payload)
	}
	p, _, _ = parent.GetPeer(ctx, "build")
	var cached peer.Summary
	if err := json.Unmarshal([]byte(p.Summary), &cached); err != nil || cached.SchemaVersion != peer.SchemaVersion {
		t.Errorf("cached summary %q: %v", p.Summary, err)
	}
	if p.EscalationCursor != esc.ID || p.Status != db.PeerLive || p.DownAttempts != 1 || !strings.Contains(p.LastError, "no_manager") {
		t.Errorf("peer after the first pass = %+v", p)
	}

	// A restart, and three more passes: no duplicate, two more ensure-up calls, then peer_down.
	for i := 0; i < 3; i++ {
		sc = &scheduler.Scheduler{Store: parent, Peers: dialPeer}
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	verbs := shimVerbs(t, f, p.ControlKey, from)
	ensureUps := 0
	for _, v := range verbs {
		if v == "ensure-up" {
			ensureUps++
		}
	}
	if ensureUps != 3 || len(verbs) != 4*2+3 {
		t.Errorf("four passes called %v; want summary and decisions each pass and 3 ensure-up", verbs)
	}
	evs = peerEventsIn(t, parent, "build")
	if countType(evs, db.EventPeerEscalation) != 1 || countType(evs, db.EventPeerDown) != 1 || countType(evs, db.EventPeerEnsureUp) != 3 || len(evs) != 5 {
		t.Errorf("events after four passes = %+v, want the one escalation, three ensure-up records and one peer_down", evs)
	}
	if p, _, _ := parent.GetPeer(ctx, "build"); p.EscalationCursor != esc.ID {
		t.Errorf("cursor = %d", p.EscalationCursor)
	}

	// A second escalation is raised once, on the next pass.
	if _, err := ps.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "and the second?"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countType(peerEventsIn(t, parent, "build"), db.EventPeerEscalation); n != 2 {
		t.Errorf("after a second escalation: %d peer_escalation events, want 2", n)
	}
}

// TestPeerPassMarksAnUnreachablePeer: once the peer no longer admits the control key, three
// passes mark it unreachable with one actionable event, a fourth adds nothing, and the first pass
// after it admits the key again sets it live and raises peer_recovered. A peer that never answers
// is cut off at the client's deadline and the poll recorded as failed.
func TestPeerPassMarksAnUnreachablePeer(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	usePeerShim(t, f, 30*time.Second)
	parent := f.parentStore(t)
	p, _, _ := parent.GetPeer(ctx, "build")
	pub := p.ControlKey + ".pub"
	if err := os.Rename(pub, pub+".away"); err != nil {
		t.Fatal(err)
	}
	sc := &scheduler.Scheduler{Store: parent, Peers: dialPeer}
	for i := 0; i < db.PeerUnreachableAfter+1; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p, _, _ = parent.GetPeer(ctx, "build")
	if p.Status != db.PeerUnreachable || p.ConsecutiveFailures != db.PeerUnreachableAfter+1 || !strings.Contains(p.LastError, "Permission denied") {
		t.Errorf("after %d refused polls: %+v", db.PeerUnreachableAfter+1, p)
	}
	evs := peerEventsIn(t, parent, "build")
	if len(evs) != 1 || evs[0].Type != db.EventPeerUnreachable || !evs[0].Actionable || !strings.Contains(evs[0].Payload, "Permission denied") {
		t.Fatalf("events = %+v, want one actionable peer_unreachable", evs)
	}

	if err := os.Rename(pub+".away", pub); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := parent.GetPeer(ctx, "build"); p.Status != db.PeerLive || p.ConsecutiveFailures != 0 {
		t.Errorf("after the key is admitted again: %+v", p)
	}
	evs = peerEventsIn(t, parent, "build")
	if countType(evs, db.EventPeerRecovered) != 1 || countType(evs, db.EventPeerUnreachable) != 1 {
		t.Errorf("events = %+v, want one peer_unreachable and one peer_recovered", evs)
	}

	// A peer that never answers: the client's deadline ends the call and the poll fails.
	usePeerShim(t, f, time.Second)
	f.cfg.Hang = true
	f.writeConfig(t)
	start := time.Now()
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("a pass against a hung peer took %s past a 1s client deadline", took)
	}
	calls := f.calls(t)
	if hung := calls[len(calls)-1]; hung.Child != 0 {
		deadline := time.Now().Add(10 * time.Second)
		for (alive(hung.PID) || alive(hung.Child)) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if alive(hung.PID) || alive(hung.Child) {
			t.Errorf("the hung ssh (%d) or its child (%d) outlived the deadline", hung.PID, hung.Child)
		}
	}
	if p, _, _ := parent.GetPeer(ctx, "build"); p.ConsecutiveFailures != 1 || !strings.Contains(p.LastError, "did not answer in time") {
		t.Errorf("after a hung poll: failures %d, last error %q", p.ConsecutiveFailures, p.LastError)
	}
}

// TestControlChannelReadsTheSummary: the adapter hands the pass the peer's own judgement of its
// manager window and scheduler, and refuses a summary in a schema this binary does not read
// rather than guess at its fields, so the poll fails with the reason.
func TestControlChannelReadsTheSummary(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	answer := func(result string) {
		t.Helper()
		resp := `{"protocol":1,"verb":"summary","ok":true,"result":` + result + `}`
		if err := os.WriteFile(ssh, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '"+resp+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	saved, timeout := peerSSH, peerCallTimeout
	peerSSH, peerCallTimeout = ssh, 10*time.Second
	t.Cleanup(func() { peerSSH, peerCallTimeout = saved, timeout })
	ch := dialPeer(db.Peer{Name: "build", ControlDest: "ttorch@build-host", ControlKey: filepath.Join(dir, "key")})
	ctx := context.Background()

	for _, c := range []struct{ window, running, stalled bool }{
		{true, false, true}, {false, true, false}, {true, true, true}, {false, false, false},
	} {
		answer(fmt.Sprintf(`{"schema_version":%d,"manager":{"window_present":%t},"scheduler":{"daemon_running":%t,"stalled":%t}}`,
			peer.SchemaVersion, c.window, c.running, c.stalled))
		sum, err := ch.Summary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sum.ManagerWindow != c.window || sum.SchedulerRunning != c.running || sum.SchedulerStalled != c.stalled || !strings.Contains(sum.JSON, `"schema_version"`) {
			t.Errorf("peer reported %+v, adapter read %+v", c, sum)
		}
	}

	answer(`{"schema_version":99,"manager":{"window_present":true},"scheduler":{"daemon_running":true}}`)
	if _, err := ch.Summary(ctx); err == nil || !strings.Contains(err.Error(), "schema 99") {
		t.Errorf("a summary in schema 99: %v, want it refused", err)
	}
}

// TestPeerPassNoticesAPeerStoreThatWentBack: the peer's escalations are emptied and its id
// sequence reset, as a recreated store would leave them, so its next escalation is numbered 1
// again, below the cursor. The pass raises one peer_cursor_reset and the new escalation, which the
// old cursor would have hidden, and moves the cursor back to 1.
func TestPeerPassNoticesAPeerStoreThatWentBack(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	usePeerShim(t, f, 30*time.Second)
	ps := f.peerStore(t)
	proj, err := ps.UpsertProject(ctx, "/srv/q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.CreateTask(ctx, db.Task{ID: "t-q", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first", "second", "third"} {
		if _, err := ps.OpenEscalation(ctx, "t-q", db.EscalationQuestion, body); err != nil {
			t.Fatal(err)
		}
	}
	parent := f.parentStore(t)
	sc := &scheduler.Scheduler{Store: parent, Peers: dialPeer}
	if _, err := sc.RunPeerPassOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := parent.GetPeer(ctx, "build"); p.EscalationCursor != 3 {
		t.Fatalf("cursor = %d, want 3", p.EscalationCursor)
	}

	raw, err := sql.Open("sqlite", f.peer.db())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []string{`DELETE FROM escalations`, `UPDATE sqlite_sequence SET seq = 0 WHERE name = 'escalations'`} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	again, err := ps.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "after the reset")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != 1 {
		t.Fatalf("the peer numbered the new escalation %d, want 1", again.ID)
	}
	for i := 0; i < 2; i++ {
		if _, err := sc.RunPeerPassOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	evs := peerEventsIn(t, parent, "build")
	if n := countType(evs, db.EventPeerCursorReset); n != 1 {
		t.Errorf("%d peer_cursor_reset events, want 1", n)
	}
	if n := countType(evs, db.EventPeerEscalation); n != 4 {
		t.Errorf("%d peer_escalation events, want 4: three from before and the new one", n)
	}
	var last db.Event
	for _, e := range evs {
		if e.Type == db.EventPeerEscalation {
			last = e
		}
	}
	if !strings.Contains(last.Payload, "after the reset") {
		t.Errorf("last escalation event = %+v, want the new escalation", last)
	}
	if p, _, _ := parent.GetPeer(ctx, "build"); p.EscalationCursor != 1 {
		t.Errorf("cursor after the re-sync = %d, want 1", p.EscalationCursor)
	}
}

package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// storeAt opens a store on a new file and returns it with the file's path, so a test can open a
// second store on the same file, as a restarted or a second scheduler would.
func storeAt(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// livePeer registers name and marks it live, as a finished `peer add` leaves it.
func livePeer(t *testing.T, s *Store, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.RegisterPeer(ctx, testPeer(name), false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPeerLive(ctx, name, 1, "v1"); err != nil {
		t.Fatal(err)
	}
}

// peerEvents lists every event recorded for peer name, in order.
func peerEvents(t *testing.T, s *Store, name string) []Event {
	t.Helper()
	all, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range all {
		if e.EntityID == PeerEntityID(name) {
			out = append(out, e)
		}
	}
	return out
}

func mustPeer(t *testing.T, s *Store, name string) Peer {
	t.Helper()
	p, ok, err := s.GetPeer(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPeer %s = %v, %v", name, ok, err)
	}
	return p
}

// esc is one escalation as a poll finds it open on the peer: the peer's id, the peer's own
// creation stamp, and the payload of the event that raises it if this poll does.
type esc struct {
	id             int64
	stamp, payload string
}

// testBounds are the scheduler's bounds, written out here.
var testBounds = PeerPollBounds{MaxID: 1<<31 - 1, MaxJump: 10000, MaxRaise: 32}

// openPoll is a poll that reached the peer and found list open.
func openPoll(healthy bool, list ...esc) PeerPoll {
	p := PeerPoll{Healthy: healthy, Bounds: testBounds}
	for _, e := range list {
		p.Open = append(p.Open, PeerOpenEscalation{ID: e.id, CreatedAt: e.stamp})
	}
	p.PayloadFor = func(i int) string { return list[i].payload }
	return p
}

// run is n escalations with ids from..from+n-1, stamped prefix<id>, each with its stamp as payload.
func run(from int64, n int, prefix string) []esc {
	var out []esc
	for id := from; id < from+int64(n); id++ {
		st := fmt.Sprintf("%s%d", prefix, id)
		out = append(out, esc{id, st, st})
	}
	return out
}

func eventsOfType(evs []Event, typ string) []Event {
	var out []Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestRecordPeerPollRaisesEachEscalationOnce: a poll raises one actionable event per open
// escalation it has not raised, lowest id first, under entity system and the peer's id and
// actor, and moves the cursor to the highest it raised, in one transaction. The same poll again,
// or from a second store on the same file (a restarted scheduler), raises nothing more.
func TestRecordPeerPollRaisesEachEscalationOnce(t *testing.T) {
	ctx := context.Background()
	s, path := storeAt(t)
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.now = clock.now
	livePeer(t, s, "build")
	clock.advance(time.Minute)

	poll := openPoll(true, esc{7, "t7", "seven"}, esc{3, "t3", "three"})
	poll.Summary = `{"schema_version":2}`
	res, err := s.RecordPeerPoll(ctx, "build", poll)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Raised) != 2 || res.Cursor != 7 || res.Recovered || res.CursorReset || len(res.Refused) != 0 {
		t.Errorf("first poll = %+v, want two raised and the cursor at 7", res)
	}
	evs := peerEvents(t, s, "build")
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want two", evs)
	}
	for i, want := range []string{"three", "seven"} {
		e := evs[i]
		if e.Type != EventPeerEscalation || e.EntityType != EntityTypeSystem || e.EntityID != "peer:build" ||
			e.Actor != "peer:build" || !e.Actionable || e.Payload != want || e.ID != res.Raised[i] {
			t.Errorf("event %d = %+v, want an actionable peer_escalation %q for peer:build", i, e, want)
		}
	}
	p := mustPeer(t, s, "build")
	if p.EscalationCursor != 7 || p.Summary != `{"schema_version":2}` || !p.LastOKAt.Equal(clock.now()) || p.ConsecutiveFailures != 0 {
		t.Errorf("peer after the poll = %+v", p)
	}

	// The same answer again (a retry, or a second scheduler that read it too) raises nothing.
	if res, err := s.RecordPeerPoll(ctx, "build", poll); err != nil || len(res.Raised) != 0 || res.Cursor != 7 {
		t.Errorf("repeated poll = %+v, %v", res, err)
	}
	// A store opened again on the same file is a restarted scheduler.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	res, err = again.RecordPeerPoll(ctx, "build", openPoll(true, esc{3, "t3", "three"}, esc{7, "t7", "seven"}, esc{9, "t9", "nine"}))
	if err != nil || len(res.Raised) != 1 || res.Cursor != 9 {
		t.Errorf("poll after a restart = %+v, %v; want only 9 raised", res, err)
	}
	if evs := peerEvents(t, s, "build"); len(evs) != 3 || evs[2].Payload != "nine" {
		t.Errorf("events after the restart = %+v", evs)
	}
	// An empty summary keeps the cached one.
	if p := mustPeer(t, s, "build"); p.Summary != `{"schema_version":2}` {
		t.Errorf("summary after a poll that read none = %q", p.Summary)
	}
}

// TestRecordPeerPollSteadyStateIsNoRegression: open escalations with nothing new above the
// cursor, and a highest open id below the cursor because the newest one was answered, are the
// normal state of a peer, not a store that went back. Neither raises anything.
func TestRecordPeerPollSteadyStateIsNoRegression(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	if _, err := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{4, "t4", "four"}, esc{5, "t5", "five"})); err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]esc{{{4, "t4", "four"}, {5, "t5", "five"}}, {{4, "t4", "four"}}, {{4, "t4", "four"}}, nil} {
		res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, list...))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Raised) != 0 || res.CursorReset || res.Regressed || res.Cursor != 5 {
			t.Errorf("open %v: %+v, want nothing raised and the cursor left at 5", list, res)
		}
	}
	if evs := peerEvents(t, s, "build"); len(evs) != 2 {
		t.Errorf("events = %+v, want only the two escalations", evs)
	}
}

// TestRecordPeerPollDetectsAStoreThatWentBack: a peer whose store was recreated numbers its
// escalations from 1 again. Open escalations the parent never raised, at or below the cursor,
// are that signal: the poll raises one actionable peer_cursor_reset, raises them, and moves the
// cursor back to the highest open id it now holds. A store restored from a backup shows the same
// way, through an escalation that was answered here and is open there again, and only what was
// not already raised and still open is raised again.
func TestRecordPeerPollDetectsAStoreThatWentBack(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	if _, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 5, "a")...)); err != nil {
		t.Fatal(err)
	}

	// Recreated: ids 1 and 2 again, with stamps of the new store.
	res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 2, "b")...))
	if err != nil {
		t.Fatal(err)
	}
	if !res.CursorReset || !res.Regressed || res.ResetEventID == 0 || len(res.Raised) != 2 || res.Cursor != 2 {
		t.Fatalf("poll of a recreated store = %+v, want a reset, both raised and the cursor at 2", res)
	}
	evs := peerEvents(t, s, "build")
	resets := eventsOfType(evs, EventPeerCursorReset)
	if len(resets) != 1 || !resets[0].Actionable || resets[0].Actor != "peer:build" {
		t.Fatalf("reset events = %+v, want one actionable", resets)
	}
	var rp PeerCursorResetPayload
	if err := json.Unmarshal([]byte(resets[0].Payload), &rp); err != nil || rp.Peer != "build" || rp.Cursor != 5 || rp.Resynced != 2 || rp.Unseen != 2 || rp.Lowest != 1 {
		t.Errorf("reset payload %q = %+v, %v", resets[0].Payload, rp, err)
	}
	if got := eventsOfType(evs, EventPeerEscalation); len(got) != 7 || got[5].Payload != "b1" || got[6].Payload != "b2" || got[5].ID < resets[0].ID {
		t.Errorf("escalation events = %+v, want b1 and b2 raised after the reset", got)
	}
	// The same list again is consistent now, and a new one above raises only itself.
	if res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 2, "b")...)); err != nil || res.CursorReset || res.Regressed || len(res.Raised) != 0 {
		t.Errorf("the same list again = %+v, %v", res, err)
	}
	if res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 3, "b")...)); err != nil || res.CursorReset || len(res.Raised) != 1 || res.Cursor != 3 {
		t.Errorf("a new escalation after the re-sync = %+v, %v", res, err)
	}

	// Restored: 3 was answered here (it left the open list), and comes back with its old stamp;
	// 4 is new in the restored store.
	if _, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 2, "b")...)); err != nil {
		t.Fatal(err)
	}
	list := append(run(1, 3, "b"), esc{4, "c4", "c4"})
	res, err = s.RecordPeerPoll(ctx, "build", openPoll(true, list...))
	if err != nil {
		t.Fatal(err)
	}
	if !res.CursorReset || len(res.Raised) != 2 || res.Cursor != 4 {
		t.Errorf("poll of a restored store = %+v, want a reset with 3 and 4 raised", res)
	}
	if n := len(eventsOfType(peerEvents(t, s, "build"), EventPeerCursorReset)); n != 2 {
		t.Errorf("%d reset events, want 2", n)
	}
}

// TestRecordPeerPollResetsOncePerEpisode: polls that keep finding the ids gone back raise one
// peer_cursor_reset between them, while still raising what they find; a consistent poll ends the
// episode, and the next regression raises another.
func TestRecordPeerPollResetsOncePerEpisode(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	steps := []struct {
		list  []esc
		reset bool
	}{
		{run(1, 5, "a"), false},
		{run(1, 1, "b"), true},
		{run(1, 1, "c"), false}, // gone back again, in the same episode
		{run(1, 1, "c"), false}, // consistent: the episode ends
		{run(1, 1, "d"), true},
	}
	for i, st := range steps {
		res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, st.list...))
		if err != nil {
			t.Fatal(err)
		}
		if res.CursorReset != st.reset {
			t.Errorf("step %d: reset = %v, want %v (%+v)", i, res.CursorReset, st.reset, res)
		}
	}
	evs := peerEvents(t, s, "build")
	if n := len(eventsOfType(evs, EventPeerCursorReset)); n != 2 {
		t.Errorf("%d reset events, want 2", n)
	}
	if n := len(eventsOfType(evs, EventPeerEscalation)); n != 8 {
		t.Errorf("%d escalation events, want 8: a1-a5, b1, c1, d1", n)
	}
}

// TestRecordPeerPollRefusesIDsOutOfRange: an id at or below zero, one above the bound
// (math.MaxInt64 among them), a second listing of one id, and an id more than the jump bound
// above the cursor the poll started from are refused: not raised, and the cursor does not move
// for them. They raise one actionable peer_protocol_error per episode, and later escalations in
// range are still raised.
func TestRecordPeerPollRefusesIDsOutOfRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	res, err := s.RecordPeerPoll(ctx, "build", openPoll(true,
		esc{math.MaxInt64, "m", "max"}, esc{0, "z", "zero"}, esc{-3, "n", "negative"}, esc{2, "t2", "two"}, esc{2, "u2", "two again"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Raised) != 1 || res.Cursor != 2 || len(res.Refused) != 4 || res.ProtocolEventID == 0 {
		t.Fatalf("poll with bad ids = %+v, want only 2 raised, four refused and an event", res)
	}
	evs := peerEvents(t, s, "build")
	bad := eventsOfType(evs, EventPeerProtocolError)
	if len(bad) != 1 || !bad[0].Actionable {
		t.Fatalf("protocol events = %+v, want one actionable", bad)
	}
	var bp PeerProtocolErrorPayload
	if err := json.Unmarshal([]byte(bad[0].Payload), &bp); err != nil || bp.Peer != "build" || bp.Refused != 4 || bp.Reason == "" || len(bp.IDs) != 4 {
		t.Errorf("protocol payload %q = %+v, %v", bad[0].Payload, bp, err)
	}
	if esc := eventsOfType(evs, EventPeerEscalation); len(esc) != 1 || esc[0].Payload != "two" {
		t.Errorf("escalation events = %+v, want only the first listing of 2", esc)
	}

	// Still there: same episode, nothing new. A clean poll ends it; the next bad id raises again.
	if res, _ := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{2, "t2", "two"}, esc{math.MaxInt64, "m", "max"})); res.ProtocolEventID != 0 || len(res.Refused) != 1 {
		t.Errorf("bad id again in the episode = %+v", res)
	}
	if _, err := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{2, "t2", "two"})); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{2, "t2", "two"}, esc{-1, "n", "neg"})); res.ProtocolEventID == 0 {
		t.Errorf("a bad id after a clean poll raised nothing: %+v", res)
	}

	// The jump bound: from cursor 2, 2+MaxJump is accepted and 2+MaxJump+1 refused, whichever
	// order they arrive in; a later poll from the new cursor accepts it.
	far, farther := 2+testBounds.MaxJump, 2+testBounds.MaxJump+1
	res, err = s.RecordPeerPoll(ctx, "build", openPoll(true, esc{farther, "f2", "farther"}, esc{2, "t2", "two"}, esc{far, "f1", "far"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Raised) != 1 || res.Cursor != far || len(res.Refused) != 1 || res.Refused[0] != farther {
		t.Errorf("jump poll = %+v, want %d raised and %d refused", res, far, farther)
	}
	if res, _ := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{farther, "f2", "farther"}, esc{2, "t2", "two"}, esc{far, "f1", "far"})); len(res.Raised) != 1 || res.Cursor != farther {
		t.Errorf("from the new cursor = %+v, want %d raised", res, farther)
	}

	// A fresh peer (cursor 0) takes any id up to the bound.
	livePeer(t, s, "fresh")
	if res, _ := s.RecordPeerPoll(ctx, "fresh", openPoll(true, esc{5_000_000, "x", "big"})); len(res.Raised) != 1 || len(res.Refused) != 0 {
		t.Errorf("a fresh peer with a large id = %+v", res)
	}
	if res, _ := s.RecordPeerPoll(ctx, "fresh", openPoll(true, esc{testBounds.MaxID + 1, "y", "over"})); len(res.Refused) != 1 || len(res.Raised) != 0 {
		t.Errorf("an id over the bound = %+v", res)
	}
}

// TestRecordPeerPollCapsRaisesPerPoll: a poll raises at most MaxRaise, lowest first; the rest
// come on the next polls, and that is not a regression, also when the cap falls in a re-sync.
func TestRecordPeerPollCapsRaisesPerPoll(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 40, "a")...))
	if err != nil || len(res.Raised) != 32 || res.Cursor != 32 {
		t.Fatalf("first poll = %+v, %v", res, err)
	}
	if res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 40, "a")...)); err != nil || len(res.Raised) != 8 || res.Cursor != 40 || res.Regressed {
		t.Errorf("second poll = %+v, %v", res, err)
	}
	// Recreated with 40 open: one reset, 32 then 8 raised.
	res, err = s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 40, "b")...))
	if err != nil || !res.CursorReset || len(res.Raised) != 32 || res.Cursor != 32 {
		t.Errorf("re-sync poll = %+v, %v", res, err)
	}
	if res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, run(1, 40, "b")...)); err != nil || res.Regressed || len(res.Raised) != 8 {
		t.Errorf("poll after the re-sync = %+v, %v", res, err)
	}
	if n := len(eventsOfType(peerEvents(t, s, "build"), EventPeerEscalation)); n != 80 {
		t.Errorf("%d escalation events, want 80", n)
	}
}

// TestRecordPeerPollConcurrent: two schedulers recording the same answer at once raise each
// escalation once between them.
func TestRecordPeerPollConcurrent(t *testing.T) {
	ctx := context.Background()
	s, path := storeAt(t)
	livePeer(t, s, "build")
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	poll := openPoll(true, esc{1, "t1", "a"}, esc{2, "t2", "b"})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := s
			if i%2 == 1 {
				st = other
			}
			_, errs[i] = st.RecordPeerPoll(ctx, "build", poll)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if evs := peerEvents(t, s, "build"); len(evs) != 2 {
		t.Errorf("events = %+v, want each escalation raised once", evs)
	}
}

// TestRecordPeerFailureMarksUnreachableOnce: failed polls count up and record the error; the
// one that reaches PeerUnreachableAfter marks the peer unreachable and raises one actionable
// event. Failures after that raise nothing. A successful poll then sets it live again, clears the
// streak and raises one peer_recovered event, which is not actionable.
func TestRecordPeerFailureMarksUnreachableOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	for i := 1; i <= PeerUnreachableAfter+2; i++ {
		res, err := s.RecordPeerFailure(ctx, "build", "ssh: connect: refused", "down payload")
		if err != nil {
			t.Fatal(err)
		}
		if res.Failures != i || res.Unreachable != (i == PeerUnreachableAfter) || (res.EventID != 0) != (i == PeerUnreachableAfter) {
			t.Errorf("failure %d = %+v", i, res)
		}
		want := PeerLive
		if i >= PeerUnreachableAfter {
			want = PeerUnreachable
		}
		if p := mustPeer(t, s, "build"); p.Status != want || p.ConsecutiveFailures != i || p.LastError != "ssh: connect: refused" {
			t.Errorf("after failure %d: %+v", i, p)
		}
	}
	evs := peerEvents(t, s, "build")
	if len(evs) != 1 || evs[0].Type != EventPeerUnreachable || !evs[0].Actionable || evs[0].Actor != "peer:build" ||
		evs[0].EntityType != EntityTypeSystem || evs[0].Payload != "down payload" {
		t.Fatalf("events = %+v, want one actionable peer_unreachable", evs)
	}

	res, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: true, RecoveredPayload: "back"})
	if err != nil || !res.Recovered || res.RecoveredEventID == 0 {
		t.Fatalf("poll after the outage = %+v, %v", res, err)
	}
	if p := mustPeer(t, s, "build"); p.Status != PeerLive || p.ConsecutiveFailures != 0 || p.LastError != "" {
		t.Errorf("after recovery: %+v", p)
	}
	evs = peerEvents(t, s, "build")
	if len(evs) != 2 || evs[1].Type != EventPeerRecovered || evs[1].Actionable || evs[1].Payload != "back" || evs[1].ID != res.RecoveredEventID {
		t.Errorf("events = %+v, want a non-actionable peer_recovered last", evs)
	}
	if res, _ := s.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: true, RecoveredPayload: "back"}); res.Recovered {
		t.Error("a second good poll recovered again")
	}

	// A failure streak shorter than the threshold, broken by a good poll, starts over.
	for i := 0; i < PeerUnreachableAfter-1; i++ {
		if _, err := s.RecordPeerFailure(ctx, "build", "x", "p"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: true}); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.RecordPeerFailure(ctx, "build", "x", "p"); res.Failures != 1 || res.Unreachable {
		t.Errorf("first failure after a good poll = %+v", res)
	}
	if evs := peerEvents(t, s, "build"); len(evs) != 2 {
		t.Errorf("a short streak raised an event: %+v", evs)
	}
}

// TestStepPeerDownBoundsEnsureUp: a down episode allows PeerEnsureUpAttempts ensure-up calls,
// then one peer_down event, then nothing until a poll finds the peer healthy, which starts the
// next episode over.
func TestStepPeerDownBoundsEnsureUp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	livePeer(t, s, "build")
	episode := func(label string) {
		t.Helper()
		ensureUps, downs := 0, 0
		for i := 0; i < PeerEnsureUpAttempts+4; i++ {
			step, err := s.StepPeerDown(ctx, "build", "no manager window")
			if err != nil {
				t.Fatal(err)
			}
			if step.EnsureUp && step.Down {
				t.Fatalf("%s: step %d both calls ensure-up and raises peer_down: %+v", label, i, step)
			}
			if step.EnsureUp {
				ensureUps++
				if step.Attempt != ensureUps {
					t.Errorf("%s: attempt = %d, want %d", label, step.Attempt, ensureUps)
				}
				if downs > 0 {
					t.Errorf("%s: ensure-up allowed after peer_down", label)
				}
			}
			if step.Down {
				downs++
				if step.EventID == 0 {
					t.Errorf("%s: peer_down step has no event", label)
				}
			}
		}
		if ensureUps != PeerEnsureUpAttempts || downs != 1 {
			t.Errorf("%s: %d ensure-up calls and %d peer_down, want %d and 1", label, ensureUps, downs, PeerEnsureUpAttempts)
		}
	}
	episode("first episode")
	evs := peerEvents(t, s, "build")
	if len(evs) != 1 || evs[0].Type != EventPeerDown || !evs[0].Actionable || evs[0].Payload != "no manager window" || evs[0].Actor != "peer:build" {
		t.Fatalf("events = %+v, want one actionable peer_down", evs)
	}
	// A poll that finds it still down changes nothing; a healthy one ends the episode.
	if _, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: false}); err != nil {
		t.Fatal(err)
	}
	if step, _ := s.StepPeerDown(ctx, "build", "x"); step.EnsureUp || step.Down {
		t.Errorf("a poll that found the peer down started a new episode: %+v", step)
	}
	if _, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: true}); err != nil {
		t.Fatal(err)
	}
	if p := mustPeer(t, s, "build"); p.DownAttempts != 0 {
		t.Errorf("down_attempts after a healthy poll = %d", p.DownAttempts)
	}
	episode("second episode")
	if evs := peerEvents(t, s, "build"); len(evs) != 2 {
		t.Errorf("events after two episodes = %+v", evs)
	}
}

// TestPeerPollRefusesAPeerNotInUse: a result for a peer that was retired or sent back to
// provisioning while the poll ran is dropped, and so is one for a name nothing is registered
// under; nothing is written.
func TestPeerPollRefusesAPeerNotInUse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.RecordPeerPoll(ctx, "ghost", PeerPoll{Healthy: true}); !errors.Is(err, ErrPeerNotFound) {
		t.Errorf("poll of an unknown peer: %v", err)
	}
	if _, err := s.RegisterPeer(ctx, testPeer("prov"), false); err != nil {
		t.Fatal(err)
	}
	livePeer(t, s, "old")
	if _, err := s.RetirePeer(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"prov", "old"} {
		before := mustPeer(t, s, name)
		poll := openPoll(true, esc{1, "t1", "x"})
		poll.Summary = "{}"
		if _, err := s.RecordPeerPoll(ctx, name, poll); !errors.Is(err, ErrPeerNotPolled) {
			t.Errorf("%s: poll = %v, want ErrPeerNotPolled", name, err)
		}
		for i := 0; i < PeerUnreachableAfter; i++ {
			if _, err := s.RecordPeerFailure(ctx, name, "x", "p"); !errors.Is(err, ErrPeerNotPolled) {
				t.Errorf("%s: failure = %v, want ErrPeerNotPolled", name, err)
			}
		}
		if _, err := s.StepPeerDown(ctx, name, "p"); !errors.Is(err, ErrPeerNotPolled) {
			t.Errorf("%s: down step = %v, want ErrPeerNotPolled", name, err)
		}
		if after := mustPeer(t, s, name); after.EscalationCursor != before.EscalationCursor || after.Status != before.Status ||
			after.ConsecutiveFailures != 0 || after.DownAttempts != 0 {
			t.Errorf("%s changed: %+v", name, after)
		}
		if evs := peerEvents(t, s, name); len(evs) != 0 {
			t.Errorf("%s: events = %+v", name, evs)
		}
	}
}

// TestRecordPeerFailureCapsTheError: the recorded error is capped like any other peer error.
func TestRecordPeerFailureCapsTheError(t *testing.T) {
	s := newTestStore(t)
	livePeer(t, s, "build")
	if _, err := s.RecordPeerFailure(context.Background(), "build", strings.Repeat("e", 3*MaxEscalationText), "p"); err != nil {
		t.Fatal(err)
	}
	if p := mustPeer(t, s, "build"); len(p.LastError) > MaxEscalationText {
		t.Errorf("last error is %d bytes", len(p.LastError))
	}
}

// TestRegisterPeerStartsThePollOver: registering a name again (a retried add, an adopt, or an add
// after retire) may point it at another machine or a fresh store whose escalation ids start at 1
// again, so the poll's state on the row starts over: the escalation cursor, the failure streak
// and the down episode all go back to zero.
func TestRegisterPeerStartsThePollOver(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		label   string
		prepare func(t *testing.T, s *Store)
		replace bool
	}{
		{"adopted while live", func(t *testing.T, s *Store) {}, true},
		{"re-added after retire", func(t *testing.T, s *Store) {
			if _, err := s.RetirePeer(ctx, "build"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"adopted while unreachable", func(t *testing.T, s *Store) {
			for i := 0; i < PeerUnreachableAfter; i++ {
				if _, err := s.RecordPeerFailure(ctx, "build", "x", "p"); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
	} {
		t.Run(c.label, func(t *testing.T) {
			s := newTestStore(t)
			livePeer(t, s, "build")
			if _, err := s.RecordPeerPoll(ctx, "build", openPoll(false, esc{1, "a1", "one"}, esc{7, "a7", "seven"})); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < PeerEnsureUpAttempts+1; i++ {
				if _, err := s.StepPeerDown(ctx, "build", "down"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.RecordPeerFailure(ctx, "build", "x", "p"); err != nil {
				t.Fatal(err)
			}
			c.prepare(t, s)
			if p := mustPeer(t, s, "build"); p.EscalationCursor != 7 || p.DownAttempts == 0 || p.ConsecutiveFailures == 0 {
				t.Fatalf("before registering again: %+v", p)
			}
			if _, err := s.RegisterPeer(ctx, testPeer("build"), c.replace); err != nil {
				t.Fatal(err)
			}
			p := mustPeer(t, s, "build")
			if p.EscalationCursor != 0 || p.ConsecutiveFailures != 0 || p.DownAttempts != 0 || p.Status != PeerProvisioning {
				t.Errorf("after registering again: cursor %d, failures %d, down %d, status %s; want all zero and provisioning",
					p.EscalationCursor, p.ConsecutiveFailures, p.DownAttempts, p.Status)
			}
			// Once live again, an escalation a new store numbers 1 is raised, and so is one the
			// old store still has open: the registration starts the record of what was raised
			// over too. Neither is a regression.
			if err := s.MarkPeerLive(ctx, "build", 1, "v2"); err != nil {
				t.Fatal(err)
			}
			res, err := s.RecordPeerPoll(ctx, "build", openPoll(true, esc{1, "b1", "new one"}, esc{7, "a7", "seven"}))
			if err != nil || len(res.Raised) != 2 || res.CursorReset {
				t.Errorf("after registering again: %+v, %v; want both raised and no reset", res, err)
			}
		})
	}
}

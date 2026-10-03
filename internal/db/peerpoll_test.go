package db

import (
	"context"
	"errors"
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

// TestRecordPeerPollRaisesEachEscalationOnce: a poll raises one actionable event per escalation
// above the cursor, under entity system and the peer's id and actor, and moves the cursor to the
// highest it raised in the same transaction. The same poll again, a repeated or lower id, or a
// second store on the same file raises nothing more.
func TestRecordPeerPollRaisesEachEscalationOnce(t *testing.T) {
	ctx := context.Background()
	s, path := storeAt(t)
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.now = clock.now
	livePeer(t, s, "build")
	clock.advance(time.Minute)

	poll := PeerPoll{Summary: `{"schema_version":2}`, Healthy: true, Raise: []PeerRaise{
		{ID: 7, Payload: "seven"}, {ID: 3, Payload: "three"}, {ID: 7, Payload: "seven again"}, {ID: 0, Payload: "zero"},
	}}
	res, err := s.RecordPeerPoll(ctx, "build", poll)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Raised) != 2 || res.Cursor != 7 || res.Recovered {
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
	if res, err := again.RecordPeerPoll(ctx, "build", PeerPoll{Healthy: true, Raise: []PeerRaise{{ID: 5}, {ID: 7}, {ID: 9, Payload: "nine"}}}); err != nil || len(res.Raised) != 1 || res.Cursor != 9 {
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
	poll := PeerPoll{Healthy: true, Raise: []PeerRaise{{ID: 1, Payload: "a"}, {ID: 2, Payload: "b"}}}
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
		raise := []PeerRaise{{ID: 1, Payload: "x"}}
		if _, err := s.RecordPeerPoll(ctx, name, PeerPoll{Healthy: true, Raise: raise, Summary: "{}"}); !errors.Is(err, ErrPeerNotPolled) {
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
			if _, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Raise: []PeerRaise{{ID: 7, Payload: "seven"}}}); err != nil {
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
			// Once live again, an escalation the new store numbers 1 is raised.
			if err := s.MarkPeerLive(ctx, "build", 1, "v2"); err != nil {
				t.Fatal(err)
			}
			if res, err := s.RecordPeerPoll(ctx, "build", PeerPoll{Raise: []PeerRaise{{ID: 1, Payload: "one"}}}); err != nil || len(res.Raised) != 1 {
				t.Errorf("escalation 1 after registering again: %+v, %v", res, err)
			}
		})
	}
}

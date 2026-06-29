package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/worktree"
)

// --- gate pass (roadmap A1) --------------------------------------------------------

// TestRunGateOnceGatesUngatedCandidate is the happy path: a done task in a TRUSTED repo with no
// verdict is atomically claimed (a "gater:" lease), handed to GateOnce, counted as recorded, and
// then has its claim released so the land pass can pick up the verdict.
func TestRunGateOnceGatesUngatedCandidate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "g1", db.KindShip, "") // no verdict ⇒ gate candidate

	f := &fakeFleet{gateable: map[string]bool{repo: true}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunGateOnce(ctx)
	if err != nil {
		t.Fatalf("RunGateOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded count = %d, want 1", n)
	}
	if got := f.gatedIDs(); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("GateOnce received %v, want [g1]", got)
	}
	// The within-tick gate claim is released, so the land pass can immediately claim it.
	if lo := leaseOwner(t, s, "g1"); lo != "" {
		t.Errorf("gate claim must be released after GateOnce, got lease %q", lo)
	}
}

// TestRunGateOnceSkipsNonGateable proves the gate pass never touches a done task in a non-trusted
// repo — there a daemon-recorded verdict would not advance delivery (a human still approves).
func TestRunGateOnceSkipsNonGateable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "g1", db.KindShip, "")

	f := &fakeFleet{} // gateable nil ⇒ Gateable returns false
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunGateOnce(ctx)
	if err != nil {
		t.Fatalf("RunGateOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("recorded count = %d, want 0 (non-trusted)", n)
	}
	if got := f.gatedIDs(); len(got) != 0 {
		t.Fatalf("GateOnce was called %v in a non-trusted repo; want none", got)
	}
	if lo := leaseOwner(t, s, "g1"); lo == "gater:g1" {
		t.Errorf("a non-gateable task must not be claimed, got lease %q", lo)
	}
}

// TestRunGateOnceSkipsPassingVerdict proves a done task that ALREADY carries a passing verdict is
// left for the LAND pass (which may even carry it forward) — the gate never re-runs reviewers on
// already-gated work. A done task with a BLOCKING verdict, by contrast, is still a candidate.
func TestRunGateOnceSkipsPassingVerdict(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "passing", db.KindShip, review.Pass)  // land owns it
	addDone(t, s, repo, "blocked", db.KindShip, review.Block) // still a gate candidate (re-gate)
	addDone(t, s, repo, "fresh", db.KindShip, "")             // ungated candidate

	f := &fakeFleet{gateable: map[string]bool{repo: true}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	if _, err := sc.RunGateOnce(ctx); err != nil {
		t.Fatalf("RunGateOnce: %v", err)
	}
	got := f.gatedIDs()
	if contains(got, "passing") {
		t.Fatalf("a passing-verdict task must not be gated again, got %v", got)
	}
	if !contains(got, "blocked") || !contains(got, "fresh") {
		t.Fatalf("ungated/blocked tasks must be gate candidates, got %v", got)
	}
	if lo := leaseOwner(t, s, "passing"); lo == "gater:passing" {
		t.Errorf("a passing-verdict task must not be gate-claimed, got lease %q", lo)
	}
}

// TestRunGateOnceExcludesCC proves ad-hoc cc sessions are never auto-gated, mirroring dispatch
// and land.
func TestRunGateOnceExcludesCC(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "ccsess", db.KindCC, "")
	addDone(t, s, repo, "ship1", db.KindShip, "")

	f := &fakeFleet{gateable: map[string]bool{repo: true}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	if _, err := sc.RunGateOnce(ctx); err != nil {
		t.Fatalf("RunGateOnce: %v", err)
	}
	if got := f.gatedIDs(); len(got) != 1 || got[0] != "ship1" {
		t.Fatalf("GateOnce received %v, want only [ship1] — cc excluded", got)
	}
}

// TestRunGateOnceCountsOnlyRecorded proves the headline count is the number of verdicts RECORDED
// this tick — a task whose reviewers are still being dispatched (GateDispatched) is gated but not
// counted, and its claim is released for the next tick.
func TestRunGateOnceCountsOnlyRecorded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "recorded", db.KindShip, "")
	addDone(t, s, repo, "dispatching", db.KindShip, "")

	f := &fakeFleet{
		gateable: map[string]bool{repo: true},
		gateOut: map[string]orchestrator.GateOutcome{
			"recorded":    orchestrator.GateRecorded,
			"dispatching": orchestrator.GateDispatched,
		},
	}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunGateOnce(ctx)
	if err != nil {
		t.Fatalf("RunGateOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded count = %d, want 1 (only the recorded task)", n)
	}
	// Both were attempted, and both claims released (cross-tick continuity is the review dir).
	if len(f.gatedIDs()) != 2 {
		t.Fatalf("both candidates should be gated once, got %v", f.gatedIDs())
	}
	for _, id := range []string{"recorded", "dispatching"} {
		if lo := leaseOwner(t, s, id); lo != "" {
			t.Errorf("gate claim on %s must be released, got lease %q", id, lo)
		}
	}
}

// TestRunGateOncePerTaskErrorDoesNotAbort proves a per-task GateOnce error is logged and the task
// left for a later tick (claim released), without aborting the whole pass — the other candidates
// still gate.
func TestRunGateOncePerTaskErrorDoesNotAbort(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "boom", db.KindShip, "")
	addDone(t, s, repo, "ok", db.KindShip, "")

	f := &fakeFleet{
		gateable: map[string]bool{repo: true},
		gateErr:  map[string]error{"boom": errors.New("transient board read")},
	}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunGateOnce(ctx)
	if err != nil {
		t.Fatalf("a per-task gate error must not abort the pass: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded count = %d, want 1 (the ok task)", n)
	}
	if !contains(f.gatedIDs(), "ok") {
		t.Fatalf("the healthy task must still be gated, got %v", f.gatedIDs())
	}
	// The errored task's claim is released so a later tick can retry it.
	if lo := leaseOwner(t, s, "boom"); lo != "" {
		t.Errorf("the errored task's gate claim must be released, got lease %q", lo)
	}
}

// TestRunGateOnceContextCancelStops proves a cancelled context halts the gate pass cleanly,
// gating nothing and leaving no claim behind.
func TestRunGateOnceContextCancelStops(t *testing.T) {
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "g1", db.KindShip, "")

	f := &fakeFleet{gateable: map[string]bool{repo: true}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := sc.RunGateOnce(ctx)
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunGateOnce on a cancelled ctx = (%d, %v), want (0, context.Canceled)", n, err)
	}
	if got := f.gatedIDs(); len(got) != 0 {
		t.Fatalf("a cancelled gate pass gated %v, want nothing", got)
	}
	if lo := leaseOwner(t, s, "g1"); lo == "gater:g1" {
		t.Errorf("a cancelled gate pass must leave no claim, got lease %q", lo)
	}
}

// TestRunTickGatesOnlyWhenEnabled proves the gate pass is OFF by default: a tick gates done work
// only when sc.Gate is set, so enabling it is an explicit opt-in with no behavior change otherwise.
func TestRunTickGatesOnlyWhenEnabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "g1", db.KindShip, "")

	// Gate disabled (the zero value): the tick must not gate.
	f := &fakeFleet{gateable: map[string]bool{repo: true}}
	off := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}} // Gate=false
	off.runTick(ctx)
	if got := f.gatedIDs(); len(got) != 0 {
		t.Fatalf("a tick with gate disabled gated %v, want nothing", got)
	}

	// Gate enabled: the tick gates the candidate.
	on := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Gate: true}
	on.runTick(ctx)
	if got := f.gatedIDs(); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("a tick with gate enabled gated %v, want [g1]", got)
	}
}

// TestConcurrentRunGateOnceNoDoubleGate is the gate race test, mirroring the dispatch/land ones:
// two scheduler instances on the SAME db file running a gate tick concurrently RECORD each
// candidate's verdict EXACTLY once and lose none. Exactly-once-record rests on two guards
// together — the within-tick atomic gate claim (which serializes overlapping ticks) AND
// GateOnce's own idempotency (a task whose verdict already covers head gates to GateSkipped, so a
// second actor running after the first never re-records). The fake models that idempotency via a
// shared recorded set, exactly as the real GateOnce consults the shared DB verdict.
func TestConcurrentRunGateOnceNoDoubleGate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	defer s1.Close()
	s2, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	defer s2.Close()

	repo := "/repo"
	const n = 12
	for i := 0; i < n; i++ {
		id := string(rune('a'+i/26)) + string(rune('a'+i%26))
		addDone(t, s1, repo, id, db.KindShip, "")
	}

	var mu sync.Mutex
	counts := map[string]int{}
	sink := func(id string) {
		mu.Lock()
		counts[id]++
		mu.Unlock()
	}
	// Shared idempotency, mirroring the DB-backed verdict the real GateOnce consults: a task
	// recorded by one instance gates to GateSkipped in the other.
	idem := &gateIdem{recorded: map[string]bool{}}

	var wg sync.WaitGroup
	for _, s := range []*db.Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &fakeFleet{gateable: map[string]bool{repo: true}, record: sink, idem: idem}
			sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
			if _, err := sc.RunGateOnce(ctx); err != nil {
				t.Errorf("concurrent RunGateOnce: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(counts) != n {
		t.Fatalf("recorded %d distinct tasks, want %d", len(counts), n)
	}
	for id, c := range counts {
		if c != 1 {
			t.Errorf("task %s recorded %d times, want exactly 1", id, c)
		}
	}
}

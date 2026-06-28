package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/worktree"
)

// spawnCall records one dispatch the fake fleet received.
type spawnCall struct {
	id        string
	repo      string
	scout     bool
	footprint []string
}

// fakeFleet is a tmux-free stand-in for *orchestrator.Manager: Status returns a configured
// live set, CheckOverlap faithfully models the manager's LIVENESS-GATED predicate
// (state.FootprintOverlap, scoped by repo, excluding cc/empty footprints AND — like the
// real m.Live gate — window-less tasks) over that set, and SpawnWithFootprint records the
// call (and optionally forces an error / forwards to a shared sink for concurrency tests)
// instead of standing up a worktree + tmux window.
type fakeFleet struct {
	mu       sync.Mutex
	live     []db.Task
	calls    []spawnCall
	spawnErr map[string]error // per-task forced dispatch failure
	landed   []string         // ids handed to LandSet, in call order
	landErr  map[string]error // per-task forced land failure
	record   func(id string)  // shared sink for the cross-instance race tests
}

func (f *fakeFleet) Status() ([]db.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Task, len(f.live))
	copy(out, f.live)
	return out, nil
}

func (f *fakeFleet) CheckOverlap(repo string, proposed []string) []orchestrator.Conflict {
	if len(proposed) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orchestrator.Conflict
	for _, t := range f.live {
		// The real CheckOverlap is liveness-gated (m.Live ⇒ a tmux window exists), so a
		// window-less task is invisible to it — model that with t.Window == "".
		if t.Project != repo || t.Kind == db.KindCC || len(t.Footprint) == 0 || t.Window == "" {
			continue
		}
		if ov := state.FootprintOverlap(proposed, t.Footprint); len(ov) > 0 {
			out = append(out, orchestrator.Conflict{TaskID: t.ID, Window: t.Window, Project: t.Project, Overlaps: ov})
		}
	}
	return out
}

func (f *fakeFleet) SpawnWithFootprint(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool) (db.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.spawnErr[taskID]; err != nil {
		return db.Task{}, err
	}
	f.calls = append(f.calls, spawnCall{id: taskID, repo: projectPath, scout: scout, footprint: footprint})
	if f.record != nil {
		f.record(taskID)
	}
	return db.Task{ID: taskID, Project: projectPath, Footprint: footprint, Status: db.StatusActive}, nil
}

func (f *fakeFleet) dispatched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, len(f.calls))
	for i, c := range f.calls {
		ids[i] = c.id
	}
	return ids
}

// LandSet stands in for the manager's concurrent land pipeline: it records each id it was
// asked to land (the seam the scheduler's land claim feeds) and returns a per-task result,
// failing exactly the ids in landErr. The real LandSet's merge gate is exercised in
// internal/orchestrator; here the fake isolates the scheduler's selection/claim/release logic.
func (f *fakeFleet) LandSet(ctx context.Context, taskIDs []string, requireVerdict bool) []orchestrator.LandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := make([]orchestrator.LandResult, len(taskIDs))
	for i, id := range taskIDs {
		f.landed = append(f.landed, id)
		if f.record != nil {
			f.record(id)
		}
		res[i] = orchestrator.LandResult{TaskID: id, Err: f.landErr[id]}
	}
	return res
}

func (f *fakeFleet) landedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.landed))
	copy(out, f.landed)
	return out
}

// --- test fixtures ----------------------------------------------------------------

func newStore(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// addPending creates a pending backlog task under repo with the given footprint and kind.
func addPending(t *testing.T, s *db.Store, repo, id, kind string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusPending, Kind: kind, Footprint: footprint,
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
}

func status(t *testing.T, s *db.Store, id string) string {
	t.Helper()
	tk, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask %s: ok=%v err=%v", id, ok, err)
	}
	return tk.Status
}

func leaseOwner(t *testing.T, s *db.Store, id string) string {
	t.Helper()
	tk, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask %s: ok=%v err=%v", id, ok, err)
	}
	return tk.LeaseOwner
}

// addDone creates a done task awaiting landing under repo with the given kind, plus an optional
// durable verdict: verdict == "" records none, otherwise it saves a verdict with that Overall
// (review.Pass / review.Block). A verdict requires the task to exist first (FK), so the order
// matters.
func addDone(t *testing.T, s *db.Store, repo, id, kind, verdict string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusDone, Kind: kind, Footprint: []string{"pkg/" + id},
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	if verdict != "" {
		if err := s.SaveVerdict(ctx, db.Verdict{TaskID: id, Overall: verdict, ReviewedSHA: "sha-" + id}); err != nil {
			t.Fatalf("SaveVerdict %s: %v", id, err)
		}
	}
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- tests ------------------------------------------------------------------------

// TestRunOnceDispatchesDisjointReady proves the happy path: disjoint declared-footprint
// pending tasks are all claimed (flipped to active in the DB) and dispatched, in
// created/id order, and a scout task is dispatched as a scout.
func TestRunOnceDispatchesDisjointReady(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "a", db.KindShip, []string{"internal/cli"})
	addPending(t, s, repo, "b", db.KindShip, []string{"internal/db"})
	addPending(t, s, repo, "c", db.KindScout, []string{"internal/worktree"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("dispatched %d, want 3", n)
	}
	got := f.dispatched()
	for _, id := range []string{"a", "b", "c"} {
		if !contains(got, id) {
			t.Errorf("%s was not dispatched (got %v)", id, got)
		}
		if status(t, s, id) != db.StatusActive {
			t.Errorf("%s status = %q, want active after claim", id, status(t, s, id))
		}
	}
	// The scout is dispatched as a scout; the ships are not.
	for _, c := range f.calls {
		if (c.id == "c") != c.scout {
			t.Errorf("scout flag for %s = %v, want %v", c.id, c.scout, c.id == "c")
		}
	}
}

// TestRunOnceSkipsOverlapping proves overlap is skipped (not failed): a task overlapping a
// LIVE worker stays pending, and of two ready tasks that overlap EACH OTHER only the first
// (created/id order) is dispatched — the just-claimed sibling, invisible to CheckOverlap,
// is caught by the in-tick footprint tracking.
func TestRunOnceSkipsOverlapping(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	// Overlaps a live worker holding internal/cli.
	addPending(t, s, repo, "live-conflict", db.KindShip, []string{"internal/cli/cli.go"})
	// Two ready tasks that overlap each other on internal/db.
	addPending(t, s, repo, "pair-1", db.KindShip, []string{"internal/db"})
	addPending(t, s, repo, "pair-2", db.KindShip, []string{"internal/db/task.go"})
	// A genuinely disjoint task.
	addPending(t, s, repo, "solo", db.KindShip, []string{"internal/orchestrator"})

	f := &fakeFleet{live: []db.Task{
		{ID: "w1", Window: "wk-w1", Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"internal/cli"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := f.dispatched()
	// live-conflict and pair-2 must NOT dispatch; pair-1 and solo must.
	if contains(got, "live-conflict") {
		t.Error("a task overlapping a live worker must be skipped")
	}
	if contains(got, "pair-1") == contains(got, "pair-2") {
		t.Errorf("exactly one of the mutually-overlapping pair must dispatch, got %v", got)
	}
	if !contains(got, "pair-1") {
		t.Errorf("pair-1 (first in order) should win over pair-2, got %v", got)
	}
	if !contains(got, "solo") {
		t.Error("the disjoint task must be dispatched")
	}
	// Skipped tasks are LEFT PENDING (never failed).
	if status(t, s, "live-conflict") != db.StatusPending || status(t, s, "pair-2") != db.StatusPending {
		t.Error("skipped tasks must remain pending, not failed")
	}
}

// TestRunOnceSkipsClaimedButNotYetLive proves the gap closure beyond the liveness-gated
// CheckOverlap: a task overlapping an ACTIVE worker that has no tmux window yet (a claim
// from a prior tick or a second scheduler instance, invisible to CheckOverlap) is still
// skipped, because the scheduler also consults active-claim footprints.
func TestRunOnceSkipsClaimedButNotYetLive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "newcomer", db.KindShip, []string{"internal/db/task.go"})

	// An active worker holding internal/db with NO window — claimed but not yet spawned.
	// CheckOverlap (liveness-gated) cannot see it; the active-claim check must.
	f := &fakeFleet{live: []db.Task{
		{ID: "claimed", Window: "", Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"internal/db"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if contains(f.dispatched(), "newcomer") {
		t.Error("a task overlapping a claimed-but-not-yet-windowed worker must be skipped")
	}
	if status(t, s, "newcomer") != db.StatusPending {
		t.Error("the skipped task must remain pending")
	}
}

// TestRunOnceMultiRepoCapacityIndependent proves per-repo capacity isolation: a full pool in
// one repo does not block dispatch in another.
func TestRunOnceMultiRepoCapacityIndependent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repoA, repoB := "/repoA", "/repoB"
	addPending(t, s, repoA, "a1", db.KindShip, []string{"pkg/a"})
	addPending(t, s, repoB, "b1", db.KindShip, []string{"pkg/b"})

	// repoA's single slot is taken; repoB's is free (Pool.Max is per-repo capacity).
	f := &fakeFleet{live: []db.Task{
		{ID: "wa", Window: "wk-wa", Project: repoA, Status: db.StatusActive, Worktree: "/wtA/0"},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 1}}
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched %d, want 1 (repoB free, repoA full)", n)
	}
	if !contains(f.dispatched(), "b1") || contains(f.dispatched(), "a1") {
		t.Fatalf("only repoB's task should dispatch, got %v", f.dispatched())
	}
	if status(t, s, "a1") != db.StatusPending {
		t.Error("repoA's task must stay pending (its pool is full)")
	}
}

// TestRunOnceCapacityLimit proves a tick never exceeds free worktree capacity: with one of
// two pool slots already held by a live worker, only one of several disjoint ready tasks is
// dispatched, the rest left pending.
func TestRunOnceCapacityLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "t1", db.KindShip, []string{"pkg/a"})
	addPending(t, s, repo, "t2", db.KindShip, []string{"pkg/b"})
	addPending(t, s, repo, "t3", db.KindShip, []string{"pkg/c"})

	// One slot of two already held by a live worker ⇒ exactly one free slot.
	f := &fakeFleet{live: []db.Task{
		{ID: "w1", Window: "wk-w1", Project: repo, Status: db.StatusActive, Worktree: "/wt/0"},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 2}}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched %d, want 1 (one free slot)", n)
	}
	// The first in order (t1) is dispatched; t2/t3 stay pending.
	if !contains(f.dispatched(), "t1") {
		t.Errorf("t1 should have taken the single free slot, got %v", f.dispatched())
	}
	if status(t, s, "t2") != db.StatusPending || status(t, s, "t3") != db.StatusPending {
		t.Error("over-capacity tasks must remain pending")
	}
}

// TestRunOnceNoCapacityIsNoOp proves a full pool dispatches nothing.
func TestRunOnceNoCapacityIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "t1", db.KindShip, []string{"pkg/a"})

	f := &fakeFleet{live: []db.Task{
		{ID: "w1", Window: "wk-w1", Project: repo, Status: db.StatusActive, Worktree: "/wt/0"},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 1}} // the one slot is taken

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || len(f.dispatched()) != 0 {
		t.Fatalf("a full pool must dispatch nothing, got n=%d dispatched=%v", n, f.dispatched())
	}
	if status(t, s, "t1") != db.StatusPending {
		t.Error("the undispatched task must remain pending")
	}
}

// TestRunOnceNoReadyWorkIsNoOp proves an empty backlog is a clean no-op.
func TestRunOnceNoReadyWorkIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || len(f.dispatched()) != 0 {
		t.Fatalf("no ready work must be a no-op, got n=%d dispatched=%v", n, f.dispatched())
	}
}

// TestRunOnceSkipsFootprintless proves a pending task with no declared footprint is left for
// the manager (not auto-dispatched), while a footprint-bearing sibling is dispatched.
func TestRunOnceSkipsFootprintless(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "bare", db.KindShip, nil)
	addPending(t, s, repo, "declared", db.KindShip, []string{"internal/db"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 || !contains(f.dispatched(), "declared") || contains(f.dispatched(), "bare") {
		t.Fatalf("only the declared-footprint task must dispatch, got n=%d dispatched=%v", n, f.dispatched())
	}
	if status(t, s, "bare") != db.StatusPending {
		t.Error("a footprint-less task must remain pending")
	}
}

// TestRunOnceCcExcluded proves ad-hoc cc tasks are never auto-dispatched.
func TestRunOnceCcExcluded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "cc-1", db.KindCC, []string{"internal/db"})
	addPending(t, s, repo, "ship-1", db.KindShip, []string{"internal/cli"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if contains(f.dispatched(), "cc-1") {
		t.Error("a cc task must never be auto-dispatched")
	}
	if !contains(f.dispatched(), "ship-1") {
		t.Error("the ship task should be dispatched")
	}
}

// TestRunOnceRevertsOnDispatchFailure proves a spawn failure after the claim reverts the
// task to pending (no phantom) with its lease cleared.
func TestRunOnceRevertsOnDispatchFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "boom", db.KindShip, []string{"internal/db"})

	f := &fakeFleet{spawnErr: map[string]error{"boom": errors.New("tmux unavailable")}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce must not surface a per-task dispatch failure: %v", err)
	}
	if n != 0 || len(f.dispatched()) != 0 {
		t.Fatalf("a failed dispatch must not count, got n=%d dispatched=%v", n, f.dispatched())
	}
	tk, _, _ := s.GetTask(ctx, "boom")
	if tk.Status != db.StatusPending {
		t.Errorf("status = %q, want pending after a reverted dispatch", tk.Status)
	}
	if tk.LeaseOwner != "" || tk.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on revert: %+v", tk)
	}
}

// TestRunOnceContextCancelStopsDispatch proves a cancelled context halts the tick before
// dispatching further work.
func TestRunOnceContextCancelStopsDispatch(t *testing.T) {
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "x", db.KindShip, []string{"pkg/a"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sc.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce on a cancelled ctx = %v, want context.Canceled", err)
	}
	if len(f.dispatched()) != 0 {
		t.Errorf("a cancelled tick must dispatch nothing, got %v", f.dispatched())
	}
}

// TestConcurrentRunOnceNoDoubleDispatch is the headline race test: two scheduler instances
// on the SAME db file (two Store handles, mirroring two processes) running a tick
// concurrently dispatch each ready task EXACTLY once — the atomic claim serializes the
// decision across instances, so no task is double-dispatched and none is lost.
func TestConcurrentRunOnceNoDoubleDispatch(t *testing.T) {
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
		// ids zero-padded so created/id order is well-defined; disjoint footprints.
		id := string(rune('a'+i/26)) + string(rune('a'+i%26))
		addPending(t, s1, repo, id, db.KindShip, []string{"pkg/" + id})
	}

	// Shared dispatch tally guarded by its own mutex.
	var mu sync.Mutex
	counts := map[string]int{}
	sink := func(id string) {
		mu.Lock()
		counts[id]++
		mu.Unlock()
	}

	mk := func(s *db.Store) *Scheduler {
		return &Scheduler{
			Store: s,
			Fleet: &fakeFleet{record: sink},
			Pool:  worktree.Pool{Max: 100}, // capacity never the limiter — isolate the claim race
		}
	}

	var wg sync.WaitGroup
	for _, s := range []*db.Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mk(s).RunOnce(ctx); err != nil {
				t.Errorf("concurrent RunOnce: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(counts) != n {
		t.Fatalf("dispatched %d distinct tasks, want %d", len(counts), n)
	}
	for id, c := range counts {
		if c != 1 {
			t.Errorf("task %s dispatched %d times, want exactly 1", id, c)
		}
	}
}

// TestRunCancelsCleanly proves the loop exits promptly with context.Canceled when its
// context is cancelled.
func TestRunCancelsCleanly(t *testing.T) {
	s := newStore(t) // empty backlog ⇒ the immediate first tick is an instant no-op
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
}

// --- land pass (phase 2a) ---------------------------------------------------------

// TestRunLandOnceLandsGatedDone proves the happy path: a done task that already carries a
// PASSING verdict is atomically claimed (a "lander:" lease) and handed to the land pipeline,
// and is counted landed.
func TestRunLandOnceLandsGatedDone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "ship1", db.KindShip, review.Pass)

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunLandOnce(ctx)
	if err != nil {
		t.Fatalf("RunLandOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("landed count = %d, want 1", n)
	}
	if got := f.landedIDs(); len(got) != 1 || got[0] != "ship1" {
		t.Fatalf("LandSet received %v, want [ship1]", got)
	}
	if lo := leaseOwner(t, s, "ship1"); lo != "lander:ship1" {
		t.Errorf("lease owner = %q, want lander:ship1 (the held land claim)", lo)
	}
}

// TestRunLandOnceSkipsUngatedDone is the headline SAFETY test: a done task with NO verdict and
// one with a BLOCKING verdict are never landed and never even claimed — the scheduler is a
// strict no-op for ungated work.
func TestRunLandOnceSkipsUngatedDone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "noverdict", db.KindShip, "")         // no verdict at all
	addDone(t, s, repo, "blocked", db.KindShip, review.Block) // a recorded BLOCK
	addDone(t, s, repo, "passing", db.KindShip, review.Pass)  // the control: this one lands

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunLandOnce(ctx)
	if err != nil {
		t.Fatalf("RunLandOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("landed count = %d, want 1 (only the passing task)", n)
	}
	got := f.landedIDs()
	if len(got) != 1 || got[0] != "passing" {
		t.Fatalf("LandSet received %v, want only [passing] — never the ungated tasks", got)
	}
	// The ungated tasks must not even be claimed (no land lease), so a later, legitimate land
	// is never blocked by a phantom claim.
	for _, id := range []string{"noverdict", "blocked"} {
		if lo := leaseOwner(t, s, id); lo == "lander:"+id {
			t.Errorf("ungated task %s was claimed (lease %q); it must be left untouched", id, lo)
		}
	}
}

// TestRunLandOnceExcludesCC proves ad-hoc cc sessions are never auto-landed even with a passing
// verdict, exactly as the dispatch pass never auto-dispatches them.
func TestRunLandOnceExcludesCC(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "ccsess", db.KindCC, review.Pass)
	addDone(t, s, repo, "ship1", db.KindShip, review.Pass)

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	if _, err := sc.RunLandOnce(ctx); err != nil {
		t.Fatalf("RunLandOnce: %v", err)
	}
	got := f.landedIDs()
	if len(got) != 1 || got[0] != "ship1" {
		t.Fatalf("LandSet received %v, want only [ship1] — cc excluded", got)
	}
}

// TestRunLandOnceReleasesClaimOnFailure proves a land that does not complete (a stale verdict,
// a refused merge, a lost fast-forward race) leaves the task safely done with its claim
// released, so a later tick can retry it.
func TestRunLandOnceReleasesClaimOnFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "stale", db.KindShip, review.Pass)

	f := &fakeFleet{landErr: map[string]error{"stale": errors.New("verdict no longer covers the head")}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunLandOnce(ctx)
	if err != nil {
		t.Fatalf("RunLandOnce must not surface a per-task land failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("a failed land must not count, got n=%d", n)
	}
	if got := f.landedIDs(); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("the task should have been attempted once, got %v", got)
	}
	if st := status(t, s, "stale"); st != db.StatusDone {
		t.Errorf("status = %q, want done after a failed land", st)
	}
	if lo := leaseOwner(t, s, "stale"); lo != "" {
		t.Errorf("land claim must be released after a failed land, got lease %q", lo)
	}
}

// TestRunLandOnceContextCancelStops proves a cancelled context halts the land pass cleanly,
// landing nothing and leaving no claim behind.
func TestRunLandOnceContextCancelStops(t *testing.T) {
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "ship1", db.KindShip, review.Pass)

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := sc.RunLandOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLandOnce on a cancelled ctx = (%d, %v), want context.Canceled", n, err)
	}
	if got := f.landedIDs(); len(got) != 0 {
		t.Errorf("a cancelled land pass must land nothing, got %v", got)
	}
	if lo := leaseOwner(t, s, "ship1"); lo == "lander:ship1" {
		t.Errorf("a cancelled land pass must leave no claim, got lease %q", lo)
	}
}

// TestConcurrentRunLandOnceNoDoubleLand is the land race test, mirroring the dispatch one: two
// scheduler instances on the SAME db file (two Store handles ⇒ two processes) running a land
// tick concurrently hand each gated done task to the land pipeline EXACTLY once — the atomic
// land claim serializes the decision across instances, so no task is double-landed and none is
// lost.
func TestConcurrentRunLandOnceNoDoubleLand(t *testing.T) {
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
		addDone(t, s1, repo, id, db.KindShip, review.Pass)
	}

	var mu sync.Mutex
	counts := map[string]int{}
	sink := func(id string) {
		mu.Lock()
		counts[id]++
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, s := range []*db.Store{s1, s2} {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := &Scheduler{Store: s, Fleet: &fakeFleet{record: sink}, Pool: worktree.Pool{Max: 100}}
			if _, err := sc.RunLandOnce(ctx); err != nil {
				t.Errorf("concurrent RunLandOnce: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(counts) != n {
		t.Fatalf("landed %d distinct tasks, want %d", len(counts), n)
	}
	for id, c := range counts {
		if c != 1 {
			t.Errorf("task %s landed %d times, want exactly 1", id, c)
		}
	}
}

// TestRunTickLandsOnlyWhenEnabled proves the land pass is OFF by default: a tick lands gated
// done work only when sc.Land is set, so enabling it is an explicit opt-in with no behavior
// change otherwise.
func TestRunTickLandsOnlyWhenEnabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "ship1", db.KindShip, review.Pass)

	// Land disabled (the zero value): the tick must not land.
	f := &fakeFleet{}
	off := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}} // Dispatch=false, Land=false
	off.runTick(ctx)
	if got := f.landedIDs(); len(got) != 0 {
		t.Fatalf("a tick with land disabled landed %v, want nothing", got)
	}

	// Land enabled: the tick lands the gated task.
	on := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Land: true}
	on.runTick(ctx)
	if got := f.landedIDs(); len(got) != 1 || got[0] != "ship1" {
		t.Fatalf("a tick with land enabled landed %v, want [ship1]", got)
	}
}

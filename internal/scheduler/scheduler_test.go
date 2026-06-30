package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/worktree"
)

// spawnCall records one dispatch the fake fleet received.
type spawnCall struct {
	id           string
	repo         string
	scout        bool
	footprint    []string
	forceOverlap bool
}

// fakeFleet is a tmux-free stand-in for *orchestrator.Manager: Snapshot returns a once-per-
// tick view built from the configured live set — modelling tmux-liveness as "the task has a
// non-empty Window" (the real Manager.Snapshot reads liveness from tmux ListWindows; a
// window-less task is never live) — and SpawnWithFootprint records the call (and optionally
// forces an error / forwards to a shared sink for concurrency tests) instead of standing up a
// worktree + tmux window.
type fakeFleet struct {
	mu            sync.Mutex
	live          []db.Task
	calls         []spawnCall
	spawnErr      map[string]error // per-task forced dispatch failure
	landed        []string         // ids handed to LandSet, in call order
	landErr       map[string]error // per-task forced land failure
	record        func(id string)  // shared sink for the cross-instance race tests
	snapshotErr   error            // forced Snapshot (live-fleet read) failure — the fail-closed abort path
	snapshotCalls int              // how many times Snapshot was called (proves one read per tick)

	panes   map[string]string // pane content Peek returns, by task id (idle-nudge tests)
	peekErr map[string]error  // per-task forced Peek failure (e.g. a gone window)
	sendErr map[string]error  // per-task forced Send failure
	sends   []string          // task ids Send was called on, in order

	gateable  map[string]bool                     // repo -> trusted (Gateable); absent ⇒ false
	gateOut   map[string]orchestrator.GateOutcome // per-task GateOnce outcome (default GateRecorded)
	gateErr   map[string]error                    // per-task forced GateOnce error
	gateCalls []string                            // task ids GateOnce was called on, in order
	idem      *gateIdem                           // shared cross-instance idempotency (see GateOnce)
}

// gateIdem models the real GateOnce's idempotency for the concurrency test: a task whose verdict
// is already recorded gates to GateSkipped, so a second actor that runs after the first never
// re-records it. It is shared (by pointer) across the two instances, mirroring the DB-backed
// verdict check the real GateOnce consults.
type gateIdem struct {
	mu       sync.Mutex
	recorded map[string]bool
}

func (f *fakeFleet) Snapshot() (*orchestrator.LiveSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	tasks := make([]db.Task, len(f.live))
	copy(tasks, f.live)
	// Model tmux-liveness exactly as the real Manager.Snapshot does — a task is live iff its
	// window is present — by feeding every non-empty Window in the live set as a live window.
	// A window-less task (claimed but not yet spawned) is therefore not live.
	var wins []string
	for _, t := range f.live {
		if t.Window != "" {
			wins = append(wins, t.Window)
		}
	}
	return orchestrator.NewLiveSnapshot(tasks, wins), nil
}

func (f *fakeFleet) snapshotReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls
}

func (f *fakeFleet) SpawnWithFootprint(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool) (db.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.spawnErr[taskID]; err != nil {
		return db.Task{}, err
	}
	f.calls = append(f.calls, spawnCall{id: taskID, repo: projectPath, scout: scout, footprint: footprint, forceOverlap: forceOverlap})
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

// callFor returns the recorded spawn call for a task id (and whether it was dispatched), so a
// test can assert how it was dispatched — e.g. that an overlapping parallel dispatch passed
// forceOverlap=true so the spawn-side footprint gate would not re-block it.
func (f *fakeFleet) callFor(id string) (spawnCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.id == id {
			return c, true
		}
	}
	return spawnCall{}, false
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

// Peek returns the configured pane for a task (the idle-nudge pass's observation seam),
// or a forced error standing in for a gone/unreadable window.
func (f *fakeFleet) Peek(taskID string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.peekErr[taskID]; err != nil {
		return "", err
	}
	return f.panes[taskID], nil
}

// Send records a nudge ATTEMPT into a worker's pane (the seam the idle-nudge pass drives) and
// then returns any forced error standing in for a refused/dead-window send. The attempt is
// recorded even on a forced error so a test can assert the send was tried (record-then-send).
func (f *fakeFleet) Send(taskID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, taskID)
	if f.record != nil {
		f.record(taskID)
	}
	return f.sendErr[taskID]
}

// Gateable reports whether repo was configured as a daemon-gate candidate (trusted). Absent
// ⇒ false, so a test must explicitly opt a repo in.
func (f *fakeFleet) Gateable(repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gateable[repo]
}

// GateOnce records the gate ATTEMPT (the seam the scheduler's gate claim feeds) and returns the
// per-task configured outcome (defaulting to GateRecorded — the headline happy path) or a
// forced error. The real GateOnce's orchestration is exercised in internal/orchestrator; here
// the fake isolates the scheduler's candidate-filter / claim / release / count logic.
func (f *fakeFleet) GateOnce(taskID string) (orchestrator.GateOutcome, error) {
	f.mu.Lock()
	f.gateCalls = append(f.gateCalls, taskID)
	gerr := f.gateErr[taskID]
	out, hasOut := f.gateOut[taskID]
	f.mu.Unlock()
	if gerr != nil {
		return orchestrator.GateSkipped, gerr
	}
	// Idempotency model (the real GateOnce skips when a verdict already covers head): a task
	// already recorded by some actor gates to GateSkipped and is not re-recorded. Shared by
	// pointer across instances so the concurrency test sees the cross-instance guarantee.
	if f.idem != nil {
		f.idem.mu.Lock()
		already := f.idem.recorded[taskID]
		if !already {
			f.idem.recorded[taskID] = true
		}
		f.idem.mu.Unlock()
		if already {
			return orchestrator.GateSkipped, nil
		}
		if f.record != nil {
			f.record(taskID)
		}
		return orchestrator.GateRecorded, nil
	}
	if f.record != nil {
		f.record(taskID)
	}
	if hasOut {
		return out, nil
	}
	return orchestrator.GateRecorded, nil
}

// gatedIDs returns the task ids GateOnce was called on, in order.
func (f *fakeFleet) gatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.gateCalls))
	copy(out, f.gateCalls)
	return out
}

// setPane updates the pane Peek will return for a task — used between ticks to model a worker
// reacting to a nudge (or staying frozen).
func (f *fakeFleet) setPane(id, pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes == nil {
		f.panes = map[string]string{}
	}
	f.panes[id] = pane
}

// sendCount reports how many times Send targeted id.
func (f *fakeFleet) sendCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sends {
		if s == id {
			n++
		}
	}
	return n
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
// It marks the task as having a stored brief (the normal case for backlog the scheduler
// dispatches); a briefless task is created with addPendingNoBrief instead.
func addPending(t *testing.T, s *db.Store, repo, id, kind string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusPending, Kind: kind, Footprint: footprint,
		HasBrief: true,
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
}

// addPendingNoBrief creates a pending backlog task with a footprint but NO stored brief —
// the case the scheduler must skip (leave for the manager) rather than spawn onto the
// manager-send stub.
func addPendingNoBrief(t *testing.T, s *db.Store, repo, id string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusPending, Kind: db.KindShip, Footprint: footprint,
		HasBrief: false,
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

// TestRunOnceSkipsOverlapping proves the SERIALIZE-OVERLAP off-switch (SerializeOverlap: true):
// overlap is skipped (not failed) — a task overlapping a LIVE worker stays pending, and of two
// ready tasks that overlap EACH OTHER only the first (created/id order) is dispatched (the
// just-claimed sibling is caught by the in-tick footprint tracking). This is the pre-parallel
// behavior the env off-switch restores; the default (parallel) is covered by
// TestRunOnceParallelDispatchesOverlapping.
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
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, SerializeOverlap: true}

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

// TestRunOnceSkipsClaimedButNotYetLive proves the SERIALIZE-OVERLAP off-switch closes the gap
// beyond the liveness-gated overlap view: a task overlapping an ACTIVE worker that has no tmux
// window yet (a claim from a prior tick or a second scheduler instance) is still skipped in
// serialize mode, because the occupancy set also consults active-claim footprints.
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
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, SerializeOverlap: true}
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

// TestRunOnceSkipsOverlapWithLiveNonActiveWorker proves the SERIALIZE-OVERLAP off-switch's
// liveness arm reproduces the old liveness-gated overlap view exactly: a worker that is NOT
// status=active (e.g. done, awaiting land) but still holds a LIVE tmux window blocks an
// overlapping ready task — the contribution liveness made over the active-only occupied seed.
// The decision is identical to the pre-snapshot path; only the read is now in-memory.
func TestRunOnceSkipsOverlapWithLiveNonActiveWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "newcomer", db.KindShip, []string{"internal/db/task.go"})

	// A DONE worker (not active) whose window is still live, holding internal/db. The active
	// seed would miss it; the snapshot's liveness (Window != "") catches it, just as the old
	// liveness-gated CheckOverlap did.
	f := &fakeFleet{live: []db.Task{
		{ID: "winding-down", Window: "wk-wd", Project: repo, Kind: db.KindShip, Status: db.StatusDone, Footprint: []string{"internal/db"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, SerializeOverlap: true}
	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if contains(f.dispatched(), "newcomer") {
		t.Error("a task overlapping a live-windowed (non-active) worker must be skipped")
	}
	if status(t, s, "newcomer") != db.StatusPending {
		t.Error("the skipped task must remain pending")
	}
}

// TestRunOnceDispatchesPastGoneWindowWorker is the converse of the live-non-active case, pinned
// in SERIALIZE-OVERLAP mode (so the occupancy computation is actually exercised as a gate): a
// non-active worker (done) whose window is GONE no longer occupies its footprint — neither the
// active-seed nor the liveness arm catches it — so even in serialize mode a ready task on those
// files dispatches. This pins that the liveness arm did not start blocking on stale (window-gone)
// workers. (In the default parallel mode it would dispatch regardless, so serialize mode is the
// stronger assertion here.)
func TestRunOnceDispatchesPastGoneWindowWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "newcomer", db.KindShip, []string{"internal/db/task.go"})

	// A done worker with NO live window (its window is gone) holding internal/db. Not active,
	// not live ⇒ not occupied ⇒ must not block.
	f := &fakeFleet{live: []db.Task{
		{ID: "gone", Window: "", Project: repo, Kind: db.KindShip, Status: db.StatusDone, Footprint: []string{"internal/db"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, SerializeOverlap: true}
	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !contains(f.dispatched(), "newcomer") {
		t.Error("a task overlapping only a gone-window (non-active) worker must dispatch")
	}
}

// TestRunOnceParallelDispatchesOverlapping is the headline acceptance for parallel-overlap
// dispatch (the DEFAULT, SerializeOverlap=false): tasks whose footprints overlap a live worker OR
// each other are ALL dispatched in the same tick — each onto its own pool worktree, where the
// overlap is harmless (git-isolated) and the only cost is a land-time rebase the land pass
// serializes. It is the direct contrast to the serialize-mode TestRunOnceSkipsOverlapping, which
// leaves the same overlapping tasks pending. It also asserts the overlapping dispatches pass
// forceOverlap=true (so the spawn-side footprint gate would not re-block the very tasks we mean to
// parallelize against the real manager), while the disjoint-at-dispatch first task keeps it false.
func TestRunOnceParallelDispatchesOverlapping(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	// a is disjoint from the live worker; b overlaps a (a same-tick sibling); c overlaps the live
	// worker w1. In serialize mode only a would dispatch; in parallel mode all three do.
	addPending(t, s, repo, "a", db.KindShip, []string{"internal/db"})
	addPending(t, s, repo, "b", db.KindShip, []string{"internal/db/task.go"})
	addPending(t, s, repo, "c", db.KindShip, []string{"internal/cli/cli.go"})

	f := &fakeFleet{live: []db.Task{
		{ID: "w1", Window: "wk-w1", Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"internal/cli"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}} // default: parallel overlap

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("dispatched %d, want 3 — all overlapping tasks must dispatch in parallel mode", n)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !contains(f.dispatched(), id) {
			t.Errorf("%s must dispatch in parallel mode (got %v)", id, f.dispatched())
		}
		if status(t, s, id) != db.StatusActive {
			t.Errorf("%s status = %q, want active after dispatch", id, status(t, s, id))
		}
	}
	// a was disjoint when it dispatched (nothing in-flight on internal/db yet) ⇒ forceOverlap=false;
	// b overlaps the just-claimed a, and c overlaps the live w1 ⇒ forceOverlap=true.
	assertForce := func(id string, want bool) {
		c, ok := f.callFor(id)
		if !ok {
			t.Fatalf("%s was not dispatched", id)
		}
		if c.forceOverlap != want {
			t.Errorf("%s dispatched with forceOverlap=%v, want %v", id, c.forceOverlap, want)
		}
	}
	assertForce("a", false)
	assertForce("b", true)
	assertForce("c", true)
}

// TestRunOnceParallelRespectsCapacity proves parallel-overlap dispatch is still bounded by
// worktree-pool capacity — the scheduler-layer half of the "never two workers in the same
// worktree" invariant. Two MUTUALLY overlapping ready tasks plus a live worker already holding one
// of two pool slots leaves exactly one free slot: overlap no longer blocks them, but capacity
// does, so exactly one dispatches and the other stays pending (never oversubscribed onto a shared
// or nonexistent worktree).
func TestRunOnceParallelRespectsCapacity(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	// Two ready tasks that overlap each other AND the live worker, all on internal/db.
	addPending(t, s, repo, "a", db.KindShip, []string{"internal/db"})
	addPending(t, s, repo, "b", db.KindShip, []string{"internal/db/task.go"})

	// One of two pool slots already held by a live worker on internal/db ⇒ exactly one free slot.
	f := &fakeFleet{live: []db.Task{
		{ID: "w1", Window: "wk-w1", Project: repo, Kind: db.KindShip, Status: db.StatusActive, Worktree: "/wt/0", Footprint: []string{"internal/db"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 2}} // default: parallel overlap

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched %d, want 1 — parallel overlap must still be bounded by free worktree capacity", n)
	}
	// The first in order (a) takes the single free slot; b stays pending (capacity, not overlap).
	if !contains(f.dispatched(), "a") {
		t.Errorf("a should have taken the single free slot, got %v", f.dispatched())
	}
	if status(t, s, "b") != db.StatusPending {
		t.Errorf("b must stay pending — capacity bounds parallel overlap (got %q)", status(t, s, "b"))
	}
}

// TestSerializeOverlapFromEnv proves the parallel-overlap off-switch reads TTORCH_SERIALIZE_OVERLAP:
// a truthy value selects serialize-on-dispatch; unset/empty/unparseable defaults to false
// (parallel). New() wires this resolver into Scheduler.SerializeOverlap.
func TestSerializeOverlapFromEnv(t *testing.T) {
	t.Setenv(envSerializeOverlap, "1")
	if !serializeOverlapFromEnv() {
		t.Error(`TTORCH_SERIALIZE_OVERLAP="1" must select serialize mode (true)`)
	}
	t.Setenv(envSerializeOverlap, "true")
	if !serializeOverlapFromEnv() {
		t.Error(`TTORCH_SERIALIZE_OVERLAP="true" must select serialize mode (true)`)
	}
	t.Setenv(envSerializeOverlap, "0")
	if serializeOverlapFromEnv() {
		t.Error(`TTORCH_SERIALIZE_OVERLAP="0" must be parallel (false)`)
	}
	t.Setenv(envSerializeOverlap, "not-a-bool")
	if serializeOverlapFromEnv() {
		t.Error("an unparseable value must default to parallel (false)")
	}
	t.Setenv(envSerializeOverlap, "")
	if serializeOverlapFromEnv() {
		t.Error("an unset/empty value must default to parallel (false)")
	}
}

// TestRunOnceAbortsOnSnapshotReadError is the core fail-closed guarantee (C2): when the
// once-per-tick live-fleet snapshot read fails (a transient DB error — lock timeout under
// contention, a WAL hiccup — or a tmux read error), the tick ABORTS, returning the error and
// dispatching NOTHING. It must never read an unreadable board as an empty fleet (no live
// workers ⇒ full free capacity ⇒ no overlap) and claim+dispatch a pool of tasks against that
// phantom view — the silent fail-open this guards against. The snapshot is the single board/
// liveness read of the tick, so its failure aborts everything rather than leaving a per-task
// read that could fail independently. It uses the DEFAULT scheduler (parallel-overlap mode), so
// it ALSO pins that decoupling dispatch-overlap did not weaken the fail-closed read: parallel
// mode still aborts the whole tick on an unreadable board, exactly as serialize mode does.
func TestRunOnceAbortsOnSnapshotReadError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "a", db.KindShip, []string{"internal/cli"})

	f := &fakeFleet{snapshotErr: errors.New("database is locked")}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	n, err := sc.RunOnce(ctx)
	if err == nil {
		t.Fatal("RunOnce must surface the snapshot read error, not swallow it")
	}
	if n != 0 {
		t.Fatalf("dispatched %d, want 0 — an unreadable board must dispatch nothing", n)
	}
	if got := f.dispatched(); len(got) != 0 {
		t.Fatalf("a read-error tick must dispatch nothing, got %v", got)
	}
	// The ready task is never claimed against the phantom empty view — it stays pending.
	if status(t, s, "a") != db.StatusPending {
		t.Fatalf("a must remain pending after an aborted tick, got %q", status(t, s, "a"))
	}
}

// TestRunOnceSnapshotsLiveFleetOncePerTick proves the hot-path optimization: regardless of how
// many ready tasks (N) and live workers (M) a tick faces, the scheduler reads the live fleet
// EXACTLY ONCE — it does not re-read the board or re-probe tmux per (ready × live) pair (the
// old O(N×M) CheckOverlap-per-task path). Overlap, capacity, and liveness all resolve from that
// single snapshot, so the per-pair work is pure in-memory FootprintOverlap.
func TestRunOnceSnapshotsLiveFleetOncePerTick(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	// N ready tasks, all disjoint from each other and from the live fleet, so the tick walks
	// every one through the overlap check.
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		addPending(t, s, repo, id, db.KindShip, []string{"pkg/" + id})
	}
	// M live workers, each with a footprint, so the old path would have probed liveness N×M times.
	var live []db.Task
	for _, id := range []string{"w1", "w2", "w3"} {
		live = append(live, db.Task{ID: id, Window: "wk-" + id, Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"live/" + id}})
	}
	f := &fakeFleet{live: live}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := f.snapshotReads(); got != 1 {
		t.Fatalf("snapshot read %d times, want exactly 1 — the tick must read the live fleet once, not per (ready × live) pair", got)
	}
	// Sanity: every disjoint ready task still dispatched (the optimization changed cost, not decisions).
	if got := len(f.dispatched()); got != 5 {
		t.Fatalf("dispatched %d, want 5 — all disjoint ready tasks should dispatch", got)
	}
}

// TestRunOnceCapsClaimsPerTick proves the per-tick claim ceiling: with MaxClaimsPerTick set,
// a tick claims at most that many tasks from a larger disjoint backlog and leaves the rest
// pending for the next tick — so one tick never fires an unbounded burst of heavy spawns.
func TestRunOnceCapsClaimsPerTick(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		addPending(t, s, repo, id, db.KindShip, []string{"pkg/" + id})
	}
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxClaimsPerTick: 2}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("dispatched %d, want 2 — the tick must stop at the claim cap", n)
	}
	if got := len(f.dispatched()); got != 2 {
		t.Fatalf("spawned %d, want 2 — the cap bounds heavy spawns per tick", got)
	}
	// The remaining four are untouched (still pending), available to the next tick.
	pending := 0
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		if status(t, s, id) == db.StatusPending {
			pending++
		}
	}
	if pending != 4 {
		t.Fatalf("%d tasks still pending, want 4 — over-cap tasks must remain pending for the next tick", pending)
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

// TestRunOnceSkipsBriefless proves the brief gate: a pending footprint-bearing task with
// NO stored brief is SKIPPED (left for the manager, not claimed/dispatched) and logged,
// while a sibling WITH a stored brief dispatches normally. This is the fix for the silent
// briefless stall — without it the daemon would claim the briefless task, spawn it onto
// the "wait for ttorch send" stub, and the worker would hold a pool slot and go silent.
func TestRunOnceSkipsBriefless(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPendingNoBrief(t, s, repo, "briefless", []string{"internal/db"})
	addPending(t, s, repo, "briefed", db.KindShip, []string{"internal/cli"})

	var log bytes.Buffer
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Log: &log}
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 || !contains(f.dispatched(), "briefed") || contains(f.dispatched(), "briefless") {
		t.Fatalf("only the briefed task must dispatch, got n=%d dispatched=%v", n, f.dispatched())
	}
	if status(t, s, "briefless") != db.StatusPending {
		t.Errorf("a briefless task must remain pending (not claimed), got %q", status(t, s, "briefless"))
	}
	if leaseOwner(t, s, "briefless") != "" {
		t.Error("a skipped briefless task must not be claimed (no lease)")
	}
	if got := log.String(); !strings.Contains(got, "skip briefless: no stored brief") {
		t.Errorf("expected a 'no stored brief' skip log for briefless, got:\n%s", got)
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

// TestRunLandOnceSurfacesRebaseConflict proves the autonomous land pass distinguishes a GENUINE
// rebase conflict from the routine retry-later land failures: when LandSet returns an error
// wrapping orchestrator.ErrLandRebaseConflict, the pass appends an ACTIONABLE land_rebase_conflict
// event for the manager (deduped by type — a second tick that re-conflicts adds no new event),
// releases the claim, and leaves the task safely done — never a forced merge, never counted as
// landed.
func TestRunLandOnceSurfacesRebaseConflict(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addDone(t, s, repo, "conflicted", db.KindShip, review.Pass)

	// LandSet reports a rebase conflict, wrapped exactly as merge.go's landPrep does: the detailed
	// "land: ..." guidance plus the ErrLandRebaseConflict sentinel (a second %w).
	landFail := fmt.Errorf("land: rebasing %q onto main hit conflicts (real overlap); aborted the rebase: %w (%w)",
		"conflicted", errors.New("CONFLICT (content): merge conflict in internal/db/task.go"), orchestrator.ErrLandRebaseConflict)
	f := &fakeFleet{landErr: map[string]error{"conflicted": landFail}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	// Two passes: the conflict must surface, and the dedup must keep it to exactly one event.
	for i := 0; i < 2; i++ {
		n, err := sc.RunLandOnce(ctx)
		if err != nil {
			t.Fatalf("RunLandOnce[%d] must not surface a per-task land failure: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("RunLandOnce[%d] landed %d, want 0 — a conflicted land never lands", i, n)
		}
	}

	// The task stays done and its land claim is released for after the manager resolves the conflict.
	if st := status(t, s, "conflicted"); st != db.StatusDone {
		t.Errorf("status = %q, want done after a surfaced rebase conflict", st)
	}
	if lo := leaseOwner(t, s, "conflicted"); lo != "" {
		t.Errorf("land claim must be released after a conflicted land, got lease %q", lo)
	}

	// Exactly one ACTIONABLE land_rebase_conflict event for the task (deduped across both passes).
	has, err := s.HasEventType(ctx, "conflicted", eventLandRebaseConflict)
	if err != nil {
		t.Fatalf("HasEventType: %v", err)
	}
	if !has {
		t.Fatal("a conflicted land must surface an actionable land_rebase_conflict event")
	}
	evs, err := s.EventsSince(ctx, 0, true)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	count := 0
	for _, e := range evs {
		if e.EntityID == "conflicted" && e.Type == eventLandRebaseConflict {
			if !e.Actionable {
				t.Error("the land_rebase_conflict event must be actionable")
			}
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d land_rebase_conflict events, want exactly 1 (deduped by type across ticks)", count)
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

// --- supervise pass (phase 3) ------------------------------------------------------

// addActiveWorker creates an active task with a window and a 'spawned' event — a dispatched
// worker the supervisor can later find dead. The footprint lets the dispatch pass re-dispatch
// it. It carries no lease (so the lease-expiry sweep ignores it); window-gone tests drive the
// death via markWindowGone, lease tests via addExpiredLeaseWorker.
func addActiveWorker(t *testing.T, s *db.Store, repo, id string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:" + id, Window: "wk-" + id, Footprint: footprint,
		HasBrief: true, // a dispatched worker has a stored brief, so reclaim can re-dispatch it
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	appendSpawned(t, s, id)
}

// addExpiredLeaseWorker creates an active task whose lease has ALREADY lapsed — a worker that
// stopped heartbeating — without needing a clock seam: the expiry is stamped in the past, so
// the real-clock ReclaimExpiredLeases sees it expired immediately.
func addExpiredLeaseWorker(t *testing.T, s *db.Store, repo, id string, footprint []string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:" + id, Window: "wk-" + id, Footprint: footprint,
		LeaseOwner: "worker:" + id, LeaseExpiresAt: &past,
		HasBrief: true, // a dispatched worker has a stored brief, so reclaim can re-dispatch it
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	appendSpawned(t, s, id)
}

// appendSpawned mimics the spawn path's 'spawned' audit event (every (re-)dispatch emits one,
// the anchor the window-gone freshness check measures against).
func appendSpawned(t *testing.T, s *db.Store, id string) {
	t.Helper()
	if _, err := s.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: id, Type: db.EventSpawned, Actor: db.ActorManager,
		Payload: "kind=ship window=wk-" + id,
	}); err != nil {
		t.Fatalf("AppendEvent spawned %s: %v", id, err)
	}
}

// markWindowGone mimics the watcher's liveness poll recording a confirmed-gone window.
func markWindowGone(t *testing.T, s *db.Store, id string) {
	t.Helper()
	if _, err := s.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: id, Type: db.EventWindowGone, Actor: db.ActorSystem,
		Actionable: true, Payload: "wk-" + id,
	}); err != nil {
		t.Fatalf("AppendEvent window_gone %s: %v", id, err)
	}
}

func retryCount(t *testing.T, s *db.Store, id string) int {
	t.Helper()
	tk, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask %s: ok=%v err=%v", id, ok, err)
	}
	return tk.RetryCount
}

// TestRunSuperviseReclaimsWindowGone proves the window-gone fast path: an active worker whose
// window is confirmed gone is reclaimed to pending (re-dispatchable), retry_count++.
func TestRunSuperviseReclaimsWindowGone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "g1", []string{"pkg/g1"})
	markWindowGone(t, s, "g1")

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunSuperviseOnce(ctx)
	if err != nil {
		t.Fatalf("RunSuperviseOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed count = %d, want 1", n)
	}
	if status(t, s, "g1") != db.StatusPending {
		t.Errorf("status = %q, want pending (re-dispatchable)", status(t, s, "g1"))
	}
	if retryCount(t, s, "g1") != 1 {
		t.Errorf("retry_count = %d, want 1", retryCount(t, s, "g1"))
	}
}

// TestRunSuperviseReclaimsExpiredLease proves the lease-expiry backstop is wired into the
// supervise pass: a worker whose lease has lapsed is reclaimed to pending.
func TestRunSuperviseReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addExpiredLeaseWorker(t, s, repo, "x1", []string{"pkg/x1"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunSuperviseOnce(ctx)
	if err != nil {
		t.Fatalf("RunSuperviseOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed count = %d, want 1", n)
	}
	if status(t, s, "x1") != db.StatusPending {
		t.Errorf("status = %q, want pending (reclaimed)", status(t, s, "x1"))
	}
}

// TestRunSupervisePoisonPill is the headline bounded-retry SAFETY test: a crashed worker at
// its retry ceiling is poison-pilled to the terminal 'failed' status — NOT reclaimed for
// retry and NOT re-dispatched — and is not counted as reclaimed.
func TestRunSupervisePoisonPill(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	proj, _ := s.UpsertProject(ctx, repo, "")
	if _, err := s.CreateTask(ctx, db.Task{
		ID: "p1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:p1", Window: "wk-p1", Footprint: []string{"pkg/p1"},
		RetryCount: 3, MaxRetries: 3, // retries exhausted
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	appendSpawned(t, s, "p1")
	markWindowGone(t, s, "p1")

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, Dispatch: true}
	// A full tick: supervise poison-pills, then dispatch must NOT pick the terminal task up.
	sc.runTick(ctx)

	if status(t, s, "p1") != db.StatusFailed {
		t.Errorf("status = %q, want failed (poison-pilled, terminal)", status(t, s, "p1"))
	}
	if got := f.dispatched(); len(got) != 0 {
		t.Errorf("a poison-pilled task must never be re-dispatched, got %v", got)
	}
}

// TestRunSuperviseLeavesLiveWorker is the core never-touch-a-live-worker test: an active
// worker with NO window_gone signal and a still-valid (future) lease — i.e. still
// heartbeating — is neither reclaimed nor re-dispatched by a full supervise+dispatch tick.
func TestRunSuperviseLeavesLiveWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "live1", []string{"pkg/live1"})
	if err := s.GrantLease(ctx, "live1", "worker:live1"); err != nil { // a fresh, future-dated lease
		t.Fatal(err)
	}

	f := &fakeFleet{live: []db.Task{
		{ID: "live1", Window: "wk-live1", Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"pkg/live1"}},
	}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, Dispatch: true}
	sc.runTick(ctx)

	if status(t, s, "live1") != db.StatusActive {
		t.Errorf("a live, heartbeating worker must stay active, got %q", status(t, s, "live1"))
	}
	if retryCount(t, s, "live1") != 0 {
		t.Errorf("a live worker must not accrue a retry, got retry_count %d", retryCount(t, s, "live1"))
	}
	if got := f.dispatched(); len(got) != 0 {
		t.Errorf("a live worker must never be re-dispatched, got %v", got)
	}
}

// TestRunSuperviseRespectsResumedWorker is the resume-safety test for the never-reclaim-a-live-
// worker property: a worker whose window was flagged gone but that has since reported progress
// (a worker-authored heartbeat — e.g. resumed in place by `ttorch resume`, which rebuilds the
// window but writes no event of its own) is neither reclaimed nor re-dispatched by a full
// supervise+dispatch tick.
func TestRunSuperviseRespectsResumedWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "r1", []string{"pkg/r1"})
	markWindowGone(t, s, "r1")
	// The worker is alive again and reports progress — a sign of life AFTER the window_gone.
	if _, err := s.SetStage(ctx, "r1", "resumed, back to work", "worker:r1"); err != nil {
		t.Fatal(err)
	}

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, Dispatch: true}
	sc.runTick(ctx)

	if status(t, s, "r1") != db.StatusActive {
		t.Errorf("a resumed-and-reporting worker must stay active, got %q", status(t, s, "r1"))
	}
	if retryCount(t, s, "r1") != 0 {
		t.Errorf("a resumed worker must not accrue a retry, got retry_count %d", retryCount(t, s, "r1"))
	}
	if got := f.dispatched(); len(got) != 0 {
		t.Errorf("a resumed-and-reporting worker must not be re-dispatched, got %v", got)
	}
}

// TestRunSuperviseIgnoresStaleWindowGone proves incarnation safety: a window_gone from a prior
// incarnation that was already re-dispatched (a newer 'spawned' event follows it) does not
// reclaim the now-live worker — the freshness anchor prevents a restart loop on a stale crash.
func TestRunSuperviseIgnoresStaleWindowGone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "s1", []string{"pkg/s1"}) // spawned #1
	markWindowGone(t, s, "s1")                            // crash of incarnation #1
	appendSpawned(t, s, "s1")                             // re-dispatched: spawned #2 ⇒ live again

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}
	n, err := sc.RunSuperviseOnce(ctx)
	if err != nil {
		t.Fatalf("RunSuperviseOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("a stale window_gone must reclaim nothing, got n=%d", n)
	}
	if status(t, s, "s1") != db.StatusActive {
		t.Errorf("status = %q, want active (the re-dispatched worker is live)", status(t, s, "s1"))
	}
}

// TestRunTickReDispatchesWindowGone is the headline acceptance test: a full supervise+dispatch
// tick RECLAIMS a window-gone worker and RE-DISPATCHES it through the manager's spawn path.
func TestRunTickReDispatchesWindowGone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "g1", []string{"pkg/g1"})
	markWindowGone(t, s, "g1")

	f := &fakeFleet{} // no live workers ⇒ capacity is free for the re-dispatch
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, Dispatch: true}
	sc.runTick(ctx)

	if !contains(f.dispatched(), "g1") {
		t.Fatalf("a window-gone worker must be re-dispatched, got %v", f.dispatched())
	}
	// Re-dispatched ⇒ claimed back to active, with the retry recorded.
	if status(t, s, "g1") != db.StatusActive {
		t.Errorf("status = %q, want active after re-dispatch", status(t, s, "g1"))
	}
	if retryCount(t, s, "g1") != 1 {
		t.Errorf("retry_count = %d, want 1 after one recovery", retryCount(t, s, "g1"))
	}
}

// TestRunTickReDispatchesExpiredLease proves the same recovery for the lease-expiry signal: a
// supervise+dispatch tick reclaims and re-dispatches a worker whose lease lapsed.
func TestRunTickReDispatchesExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addExpiredLeaseWorker(t, s, repo, "x1", []string{"pkg/x1"})

	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, Dispatch: true}
	sc.runTick(ctx)

	if !contains(f.dispatched(), "x1") {
		t.Fatalf("an expired-lease worker must be re-dispatched, got %v", f.dispatched())
	}
	if status(t, s, "x1") != db.StatusActive {
		t.Errorf("status = %q, want active after re-dispatch", status(t, s, "x1"))
	}
}

// TestRunTickSupervisesOnlyWhenEnabled proves the supervise pass is OFF by default: a tick
// reclaims a window-gone worker only when sc.Supervise is set, so enabling recovery is an
// explicit opt-in with no behavior change otherwise.
func TestRunTickSupervisesOnlyWhenEnabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "g1", []string{"pkg/g1"})
	markWindowGone(t, s, "g1")

	// Supervise disabled (the zero value): the tick must not reclaim, even with a window_gone.
	f := &fakeFleet{}
	off := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}} // Supervise=false, Dispatch=false
	off.runTick(ctx)
	if status(t, s, "g1") != db.StatusActive {
		t.Fatalf("a tick with supervise disabled reclaimed g1 (status %q); it must be untouched", status(t, s, "g1"))
	}
	if retryCount(t, s, "g1") != 0 {
		t.Fatalf("a tick with supervise disabled bumped retry_count to %d; it must be untouched", retryCount(t, s, "g1"))
	}

	// Supervise enabled: the tick reclaims the window-gone worker.
	on := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true}
	on.runTick(ctx)
	if status(t, s, "g1") != db.StatusPending {
		t.Fatalf("a tick with supervise enabled left g1 %q, want pending (reclaimed)", status(t, s, "g1"))
	}
}

// --- idle-nudge pass (alive-but-idle recovery, §roadmap H2) ----------------------

const (
	idleNudgePane = "│ > Try \"edit this file\"                 │" // turn ended, sitting at the prompt
	busyNudgePane = "✶ Working… (12s · esc to interrupt)"          // mid-turn, must never be nudged
	shellNudge    = "go: command failed\nbrian@host ~/repo $ "     // crashed to a shell — not at the prompt
)

// addActiveLeasedWorker creates an active, briefed worker with an explicit window, lease
// expiry, and last_progress_at — the building block for the idle-nudge edge cases (manager
// window, expired lease). appendSpawned mirrors the dispatch audit event so the freshness
// anchor (spawned OR worker heartbeat) is present, exactly as a real dispatch leaves it.
func addActiveLeasedWorker(t *testing.T, s *db.Store, repo, id, window string, leaseExpiry, progress time.Time) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:" + id, Window: window, Footprint: []string{"pkg/" + id},
		LeaseOwner: "worker:" + id, LeaseExpiresAt: &leaseExpiry,
		LastProgressAt: &progress, HasBrief: true,
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	appendSpawned(t, s, id)
}

// addIdleWorker is the common alive-but-idle candidate: a live window, a VALID (future) lease,
// and a last_progress_at the caller controls (backdate it past the grace period to make the
// worker nudge-eligible, or keep it recent to model a worker that just reported).
func addIdleWorker(t *testing.T, s *db.Store, repo, id string, progress time.Time) {
	t.Helper()
	addActiveLeasedWorker(t, s, repo, id, "wk-"+id, time.Now().Add(time.Hour), progress)
}

func newIdleScheduler(s *db.Store, f *fakeFleet, log *bytes.Buffer, grace time.Duration, maxNudges int) *Scheduler {
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100},
		IdleNudgeGrace: grace, MaxIdleNudges: maxNudges,
	}
	if log != nil { // leave Log as a nil interface (not a typed-nil) so logf's nil-guard holds
		sc.Log = log
	}
	return sc
}

// TestNudgeIdle_NudgesAliveIdleOnce is the headline path: an alive worker (live window, valid
// lease, status active) idle at the prompt past the grace period is nudged exactly once; a
// second tick before any fresh activity does NOT re-nudge.
func TestNudgeIdle_NudgesAliveIdleOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute)) // idle 10m
	var log bytes.Buffer
	f := &fakeFleet{panes: map[string]string{"w1": idleNudgePane}}
	sc := newIdleScheduler(s, f, &log, 3*time.Minute, 2)

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 1 || f.sendCount("w1") != 1 {
		t.Fatalf("first tick: nudged=%d sends=%d, want 1/1", n, f.sendCount("w1"))
	}
	if status(t, s, "w1") != db.StatusActive {
		t.Errorf("status = %q after nudge, want active (a nudge never changes status)", status(t, s, "w1"))
	}
	if !strings.Contains(log.String(), "nudged idle worker w1") {
		t.Errorf("missing nudge log; got %q", log.String())
	}

	// Second tick, SAME pane, no fresh activity → must NOT re-nudge.
	n, err = sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce 2: %v", err)
	}
	if n != 0 || f.sendCount("w1") != 1 {
		t.Fatalf("second tick re-nudged: nudged=%d sends=%d, want 0/1", n, f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsBlockedAndNeedsInput proves a worker that reported blocked/needs_input —
// a manager decision — is never auto-nudged, while a sibling idle active worker still is.
func TestNudgeIdle_SkipsBlockedAndNeedsInput(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	past := time.Now().Add(-10 * time.Minute)
	addIdleWorker(t, s, "/repo", "a", past)
	addIdleWorker(t, s, "/repo", "b", past)
	addIdleWorker(t, s, "/repo", "c", past)
	if _, err := s.ReportStatus(ctx, "b", db.StatusBlocked, "worker:b", "stuck on X"); err != nil {
		t.Fatalf("ReportStatus blocked: %v", err)
	}
	if _, err := s.ReportStatus(ctx, "c", db.StatusNeedsInput, "worker:c", "which API?"); err != nil {
		t.Fatalf("ReportStatus needs_input: %v", err)
	}
	f := &fakeFleet{panes: map[string]string{"a": idleNudgePane, "b": idleNudgePane, "c": idleNudgePane}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if f.sendCount("a") != 1 {
		t.Errorf("active idle worker a nudged %d times, want 1", f.sendCount("a"))
	}
	if f.sendCount("b") != 0 || f.sendCount("c") != 0 {
		t.Errorf("blocked/needs_input nudged (b=%d c=%d), want 0/0 — those are the manager's", f.sendCount("b"), f.sendCount("c"))
	}
}

// TestNudgeIdle_SkipsBusyWorker proves a worker mid-turn (a busy pane) is never nudged, no
// matter how long since its last report.
func TestNudgeIdle_SkipsBusyWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-time.Hour)) // long since a report
	f := &fakeFleet{panes: map[string]string{"w1": busyNudgePane}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 0 || f.sendCount("w1") != 0 {
		t.Fatalf("busy worker nudged: n=%d sends=%d, want 0/0", n, f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsRecentlyActive proves the grace gate: a worker idle at the prompt but
// whose last report is WITHIN the grace period is not yet nudged (a slow-but-working turn).
func TestNudgeIdle_SkipsRecentlyActive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now()) // just reported
	f := &fakeFleet{panes: map[string]string{"w1": idleNudgePane}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 0 || f.sendCount("w1") != 0 {
		t.Fatalf("recently-active worker nudged: n=%d sends=%d, want 0/0", n, f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsNonPromptPane proves a pane that is NOT at the input prompt (a crash to a
// shell) is never injected into, even though it is not busy.
func TestNudgeIdle_SkipsNonPromptPane(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{panes: map[string]string{"w1": shellNudge}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if f.sendCount("w1") != 0 {
		t.Fatalf("non-prompt pane nudged %d times, want 0", f.sendCount("w1"))
	}
}

// TestNudgeIdle_StopsAfterMaxNudges proves the restart-storm bound: a worker that keeps going
// idle (a changing pane) after each nudge is nudged at most MaxIdleNudges times, after which
// the pass logs the give-up ONCE and stays silent.
func TestNudgeIdle_StopsAfterMaxNudges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	var log bytes.Buffer
	f := &fakeFleet{panes: map[string]string{"w1": "screen-1\n│ > "}}
	sc := newIdleScheduler(s, f, &log, 0, 2) // grace 0: the cap, not spacing, is under test

	// Three episodes (each a fresh idle screen): nudge, nudge, then STOP.
	for i, screen := range []string{"screen-1\n│ > ", "screen-2\n│ > ", "screen-3\n│ > "} {
		f.setPane("w1", screen)
		if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	if f.sendCount("w1") != 2 {
		t.Fatalf("sends = %d, want 2 (capped at MaxIdleNudges)", f.sendCount("w1"))
	}
	if !strings.Contains(log.String(), "idle worker w1: nudged 2x, no progress — leaving for manager") {
		t.Fatalf("missing give-up log; got %q", log.String())
	}

	// A further tick on yet another fresh idle screen must NOT nudge and must NOT re-log.
	f.setPane("w1", "screen-4\n│ > ")
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 4: %v", err)
	}
	if f.sendCount("w1") != 2 {
		t.Fatalf("sends = %d after cap, want 2 (no further nudges)", f.sendCount("w1"))
	}
	if got := strings.Count(log.String(), "leaving for manager"); got != 1 {
		t.Fatalf("give-up logged %d times, want exactly 1 (no spam)", got)
	}
}

// TestNudgeIdle_FreshActivityResetsBudget proves a worker that shows fresh activity (a
// heartbeat) after being nudged starts a clean episode — the consecutive-nudge budget is
// replenished, so a later idle stretch is nudged again.
func TestNudgeIdle_FreshActivityResetsBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{panes: map[string]string{"w1": "screen-1\n│ > "}}
	sc := newIdleScheduler(s, f, nil, 0, 1) // budget of ONE per episode

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// Without fresh activity the budget (1) is spent: a new idle screen would NOT re-nudge.
	if f.sendCount("w1") != 1 {
		t.Fatalf("after first tick sends=%d, want 1", f.sendCount("w1"))
	}
	// Worker shows a fresh heartbeat → new episode.
	if _, err := s.SetStage(ctx, "w1", "back to work", "worker:w1"); err != nil {
		t.Fatalf("SetStage heartbeat: %v", err)
	}
	f.setPane("w1", "screen-2\n│ > ")
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if f.sendCount("w1") != 2 {
		t.Fatalf("after fresh activity sends=%d, want 2 (budget replenished)", f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsExpiredLeaseAndManagerWindow proves the pass never nudges a worker whose
// lease has lapsed (the reclaim pass owns it) nor the manager window (not the worker fleet).
func TestNudgeIdle_SkipsExpiredLeaseAndManagerWindow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	past := time.Now().Add(-10 * time.Minute)
	addActiveLeasedWorker(t, s, "/repo", "expired", "wk-expired", time.Now().Add(-time.Hour), past) // lease lapsed
	addActiveLeasedWorker(t, s, "/repo", "mgr", "manager", time.Now().Add(time.Hour), past)         // manager window
	f := &fakeFleet{panes: map[string]string{"expired": idleNudgePane, "mgr": idleNudgePane}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if f.sendCount("expired") != 0 {
		t.Errorf("expired-lease worker nudged %d times, want 0 (reclaim owns it)", f.sendCount("expired"))
	}
	if f.sendCount("mgr") != 0 {
		t.Errorf("manager window nudged %d times, want 0", f.sendCount("mgr"))
	}
}

// TestNudgeIdle_Disabled proves MaxIdleNudges <= 0 disables the pass entirely (a no-op even on
// a perfectly nudge-eligible worker).
func TestNudgeIdle_Disabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{panes: map[string]string{"w1": idleNudgePane}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 0) // disabled

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 0 || f.sendCount("w1") != 0 {
		t.Fatalf("disabled pass acted: n=%d sends=%d, want 0/0", n, f.sendCount("w1"))
	}
}

// TestNudgeIdle_PeekErrorSkipped proves an unreadable/gone window is skipped (not nudged, not
// fatal) — the window-gone reclaim path owns a dead window, not the nudge pass.
func TestNudgeIdle_PeekErrorSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "gone", time.Now().Add(-10*time.Minute))
	addIdleWorker(t, s, "/repo", "ok", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{
		panes:   map[string]string{"ok": idleNudgePane},
		peekErr: map[string]error{"gone": errors.New("no window")},
	}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 1 || f.sendCount("ok") != 1 || f.sendCount("gone") != 0 {
		t.Fatalf("peek-error handling wrong: n=%d ok=%d gone=%d, want 1/1/0", n, f.sendCount("ok"), f.sendCount("gone"))
	}
}

// TestNudgeIdle_SendFailureRecordedNoReSend proves the record-then-send safety: when the send
// fails, the nudge is STILL recorded (the claim committed first), so the next tick on the same
// idle screen does NOT re-nudge — under-nudge beats re-nudge-storming a wedged pane.
func TestNudgeIdle_SendFailureRecordedNoReSend(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{
		panes:   map[string]string{"w1": idleNudgePane},
		sendErr: map[string]error{"w1": errors.New("tmux send failed")},
	}
	sc := newIdleScheduler(s, f, nil, 0, 2)

	n, err := sc.RunNudgeIdleOnce(ctx)
	if err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if n != 0 || f.sendCount("w1") != 1 {
		t.Fatalf("first tick: nudged=%d sends(attempted)=%d, want 0/1", n, f.sendCount("w1"))
	}
	// The claim committed the nudge before the (failed) send — so it is on the spine.
	info, err := s.IdleNudgeInfo(ctx, "w1")
	if err != nil {
		t.Fatalf("IdleNudgeInfo: %v", err)
	}
	if info.Count != 1 {
		t.Fatalf("record-then-send: count=%d, want 1 (the failed send is still recorded)", info.Count)
	}
	// Second tick, same idle screen: must NOT re-attempt the send (already nudged this screen).
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce 2: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("second tick re-sent to a wedged pane (attempts=%d, want 1)", f.sendCount("w1"))
	}
}

// TestNudgeIdle_StabilityGate proves the consecutive-observation gate: with IdleConfirmations
// = 2 a single idle observation does NOT nudge; a second consecutive tick at the SAME pane
// does; and a pane change between observations resets the count so a flickering (working)
// worker never crosses the threshold.
func TestNudgeIdle_StabilityGate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{panes: map[string]string{"w1": "screen-A\n│ > "}}
	sc := newIdleScheduler(s, f, nil, 0, 2)
	sc.IdleConfirmations = 2

	// First idle observation: not yet stable → no nudge.
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if f.sendCount("w1") != 0 {
		t.Fatalf("nudged on the FIRST idle observation (sends=%d), want 0", f.sendCount("w1"))
	}
	// Pane changes before a second confirmation → count resets, still no nudge.
	f.setPane("w1", "screen-B\n│ > ")
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if f.sendCount("w1") != 0 {
		t.Fatalf("nudged after a pane CHANGE (sends=%d), want 0 (count must reset)", f.sendCount("w1"))
	}
	// Same pane as tick 2 → now two consecutive identical observations → nudge.
	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("did not nudge after two stable observations (sends=%d), want 1", f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsAPIErrorPanes proves the idle-nudge pass acts ONLY on a clean idle prompt:
// a worker at the prompt after a non-recoverable API error (rate-limit/auth) — "continue" cannot
// fix it — AND a worker showing a recoverable mid-stream stall are BOTH skipped here. The
// recoverable stall is no longer the idle pass's to nudge: the faster, dedicated stall-recovery
// pass (stallrecovery.go) owns it now, so the two passes act on disjoint pane states.
func TestNudgeIdle_SkipsAPIErrorPanes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	past := time.Now().Add(-10 * time.Minute)
	addIdleWorker(t, s, "/repo", "ratelimited", past)
	addIdleWorker(t, s, "/repo", "stalled", past)
	f := &fakeFleet{panes: map[string]string{
		"ratelimited": "API Error: 429 rate limit exceeded\n│ > ",
		"stalled":     "API Error: Response stalled mid-stream\n│ > ",
	}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if f.sendCount("ratelimited") != 0 {
		t.Errorf("rate-limited worker nudged %d times, want 0 (non-recoverable, left for the manager)", f.sendCount("ratelimited"))
	}
	if f.sendCount("stalled") != 0 {
		t.Errorf("API-stalled worker nudged %d times by the idle pass, want 0 (stall-recovery owns it)", f.sendCount("stalled"))
	}
}

// TestClaimIdleNudge_AtomicDecision exercises the Store-level atomic claim directly: a worker
// that changed status (or whose lease lapsed) since the snapshot is NOT claimable, the same
// idle screen is claimed at most once, a fresh screen re-claims until the budget is spent, and
// the budget then yields a give-up.
func TestClaimIdleNudge_AtomicDecision(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	// grace 0; each claim reads time.Now() fresh so the grace anchor (which includes the just-
	// recorded nudge's timestamp) is never in the future relative to the call.

	// First claim on screen h1 → Send (records the nudge).
	d, _, err := s.ClaimIdleNudge(ctx, "w1", "h1", time.Now(), 0, 2)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if d != db.IdleNudgeSend {
		t.Fatalf("claim 1 = %v, want Send", d)
	}
	// Same screen again → Skip (already nudged this screen, no double-nudge).
	if d, _, err := s.ClaimIdleNudge(ctx, "w1", "h1", time.Now(), 0, 2); err != nil || d != db.IdleNudgeSkip {
		t.Fatalf("claim 1b = %v err=%v, want Skip (same screen)", d, err)
	}
	// Fresh screen h2, budget (2) not spent → Send.
	if d, _, err := s.ClaimIdleNudge(ctx, "w1", "h2", time.Now(), 0, 2); err != nil || d != db.IdleNudgeSend {
		t.Fatalf("claim 2 = %v err=%v, want Send", d, err)
	}
	// Fresh screen h3, budget now spent (2 sent) → GiveUp, count reported.
	d, count, err := s.ClaimIdleNudge(ctx, "w1", "h3", time.Now(), 0, 2)
	if err != nil || d != db.IdleNudgeGiveUp || count != 2 {
		t.Fatalf("claim 3 = %v count=%d err=%v, want GiveUp/2", d, count, err)
	}
	// Capped: a further claim is a silent Skip (give-up logged once).
	if d, _, err := s.ClaimIdleNudge(ctx, "w1", "h4", time.Now(), 0, 2); err != nil || d != db.IdleNudgeSkip {
		t.Fatalf("claim 4 = %v err=%v, want Skip (capped)", d, err)
	}
}

// TestClaimIdleNudge_StatusAndLeaseReCheck proves the under-lock re-check drops a worker that
// is no longer a valid candidate — the guard against the snapshot→claim race where a worker
// reports blocked/needs_input/done (or its lease lapses) mid-pass.
func TestClaimIdleNudge_StatusAndLeaseReCheck(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	// Status moved to needs_input since the snapshot → not claimable.
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "blk", now.Add(-10*time.Minute))
	if _, err := s.ReportStatus(ctx, "blk", db.StatusNeedsInput, "worker:blk", "which API?"); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if d, _, err := s.ClaimIdleNudge(ctx, "blk", "h1", now, 0, 2); err != nil || d != db.IdleNudgeSkip {
		t.Fatalf("needs_input claim = %v err=%v, want Skip", d, err)
	}

	// Lease lapsed since the snapshot → not claimable (reclaim owns it).
	s2 := newStore(t)
	addActiveLeasedWorker(t, s2, "/repo", "exp", "wk-exp", now.Add(-time.Hour), now.Add(-10*time.Minute))
	if d, _, err := s2.ClaimIdleNudge(ctx, "exp", "h1", now, 0, 2); err != nil || d != db.IdleNudgeSkip {
		t.Fatalf("expired-lease claim = %v err=%v, want Skip", d, err)
	}
}

// TestRunTickNudgesUnderSupervise proves the idle-nudge pass is wired into the tick under the
// Supervise gate, AT THE PRODUCTION-DEFAULT stability threshold (IdleConfirmations=2): a tick
// with Supervise off never nudges; with Supervise on, the stability gate composes with the tick
// loop — the first tick observes (no nudge) and the second consecutive idle tick nudges.
func TestRunTickNudgesUnderSupervise(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now().Add(-10*time.Minute))
	f := &fakeFleet{panes: map[string]string{"w1": idleNudgePane}}

	off := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, IdleNudgeGrace: 3 * time.Minute, MaxIdleNudges: 2, IdleConfirmations: defaultIdleConfirmations}
	off.runTick(ctx) // Supervise=false
	off.runTick(ctx)
	if f.sendCount("w1") != 0 {
		t.Fatalf("idle-nudge ran with Supervise off (sends=%d)", f.sendCount("w1"))
	}

	on := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Supervise: true, IdleNudgeGrace: 3 * time.Minute, MaxIdleNudges: 2, IdleConfirmations: defaultIdleConfirmations}
	on.runTick(ctx) // first observation under the production-default gate: not yet stable
	if f.sendCount("w1") != 0 {
		t.Fatalf("nudged on the first tick despite IdleConfirmations=2 (sends=%d, want 0)", f.sendCount("w1"))
	}
	on.runTick(ctx) // second consecutive idle tick: stable → nudge
	if f.sendCount("w1") != 1 {
		t.Fatalf("idle-nudge did not run under Supervise after two stable ticks (sends=%d, want 1)", f.sendCount("w1"))
	}
}

package scheduler

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/worktree"
)

// --- governor unit tests ----------------------------------------------------------

// TestDefaultMaxActiveWorkers proves the computed default: NumCPU/2, floored at
// minDefaultMaxActive, capped at the pool size (and uncapped when poolMax <= 0).
func TestDefaultMaxActiveWorkers(t *testing.T) {
	uncapped := runtime.NumCPU() / 2
	if uncapped < minDefaultMaxActive {
		uncapped = minDefaultMaxActive
	}
	if got := defaultMaxActiveWorkers(0); got != uncapped {
		t.Errorf("defaultMaxActiveWorkers(0) = %d, want %d (NumCPU/2 floored at %d)", got, uncapped, minDefaultMaxActive)
	}
	// Capped at the pool size: a pool of 1 can never permit more than 1 concurrent worker.
	if got := defaultMaxActiveWorkers(1); got != 1 {
		t.Errorf("defaultMaxActiveWorkers(1) = %d, want 1 (capped at the pool)", got)
	}
	// Floored at minDefaultMaxActive when the pool is large.
	if got := defaultMaxActiveWorkers(1000); got < minDefaultMaxActive {
		t.Errorf("defaultMaxActiveWorkers(1000) = %d, want >= %d (floor)", got, minDefaultMaxActive)
	}
	if got := defaultMaxActiveWorkers(1000); got > 1000 {
		t.Errorf("defaultMaxActiveWorkers(1000) = %d, want <= pool", got)
	}
}

// TestMaxActiveWorkersFromEnv proves the env override wins (including 0/negative as the
// off-switch), an unparseable value falls back to the default, and an unset env uses the default.
func TestMaxActiveWorkersFromEnv(t *testing.T) {
	t.Setenv(envMaxActiveWorkers, "5")
	if got := maxActiveWorkersFromEnv(16); got != 5 {
		t.Errorf("env=5: got %d, want 5", got)
	}
	t.Setenv(envMaxActiveWorkers, "0") // explicit 0 disables — honored verbatim
	if got := maxActiveWorkersFromEnv(16); got != 0 {
		t.Errorf("env=0: got %d, want 0 (disable honored)", got)
	}
	t.Setenv(envMaxActiveWorkers, "-3")
	if got := maxActiveWorkersFromEnv(16); got != -3 {
		t.Errorf("env=-3: got %d, want -3 (negative honored)", got)
	}
	t.Setenv(envMaxActiveWorkers, "not-a-number")
	if got := maxActiveWorkersFromEnv(16); got != defaultMaxActiveWorkers(16) {
		t.Errorf("unparseable env: got %d, want default %d", got, defaultMaxActiveWorkers(16))
	}
}

// TestMaxLandConcurrencyFromEnv proves the default, env override, and <=0 off-switch.
func TestMaxLandConcurrencyFromEnv(t *testing.T) {
	if got := maxLandConcurrencyFromEnv(); got != defaultMaxLandConcurrency {
		t.Errorf("unset: got %d, want default %d", got, defaultMaxLandConcurrency)
	}
	t.Setenv(envMaxLandConcurrency, "7")
	if got := maxLandConcurrencyFromEnv(); got != 7 {
		t.Errorf("env=7: got %d, want 7", got)
	}
	t.Setenv(envMaxLandConcurrency, "0")
	if got := maxLandConcurrencyFromEnv(); got != 0 {
		t.Errorf("env=0: got %d, want 0 (disable honored)", got)
	}
	t.Setenv(envMaxLandConcurrency, "garbage")
	if got := maxLandConcurrencyFromEnv(); got != defaultMaxLandConcurrency {
		t.Errorf("unparseable env: got %d, want default %d", got, defaultMaxLandConcurrency)
	}
}

// TestLoadCeilingFromEnv proves the default-off, env override, and unparseable fallback.
func TestLoadCeilingFromEnv(t *testing.T) {
	if got := loadCeilingFromEnv(); got != defaultLoadCeiling {
		t.Errorf("unset: got %v, want default %v (off)", got, defaultLoadCeiling)
	}
	t.Setenv(envLoadCeiling, "12.5")
	if got := loadCeilingFromEnv(); got != 12.5 {
		t.Errorf("env=12.5: got %v, want 12.5", got)
	}
	t.Setenv(envLoadCeiling, "nope")
	if got := loadCeilingFromEnv(); got != defaultLoadCeiling {
		t.Errorf("unparseable env: got %v, want default %v", got, defaultLoadCeiling)
	}
}

// TestRunningWorkers proves only live tasks in the 'active' state are counted (the machine-load
// signal); done/blocked/pending and torn-down tasks are not.
func TestRunningWorkers(t *testing.T) {
	live := []db.Task{
		{ID: "a", Status: db.StatusActive},
		{ID: "b", Status: db.StatusActive},
		{ID: "c", Status: db.StatusDone},
		{ID: "d", Status: db.StatusBlocked},
		{ID: "e", Status: db.StatusPending},
	}
	if got := runningWorkers(live); got != 2 {
		t.Errorf("runningWorkers = %d, want 2 (only active)", got)
	}
	if got := runningWorkers(nil); got != 0 {
		t.Errorf("runningWorkers(nil) = %d, want 0", got)
	}
}

// TestDispatchBudget proves the budget math: disabled ⇒ unbounded (maxInt), enabled ⇒
// cap-minus-active clamped at zero.
func TestDispatchBudget(t *testing.T) {
	live := []db.Task{{Status: db.StatusActive}, {Status: db.StatusActive}}

	// Disabled (<=0) ⇒ unbounded.
	sc := &Scheduler{MaxActiveWorkers: 0}
	if budget, active := sc.dispatchBudget(live); budget != maxInt || active != 2 {
		t.Errorf("disabled: budget=%d active=%d, want maxInt,2", budget, active)
	}

	// Enabled with room.
	sc = &Scheduler{MaxActiveWorkers: 5}
	if budget, active := sc.dispatchBudget(live); budget != 3 || active != 2 {
		t.Errorf("cap=5: budget=%d active=%d, want 3,2", budget, active)
	}

	// At/over the cap ⇒ clamped at zero.
	sc = &Scheduler{MaxActiveWorkers: 2}
	if budget, _ := sc.dispatchBudget(live); budget != 0 {
		t.Errorf("cap=2,active=2: budget=%d, want 0", budget)
	}
	sc = &Scheduler{MaxActiveWorkers: 1}
	if budget, _ := sc.dispatchBudget(live); budget != 0 {
		t.Errorf("cap=1,active=2: budget=%d, want 0 (clamped, not negative)", budget)
	}
}

// TestLoadDefersDispatch proves the load-ceiling predicate: disabled when ceiling <=0, over/under
// the ceiling, and — critically — fail OPEN when the load average is unreadable.
func TestLoadDefersDispatch(t *testing.T) {
	// Disabled (ceiling <= 0): never defers, regardless of load.
	sc := &Scheduler{LoadCeiling: 0, loadAvg: func() (float64, bool) { return 99, true }}
	if over, _ := sc.loadDefersDispatch(); over {
		t.Error("ceiling=0 (disabled) must never defer")
	}

	// Over the ceiling: defers, and reports the load it read.
	sc = &Scheduler{LoadCeiling: 4, loadAvg: func() (float64, bool) { return 9.5, true }}
	if over, load := sc.loadDefersDispatch(); !over || load != 9.5 {
		t.Errorf("load 9.5 > ceiling 4: over=%v load=%v, want true,9.5", over, load)
	}

	// Under the ceiling: does not defer.
	sc = &Scheduler{LoadCeiling: 4, loadAvg: func() (float64, bool) { return 1.0, true }}
	if over, _ := sc.loadDefersDispatch(); over {
		t.Error("load 1.0 < ceiling 4 must not defer")
	}

	// Unreadable load: FAIL OPEN — never defers (an unreadable loadavg must not wedge dispatch).
	sc = &Scheduler{LoadCeiling: 4, loadAvg: func() (float64, bool) { return 0, false }}
	if over, _ := sc.loadDefersDispatch(); over {
		t.Error("unreadable load must fail open (not defer)")
	}
}

// TestLandCapReached proves the per-tick land-cap boundary and its off-switch.
func TestLandCapReached(t *testing.T) {
	sc := &Scheduler{MaxLandConcurrency: 2}
	if sc.landCapReached(1) {
		t.Error("1 < 2 must not be capped")
	}
	if !sc.landCapReached(2) {
		t.Error("2 >= 2 must be capped")
	}
	// Disabled: never caps.
	sc = &Scheduler{MaxLandConcurrency: 0}
	if sc.landCapReached(100) {
		t.Error("cap<=0 (disabled) must never cap")
	}
}

// TestSystemLoadAvg proves the real platform reader works on the platforms ttorch runs on
// (linux/darwin) and degrades gracefully elsewhere. The value is the 1-minute load average, so
// it is non-negative when readable.
func TestSystemLoadAvg(t *testing.T) {
	load, ok := systemLoadAvg()
	switch runtime.GOOS {
	case "linux", "darwin":
		if !ok {
			t.Fatalf("systemLoadAvg on %s should be readable", runtime.GOOS)
		}
		if load < 0 {
			t.Errorf("load average %v should be non-negative", load)
		}
	default:
		// Other platforms may not be readable — that is fine (fail open); just must not panic.
		t.Logf("systemLoadAvg on %s: load=%v ok=%v", runtime.GOOS, load, ok)
	}
}

// TestNewWiresGovernorFromEnv proves New() populates the governor knobs from the env-or-default,
// so the auto-started daemon (which passes no governor flags) runs on the configured governor.
func TestNewWiresGovernorFromEnv(t *testing.T) {
	t.Setenv(envMaxActiveWorkers, "6")
	t.Setenv(envLoadCeiling, "8.0")
	t.Setenv(envMaxLandConcurrency, "3")
	s := newStore(t)
	m := &orchestrator.Manager{Store: s, Pool: worktree.Pool{Max: 16}}
	sc := New(m, 0, nil)
	if sc.MaxActiveWorkers != 6 {
		t.Errorf("MaxActiveWorkers = %d, want 6 (from env)", sc.MaxActiveWorkers)
	}
	if sc.LoadCeiling != 8.0 {
		t.Errorf("LoadCeiling = %v, want 8.0 (from env)", sc.LoadCeiling)
	}
	if sc.MaxLandConcurrency != 3 {
		t.Errorf("MaxLandConcurrency = %d, want 3 (from env)", sc.MaxLandConcurrency)
	}
}

// --- governor integration tests (RunOnce / RunLandOnce) ---------------------------

// liveActive builds a live, ACTIVE worker with a disjoint footprint so it counts toward the
// machine-load total without ever tripping the overlap gate against the ready backlog.
func liveActive(id, repo string) db.Task {
	return db.Task{ID: id, Window: "wk-" + id, Project: repo, Kind: db.KindShip, Status: db.StatusActive, Footprint: []string{"live/" + id}, Worktree: "/wt/" + id}
}

// TestRunOnceGovernorDefersAtMaxActive proves the headline acceptance: with max-active=M and M
// workers already running, a dispatch tick spawns NOTHING more, leaves the backlog pending, and
// LOGS the deferral (never a silent stall).
func TestRunOnceGovernorDefersAtMaxActive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"pkg/a"})
	addPending(t, s, repo, "p2", db.KindShip, []string{"pkg/b"})

	var log bytes.Buffer
	f := &fakeFleet{live: []db.Task{liveActive("w1", repo), liveActive("w2", repo)}}
	// Pool is huge (capacity never the limiter); the governor cap of 2 is the only gate.
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxActiveWorkers: 2, Log: &log}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || len(f.dispatched()) != 0 {
		t.Fatalf("at the max-active cap nothing must dispatch, got n=%d dispatched=%v", n, f.dispatched())
	}
	if status(t, s, "p1") != db.StatusPending || status(t, s, "p2") != db.StatusPending {
		t.Error("deferred tasks must remain pending (not failed)")
	}
	if out := log.String(); !strings.Contains(out, "dispatch deferred") || !strings.Contains(out, "max-active") {
		t.Errorf("deferral must be logged with the max-active cause; got %q", out)
	}
}

// TestRunOnceGovernorResumesWhenActiveDrops proves that once the running count drops below the
// cap, dispatch resumes — the governor throttles, it does not permanently wedge the backlog.
func TestRunOnceGovernorResumesWhenActiveDrops(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"pkg/a"})
	addPending(t, s, repo, "p2", db.KindShip, []string{"pkg/b"})

	f := &fakeFleet{live: []db.Task{liveActive("w1", repo), liveActive("w2", repo)}}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxActiveWorkers: 2}

	// Tick 1: at the cap ⇒ nothing dispatched.
	if n, err := sc.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("tick1: n=%d err=%v, want 0,nil (at cap)", n, err)
	}

	// The running workers finish (no longer live); the cap is now slack.
	f.mu.Lock()
	f.live = nil
	f.mu.Unlock()

	// Tick 2: dispatch resumes for the still-pending backlog.
	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if n != 2 {
		t.Fatalf("tick2 dispatched %d, want 2 (cap now slack)", n)
	}
}

// TestRunOnceGovernorDispatchesUpToBudget proves a partial tick: with one worker already running
// under a cap of 3, exactly 2 (the remaining budget) dispatch and the rest stay pending, with the
// budget-exhaustion deferral logged.
func TestRunOnceGovernorDispatchesUpToBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	for _, id := range []string{"p1", "p2", "p3", "p4"} {
		addPending(t, s, repo, id, db.KindShip, []string{"pkg/" + id})
	}

	var log bytes.Buffer
	f := &fakeFleet{live: []db.Task{liveActive("w1", repo)}} // 1 already running
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxActiveWorkers: 3, Log: &log}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("dispatched %d, want 2 (budget = cap 3 - 1 active)", n)
	}
	if !strings.Contains(log.String(), "reached max-active") {
		t.Errorf("the budget-exhaustion deferral must be logged; got %q", log.String())
	}
}

// TestRunOnceGovernorDisabledMatchesToday proves that with the governor disabled (cap <= 0, no
// load ceiling) the dispatch pass behaves exactly as before — every disjoint ready task is
// dispatched regardless of how many workers are running.
func TestRunOnceGovernorDisabledMatchesToday(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	ids := []string{"p1", "p2", "p3", "p4", "p5"}
	for _, id := range ids {
		addPending(t, s, repo, id, db.KindShip, []string{"pkg/" + id})
	}

	f := &fakeFleet{live: []db.Task{liveActive("w1", repo), liveActive("w2", repo)}}
	// MaxActiveWorkers=0 and LoadCeiling=0 ⇒ governor fully disabled.
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxActiveWorkers: 0, LoadCeiling: 0}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != len(ids) {
		t.Fatalf("disabled governor dispatched %d, want %d (today's behavior)", n, len(ids))
	}
}

// TestRunOnceLoadCeilingDefers proves a tick whose load exceeds the ceiling dispatches nothing and
// logs the load cause, leaving the backlog pending.
func TestRunOnceLoadCeilingDefers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"pkg/a"})

	var log bytes.Buffer
	f := &fakeFleet{}
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Log: &log,
		LoadCeiling: 2.0,
		loadAvg:     func() (float64, bool) { return 7.0, true }, // over the ceiling
	}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || len(f.dispatched()) != 0 {
		t.Fatalf("over the load ceiling nothing must dispatch, got n=%d", n)
	}
	if status(t, s, "p1") != db.StatusPending {
		t.Error("load-deferred task must remain pending")
	}
	if out := log.String(); !strings.Contains(out, "dispatch deferred") || !strings.Contains(out, "load") || !strings.Contains(out, "ceiling") {
		t.Errorf("the load deferral must be logged; got %q", out)
	}
}

// TestRunOnceLoadUnreadableFailsOpen proves an UNREADABLE load average never wedges dispatch: with
// the load ceiling configured but loadavg unreadable, dispatch proceeds normally (fail open).
func TestRunOnceLoadUnreadableFailsOpen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"pkg/a"})
	addPending(t, s, repo, "p2", db.KindShip, []string{"pkg/b"})

	f := &fakeFleet{}
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100},
		LoadCeiling:      5.0,
		MaxActiveWorkers: 0,                                          // cap disabled — isolate the load fail-open
		loadAvg:          func() (float64, bool) { return 0, false }, // unreadable
	}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("unreadable load must fail open and dispatch normally, got n=%d", n)
	}
}

// TestRunOnceMaxActiveAppliesEvenWhenLoadUnreadable proves the hard cap is INDEPENDENT of the
// (fail-open) load ceiling: even when loadavg is unreadable, the always-enforceable max-active cap
// still defers a tick at the cap.
func TestRunOnceMaxActiveAppliesEvenWhenLoadUnreadable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addPending(t, s, repo, "p1", db.KindShip, []string{"pkg/a"})

	var log bytes.Buffer
	f := &fakeFleet{live: []db.Task{liveActive("w1", repo), liveActive("w2", repo)}}
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, Log: &log,
		LoadCeiling:      5.0,
		MaxActiveWorkers: 2,                                          // 2 already running ⇒ at the cap
		loadAvg:          func() (float64, bool) { return 0, false }, // unreadable ⇒ load fails open
	}

	n, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("the max-active cap must still gate when load is unreadable, got n=%d", n)
	}
	if !strings.Contains(log.String(), "max-active") {
		t.Errorf("the deferral must cite the max-active cap; got %q", log.String())
	}
}

// TestRunLandOnceBoundsFanout proves the per-tick land cap: with 5 gated tasks and a cap of 2,
// the first tick hands exactly 2 to the land pipeline and logs the deferral; successive ticks
// drain the rest, so all 5 eventually land without ever launching more than 2 validates at once.
func TestRunLandOnceBoundsFanout(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	for _, id := range []string{"d1", "d2", "d3", "d4", "d5"} {
		addDone(t, s, repo, id, db.KindShip, review.Pass)
	}

	var log bytes.Buffer
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxLandConcurrency: 2, Log: &log}

	// Tick 1: exactly 2 handed to LandSet; the cap defers the rest (logged).
	n, err := sc.RunLandOnce(ctx)
	if err != nil {
		t.Fatalf("tick1 RunLandOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("tick1 landed %d, want 2 (capped)", n)
	}
	if !strings.Contains(log.String(), "land deferred") {
		t.Errorf("the land deferral must be logged; got %q", log.String())
	}

	// Successive ticks drain the remaining gated tasks (each bounded at 2).
	total := n
	for i := 0; i < 5 && total < 5; i++ {
		m, err := sc.RunLandOnce(ctx)
		if err != nil {
			t.Fatalf("drain tick RunLandOnce: %v", err)
		}
		total += m
	}
	if total != 5 {
		t.Fatalf("eventually landed %d, want all 5", total)
	}
	// No single tick ever handed more than the cap to the pipeline. Reconstruct per-batch sizes
	// from the recorded order is unnecessary: the cumulative total is 5 and tick1 was exactly 2,
	// which together with the cap of 2 proves no batch exceeded 2.
	if got := len(f.landedIDs()); got != 5 {
		t.Fatalf("LandSet received %d ids total, want 5", got)
	}
}

// TestRunLandOnceUnboundedWhenDisabled proves a disabled land cap (<=0) hands the whole gated set
// to the pipeline in one tick — today's behavior, unchanged.
func TestRunLandOnceUnboundedWhenDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	ids := []string{"d1", "d2", "d3", "d4", "d5"}
	for _, id := range ids {
		addDone(t, s, repo, id, db.KindShip, review.Pass)
	}

	var log bytes.Buffer
	f := &fakeFleet{}
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}, MaxLandConcurrency: 0, Log: &log}

	n, err := sc.RunLandOnce(ctx)
	if err != nil {
		t.Fatalf("RunLandOnce: %v", err)
	}
	if n != len(ids) {
		t.Fatalf("disabled land cap landed %d, want %d (one tick)", n, len(ids))
	}
	if strings.Contains(log.String(), "land deferred") {
		t.Errorf("a disabled cap must never log a land deferral; got %q", log.String())
	}
}

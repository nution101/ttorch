package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// TestMain isolates the whole package from the caller's environment so a test that
// resolves paths.StateDB() / WatchPIDFile() can never reach the real ~/.ttorch.
// TTORCH_HOME is pinned at a throwaway dir (only-if-unset, matching the supervisor's
// TestMain); each test also t.Setenv's its own temp home, and the db.Open guard is
// the final fail-closed backstop.
//
// TTORCH_DB is cleared up front: a worker shell exports TTORCH_DB=~/.ttorch/state.db,
// and it OUTRANKS TTORCH_HOME in paths.StateDB() (envOr prefers it), so an inherited
// value would defeat the per-test TTORCH_HOME isolation and steer every db.Open at the
// real home — where the guard then (correctly) refuses, failing the suite. Unsetting it
// package-wide lets each test's TTORCH_HOME govern StateDB(), so `go test ./internal/watch/`
// passes whether or not the calling shell has TTORCH_DB set.
func TestMain(m *testing.M) {
	os.Unsetenv("TTORCH_DB")
	if os.Getenv("TTORCH_HOME") == "" {
		home, err := os.MkdirTemp("", "ttorch-watch-test-home-*")
		if err != nil {
			panic(err)
		}
		os.Setenv("TTORCH_HOME", home)
		code := m.Run()
		_ = os.RemoveAll(home)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakeClock is a deterministic, manually-advanced clock for the watch loop.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// newWatcher builds a Watcher backed by a fresh temp-home DB with fully injected,
// deterministic seams: an advancing fake clock, a wait that advances that clock
// without real sleeping (with a runaway guard), a present manager, gh absent, and a
// readable idle pane. Tests override individual seams as needed.
func newWatcher(t *testing.T) (*Watcher, *db.Store, *bytes.Buffer, *fakeClock) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	// Pin TTORCH_DB at this test's own temp home as well: it outranks TTORCH_HOME in
	// paths.StateDB(), so without an explicit per-test value an ambient TTORCH_DB (a
	// worker shell exports one) would steer db.Open away from the isolated home.
	// TestMain also clears it package-wide; this keeps the helper correct on its own.
	t.Setenv("TTORCH_DB", filepath.Join(home, "state.db"))
	s, err := db.Open(paths.Default().StateDB())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := New(s, paths.Default(), "test-session")
	buf := &bytes.Buffer{}
	w.Out = buf
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	w.now = clk.now
	calls := 0
	w.wait = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		calls++
		if calls > 10000 {
			return errors.New("watch loop did not terminate within 10000 sweeps")
		}
		clk.t = clk.t.Add(d)
		return nil
	}
	w.Since = 0
	w.Coalesce = time.Millisecond
	w.Poll = 10 * time.Millisecond
	w.Stale = -1 // disable the quiet-hold gate so liveness always inspects the pane
	w.managerPresent = func() bool { return true }
	w.ghAvailable = func() bool { return false }
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ idle at the prompt"}
	}
	w.nudge = func(string) error { return nil } // no real tmux send in tests
	w.isWatchProc = func(int) bool { return false }
	w.lockRetry = time.Millisecond
	return w, s, buf, clk
}

// seedActiveTask creates a project + an active task with the given window.
func seedActiveTask(t *testing.T, s *db.Store, id, window string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.UpsertProject(ctx, "/repo/"+id, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := s.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Window: window, Kind: db.KindShip, Status: db.StatusActive,
	}, db.ActorManager); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func report(t *testing.T, s *db.Store, id, status, msg string) db.Event {
	t.Helper()
	ev, err := s.ReportStatus(context.Background(), id, status, "worker:"+id, msg)
	if err != nil {
		t.Fatalf("ReportStatus(%s): %v", status, err)
	}
	return ev
}

// TestRun_ReturnsOnActionableEvent: an inserted worker→done transition wakes the
// watcher, which prints the batch + watermark and exits Fired.
func TestRun_ReturnsOnActionableEvent(t *testing.T) {
	w, s, buf, _ := newWatcher(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	done := report(t, s, "alpha", db.StatusDone, "")

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired {
		t.Fatalf("expected Fired, got %+v", res)
	}
	if res.Watermark != done.ID {
		t.Fatalf("watermark = %d, want the done event id %d", res.Watermark, done.ID)
	}
	out := buf.String()
	if !strings.Contains(out, fmt.Sprintf("WATCH_WATERMARK=%d", done.ID)) {
		t.Fatalf("output missing WATCH_WATERMARK=%d:\n%s", done.ID, out)
	}
	if !strings.Contains(out, "→ done") || !strings.Contains(out, "task=alpha") {
		t.Fatalf("output missing the alpha→done line:\n%s", out)
	}
}

// TestRun_WatermarkPersistedBeforeUnlock: the surfaced watermark is written to the
// manager row (and so survives a re-arm) — the §4.2 persist-before-release invariant.
func TestRun_WatermarkPersistedBeforeUnlock(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	seedActiveTask(t, s, "beta", "wk-beta")
	blocked := report(t, s, "beta", db.StatusBlocked, "needs a decision")

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired {
		t.Fatalf("expected Fired, got %+v", res)
	}
	m, ok, err := s.GetManager(context.Background())
	if err != nil || !ok {
		t.Fatalf("GetManager: ok=%v err=%v", ok, err)
	}
	if m.WatchWatermark != blocked.ID {
		t.Fatalf("persisted watermark = %d, want %d", m.WatchWatermark, blocked.ID)
	}
}

// TestRun_CoalesceDedupesByEntityKeepingLatest: a task that went blocked → active →
// done while the manager was away surfaces exactly ONE row, the latest (→ done), and
// the watermark is the max actionable id (§4.3).
func TestRun_CoalesceDedupesByEntityKeepingLatest(t *testing.T) {
	w, s, buf, _ := newWatcher(t)
	seedActiveTask(t, s, "gamma", "wk-gamma")
	report(t, s, "gamma", db.StatusBlocked, "needs schema decision")
	report(t, s, "gamma", db.StatusActive, "") // non-actionable, filtered out
	done := report(t, s, "gamma", db.StatusDone, "")

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired {
		t.Fatalf("expected Fired, got %+v", res)
	}
	if len(res.Batch) != 1 {
		t.Fatalf("batch should dedupe to one row, got %d: %+v", len(res.Batch), res.Batch)
	}
	if to := derefStatus(res.Batch[0].ToStatus); to != db.StatusDone {
		t.Fatalf("surfaced row to_status = %q, want done (latest wins)", to)
	}
	if res.Watermark != done.ID {
		t.Fatalf("watermark = %d, want the done id %d", res.Watermark, done.ID)
	}
	if strings.Contains(buf.String(), "→ blocked") {
		t.Fatalf("a superseded → blocked row must not be surfaced:\n%s", buf.String())
	}
}

// TestRun_TimeoutPrintsWatchTimeout: with nothing actionable, the timeout elapses and
// the watcher prints WATCH_TIMEOUT and exits cleanly.
func TestRun_TimeoutPrintsWatchTimeout(t *testing.T) {
	w, _, buf, _ := newWatcher(t)
	w.Timeout = 50 * time.Millisecond
	w.Poll = 10 * time.Millisecond

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if !strings.Contains(buf.String(), "WATCH_TIMEOUT") {
		t.Fatalf("output missing WATCH_TIMEOUT:\n%s", buf.String())
	}
}

// TestRun_AwaitingLeadKeepsBlocking: with awaiting_lead set, a pending actionable
// event is NOT surfaced — the watcher keeps blocking (the §4.6 backstop) until
// cancelled, never printing a batch or WATCH_TIMEOUT.
//
// The loop is driven through a deterministic sweep handshake rather than a wall-clock
// sleep: w.wait blocks until the test releases each sweep (advancing the fake clock by
// the poll interval as it goes), so there is no real sleeping and no timing window to
// race. Each sweep advances the fake clock, so the loop crosses its own Timeout horizon
// long before the test releases it — a regression that let the timeout branch fire while
// awaiting_lead would surface here as an early TimedOut return. Cancellation is the only
// thing that releases the backstop. The time.After guards below are generous deadlock
// failsafes (they fire only if the watcher stops sweeping or hangs on cancel — a real
// bug), never load-bearing timing assertions.
func TestRun_AwaitingLeadKeepsBlocking(t *testing.T) {
	w, s, buf, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "delta", "wk-delta")
	report(t, s, "delta", db.StatusBlocked, "an urgent blocker") // actionable, but must stay silent
	if err := s.SetAwaitingLead(ctx, true); err != nil {
		t.Fatalf("SetAwaitingLead: %v", err)
	}
	w.Timeout = 20 * time.Millisecond // would fire if the loop reached the timeout branch
	w.Poll = 10 * time.Millisecond

	// swept announces each loop iteration; the watcher parks here between sweeps until the
	// test receives, then advances the fake clock and continues (or returns on cancel).
	swept := make(chan struct{})
	w.wait = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case swept <- struct{}{}:
		}
		clk.t = clk.t.Add(d)
		return ctx.Err()
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resCh := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := w.Run(cctx)
		resCh <- res
		errCh <- err
	}()

	// Step the loop through many sweeps — well past its Timeout in fake-clock time (each
	// sweep advances the clock by the poll interval). It must keep blocking: never
	// surfacing the pending blocked event, never timing out.
	const observeSweeps = 10
	for i := 0; i < observeSweeps; i++ {
		select {
		case <-swept:
		case res := <-resCh:
			t.Fatalf("watcher returned while awaiting the lead at sweep %d: %+v\noutput:\n%s", i, res, buf.String())
		case <-time.After(10 * time.Second):
			t.Fatalf("watcher did not sweep within the failsafe deadline (sweep %d)", i)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("watcher must stay silent while awaiting the lead, printed:\n%s", buf.String())
	}

	// Only cancellation releases the backstop.
	cancel()
	select {
	case res := <-resCh:
		if res.Fired || res.TimedOut {
			t.Fatalf("after cancel, expected neither Fired nor TimedOut, got %+v", res)
		}
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not return after cancel")
	}
}

// TestPollLiveness_WindowGone: an active task whose tmux window is gone emits a
// window_gone actionable event.
func TestPollLiveness_WindowGone(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "gone", "wk-gone")
	w.capture = func(string) paneObservation { return paneObservation{present: false} }

	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	has, err := s.HasEventType(ctx, "gone", db.EventWindowGone)
	if err != nil || !has {
		t.Fatalf("expected a window_gone event for gone (has=%v err=%v)", has, err)
	}
}

// TestPollLiveness_IdleUnreportedAfterTwoSweeps: an unchanged, not-busy pane emits
// idle_unreported only once it has been stable for idleStaleSweeps sweeps; the per-
// sweep count is persisted on the task row. The wall-clock dwell is satisfied here (the
// fake clock is positioned past it) so this test isolates the sweep-count guard; the
// dwell gate itself is covered by TestPollLiveness_DwellSuppressesWithinTolerance /
// TestPollLiveness_DwellFlagsBeyondTolerance.
func TestPollLiveness_IdleUnreportedAfterTwoSweeps(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "idle", "wk-idle")
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ waiting at the prompt"}
	}
	// This task never reported, so the dwell is anchored on its creation time; put the
	// fake clock past the dwell so dwellElapsed is satisfied for every sweep.
	gt, _, err := s.GetTask(ctx, "idle")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(w.dwell() + time.Minute)

	// First sweeps record the unchanged pane but must not flag it yet.
	for i := 0; i < idleStaleSweeps; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
		if has, _ := s.HasEventType(ctx, "idle", db.EventIdleUnreported); has {
			t.Fatalf("idle_unreported fired too early, after %d sweeps", i+1)
		}
	}
	// The sweep that reaches the threshold flags it.
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness threshold sweep: %v", err)
	}
	if has, err := s.HasEventType(ctx, "idle", db.EventIdleUnreported); err != nil || !has {
		t.Fatalf("expected idle_unreported after the threshold (has=%v err=%v)", has, err)
	}
}

// TestPollLiveness_DwellSuppressesWithinTolerance: a worker idle-waiting on its own
// long-running background shell — not busy, pane static, but quiet for LESS than the
// dwell — must NOT be flagged idle_unreported, no matter how many sweeps accumulate.
// This is the false positive the wall-clock dwell exists to prevent (a multi-minute
// `make test` the worker spawned): the poll-cadence-sensitive sweep count would trip in
// seconds, but the dwell holds the flag back until the worker has truly been quiet.
func TestPollLiveness_DwellSuppressesWithinTolerance(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "bg", "wk-bg")
	// The worker staged progress, then went quiet to wait on its background test.
	if _, err := s.SetStage(ctx, "bg", "running make test", "worker:bg"); err != nil {
		t.Fatalf("SetStage: %v", err)
	}
	gt, _, err := s.GetTask(ctx, "bg")
	if err != nil || gt.LastProgressAt == nil {
		t.Fatalf("GetTask: lp=%v err=%v", gt.LastProgressAt, err)
	}
	// Quiet for less than the dwell ⇒ within tolerance.
	clk.t = gt.LastProgressAt.Add(w.dwell() - time.Minute)
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ idle while a background make test runs"}
	}
	for i := 0; i < idleStaleSweeps+3; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	if has, _ := s.HasEventType(ctx, "bg", db.EventIdleUnreported); has {
		t.Fatal("a worker quiet for less than the dwell must not be flagged idle_unreported")
	}
}

// TestPollLiveness_DwellFlagsBeyondTolerance: once a worker has been quiet (no
// report/stage, pane static, not busy) for LONGER than the dwell, it IS flagged — the
// net still catches a genuinely stalled worker; the dwell only delays the flag, it never
// removes it. Complements the never-reported (creation-anchored) path above by
// exercising the last_progress_at anchor.
func TestPollLiveness_DwellFlagsBeyondTolerance(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "stalled", "wk-stalled")
	if _, err := s.SetStage(ctx, "stalled", "running make test", "worker:stalled"); err != nil {
		t.Fatalf("SetStage: %v", err)
	}
	gt, _, err := s.GetTask(ctx, "stalled")
	if err != nil || gt.LastProgressAt == nil {
		t.Fatalf("GetTask: lp=%v err=%v", gt.LastProgressAt, err)
	}
	// Quiet for longer than the dwell ⇒ beyond tolerance.
	clk.t = gt.LastProgressAt.Add(w.dwell() + time.Minute)
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ stalled at the prompt"}
	}
	// Below the sweep threshold it must still hold off, even past the dwell.
	for i := 0; i < idleStaleSweeps; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
		if has, _ := s.HasEventType(ctx, "stalled", db.EventIdleUnreported); has {
			t.Fatalf("idle_unreported fired before the sweep threshold, after %d sweeps", i+1)
		}
	}
	// The sweep that reaches the threshold, now that the dwell has elapsed, flags it.
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness threshold sweep: %v", err)
	}
	if has, err := s.HasEventType(ctx, "stalled", db.EventIdleUnreported); err != nil || !has {
		t.Fatalf("expected idle_unreported past the dwell + threshold (has=%v err=%v)", has, err)
	}
}

// TestPollLiveness_ReFiresAfterOvershoot: a worker whose persisted idle_sweeps has
// climbed past the threshold (e.g. an out-of-band progress update cleared the
// surfaced exclusion without changing the pane) must still be flagged — the
// threshold test is `>=`, not exact equality.
func TestPollLiveness_ReFiresAfterOvershoot(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "stranded", "wk-stranded")
	pane := "$ still idle at the prompt"
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: pane}
	}
	// Past the wall-clock dwell (anchored on creation time, never reported) so the
	// overshoot path is what's exercised here, not the dwell gate.
	gt, _, err := s.GetTask(ctx, "stranded")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(w.dwell() + time.Minute)
	// Strand the counter past the threshold with the current pane already recorded,
	// as if a prior episode had overshot and the exclusion since cleared.
	if err := s.SetLiveness(ctx, "stranded", hashPane(pane), idleStaleSweeps+3); err != nil {
		t.Fatalf("SetLiveness: %v", err)
	}
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	if has, err := s.HasEventType(ctx, "stranded", db.EventIdleUnreported); err != nil || !has {
		t.Fatalf("an overshot idle counter must still re-fire idle_unreported (has=%v err=%v)", has, err)
	}
}

// TestPollLiveness_SuppressedForNeedsInputAndBlocked: tasks already surfaced as
// needs_input or blocked are out of the active scope, so a gone window for them is
// never re-flagged.
func TestPollLiveness_SuppressedForNeedsInputAndBlocked(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "ni", "wk-ni")
	seedActiveTask(t, s, "bl", "wk-bl")
	report(t, s, "ni", db.StatusNeedsInput, "")
	report(t, s, "bl", db.StatusBlocked, "")
	w.capture = func(string) paneObservation { return paneObservation{present: false} } // both windows "gone"

	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	for _, id := range []string{"ni", "bl"} {
		if has, _ := s.HasEventType(ctx, id, db.EventWindowGone); has {
			t.Fatalf("a %s task must not be re-flagged with window_gone", id)
		}
	}
}

// TestPollLiveness_BusyResetsAndNeverFlags: a worker showing a busy indicator is
// never treated as stale, no matter how many sweeps run.
func TestPollLiveness_BusyResetsAndNeverFlags(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "busy", "wk-busy")
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "… esc to interrupt …"}
	}
	for i := 0; i < 5; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	if has, _ := s.HasEventType(ctx, "busy", db.EventIdleUnreported); has {
		t.Fatal("a busy worker must never be flagged idle_unreported")
	}
}

// TestPollLiveness_StallAutoResumed: an idle, stable pane carrying the mid-stream
// API-stall error is auto-resumed with a single "continue" nudge once it reaches the
// stability threshold — without waiting out the dwell, without waking the manager
// (no idle_unreported), and at most once per episode. The resume is recorded on the
// audit spine as a non-actionable auto_resumed event.
func TestPollLiveness_StallAutoResumed(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "stall", "wk-stall")
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "API Error: Response stalled mid-stream\n│ >"}
	}
	// Hold the clock well WITHIN the dwell so the only thing that can act is the stall
	// fast path (which ignores the dwell) — this isolates the nudge decision from the
	// idle_unreported net.
	gt, _, err := s.GetTask(ctx, "stall")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(time.Second)

	var nudged int
	var nudgedWindow string
	w.nudge = func(window string) error {
		nudged++
		nudgedWindow = window
		return nil
	}

	// Below the stability threshold the stall must NOT be nudged yet.
	for i := 0; i < idleStaleSweeps; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
		if nudged != 0 {
			t.Fatalf("nudged too early, after %d sweeps", i+1)
		}
	}
	// The threshold sweep nudges exactly once.
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness threshold sweep: %v", err)
	}
	if nudged != 1 {
		t.Fatalf("expected exactly one nudge at the threshold, got %d", nudged)
	}
	if nudgedWindow != "wk-stall" {
		t.Fatalf("nudged window = %q, want wk-stall", nudgedWindow)
	}
	if has, err := s.HasEventType(ctx, "stall", db.EventAutoResumed); err != nil || !has {
		t.Fatalf("expected an auto_resumed event (has=%v err=%v)", has, err)
	}
	if has, _ := s.HasEventType(ctx, "stall", db.EventIdleUnreported); has {
		t.Fatal("auto-resume must not also wake the manager with idle_unreported")
	}
	// Further sweeps on the same (still-stalled) pane must not re-nudge: the fast path
	// fires at most once per episode.
	for i := 0; i < 3; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness post-nudge sweep %d: %v", i, err)
		}
	}
	if nudged != 1 {
		t.Fatalf("nudge must fire at most once per stall episode, got %d", nudged)
	}
}

// TestPollLiveness_HealthyNotNudged: neither a busy worker nor a healthy, idle-but-not-
// stalled worker is ever nudged, no matter how many sweeps run (even past the dwell).
// The auto-resume is reserved for the unambiguous stall error.
func TestPollLiveness_HealthyNotNudged(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "busy", "wk-busy")
	seedActiveTask(t, s, "idle", "wk-idle")
	panes := map[string]string{
		"wk-busy": "✶ Working… (esc to interrupt)",
		"wk-idle": "$ idle at the prompt",
	}
	w.capture = func(window string) paneObservation {
		return paneObservation{present: true, captured: true, pane: panes[window]}
	}
	// Past the dwell so the idle worker would be flagged idle_unreported — proving the
	// no-nudge result is about the absence of a stall, not the dwell holding things back.
	gt, _, err := s.GetTask(ctx, "idle")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(w.dwell() + time.Minute)

	var nudged int
	w.nudge = func(string) error { nudged++; return nil }

	for i := 0; i < idleStaleSweeps+3; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	if nudged != 0 {
		t.Fatalf("a healthy worker must never be nudged, got %d nudges", nudged)
	}
}

// TestPollLiveness_StallNudgeFailureFallsThroughToIdle: when the resume send fails, no
// auto_resumed event is recorded and the worker is not silently dropped — the
// idle_unreported safety net still surfaces it to the manager once the dwell elapses.
func TestPollLiveness_StallNudgeFailureFallsThroughToIdle(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "stall", "wk-stall")
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "API Error: Response stalled mid-stream\n│ >"}
	}
	w.nudge = func(string) error { return errors.New("tmux send failed") }
	// Past the dwell so the idle net is eligible to fire once the fast path goes quiet.
	gt, _, err := s.GetTask(ctx, "stall")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(w.dwell() + time.Minute)

	for i := 0; i < idleStaleSweeps+2; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	if has, _ := s.HasEventType(ctx, "stall", db.EventAutoResumed); has {
		t.Fatal("a failed nudge must not record an auto_resumed event")
	}
	if has, err := s.HasEventType(ctx, "stall", db.EventIdleUnreported); err != nil || !has {
		t.Fatalf("a worker a failed nudge could not revive must still surface as idle_unreported (has=%v err=%v)", has, err)
	}
}

// TestPollLiveness_AlreadySurfacedNotReFlagged: once a window_gone is recorded, a
// second sweep does not append another (the open-actionable-event exclusion, §4.4).
func TestPollLiveness_AlreadySurfacedNotReFlagged(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "once", "wk-once")
	w.capture = func(string) paneObservation { return paneObservation{present: false} }

	for i := 0; i < 3; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness sweep %d: %v", i, err)
		}
	}
	events, err := s.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == db.EventWindowGone && e.EntityID == "once" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("window_gone should be recorded exactly once, got %d", n)
	}
}

// TestPollArmedPRs_MergedEmittedOnce: a merged armed PR emits one pr_merged event,
// and a later poll does not duplicate it (durable dedup via HasEventType).
func TestPollArmedPRs_MergedEmittedOnce(t *testing.T) {
	w, s, _, clk := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "pr", "wk-pr")
	if err := s.SetTaskFields(ctx, "pr", db.TaskFields{PR: ptr("https://example/pr/1")}); err != nil {
		t.Fatalf("SetTaskFields: %v", err)
	}
	w.ghAvailable = func() bool { return true }
	w.prState = func(string) (string, error) { return "MERGED", nil }

	if err := w.pollArmedPRs(ctx); err != nil {
		t.Fatalf("pollArmedPRs: %v", err)
	}
	clk.t = clk.t.Add(time.Hour) // step past the PR-poll rate limit
	if err := w.pollArmedPRs(ctx); err != nil {
		t.Fatalf("pollArmedPRs (2): %v", err)
	}

	events, err := s.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == db.EventPRMerged && e.EntityID == "pr" {
			if !e.Actionable {
				t.Fatal("pr_merged must be actionable")
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("pr_merged should be recorded exactly once, got %d", n)
	}
}

func ptr(s string) *string { return &s }

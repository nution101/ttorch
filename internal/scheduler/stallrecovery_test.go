package scheduler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/worktree"
)

// stallPane is the canonical recoverable-stall capture: the harness's mid-stream API error,
// then the redrawn input prompt (turn ended, sitting idle) — exactly what livestate.APIStalled
// recognizes as the recovery signal.
const stallPane = "API Error: Response stalled mid-stream. The response above may be incomplete.\n│ > "

// newStallScheduler builds a stall-recovery Scheduler over a controllable clock (advance *clk
// between RunStallRecoveryOnce calls to drive the grace deterministically without sleeping).
func newStallScheduler(s *db.Store, f *fakeFleet, log *bytes.Buffer, grace time.Duration, maxNudges int, clk *time.Time) *Scheduler {
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100},
		StallNudgeGrace: grace, MaxStallNudges: maxNudges,
		now: func() time.Time { return *clk },
	}
	if log != nil { // leave Log a nil interface (not a typed-nil) so logf's nil-guard holds
		sc.Log = log
	}
	return sc
}

// TestStallRecovery_NudgesAfterGrace is the headline worker path: a worker API-stalled at the
// prompt is NOT nudged before the grace elapses, IS nudged exactly once once it does, and is
// NOT re-nudged on the next tick before the grace since the nudge has elapsed.
func TestStallRecovery_NudgesAfterGrace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now()) // last_progress is irrelevant to stall recovery
	var log bytes.Buffer
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, &log, 15*time.Second, 2, &clk)

	// Tick 1: stall just observed — grace has not elapsed, so no nudge.
	if n, err := sc.RunStallRecoveryOnce(ctx); err != nil || n != 0 || f.sendCount("w1") != 0 {
		t.Fatalf("first tick: n=%d sends=%d err=%v, want 0/0/nil (within grace)", n, f.sendCount("w1"), err)
	}

	// Tick 2: past the grace → exactly one nudge.
	clk = clk.Add(16 * time.Second)
	if n, err := sc.RunStallRecoveryOnce(ctx); err != nil || n != 1 || f.sendCount("w1") != 1 {
		t.Fatalf("post-grace tick: n=%d sends=%d err=%v, want 1/1/nil", n, f.sendCount("w1"), err)
	}
	if status(t, s, "w1") != db.StatusActive {
		t.Errorf("status = %q after nudge, want active (a nudge never changes status)", status(t, s, "w1"))
	}
	if !strings.Contains(log.String(), "recovered API-stalled worker w1") {
		t.Errorf("missing recovery log; got %q", log.String())
	}

	// Tick 3: same stall, only 5s since the nudge → must NOT re-nudge.
	clk = clk.Add(5 * time.Second)
	if n, err := sc.RunStallRecoveryOnce(ctx); err != nil || n != 0 || f.sendCount("w1") != 1 {
		t.Fatalf("re-nudge tick: n=%d sends=%d err=%v, want 0/1/nil (grace since nudge not elapsed)", n, f.sendCount("w1"), err)
	}
}

// TestStallRecovery_CapsAndGivesUp proves the bound: a worker frozen on the same stall is nudged
// at most MaxStallNudges times, then the pass gives up (logged once) and sends no more.
func TestStallRecovery_CapsAndGivesUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	var log bytes.Buffer
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, &log, 15*time.Second, 2, &clk)

	// Advance past the grace four times; only the first two ticks may nudge.
	for i := 0; i < 4; i++ {
		clk = clk.Add(20 * time.Second)
		if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if f.sendCount("w1") != 2 {
		t.Fatalf("sends = %d, want 2 (capped at MaxStallNudges)", f.sendCount("w1"))
	}
	if !strings.Contains(log.String(), "stall recovery: worker w1 nudged 2x, no progress — leaving for manager") {
		t.Errorf("missing give-up log; got %q", log.String())
	}
}

// TestStallRecovery_SkipsHealthyBusyAndCleanIdle proves the pass acts ONLY on a stall: a clean
// idle prompt (no error) and a busy/streaming pane are never nudged, no matter how long they sit.
func TestStallRecovery_SkipsHealthyBusyAndCleanIdle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "idle", time.Now())
	addIdleWorker(t, s, "/repo", "busy", time.Now())
	f := &fakeFleet{panes: map[string]string{
		"idle": idleNudgePane, // a clean idle prompt — the H2 idle-nudge's domain, not this pass's
		"busy": busyNudgePane, // mid-turn
	}}
	clk := time.Now()
	sc := newStallScheduler(s, nil, nil, 15*time.Second, 2, &clk)
	sc.Fleet = f

	for i := 0; i < 3; i++ {
		clk = clk.Add(30 * time.Second)
		if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if f.sendCount("idle") != 0 || f.sendCount("busy") != 0 {
		t.Fatalf("non-stalled workers nudged (idle=%d busy=%d), want 0/0", f.sendCount("idle"), f.sendCount("busy"))
	}
}

// TestStallRecovery_SkipsNonActive proves a worker that reported blocked/needs_input — a manager
// decision — is out of scope (not in the active set), even when its pane shows a stall.
func TestStallRecovery_SkipsNonActive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "b", time.Now())
	if _, err := s.ReportStatus(ctx, "b", db.StatusBlocked, "worker:b", "stuck"); err != nil {
		t.Fatalf("ReportStatus blocked: %v", err)
	}
	f := &fakeFleet{panes: map[string]string{"b": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)

	clk = clk.Add(60 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("RunStallRecoveryOnce: %v", err)
	}
	if f.sendCount("b") != 0 {
		t.Fatalf("blocked worker nudged %d times, want 0 — that is the manager's decision", f.sendCount("b"))
	}
}

// TestStallRecovery_SkipsExpiredLease proves a stalled worker whose lease has lapsed is left for
// the reclaim pass (the recovery pass owns only live, leased workers).
func TestStallRecovery_SkipsExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()
	addActiveLeasedWorker(t, s, "/repo", "w1", "wk-w1", now.Add(-time.Minute), now.Add(-2*time.Minute)) // lease expired
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := now
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)

	clk = clk.Add(60 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("RunStallRecoveryOnce: %v", err)
	}
	if f.sendCount("w1") != 0 {
		t.Fatalf("expired-lease worker nudged %d times, want 0 (reclaim's domain)", f.sendCount("w1"))
	}
}

// TestStallRecovery_PeekErrorSkipped proves an unreadable/gone window is skipped, not nudged.
func TestStallRecovery_PeekErrorSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	f := &fakeFleet{
		panes:   map[string]string{"w1": stallPane},
		peekErr: map[string]error{"w1": errors.New("window gone")},
	}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)

	clk = clk.Add(60 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("RunStallRecoveryOnce: %v", err)
	}
	if f.sendCount("w1") != 0 {
		t.Fatalf("unreadable window nudged %d times, want 0", f.sendCount("w1"))
	}
}

// TestStallRecovery_RevivalResetsEpisode proves that once a nudged worker revives (pane no
// longer stalled), a fresh stall starts a brand-new episode with a full budget.
func TestStallRecovery_RevivalResetsEpisode(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)

	// First episode: observe the stall (registers firstSeen), then past grace → nudge once.
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("observe tick: %v", err)
	}
	clk = clk.Add(20 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("nudge tick: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("first episode sends=%d, want 1", f.sendCount("w1"))
	}

	// Revived: the worker is now busy. The episode must end (no nudge).
	f.setPane("w1", busyNudgePane)
	clk = clk.Add(5 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("revival tick: %v", err)
	}

	// Stalls again: a NEW episode, full budget. Observe (fresh firstSeen), then past grace → nudge.
	f.setPane("w1", stallPane)
	clk = clk.Add(5 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("re-stall observe tick: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("sends=%d on the re-stall observation, want 1 (new episode within grace)", f.sendCount("w1"))
	}
	clk = clk.Add(20 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("re-stall post-grace tick: %v", err)
	}
	if f.sendCount("w1") != 2 {
		t.Fatalf("sends=%d, want 2 (budget replenished on the new episode)", f.sendCount("w1"))
	}
}

// TestStallRecovery_Disabled proves MaxStallNudges <= 0 turns the pass off entirely.
func TestStallRecovery_Disabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 0, &clk) // disabled
	mgrSends := 0
	sc.mgrPeek = func(int) (string, bool) { return stallPane, true }
	sc.mgrSend = func() error { mgrSends++; return nil }

	clk = clk.Add(60 * time.Second)
	n, err := sc.RunStallRecoveryOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("disabled pass: n=%d err=%v, want 0/nil", n, err)
	}
	if f.sendCount("w1") != 0 || mgrSends != 0 {
		t.Fatalf("disabled pass acted: worker=%d manager=%d, want 0/0", f.sendCount("w1"), mgrSends)
	}
}

// TestStallRecovery_SendFailureCountedNotReSent proves record-then-send: a failed send still
// counts against the budget (so a wedged pane is not re-nudge-stormed) but the recovery is not
// counted as successful.
func TestStallRecovery_SendFailureCountedNotReSent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	var log bytes.Buffer
	f := &fakeFleet{
		panes:   map[string]string{"w1": stallPane},
		sendErr: map[string]error{"w1": errors.New("no live window")},
	}
	clk := time.Now()
	sc := newStallScheduler(s, f, &log, 15*time.Second, 2, &clk)

	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil { // observe (registers firstSeen)
		t.Fatalf("observe tick: %v", err)
	}
	clk = clk.Add(20 * time.Second)
	n, err := sc.RunStallRecoveryOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("failing nudge: n=%d err=%v, want 0/nil (send failed → not counted recovered)", n, err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("send attempts=%d, want 1 (the attempt is made once)", f.sendCount("w1"))
	}
	if !strings.Contains(log.String(), "nudge send failed") {
		t.Errorf("missing send-failure log; got %q", log.String())
	}

	// Immediately (within grace since the recorded attempt): must NOT re-send.
	clk = clk.Add(5 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("follow-up tick: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("send attempts=%d after a failed nudge, want 1 (budget consumed, not retried this screen)", f.sendCount("w1"))
	}
}

// --- Manager window recovery: the crux — the daemon nudges the manager that cannot nudge itself ---

// TestNew_WiresManagerStallSeams is the production-wiring assertion: scheduler.New must WIRE the
// manager-window stall-recovery seams (mgrPeek/mgrSend), so the manager half of RunStallRecoveryOnce
// is LIVE in the real daemon — not dormant. The seams' behavior (nudge a stalled manager once per
// episode, bounded; never inject into a healthy one) is covered by the injected-seam tests below;
// this guards that production actually installs them, so a regression that drops the wiring (and
// silently re-dormants the manager half) fails the build.
func TestNew_WiresManagerStallSeams(t *testing.T) {
	s := newStore(t)
	m := &orchestrator.Manager{Session: "ttorch", Store: s, Pool: worktree.Pool{Max: 1}}
	sc := New(m, DefaultInterval, nil)
	if sc.mgrPeek == nil || sc.mgrSend == nil {
		t.Fatalf("New did not wire the manager stall-recovery seams: mgrPeek wired=%v, mgrSend wired=%v "+
			"(the manager half would stay dormant in production)", sc.mgrPeek != nil, sc.mgrSend != nil)
	}
}

// TestStallRecovery_Manager_NudgedWhenStalled proves the manager pane, when API-stalled past the
// grace, is detected and nudged with "continue" through the manager seams.
func TestStallRecovery_Manager_NudgedWhenStalled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var log bytes.Buffer
	f := &fakeFleet{} // no workers
	clk := time.Now()
	sc := newStallScheduler(s, f, &log, 15*time.Second, 2, &clk)
	mgrSends := 0
	sc.mgrPeek = func(int) (string, bool) { return stallPane, true }
	// mgrSend takes no argument — the manager nudge is intrinsically the fixed "continue" resume
	// (production hardcodes the literal; the source-scan invariant enforces it). The fake just counts.
	sc.mgrSend = func() error { mgrSends++; return nil }

	// Within grace → no nudge.
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if mgrSends != 0 {
		t.Fatalf("manager nudged within grace (sends=%d), want 0", mgrSends)
	}
	// Past grace → exactly one nudge, logged.
	clk = clk.Add(16 * time.Second)
	n, err := sc.RunStallRecoveryOnce(ctx)
	if err != nil || n != 1 || mgrSends != 1 {
		t.Fatalf("post-grace: n=%d mgrSends=%d err=%v, want 1/1/nil", n, mgrSends, err)
	}
	if !strings.Contains(log.String(), "recovered API-stalled manager") {
		t.Errorf("missing manager recovery log; got %q", log.String())
	}
}

// TestStallRecovery_Manager_HealthyNeverInjected is the silent-wake guarantee: a healthy
// (busy or cleanly idle) manager pane is NEVER written to, no matter how long it is observed.
func TestStallRecovery_Manager_HealthyNeverInjected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := &fakeFleet{}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)
	mgrSends := 0
	// Alternate between a busy and a clean-idle healthy manager across ticks.
	healthy := []string{busyNudgePane, idleNudgePane}
	tick := 0
	sc.mgrPeek = func(int) (string, bool) {
		p := healthy[tick%len(healthy)]
		return p, true
	}
	sc.mgrSend = func() error { mgrSends++; return nil }

	for ; tick < 6; tick++ {
		clk = clk.Add(30 * time.Second) // well past any grace
		if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	if mgrSends != 0 {
		t.Fatalf("healthy manager injected into %d times, want 0 (silent-wake violated)", mgrSends)
	}
}

// TestStallRecovery_Manager_Unobservable proves an unobservable manager (no tmux / window gone)
// is silently skipped — no send, no panic — and never blocks the worker half of the pass.
func TestStallRecovery_Manager_Unobservable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk)
	mgrSends := 0
	sc.mgrPeek = func(int) (string, bool) { return "", false } // unobservable
	sc.mgrSend = func() error { mgrSends++; return nil }

	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil { // observe the worker stall
		t.Fatalf("observe tick: %v", err)
	}
	clk = clk.Add(20 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("RunStallRecoveryOnce: %v", err)
	}
	if mgrSends != 0 {
		t.Fatalf("unobservable manager nudged %d times, want 0", mgrSends)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("worker recovery blocked by the manager half: sends=%d, want 1", f.sendCount("w1"))
	}
}

// TestStallRecovery_NilManagerSeamSkipped proves a bare Scheduler with no manager seams wired
// (mgrPeek == nil) runs the worker half cleanly and never panics on the manager half.
func TestStallRecovery_NilManagerSeamSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addIdleWorker(t, s, "/repo", "w1", time.Now())
	f := &fakeFleet{panes: map[string]string{"w1": stallPane}}
	clk := time.Now()
	sc := newStallScheduler(s, f, nil, 15*time.Second, 2, &clk) // mgrPeek/mgrSend left nil

	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil { // observe (registers firstSeen)
		t.Fatalf("observe tick: %v", err)
	}
	clk = clk.Add(20 * time.Second)
	if _, err := sc.RunStallRecoveryOnce(ctx); err != nil {
		t.Fatalf("RunStallRecoveryOnce: %v", err)
	}
	if f.sendCount("w1") != 1 {
		t.Fatalf("worker sends=%d, want 1 (worker half must run with no manager seam)", f.sendCount("w1"))
	}
}

// TestNudgeIdle_SkipsAPIStalledWorker proves the disjointness with the H2 idle-nudge: a worker
// API-stalled at the prompt is NOT nudged by the idle pass (the stall-recovery pass owns it),
// while a sibling cleanly-idle worker still is — so a session is nudged by exactly one path.
func TestNudgeIdle_SkipsAPIStalledWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	past := time.Now().Add(-10 * time.Minute) // well past the idle grace
	addIdleWorker(t, s, "/repo", "stalled", past)
	addIdleWorker(t, s, "/repo", "clean", past)
	f := &fakeFleet{panes: map[string]string{
		"stalled": stallPane,
		"clean":   idleNudgePane,
	}}
	sc := newIdleScheduler(s, f, nil, 3*time.Minute, 2)
	sc.IdleConfirmations = 1 // nudge on first idle observation

	if _, err := sc.RunNudgeIdleOnce(ctx); err != nil {
		t.Fatalf("RunNudgeIdleOnce: %v", err)
	}
	if f.sendCount("stalled") != 0 {
		t.Errorf("idle-nudge nudged an API-stalled worker %d times, want 0 (stall-recovery owns it)", f.sendCount("stalled"))
	}
	if f.sendCount("clean") != 1 {
		t.Errorf("idle-nudge nudged the cleanly-idle worker %d times, want 1", f.sendCount("clean"))
	}
}

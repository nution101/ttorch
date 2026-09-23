package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// testStallPolicy is the ladder the tests run: first update at 10m, re-raise every 15m,
// needs-inspection after 3 re-raises.
var testStallPolicy = stallPolicy{After: 10 * time.Minute, Repeat: 15 * time.Minute, Reraises: 3}

// stallFixture seeds one active worker, points the watcher at an idle pane and the test
// ladder, and positions the fake clock just after the task's creation. It returns the
// clock's starting instant, so tests can name sweep times as offsets from it.
func stallFixture(t *testing.T, id string) (*Watcher, *db.Store, *fakeClock, time.Time) {
	t.Helper()
	w, s, _, clk := newWatcher(t)
	seedActiveTask(t, s, id, "wk-"+id)
	w.stall.policy = testStallPolicy
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ waiting at the prompt"}
	}
	gt, _, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	clk.t = gt.Created.Add(time.Second)
	return w, s, clk, clk.t
}

// sweepUntil runs pollLiveness every step, advancing the fake clock, until it passes end.
func sweepUntil(t *testing.T, w *Watcher, clk *fakeClock, end time.Time, step time.Duration) {
	t.Helper()
	for !clk.t.After(end) {
		if err := w.pollLiveness(context.Background()); err != nil {
			t.Fatalf("pollLiveness at %v: %v", clk.t, err)
		}
		clk.t = clk.t.Add(step)
	}
}

type raised struct {
	at time.Time
	p  stallPayload
}

// stallRaises returns the task's stalled events, oldest first, with decoded payloads.
func stallRaises(t *testing.T, s *db.Store, id string) []raised {
	t.Helper()
	evs, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []raised
	for _, e := range evs {
		if e.EntityID != id || e.Type != db.EventStalled {
			continue
		}
		if !e.Actionable {
			t.Fatalf("stalled event #%d is not actionable", e.ID)
		}
		var p stallPayload
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			t.Fatalf("stalled payload %q: %v", e.Payload, err)
		}
		out = append(out, raised{at: e.TS, p: p})
	}
	return out
}

func countEvents(t *testing.T, s *db.Store, id, typ string) int {
	t.Helper()
	evs, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.EntityID == id && e.Type == typ {
			n++
		}
	}
	return n
}

// TestStall_FirstRaiseAfterThreshold: an idle pane on an active task with no progress
// for the threshold produces exactly one stalled update, and not a moment before.
// idle_unreported fires at the 5m dwell and, before the ladder, nothing more ever did.
func TestStall_FirstRaiseAfterThreshold(t *testing.T) {
	w, s, clk, start := stallFixture(t, "quiet")

	sweepUntil(t, w, clk, start.Add(10*time.Minute-time.Second), 30*time.Second)
	if got := stallRaises(t, s, "quiet"); len(got) != 0 {
		t.Fatalf("raised before the 10m threshold: %+v", got)
	}

	sweepUntil(t, w, clk, start.Add(24*time.Minute), 30*time.Second)
	got := stallRaises(t, s, "quiet")
	if len(got) != 1 {
		t.Fatalf("want exactly one stalled update between 10m and 24m, got %d: %+v", len(got), got)
	}
	r := got[0]
	if !r.at.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("raised at +%v, want +10m", r.at.Sub(start))
	}
	want := stallPayload{Level: stallLevelStalled, Raise: 1, Idle: "10m0s", Window: "wk-quiet"}
	if r.p != want {
		t.Fatalf("payload = %+v, want %+v", r.p, want)
	}
}

// TestStall_ReraiseCadenceThenNeedsInspection: a worker that stays silent is re-raised
// every 15m; after 3 re-raises the level becomes needs-inspection and stays there.
func TestStall_ReraiseCadenceThenNeedsInspection(t *testing.T) {
	w, s, clk, start := stallFixture(t, "silent")

	sweepUntil(t, w, clk, start.Add(90*time.Minute), 30*time.Second)
	got := stallRaises(t, s, "silent")
	wantAt := []time.Duration{10, 25, 40, 55, 70, 85}
	if len(got) != len(wantAt) {
		t.Fatalf("want %d updates by 90m, got %d: %+v", len(wantAt), len(got), got)
	}
	for i, r := range got {
		at := wantAt[i] * time.Minute
		level := stallLevelStalled
		if i > testStallPolicy.Reraises {
			level = stallLevelInspect
		}
		if !r.at.Equal(start.Add(at)) || r.p.Raise != i+1 || r.p.Level != level {
			t.Fatalf("update %d = +%v raise=%d level=%s, want +%v raise=%d level=%s",
				i, r.at.Sub(start), r.p.Raise, r.p.Level, at, i+1, level)
		}
		if r.p.Idle != at.String() {
			t.Fatalf("update %d idle = %s, want %s", i, r.p.Idle, at)
		}
	}
}

// TestStall_PaneChangeResetsLadder: a pane change after an update restarts the clock, so
// the next update is a fresh raise=1 a full threshold later, not the scheduled re-raise.
func TestStall_PaneChangeResetsLadder(t *testing.T) {
	w, s, clk, start := stallFixture(t, "moved")

	sweepUntil(t, w, clk, start.Add(12*time.Minute), 30*time.Second)
	if got := stallRaises(t, s, "moved"); len(got) != 1 {
		t.Fatalf("want the first update by 12m, got %+v", got)
	}

	// The worker prints something and settles at a new idle screen.
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "done with step 2\n$ waiting at the prompt"}
	}
	resetAt := clk.t
	sweepUntil(t, w, clk, start.Add(40*time.Minute), 30*time.Second)
	got := stallRaises(t, s, "moved")
	if len(got) != 3 {
		t.Fatalf("want the first update, a fresh raise and one re-raise by 40m, got %d: %+v", len(got), got)
	}
	if r := got[1]; !r.at.Equal(resetAt.Add(10*time.Minute)) || r.p.Raise != 1 || r.p.Idle != "10m0s" {
		t.Fatalf("after the pane change want raise=1 at +%v idle=10m0s, got raise=%d at +%v idle=%s",
			resetAt.Add(10*time.Minute).Sub(start), r.p.Raise, r.at.Sub(start), r.p.Idle)
	}
	if r := got[2]; r.p.Raise != 2 || !r.at.Equal(got[1].at.Add(15*time.Minute)) {
		t.Fatalf("want the re-raise 15m after the fresh raise, got raise=%d at +%v", r.p.Raise, r.at.Sub(start))
	}
}

// TestStall_ReportAndStatusChangeResetLadder: a worker stage update, and separately a
// status round trip (blocked, then active again), each restart the clock.
func TestStall_ReportAndStatusChangeResetLadder(t *testing.T) {
	cases := []struct {
		name     string
		progress func(t *testing.T, s *db.Store, id string)
	}{
		{"stage update", func(t *testing.T, s *db.Store, id string) {
			if _, err := s.SetStage(context.Background(), id, "rerunning tests", "worker:"+id); err != nil {
				t.Fatal(err)
			}
		}},
		{"status change", func(t *testing.T, s *db.Store, id string) {
			report(t, s, id, db.StatusBlocked, "need a decision")
			report(t, s, id, db.StatusActive, "")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, s, clk, start := stallFixture(t, "ack")
			sweepUntil(t, w, clk, start.Add(11*time.Minute), 30*time.Second)
			if got := stallRaises(t, s, "ack"); len(got) != 1 {
				t.Fatalf("want the first update by 11m, got %+v", got)
			}
			c.progress(t, s, "ack")
			resetAt := clk.t
			sweepUntil(t, w, clk, start.Add(30*time.Minute), 30*time.Second)
			got := stallRaises(t, s, "ack")
			if len(got) != 2 {
				t.Fatalf("want one fresh raise after the progress by 30m, got %d: %+v", len(got), got)
			}
			if r := got[1]; r.p.Raise != 1 || !r.at.Equal(resetAt.Add(10*time.Minute)) {
				t.Fatalf("want raise=1 at +%v, got raise=%d at +%v",
					resetAt.Add(10*time.Minute).Sub(start), r.p.Raise, r.at.Sub(start))
			}
		})
	}
}

// TestStall_BusyChangingPaneNeverEscalates: a worker mid-turn (busy indicator, pane
// changing every sweep) is never raised, however long it runs, and costs one clock row.
func TestStall_BusyChangingPaneNeverEscalates(t *testing.T) {
	w, s, clk, start := stallFixture(t, "busy")
	frame := 0
	w.capture = func(string) paneObservation {
		frame++
		return paneObservation{present: true, captured: true,
			pane: fmt.Sprintf("✶ Working… (%ds · esc to interrupt)", frame)}
	}
	sweepUntil(t, w, clk, start.Add(2*time.Hour), 30*time.Second)
	if got := stallRaises(t, s, "busy"); len(got) != 0 {
		t.Fatalf("a busy worker must never be raised, got %+v", got)
	}
	if n := countEvents(t, s, "busy", db.EventStallClock); n != 1 {
		t.Fatalf("a busy stretch should write exactly one stall_clock, got %d", n)
	}
}

// TestStall_BusyThenIdleStartsClockAtIdle: the clock runs from when the pane settled,
// not from the start of the busy stretch before it.
func TestStall_BusyThenIdleStartsClockAtIdle(t *testing.T) {
	w, s, clk, start := stallFixture(t, "turn")
	busyUntil := start.Add(30 * time.Minute)
	w.capture = func(string) paneObservation {
		if clk.t.Before(busyUntil) {
			return paneObservation{present: true, captured: true, pane: "✶ Thinking… (esc to interrupt)"}
		}
		return paneObservation{present: true, captured: true, pane: "$ waiting at the prompt"}
	}
	sweepUntil(t, w, clk, start.Add(39*time.Minute), 30*time.Second)
	if got := stallRaises(t, s, "turn"); len(got) != 0 {
		t.Fatalf("raised within 10m of the pane settling: %+v", got)
	}
	sweepUntil(t, w, clk, start.Add(41*time.Minute), 30*time.Second)
	if got := stallRaises(t, s, "turn"); len(got) != 1 {
		t.Fatalf("want one update 10m after the pane settled, got %+v", got)
	}
}

// TestStall_CommitCountsAsProgress: a new commit in the worktree restarts the clock even
// though the pane never changed. It runs against a real git repository through the
// production commit-time reader.
func TestStall_CommitCountsAsProgress(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	w, s, clk, start := stallFixture(t, "commits")
	repo := t.TempDir()
	gitAt := func(when time.Time, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "commit.gpgsign=false"}, args...)...)
		stamp := fmt.Sprintf("@%d +0000", when.Unix())
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_AUTHOR_DATE="+stamp,
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_COMMITTER_DATE="+stamp)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitAt(start.Add(-time.Hour), "init", "-q")
	gitAt(start.Add(-time.Hour), "commit", "-q", "--allow-empty", "-m", "base")
	if err := s.SetTaskFields(context.Background(), "commits", db.TaskFields{Worktree: &repo}); err != nil {
		t.Fatalf("SetTaskFields: %v", err)
	}

	sweepUntil(t, w, clk, start.Add(9*time.Minute), 30*time.Second)
	commitAt := clk.t
	gitAt(commitAt, "commit", "-q", "--allow-empty", "-m", "step 1")

	sweepUntil(t, w, clk, start.Add(15*time.Minute), 30*time.Second)
	if got := stallRaises(t, s, "commits"); len(got) != 0 {
		t.Fatalf("a commit at +9m must hold off the +10m update, got %+v", got)
	}
	// The commit is seen when the update comes due at +10m; the clock restarts there.
	sweepUntil(t, w, clk, start.Add(21*time.Minute), 30*time.Second)
	got := stallRaises(t, s, "commits")
	if len(got) != 1 || got[0].p.Raise != 1 || !got[0].at.Equal(start.Add(20*time.Minute)) {
		t.Fatalf("want one raise=1 at +20m (10m after the clock restarted), got %+v", got)
	}
}

// TestStall_SurvivesWatcherRestart: the ladder is on the event spine, so a watcher that
// exits and is re-armed picks up where the last one left off.
func TestStall_SurvivesWatcherRestart(t *testing.T) {
	w, s, clk, start := stallFixture(t, "rearm")
	sweepUntil(t, w, clk, start.Add(6*time.Minute), 30*time.Second)

	w2 := New(s, paths.Default(), w.Backend, "test-session")
	w2.now, w2.capture, w2.Stale = w.now, w.capture, w.Stale
	w2.stall.policy = testStallPolicy
	sweepUntil(t, w2, clk, start.Add(11*time.Minute), 30*time.Second)
	got := stallRaises(t, s, "rearm")
	if len(got) != 1 || !got[0].at.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("the re-armed watcher should raise at +10m on the persisted clock, got %+v", got)
	}
}

// TestStall_GoneWindowNotRaised: a gone window is window_gone's business, not the ladder's.
func TestStall_GoneWindowNotRaised(t *testing.T) {
	w, s, clk, start := stallFixture(t, "gone")
	w.capture = func(string) paneObservation { return paneObservation{} }
	sweepUntil(t, w, clk, start.Add(time.Hour), 30*time.Second)
	if got := stallRaises(t, s, "gone"); len(got) != 0 {
		t.Fatalf("a gone window must not be raised, got %+v", got)
	}
}

// TestStall_DisabledWritesNothing: with the ladder off, nothing is recorded at all.
func TestStall_DisabledWritesNothing(t *testing.T) {
	w, s, clk, start := stallFixture(t, "off")
	w.stall.policy.After = 0
	sweepUntil(t, w, clk, start.Add(time.Hour), 30*time.Second)
	if n := countEvents(t, s, "off", db.EventStallClock) + countEvents(t, s, "off", db.EventStalled); n != 0 {
		t.Fatalf("a disabled ladder wrote %d events", n)
	}
}

// TestStall_SurfacesInWatchBatch: a stalled update wakes an armed watcher and renders as
// its own line in the batch.
func TestStall_SurfacesInWatchBatch(t *testing.T) {
	w, s, buf, clk := newWatcher(t)
	seedActiveTask(t, s, "wake", "wk-wake")
	gt, _, err := s.GetTask(context.Background(), "wake")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = gt.Created.Add(time.Second)
	w.Poll = time.Second
	w.stall.policy = stallPolicy{After: 2 * time.Minute, Repeat: 15 * time.Minute, Reraises: 3}

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired || len(res.Batch) != 1 || res.Batch[0].Type != db.EventStalled {
		t.Fatalf("want a batch of one stalled update, got %+v", res)
	}
	out := buf.String()
	if !strings.Contains(out, "stalled               task=wake") || !strings.Contains(out, "window=wk-wake idle=2m0s raise=1") {
		t.Fatalf("batch output missing the stalled line:\n%s", out)
	}
}

func TestFormatStallLine_NeedsInspection(t *testing.T) {
	line := formatStallLine(db.Event{
		EntityID: "x", Payload: `{"level":"needs-inspection","raise":5,"idle":"1h10m0s","window":"wk-x"}`,
	}, " (#9)")
	want := "needs-inspection      task=x                  window=wk-x idle=1h10m0s raise=5 (#9)"
	if line != want {
		t.Fatalf("line =\n%q\nwant\n%q", line, want)
	}
}

// TestProgressSinceClock covers each progress signal the task row and event spine carry.
func TestProgressSinceClock(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	before, after := clock.Add(-time.Minute), clock.Add(time.Minute)
	lease := func(granted time.Time) *time.Time { e := granted.Add(db.DefaultLeaseDuration); return &e }
	st := db.StallState{HasClock: true, ClockID: 10, ClockAt: clock}
	cases := []struct {
		name string
		t    db.Task
		sol  int64
		want bool
	}{
		{"nothing newer", db.Task{LastProgressAt: &before, LeaseExpiresAt: lease(before)}, 9, false},
		{"newer sign of life", db.Task{}, 11, true},
		{"report after clock", db.Task{LastProgressAt: &after}, 0, true},
		{"lease extended after clock", db.Task{LeaseExpiresAt: lease(after)}, 0, true},
	}
	for _, c := range cases {
		st.SignOfLifeID = c.sol
		if got := progressSinceClock(c.t, st); got != c.want {
			t.Errorf("%s: progressSinceClock = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStallPolicyFromEnv(t *testing.T) {
	def := stallPolicy{After: defaultStallAfter, Repeat: defaultStallRepeat, Reraises: defaultStallReraises}
	cases := []struct {
		after, repeat, reraises string
		want                    stallPolicy
	}{
		{"", "", "", def},
		{"20m", "5m", "1", stallPolicy{After: 20 * time.Minute, Repeat: 5 * time.Minute, Reraises: 1}},
		{"off", "", "0", stallPolicy{After: 0, Repeat: defaultStallRepeat, Reraises: 0}},
		{"0", "", "", stallPolicy{After: 0, Repeat: defaultStallRepeat, Reraises: defaultStallReraises}},
		{"soon", "-1m", "-2", def},
	}
	for _, c := range cases {
		t.Setenv("TTORCH_STALL_AFTER", c.after)
		t.Setenv("TTORCH_STALL_REPEAT", c.repeat)
		t.Setenv("TTORCH_STALL_RERAISES", c.reraises)
		if got := stallPolicyFromEnv(); got != c.want {
			t.Errorf("env (%q,%q,%q) = %+v, want %+v", c.after, c.repeat, c.reraises, got, c.want)
		}
	}
}

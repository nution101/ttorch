package watch

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/backend"
	"github.com/nution101/ttorch/internal/db"
)

// Manager pane captures in the shape the harness renders them. idleManagerPane is a turn
// that ended with an empty "❯" prompt; the busy variants are the same screen mid-turn.
const (
	idleManagerPane = "⏺ All eight are in the backlog.\n\n" +
		"✻ Baked for 1m 18s · done 5:37 PM\n\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────\n" +
		"  [model] effort:medium | ctx 47% left\n" +
		"  ⏵⏵ bypass permissions on · 1 shell"
	spinnerManagerPane = "  Running 1 shell command…\n" +
		"  ⎿  $ ttorch tasks\n\n" +
		"· Pondering… (6m 13s · ↓ 19.0k tokens)\n\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on · 1 shell"
	interruptManagerPane = "✢ Cogitating… (12s · esc to interrupt)\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────"
	draftManagerPane = "✻ Baked for 1m 18s · done 5:37 PM\n\n" +
		"────────────────────────────────────────\n" +
		"❯ can you also check the\n" +
		"────────────────────────────────────────"
	boxedIdleManagerPane = "╭──────────────────────────────────────╮\n" +
		"│ > Try \"edit this file\"               │\n" +
		"╰──────────────────────────────────────╯\n" +
		"  ? for shortcuts"
)

// fakeManager is the tmux the Daemon sees for the manager window: what its pane shows,
// whether the window exists, the pane's foreground process, and every wake typed and
// submitted.
type fakeManager struct {
	present  bool
	pane     string
	fg       string // argv of the pane's foreground process group leader
	typed    int    // wake lines typed (attempts)
	sends    int    // Enter presses: wakes submitted
	typeErr  error
	enterErr error
	// afterType sets what the pane shows once the wake is typed; nil shows the wake alone in an
	// otherwise idle input. It may queue frames in renders, which successive captures return
	// in order before settling on the last.
	afterType func(f *fakeManager)
	renders   []string
}

// inputPane is an idle harness screen whose input box holds text: the first argument on the
// caret line, any further arguments on continuation lines.
func inputPane(first string, more ...string) string {
	var b strings.Builder
	b.WriteString("✻ Baked for 1m 18s · done 5:37 PM\n\n────────────────────────────────────────\n❯ " + first + "\n")
	for _, m := range more {
		b.WriteString("  " + m + "\n")
	}
	b.WriteString("────────────────────────────────────────\n  ⏵⏵ bypass permissions on · 1 shell")
	return b.String()
}

// harnessArgs is the manager pane's foreground leader as ps reports it for a running harness.
const harnessArgs = "claude --dangerously-skip-permissions --effort medium --model opus"

// newDaemon builds a Daemon over a fresh temp-home store with every tmux/gh seam faked:
// the watcher seams from newWatcher, plus a manager window that starts idle. Nothing here
// can reach a real tmux server.
func newDaemon(t *testing.T) (*Daemon, *db.Store, *fakeManager, *fakeClock) {
	t.Helper()
	w, s, _, clk := newWatcher(t)
	fake := &fakeManager{present: true, pane: idleManagerPane, fg: harnessArgs}
	return wireFakeDaemon(w, s, fake), s, fake, clk
}

// wireFakeDaemon builds a Daemon around an already-faked Watcher and fake manager window.
// Calling it again on the same store is how the tests model a scheduler restart: a new
// process with no in-memory state, over the same database.
func wireFakeDaemon(w *Watcher, s *db.Store, fake *fakeManager) *Daemon {
	workerCapture := w.capture
	w.capture = func(window string) paneObservation {
		if window != managerWindow {
			return workerCapture(window)
		}
		if !fake.present {
			return paneObservation{}
		}
		if len(fake.renders) > 0 {
			fake.pane, fake.renders = fake.renders[0], fake.renders[1:]
		}
		return paneObservation{present: true, captured: true, pane: fake.pane}
	}
	d := NewDaemon(s, w.P, w.Backend, w.Session, nil)
	d.w = w
	d.managerForeground = func() string { return fake.fg }
	d.typeWake = func() error {
		fake.typed++
		if fake.typeErr != nil {
			return fake.typeErr
		}
		if fake.afterType != nil {
			fake.afterType(fake)
		} else {
			fake.pane = inputPane(wakeLine)
		}
		return nil
	}
	d.pressEnter = func() error {
		fake.sends++
		if fake.enterErr != nil {
			return fake.enterErr
		}
		fake.pane = idleManagerPane
		return nil
	}
	return d
}

func tick(t *testing.T, d *Daemon) DaemonTick {
	t.Helper()
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	return res
}

func readInbox(t *testing.T, s *db.Store) (InboxResult, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := ReadInbox(context.Background(), s, &out)
	if err != nil {
		t.Fatalf("ReadInbox: %v", err)
	}
	return res, out.String()
}

// TestDaemon_WorkerTransitionRecordsAndWakesOnce: with no manual watcher armed, a worker
// reaching done, needs_input or blocked leaves an inbox entry and produces exactly one wake
// to an idle manager pane. The wake itself is recorded non-actionably, so it never shows up
// as an update.
func TestDaemon_WorkerTransitionRecordsAndWakesOnce(t *testing.T) {
	for _, status := range []string{db.StatusDone, db.StatusNeedsInput, db.StatusBlocked} {
		t.Run(status, func(t *testing.T) {
			d, s, fake, _ := newDaemon(t)
			seedActiveTask(t, s, "alpha", "wk-alpha")
			ev := report(t, s, "alpha", status, "over to you")

			res := tick(t, d)
			if !res.Woke || fake.sends != 1 {
				t.Fatalf("first sweep = %+v, sends=%d; want exactly one wake", res, fake.sends)
			}
			for i := 0; i < 5; i++ {
				tick(t, d)
			}
			if fake.sends != 1 {
				t.Fatalf("sends = %d after further sweeps with the same update, want 1", fake.sends)
			}

			woken, ok, err := s.LatestEvent(context.Background(), db.EntityTypeManager, managerEntityID, EventManagerWoken)
			if err != nil || !ok || woken.Actionable || woken.Payload != strconv.FormatInt(ev.ID, 10) {
				t.Fatalf("wake record = %+v ok=%v err=%v; want a non-actionable row announcing #%d", woken, ok, err, ev.ID)
			}

			inbox, text := readInbox(t, s)
			if len(inbox.Batch) != 1 || inbox.Batch[0].ID != ev.ID {
				t.Fatalf("inbox = %+v, want the %s update #%d only\n%s", inbox.Batch, status, ev.ID, text)
			}
		})
	}
}

// TestDaemon_NeverTypesIntoABusyPane: while the manager pane is mid-turn, holds a draft the
// lead is typing, has fallen back to a shell, cannot be identified, or is missing, no wake
// is typed and the update stays unread. The wake arrives on the first sweep that finds the
// pane idle.
func TestDaemon_NeverTypesIntoABusyPane(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		pane    string
		fg      string
	}{
		{"spinner line", true, spinnerManagerPane, harnessArgs},
		{"esc to interrupt", true, interruptManagerPane, harnessArgs},
		{"lead typing a draft", true, draftManagerPane, harnessArgs},
		{"harness exited to a shell", true, idleManagerPane, "zsh"},
		{"login shell", true, idleManagerPane, "-zsh"},
		{"ssh in the foreground", true, idleManagerPane, "ssh buildhost"},
		{"sudo in the foreground", true, idleManagerPane, "sudo -s"},
		{"unknown foreground", true, idleManagerPane, ""},
		{"no manager window", false, idleManagerPane, harnessArgs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, s, fake, clk := newDaemon(t)
			fake.present, fake.pane, fake.fg = c.present, c.pane, c.fg
			seedActiveTask(t, s, "alpha", "wk-alpha")
			report(t, s, "alpha", db.StatusDone, "")

			for i := 0; i < 5; i++ {
				res := tick(t, d)
				if res.Woke || res.Unread != 1 {
					t.Fatalf("sweep %d = %+v; want the update recorded and no wake", i, res)
				}
				clk.t = clk.t.Add(10 * time.Minute) // well past any re-wake cadence
			}
			if fake.typed != 0 {
				t.Fatalf("typed %d wake(s) into a pane that must not be typed into", fake.typed)
			}

			fake.present, fake.pane, fake.fg = true, idleManagerPane, harnessArgs
			if res := tick(t, d); !res.Woke || fake.sends != 1 {
				t.Fatalf("once idle: %+v sends=%d; want the wake now", res, fake.sends)
			}
		})
	}
}

// TestDaemon_CoalescesWhilePendingAndRewakesOnCadence: more updates while a wake is pending
// add no wakes; an unanswered wake is repeated once the Rewake cadence has passed, and only
// then.
func TestDaemon_CoalescesWhilePendingAndRewakesOnCadence(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	d.Rewake = 3 * time.Minute
	for _, id := range []string{"alpha", "beta", "gamma"} {
		seedActiveTask(t, s, id, "wk-"+id)
	}
	report(t, s, "alpha", db.StatusDone, "")
	if res := tick(t, d); !res.Woke {
		t.Fatalf("first sweep = %+v, want a wake", res)
	}

	report(t, s, "beta", db.StatusBlocked, "")
	report(t, s, "gamma", db.StatusNeedsInput, "")
	for i := 0; i < 17; i++ { // 17 × 10s = 2m50s, still inside the cadence
		clk.t = clk.t.Add(10 * time.Second)
		if res := tick(t, d); res.Woke {
			t.Fatalf("sweep at +%s woke again while a wake was pending: %+v", time.Duration(i+1)*10*time.Second, res)
		}
	}
	if fake.sends != 1 {
		t.Fatalf("sends = %d inside the cadence, want 1", fake.sends)
	}

	clk.t = clk.t.Add(10 * time.Second) // +3m00s
	if res := tick(t, d); !res.Woke || fake.sends != 2 {
		t.Fatalf("at the cadence: %+v sends=%d; want one re-wake", res, fake.sends)
	}
	tick(t, d)
	if fake.sends != 2 {
		t.Fatalf("sends = %d right after the re-wake, want 2", fake.sends)
	}
}

// TestDaemon_WakesAgainAtOnceAfterTheManagerReads: once the manager has consumed its inbox
// the pending wake is answered, so the next update wakes it on the next idle sweep without
// waiting out the cadence.
func TestDaemon_WakesAgainAtOnceAfterTheManagerReads(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	seedActiveTask(t, s, "beta", "wk-beta")
	report(t, s, "alpha", db.StatusDone, "")
	tick(t, d)
	readInbox(t, s)

	clk.t = clk.t.Add(5 * time.Second)
	report(t, s, "beta", db.StatusBlocked, "")
	if res := tick(t, d); !res.Woke || fake.sends != 2 {
		t.Fatalf("after the manager read its inbox: %+v sends=%d; want a fresh wake", res, fake.sends)
	}
}

// TestDaemon_AwaitingLeadRecordsOnly: while the manager is awaiting the lead nothing is
// typed, however long the update waits; clearing the flag lets the wake through.
func TestDaemon_AwaitingLeadRecordsOnly(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	report(t, s, "alpha", db.StatusDone, "")
	if err := s.SetAwaitingLead(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if res := tick(t, d); res.Woke || res.Unread != 1 {
			t.Fatalf("awaiting the lead: %+v; want recorded, not woken", res)
		}
		clk.t = clk.t.Add(time.Hour)
	}
	if err := s.SetAwaitingLead(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if res := tick(t, d); !res.Woke || fake.sends != 1 {
		t.Fatalf("after the lead returned: %+v sends=%d; want the wake", res, fake.sends)
	}
}

// TestDaemon_RestartLosesNothing: an update recorded while the old scheduler process could
// not wake the manager is still unread for the new one, which wakes on it; and a wake the old
// process already sent is still pending after the restart, so the new process does not
// repeat it early.
func TestDaemon_RestartLosesNothing(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	seedActiveTask(t, s, "beta", "wk-beta")
	fake.pane = spinnerManagerPane
	first := report(t, s, "alpha", db.StatusDone, "")
	if res := tick(t, d); res.Woke || res.Unread != 1 {
		t.Fatalf("before restart: %+v; want recorded, not woken", res)
	}

	fake.pane = idleManagerPane
	restarted := wireFakeDaemon(d.w, s, fake)
	if res := tick(t, restarted); !res.Woke || res.Unread != 1 {
		t.Fatalf("after restart: %+v; want the pre-restart update still unread and a wake", res)
	}

	second := report(t, s, "beta", db.StatusBlocked, "")
	clk.t = clk.t.Add(30 * time.Second)
	again := wireFakeDaemon(d.w, s, fake)
	if res := tick(t, again); res.Woke {
		t.Fatalf("a second restart inside the cadence repeated the pending wake: %+v", res)
	}
	if fake.sends != 1 {
		t.Fatalf("sends = %d across two restarts, want 1", fake.sends)
	}

	inbox, text := readInbox(t, s)
	if len(inbox.Batch) != 2 || inbox.Batch[0].ID != first.ID || inbox.Batch[1].ID != second.ID {
		t.Fatalf("inbox after restarts = %+v, want #%d and #%d\n%s", inbox.Batch, first.ID, second.ID, text)
	}
}

// TestDaemon_RecordsLivenessWithoutAManualWatcher: detection is the watcher's own. A worker
// whose window disappeared is recorded as window_gone by the loop alone and wakes the
// manager.
func TestDaemon_RecordsLivenessWithoutAManualWatcher(t *testing.T) {
	d, s, fake, _ := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	workers := d.w.capture
	d.w.capture = func(window string) paneObservation {
		if window == "wk-alpha" {
			return paneObservation{} // gone
		}
		return workers(window)
	}

	if res := tick(t, d); !res.Woke || fake.sends != 1 {
		t.Fatalf("sweep = %+v sends=%d; want window_gone recorded and a wake", res, fake.sends)
	}
	inbox, text := readInbox(t, s)
	if len(inbox.Batch) != 1 || inbox.Batch[0].Type != db.EventWindowGone {
		t.Fatalf("inbox = %+v, want one window_gone\n%s", inbox.Batch, text)
	}
}

// TestDaemon_FailedSendRetriesAfterBackoff: a send that fails (the pane in copy-mode, say)
// is not recorded as a wake, is not retried every sweep, and is retried once SendRetry has
// passed.
func TestDaemon_FailedSendRetriesAfterBackoff(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	d.SendRetry = 30 * time.Second
	seedActiveTask(t, s, "alpha", "wk-alpha")
	report(t, s, "alpha", db.StatusDone, "")
	fake.typeErr = errors.New("pane is in copy-mode")

	if res := tick(t, d); res.Woke || fake.typed != 1 {
		t.Fatalf("failed send: %+v typed=%d; want one attempt, not woken", res, fake.typed)
	}
	if _, ok, _ := s.LatestEvent(context.Background(), db.EntityTypeManager, managerEntityID, EventManagerWoken); ok {
		t.Fatal("a failed send was recorded as a wake")
	}
	for i := 0; i < 2; i++ {
		clk.t = clk.t.Add(10 * time.Second)
		tick(t, d)
	}
	if fake.typed != 1 {
		t.Fatalf("typed = %d inside the retry window, want 1", fake.typed)
	}

	fake.typeErr = nil
	clk.t = clk.t.Add(10 * time.Second)
	if res := tick(t, d); !res.Woke || fake.typed != 2 || fake.sends != 1 {
		t.Fatalf("after the retry window: %+v typed=%d sends=%d; want the wake", res, fake.typed, fake.sends)
	}
}

// fakeWatcherSeams gives w the same deterministic, tmux-free seams newWatcher installs, so a
// second Watcher can share another test's store and paths.
func fakeWatcherSeams(w *Watcher, clk *fakeClock) {
	w.Out = &bytes.Buffer{}
	w.now = clk.now
	w.Coalesce = time.Millisecond
	w.Poll = 10 * time.Millisecond
	w.Stale = -1
	w.managerPresent = func() bool { return true }
	w.ghAvailable = func() bool { return false }
	w.capture = func(string) paneObservation {
		return paneObservation{present: true, captured: true, pane: "$ idle at the prompt"}
	}
	w.nudge = func(string) error { return nil }
	w.isWatchProc = func(int) bool { return false }
	w.lockRetry = time.Millisecond
}

// TestDaemon_ManualWatchAndLoopNeverBothReport: while a hand-armed `ttorch watch` holds the
// singleton the loop stands down, the watcher reports the update, and the loop then finds
// nothing unread. In the other order, an update the loop woke the manager for and the
// manager read is not reported again by a watcher armed afterwards.
func TestDaemon_ManualWatchAndLoopNeverBothReport(t *testing.T) {
	d, s, fake, clk := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	seedActiveTask(t, s, "beta", "wk-beta")
	first := report(t, s, "alpha", db.StatusDone, "")

	manual := New(s, d.P, backend.Tmux{}, "test-session")
	fakeWatcherSeams(manual, clk)
	manual.Since = -1
	holding := make(chan struct{})
	release := make(chan struct{})
	coalescing := true
	manual.wait = func(ctx context.Context, dur time.Duration) error {
		if coalescing { // the watcher has seen the update and holds the flock
			coalescing = false
			close(holding)
			<-release
		}
		return nil
	}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := manual.Run(context.Background())
		done <- outcome{res, err}
	}()
	<-holding

	if res := tick(t, d); !res.Standby || res.Woke {
		t.Fatalf("loop sweep with a watcher armed = %+v; want standby, no wake", res)
	}
	close(release)
	got := <-done
	if got.err != nil || !got.res.Fired || len(got.res.Batch) != 1 || got.res.Batch[0].ID != first.ID {
		t.Fatalf("manual watcher = %+v err=%v; want it to report #%d", got.res, got.err, first.ID)
	}
	if res := tick(t, d); res.Woke || res.Unread != 0 {
		t.Fatalf("loop after the watcher reported = %+v; want nothing unread and no wake", res)
	}
	if fake.sends != 0 {
		t.Fatalf("loop typed %d wake(s) for an update the watcher reported", fake.sends)
	}

	second := report(t, s, "beta", db.StatusBlocked, "")
	if res := tick(t, d); !res.Woke {
		t.Fatalf("loop sweep for the second update = %+v, want a wake", res)
	}
	inbox, _ := readInbox(t, s)
	if len(inbox.Batch) != 1 || inbox.Batch[0].ID != second.ID {
		t.Fatalf("inbox = %+v, want only #%d", inbox.Batch, second.ID)
	}
	later := New(s, d.P, backend.Tmux{}, "test-session")
	fakeWatcherSeams(later, clk)
	later.Since = -1
	later.Timeout = time.Second
	calls := 0
	later.wait = func(ctx context.Context, dur time.Duration) error {
		calls++
		if calls > 1000 {
			return errors.New("watch loop did not terminate")
		}
		clk.t = clk.t.Add(dur)
		return nil
	}
	res, err := later.Run(context.Background())
	if err != nil || res.Fired || !res.TimedOut {
		t.Fatalf("watcher armed after the inbox read = %+v err=%v; want a quiet timeout", res, err)
	}
}

// TestDaemon_ManualArmWaitsOutASweep: a hand-armed `ttorch watch` that collides with a loop
// sweep holding the flock waits for the sweep to finish rather than being refused, and never
// signals the scheduler process.
func TestDaemon_ManualArmWaitsOutASweep(t *testing.T) {
	d, s, _, clk := newDaemon(t)
	seedActiveTask(t, s, "alpha", "wk-alpha")
	ev := report(t, s, "alpha", db.StatusDone, "")

	path := d.P.WatchPIDFile()
	lock, err := acquireFlock(path, daemonToken)
	if err != nil {
		t.Fatal(err)
	}
	// The scheduler is another process in production; record a pid that is not this test's.
	if err := os.WriteFile(path, []byte(formatWatchRecord(os.Getpid()+100000, daemonToken)), 0o600); err != nil {
		t.Fatal(err)
	}

	manual := New(s, d.P, backend.Tmux{}, "test-session")
	fakeWatcherSeams(manual, clk)
	manual.Since = -1
	manual.wait = func(ctx context.Context, dur time.Duration) error { return nil }
	manual.briefGrace = 20 * time.Millisecond
	manual.resetGrace = 5 * time.Second
	manual.procAlive = func(int) bool { return true }
	manual.sessionToken = func() string { return "pane:4242:Mon-Jun-29-00:16:32-2026" }
	signalled := false
	manual.kill = func(int) { signalled = true }

	go func() {
		time.Sleep(200 * time.Millisecond) // the sweep finishes
		releaseFlock(lock, path)
	}()
	res, err := manual.Run(context.Background())
	if err != nil {
		t.Fatalf("manual arm during a sweep was refused: %v", err)
	}
	if !res.Fired || res.Batch[0].ID != ev.ID {
		t.Fatalf("manual watcher = %+v, want it to report #%d", res, ev.ID)
	}
	if signalled {
		t.Fatal("the manual arm signalled the scheduler process")
	}
}

// TestManagerAtEmptyPrompt pins the pane classification the wake depends on.
func TestManagerAtEmptyPrompt(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"idle, current caret", idleManagerPane, true},
		{"idle, boxed placeholder", boxedIdleManagerPane, true},
		{"idle, bare caret", "done\n>", true},
		{"gerund spinner", spinnerManagerPane, false},
		{"esc to interrupt", interruptManagerPane, false},
		{"lead typing", draftManagerPane, false},
		{"legacy caret with draft", "│ > also check the tests │", false},
		{"permission menu", "Do you want to proceed?\n❯ 1. Yes\n  2. No", false},
		{"no prompt at all", "$ ", false},
		{"quote in history above an empty prompt", "> quoted line\n\n❯ ", true},
		{"empty prompt above a draft is not what counts", "❯ \n❯ draft", false},
		{"ascii spinner", "* Thinking... (3s)\n❯ ", false},
		{"finished turn line is not a spinner", "✻ Worked for 2m 3s\n❯ ", true},
		{"exact placeholder, current caret", inputPane(`Try "fix lint errors"`), true},
		{"placeholder followed by the lead's text", inputPane(`Try "fix lint errors" and then`), false},
		{"lead typing something that starts like the placeholder", inputPane(`Try "the other approach`), false},
		{"multi-line draft", inputPane("first line of a draft", "second line"), false},
		{"draft below an empty caret line", inputPane("", "continued text"), false},
		{"blank lines inside the input box", inputPane("", ""), true},
	}
	for _, c := range cases {
		if got := managerAtEmptyPrompt(c.pane); got != c.want {
			t.Errorf("%s: managerAtEmptyPrompt = %v, want %v\n%s", c.name, got, c.want, c.pane)
		}
	}
}

// TestNewDaemon_WiresProductionSeams: the constructor installs the real manager-window seams
// and the documented defaults. It does not call them, so no tmux is touched.
func TestNewDaemon_WiresProductionSeams(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	d := NewDaemon(s, w.P, w.Backend, "test-session", nil)
	if d.typeWake == nil || d.pressEnter == nil || d.managerForeground == nil || d.w == nil {
		t.Fatal("NewDaemon left a production seam unwired")
	}
	if d.poll() != defaultDaemonPoll || d.rewake() != defaultRewake || d.sendRetry() != defaultSendRetry {
		t.Fatalf("defaults = %s/%s/%s", d.poll(), d.rewake(), d.sendRetry())
	}
}

// TestWakeLineIsInertInAShell reads the literal wireManagerWake types and checks it carries
// no shell metacharacter, so even a wake that reached a shell prompt could only print
// "command not found". It parses daemon.go rather than trusting a copy of the string.
func TestWakeLineIsInertInAShell(t *testing.T) {
	line := wakeLiteralFromSource(t)
	if strings.ContainsAny(line, "`$'\"|;&<>()*?\\#!{}[]~") {
		t.Fatalf("wake line %q carries a shell metacharacter", line)
	}
	if !strings.Contains(line, "ttorch inbox") {
		t.Fatalf("wake line %q does not tell the manager to run ttorch inbox", line)
	}
}

// wakeLiteralFromSource returns the one literal payload wireManagerWake types into the
// manager window, read from daemon.go's AST rather than a copy of the string.
func wakeLiteralFromSource(t *testing.T) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "wireManagerWake" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "TypeLine" || len(call.Args) != 3 {
				return true
			}
			if lit, ok := call.Args[2].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				v, _ := strconv.Unquote(lit.Value)
				lines = append(lines, v)
			}
			return true
		})
	}
	if len(lines) != 1 {
		t.Fatalf("found %d literal TypeLine payload(s) in wireManagerWake, want 1", len(lines))
	}
	return lines[0]
}

// TestWakeLineCannotPassForTheLead: the wake is typed into the input the lead uses, so it must
// say it is automated and not from the lead, and must carry no instruction beyond naming the
// command. A line ending "act on them" read as the lead telling the manager to act on whatever
// worker text followed.
func TestWakeLineCannotPassForTheLead(t *testing.T) {
	line := wakeLiteralFromSource(t)
	low := strings.ToLower(line)
	for _, want := range []string{"automated", "not the lead", "run ttorch inbox"} {
		if !strings.Contains(low, want) {
			t.Errorf("wake line %q does not say %q", line, want)
		}
	}
	for _, banned := range []string{"act on", "approve", "land", "merge"} {
		if strings.Contains(low, banned) {
			t.Errorf("wake line %q carries an instruction (%q)", line, banned)
		}
	}
}

// TestIsHarnessCommand pins the foreground allowlist: only the harness itself passes; shells,
// ssh, sudo, editors, other node programs and an unreadable leader all refuse.
func TestIsHarnessCommand(t *testing.T) {
	cases := []struct {
		args string
		want bool
	}{
		{harnessArgs, true},
		{"/Users/x/.local/bin/claude --resume abc", true},
		{"/Users/x/.local/share/claude/versions/2.1.281 --model opus", true},
		{"node /usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js", true},
		{"node --no-warnings /opt/homebrew/bin/claude", true},
		{"bun /Users/x/.bun/bin/claude", true},
		{"", false},
		{"zsh", false},
		{"-zsh", false},
		{"bash --login", false},
		{"ssh buildhost", false},
		{"sudo -s", false},
		{"sudo claude", false},
		{"vim claude.md", false},
		{"node server.js", false},
		{"node --inspect", false},
		{"claude-wrapper --x", false},
		{"python3 claude", false},
	}
	for _, c := range cases {
		if got := isHarnessCommand(c.args); got != c.want {
			t.Errorf("isHarnessCommand(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestWakeLiteralMatchesWakeLine: Tick compares the pane against wakeLine before pressing
// Enter, so it must be exactly the literal wireManagerWake types.
func TestWakeLiteralMatchesWakeLine(t *testing.T) {
	if got := wakeLiteralFromSource(t); got != wakeLine {
		t.Fatalf("wireManagerWake types %q but Tick confirms against %q", got, wakeLine)
	}
}

// TestDaemon_DoesNotSubmitWhenThePromptChanges: the lead starts typing in the gap between the
// idle check and the wake's keys landing, or the manager wakes on its own. Whatever the input
// then holds, if it is not exactly the wake line Enter is never pressed and nothing is recorded
// as a wake. The prompt now holds text, so later sweeps type nothing more.
func TestDaemon_DoesNotSubmitWhenThePromptChanges(t *testing.T) {
	cases := map[string]string{
		"lead keystroke before the wake": inputPane("x" + wakeLine),
		"lead keystrokes after the wake": inputPane(wakeLine + " and also check"),
		"lead's multi-line draft":        inputPane(wakeLine, "please look at this"),
		"manager went busy":              strings.Replace(spinnerManagerPane, "❯ ", "❯ "+wakeLine, 1),
		"prompt gone":                    "$ ",
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			d, s, fake, clk := newDaemon(t)
			fake.afterType = func(f *fakeManager) { f.pane = after }
			seedActiveTask(t, s, "alpha", "wk-alpha")
			report(t, s, "alpha", db.StatusDone, "")

			res := tick(t, d)
			if res.Woke || fake.typed != 1 || fake.sends != 0 {
				t.Fatalf("sweep = %+v typed=%d enters=%d; want typed once and never submitted", res, fake.typed, fake.sends)
			}
			if _, ok, _ := s.LatestEvent(context.Background(), db.EntityTypeManager, managerEntityID, EventManagerWoken); ok {
				t.Fatal("an unsubmitted wake was recorded as a wake")
			}
			for i := 0; i < 3; i++ {
				clk.t = clk.t.Add(10 * time.Minute)
				tick(t, d)
			}
			if fake.typed != 1 || fake.sends != 0 {
				t.Fatalf("later sweeps typed=%d enters=%d; want nothing more while the prompt holds text", fake.typed, fake.sends)
			}
		})
	}
}

// TestDaemon_SubmitsOnceTheTypedWakeRenders: the input can lag the keys, showing only part of
// the line at first. The loop reads again and presses Enter once the whole line is there; a
// line that never completes is not submitted.
func TestDaemon_SubmitsOnceTheTypedWakeRenders(t *testing.T) {
	d, s, fake, _ := newDaemon(t)
	fake.afterType = func(f *fakeManager) {
		f.renders = []string{inputPane(wakeLine[:12]), inputPane(wakeLine[:50]), inputPane(wakeLine)}
	}
	seedActiveTask(t, s, "alpha", "wk-alpha")
	report(t, s, "alpha", db.StatusDone, "")
	if res := tick(t, d); !res.Woke || fake.sends != 1 {
		t.Fatalf("lagging render: %+v enters=%d; want the wake submitted once complete", res, fake.sends)
	}

	d2, s2, fake2, _ := newDaemon(t)
	fake2.afterType = func(f *fakeManager) { f.pane = inputPane(wakeLine[:40]) }
	seedActiveTask(t, s2, "alpha", "wk-alpha")
	report(t, s2, "alpha", db.StatusDone, "")
	if res := tick(t, d2); res.Woke || fake2.sends != 0 {
		t.Fatalf("never-complete render: %+v enters=%d; want no Enter", res, fake2.sends)
	}
}

// TestDaemon_SubmitsAWrappedWake: in a narrow pane the wake wraps onto a second input line at a
// space. That is still exactly the wake line, so it is submitted.
func TestDaemon_SubmitsAWrappedWake(t *testing.T) {
	d, s, fake, _ := newDaemon(t)
	cut := strings.LastIndex(wakeLine[:60], " ")
	fake.afterType = func(f *fakeManager) { f.pane = inputPane(wakeLine[:cut], wakeLine[cut+1:]) }
	seedActiveTask(t, s, "alpha", "wk-alpha")
	report(t, s, "alpha", db.StatusDone, "")
	if res := tick(t, d); !res.Woke || fake.sends != 1 {
		t.Fatalf("wrapped wake: %+v enters=%d; want it submitted", res, fake.sends)
	}
}

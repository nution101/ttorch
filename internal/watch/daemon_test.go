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
// whether the window exists, the pane's foreground command, and every wake typed into it.
type fakeManager struct {
	present bool
	pane    string
	cmd     string
	sends   int
	sendErr error
}

// newDaemon builds a Daemon over a fresh temp-home store with every tmux/gh seam faked:
// the watcher seams from newWatcher, plus a manager window that starts idle. Nothing here
// can reach a real tmux server.
func newDaemon(t *testing.T) (*Daemon, *db.Store, *fakeManager, *fakeClock) {
	t.Helper()
	w, s, _, clk := newWatcher(t)
	fake := &fakeManager{present: true, pane: idleManagerPane, cmd: "2.1.281"}
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
		return paneObservation{present: true, captured: true, pane: fake.pane}
	}
	d := NewDaemon(s, w.P, w.Backend, w.Session, nil)
	d.w = w
	d.managerCommand = func() string { return fake.cmd }
	d.sendWake = func() error {
		fake.sends++
		return fake.sendErr
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
		cmd     string
	}{
		{"spinner line", true, spinnerManagerPane, "2.1.281"},
		{"esc to interrupt", true, interruptManagerPane, "2.1.281"},
		{"lead typing a draft", true, draftManagerPane, "2.1.281"},
		{"harness exited to a shell", true, idleManagerPane, "zsh"},
		{"login shell", true, idleManagerPane, "-zsh"},
		{"unknown foreground command", true, idleManagerPane, ""},
		{"no manager window", false, idleManagerPane, "2.1.281"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, s, fake, clk := newDaemon(t)
			fake.present, fake.pane, fake.cmd = c.present, c.pane, c.cmd
			seedActiveTask(t, s, "alpha", "wk-alpha")
			report(t, s, "alpha", db.StatusDone, "")

			for i := 0; i < 5; i++ {
				res := tick(t, d)
				if res.Woke || res.Unread != 1 {
					t.Fatalf("sweep %d = %+v; want the update recorded and no wake", i, res)
				}
				clk.t = clk.t.Add(10 * time.Minute) // well past any re-wake cadence
			}
			if fake.sends != 0 {
				t.Fatalf("typed %d wake(s) into a pane that must not be typed into", fake.sends)
			}

			fake.present, fake.pane, fake.cmd = true, idleManagerPane, "2.1.281"
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
	fake.sendErr = errors.New("pane is in copy-mode")

	if res := tick(t, d); res.Woke || fake.sends != 1 {
		t.Fatalf("failed send: %+v sends=%d; want one attempt, not woken", res, fake.sends)
	}
	if _, ok, _ := s.LatestEvent(context.Background(), db.EntityTypeManager, managerEntityID, EventManagerWoken); ok {
		t.Fatal("a failed send was recorded as a wake")
	}
	for i := 0; i < 2; i++ {
		clk.t = clk.t.Add(10 * time.Second)
		tick(t, d)
	}
	if fake.sends != 1 {
		t.Fatalf("sends = %d inside the retry window, want 1", fake.sends)
	}

	fake.sendErr = nil
	clk.t = clk.t.Add(10 * time.Second)
	if res := tick(t, d); !res.Woke || fake.sends != 2 {
		t.Fatalf("after the retry window: %+v sends=%d; want the wake", res, fake.sends)
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
	if d.sendWake == nil || d.managerCommand == nil || d.w == nil {
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
			if !ok || sel.Sel.Name != "SendLine" || len(call.Args) != 3 {
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
		t.Fatalf("found %d literal SendLine payload(s) in wireManagerWake, want 1", len(lines))
	}
	if strings.ContainsAny(lines[0], "`$'\"|;&<>()*?\\#!{}[]~") {
		t.Fatalf("wake line %q carries a shell metacharacter", lines[0])
	}
	if !strings.Contains(lines[0], "ttorch inbox") {
		t.Fatalf("wake line %q does not tell the manager to run ttorch inbox", lines[0])
	}
}

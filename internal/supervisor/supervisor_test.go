package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/wake"
)

func TestTick_SignalsHeartbeatAndDedup(t *testing.T) {
	// This test is about the wake-queue, not the auto-driver: opt out so the default
	// poke seam never reaches a live tmux/manager window on a dev box.
	t.Setenv("TTORCH_NO_AUTODRIVE", "1")
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(p)
	q := wake.Queue{Path: p.WakeQueue()}

	// A worker reports a turn boundary.
	if err := os.WriteFile(p.TurnEndMarker("t1"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	s.tick()
	ws, _ := q.Drain()
	if !hasWake(ws, "signal", "t1") {
		t.Fatalf("expected a signal wake for t1, got %+v", ws)
	}

	// Unchanged file -> no repeat signal.
	s.tick()
	ws, _ = q.Drain()
	if hasWake(ws, "signal", "t1") {
		t.Fatal("signal re-fired for an unchanged marker")
	}

	// Heartbeat fires when due.
	s.lastHeartbeat = time.Now().Add(-time.Hour)
	s.tick()
	ws, _ = q.Drain()
	if !hasWake(ws, "heartbeat", "") {
		t.Fatalf("expected a heartbeat wake, got %+v", ws)
	}
}

func hasWake(ws []wake.Wake, kind, key string) bool {
	for _, w := range ws {
		if w.Kind == kind && w.Key == key {
			return true
		}
	}
	return false
}

// fakeManager stands in for the manager window so the auto-driver tests can count
// pokes and steer the manager's presence/busy state without a live tmux.
type fakeManager struct {
	pokes int
	live  bool
	busy  bool
}

// newAutodriveSup builds a Supervisor whose poke seams are wired to a fakeManager
// (present and idle by default), so no test ever reaches real tmux.
func newAutodriveSup(t *testing.T) (*Supervisor, *fakeManager) {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(p)
	fm := &fakeManager{live: true}
	s.sendPoke = func() error { fm.pokes++; return nil }
	s.inspectManager = func() (bool, bool) { return fm.live, fm.busy }
	return s, fm
}

// TestAutodrive_ActionableWakePokesOnce: a worker turn-end pokes the manager once.
func TestAutodrive_ActionableWakePokesOnce(t *testing.T) {
	s, fm := newAutodriveSup(t)
	if err := os.WriteFile(s.P.TurnEndMarker("t1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.tick()
	if fm.pokes != 1 {
		t.Fatalf("a worker turn-end should poke the manager exactly once, got %d", fm.pokes)
	}
}

// TestAutodrive_SecondWakeWithinCooldownSuppressed: the debounce drops a second poke
// inside the cooldown, then delivers the coalesced poke once the cooldown elapses.
func TestAutodrive_SecondWakeWithinCooldownSuppressed(t *testing.T) {
	s, fm := newAutodriveSup(t)
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }
	s.Cfg.PokeCooldown = 25 * time.Second

	s.requestPoke()
	if fm.pokes != 1 {
		t.Fatalf("first actionable wake should poke, got %d", fm.pokes)
	}

	clock = clock.Add(10 * time.Second) // still within the cooldown
	s.requestPoke()
	if fm.pokes != 1 {
		t.Fatalf("a second wake within the cooldown must be suppressed, got %d pokes", fm.pokes)
	}

	clock = clock.Add(20 * time.Second) // now 30s after the first poke, cooldown elapsed
	s.flushPoke()
	if fm.pokes != 2 {
		t.Fatalf("after the cooldown the coalesced poke should fire once, got %d", fm.pokes)
	}
}

// TestAutodrive_BusyManagerDefersUntilIdle: a busy manager is never poked; the poke
// lands once it goes idle.
func TestAutodrive_BusyManagerDefersUntilIdle(t *testing.T) {
	s, fm := newAutodriveSup(t)
	fm.busy = true

	s.requestPoke()
	if fm.pokes != 0 {
		t.Fatalf("a busy manager must not be poked, got %d", fm.pokes)
	}

	fm.busy = false
	s.flushPoke()
	if fm.pokes != 1 {
		t.Fatalf("once the manager is idle the coalesced poke should fire, got %d", fm.pokes)
	}
}

// TestAutodrive_HeartbeatPokes: the heartbeat backstop pokes even with no event.
func TestAutodrive_HeartbeatPokes(t *testing.T) {
	s, fm := newAutodriveSup(t)
	s.lastHeartbeat = time.Now().Add(-time.Hour) // due
	s.tick()
	if fm.pokes != 1 {
		t.Fatalf("the heartbeat backstop should poke the manager, got %d", fm.pokes)
	}
}

// TestAutodrive_OptOutDisables: TTORCH_NO_AUTODRIVE suppresses every poke while the
// wake-queue keeps filling.
func TestAutodrive_OptOutDisables(t *testing.T) {
	t.Setenv("TTORCH_NO_AUTODRIVE", "1")
	s, fm := newAutodriveSup(t)
	if err := os.WriteFile(s.P.TurnEndMarker("t1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.lastHeartbeat = time.Now().Add(-time.Hour) // heartbeat also due
	s.tick()
	if fm.pokes != 0 {
		t.Fatalf("TTORCH_NO_AUTODRIVE must disable all pokes, got %d", fm.pokes)
	}

	// The queue still received the events — opt-out gates poking, not queueing.
	q := wake.Queue{Path: s.P.WakeQueue()}
	ws, _ := q.Drain()
	if !hasWake(ws, "signal", "t1") || !hasWake(ws, "heartbeat", "") {
		t.Fatalf("opt-out should still queue wakes, got %+v", ws)
	}
}

// labeledTab records one tab-label write so the coloring tests can assert the exact
// title strings the supervisor pushed to tmux, in order.
type labeledTab struct {
	window string
	label  string
}

// newLabelSup builds a Supervisor whose tab-coloring seams are driven by an in-memory
// pane map (window -> pane text) and record every label write, so no test reaches a
// live tmux. A window absent from the pane map is treated as gone: capture returns
// ok=false. The returned slice pointer accumulates label writes in order.
func newLabelSup(t *testing.T) (s *Supervisor, panes map[string]string, writes *[]labeledTab) {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s = New(p)
	panes = map[string]string{}
	recorded := []labeledTab{}
	s.captureWorker = func(window string) (string, bool) {
		out, ok := panes[window]
		return out, ok
	}
	s.labelWindow = func(window, label string) error {
		recorded = append(recorded, labeledTab{window, label})
		return nil
	}
	return s, panes, &recorded
}

func mustSaveTask(t *testing.T, s *Supervisor, task state.Task) {
	t.Helper()
	if err := s.Store.Save(task); err != nil {
		t.Fatal(err)
	}
}

// lastLabel returns the most recent label written for a window, or "" if none.
func lastLabel(writes []labeledTab, window string) string {
	last := ""
	for _, w := range writes {
		if w.window == window {
			last = w.label
		}
	}
	return last
}

// TestScanLabels_GlyphPerState: a busy worker gets the blue glyph, an idle one the
// amber glyph — both prefixed onto the task id.
func TestScanLabels_GlyphPerState(t *testing.T) {
	s, panes, writes := newLabelSup(t)
	mustSaveTask(t, s, state.Task{ID: "trust-gate", Window: "w-trust-gate", Kind: "ship"})
	mustSaveTask(t, s, state.Task{ID: "scout-trust-surface", Window: "w-scout", Kind: "scout"})
	panes["w-trust-gate"] = "… esc to interrupt …" // a busy indicator
	panes["w-scout"] = "$ waiting at the prompt"   // no busy indicator -> idle

	s.scanLabels()

	if got := lastLabel(*writes, "w-trust-gate"); got != "🔵 trust-gate" {
		t.Fatalf("busy worker label = %q, want %q", got, "🔵 trust-gate")
	}
	if got := lastLabel(*writes, "w-scout"); got != "🟡 scout-trust-surface" {
		t.Fatalf("idle worker label = %q, want %q", got, "🟡 scout-trust-surface")
	}
}

// TestScanLabels_OnlyWritesOnChange: identical state across ticks is not relabeled;
// a state change writes exactly one new label.
func TestScanLabels_OnlyWritesOnChange(t *testing.T) {
	s, panes, writes := newLabelSup(t)
	mustSaveTask(t, s, state.Task{ID: "t1", Window: "w1", Kind: "ship"})
	panes["w1"] = "thinking…" // busy

	s.scanLabels()
	s.scanLabels() // identical state — must not relabel
	if n := len(*writes); n != 1 {
		t.Fatalf("unchanged state relabeled %d times, want 1", n)
	}

	panes["w1"] = "$ back at the prompt" // now idle
	s.scanLabels()
	if n := len(*writes); n != 2 {
		t.Fatalf("a state change should add exactly one relabel, got %d total", n)
	}
	if last := lastLabel(*writes, "w1"); last != "🟡 t1" {
		t.Fatalf("idle relabel = %q, want %q", last, "🟡 t1")
	}
}

// TestScanLabels_SkipsManagerAndCC: the manager window and attached cc sessions are
// not ttorch workers and are never colored, even when busy.
func TestScanLabels_SkipsManagerAndCC(t *testing.T) {
	s, panes, writes := newLabelSup(t)
	mustSaveTask(t, s, state.Task{ID: "mgr", Window: managerWindow, Kind: "ship"})
	mustSaveTask(t, s, state.Task{ID: "attached", Window: "w-cc", Kind: "cc"})
	mustSaveTask(t, s, state.Task{ID: "real", Window: "w-real", Kind: "ship"})
	panes[managerWindow] = "thinking…"
	panes["w-cc"] = "thinking…"
	panes["w-real"] = "thinking…"

	s.scanLabels()

	if n := len(*writes); n != 1 {
		t.Fatalf("only the real worker should be labeled, got %d writes: %+v", n, *writes)
	}
	if (*writes)[0].window != "w-real" {
		t.Fatalf("labeled window = %q, want w-real", (*writes)[0].window)
	}
}

// TestScanLabels_SkipsUnreadableWindow: a worker whose window has gone (capture
// fails) keeps its last label rather than being relabeled blind.
func TestScanLabels_SkipsUnreadableWindow(t *testing.T) {
	s, _, writes := newLabelSup(t)
	mustSaveTask(t, s, state.Task{ID: "gone", Window: "w-gone", Kind: "ship"})
	// No pane entry for w-gone -> captureWorker returns ok=false.

	s.scanLabels()

	if n := len(*writes); n != 0 {
		t.Fatalf("a gone window must not be labeled, got %d writes", n)
	}
}

// TestScanLabels_RetriesAfterLabelError: a failed label write is left unrecorded, so
// the next tick retries it even though the state is unchanged (best-effort).
func TestScanLabels_RetriesAfterLabelError(t *testing.T) {
	s, panes, _ := newLabelSup(t)
	mustSaveTask(t, s, state.Task{ID: "t1", Window: "w1", Kind: "ship"})
	panes["w1"] = "working…"
	calls := 0
	s.labelWindow = func(window, label string) error {
		calls++
		if calls == 1 {
			return errors.New("tmux blip")
		}
		return nil
	}

	s.scanLabels() // first attempt fails -> glyph not recorded
	s.scanLabels() // same state -> must retry because the failure wasn't recorded
	if calls != 2 {
		t.Fatalf("a failed label write should be retried next tick, got %d calls", calls)
	}
}

// TestAcquire covers claiming a slot that has no live lock-holder: a fresh slot, a
// stale dead-pid file, an empty file (the crash-mid-write case the file-only design
// wedged on), and a garbage file. The advisory lock is free in every case, so
// acquire must succeed and record our pid — flock reclaims automatically, with no
// liveness guessing and no pid-reuse hazard.
func TestAcquire(t *testing.T) {
	cases := []struct {
		name     string
		prewrite func(t *testing.T, p paths.Paths)
	}{
		{name: "fresh slot"},
		{name: "stale dead pid", prewrite: func(t *testing.T, p paths.Paths) { writePID(t, p, deadPID(t)) }},
		{name: "empty file", prewrite: func(t *testing.T, p paths.Paths) { writeRaw(t, p, "") }},
		{name: "garbage file", prewrite: func(t *testing.T, p paths.Paths) { writeRaw(t, p, "not-a-pid\n") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TTORCH_HOME", t.TempDir())
			p := paths.Default()
			if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.prewrite != nil {
				tc.prewrite(t, p)
			}
			s := New(p)
			if err := s.acquire(); err != nil {
				t.Fatalf("acquire: unexpected error: %v", err)
			}
			defer s.release()
			if got := readPID(t, p); got != os.Getpid() {
				t.Fatalf("pid file holder = %d, want our pid %d", got, os.Getpid())
			}
		})
	}
}

// TestAcquire_HeldByLiveHolderLoses asserts that while another holder owns the lock,
// acquire loses with AlreadyRunningError and does not overwrite the holder's pid.
// The holder is a second open descriptor in this process: flock treats descriptors
// independently, so it contends even within one process.
func TestAcquire_HeldByLiveHolderLoses(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(p.PIDFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	if _, err := holder.WriteAt([]byte(strconv.Itoa(os.Getppid())), 0); err != nil {
		t.Fatal(err)
	}

	err = New(p).acquire()
	var running *AlreadyRunningError
	if !errors.As(err, &running) {
		t.Fatalf("acquire while held: want AlreadyRunningError, got %v", err)
	}
	if got := readPID(t, p); got != os.Getppid() {
		t.Fatalf("acquire stole a live holder's claim: pid file = %d, want %d", got, os.Getppid())
	}
}

// TestAcquire_IdempotentForSameSupervisor asserts a second acquire on a Supervisor
// that already holds the lock is a no-op success (not a self-deadlock).
func TestAcquire_IdempotentForSameSupervisor(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	s := New(paths.Default())
	if err := s.acquire(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer s.release()
	if err := s.acquire(); err != nil {
		t.Fatalf("second acquire on the holder: want nil, got %v", err)
	}
}

// TestAcquire_PIDFileMode verifies the claimed pid file is private (0o600).
func TestAcquire_PIDFileMode(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	s := New(p)
	if err := s.acquire(); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer s.release()
	fi, err := os.Stat(p.PIDFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("pid file mode = %#o, want 0600", perm)
	}
}

// TestRelease covers both branches of the ownership-aware release.
func TestRelease(t *testing.T) {
	t.Run("removes the pid file when it still names us", func(t *testing.T) {
		t.Setenv("TTORCH_HOME", t.TempDir())
		p := paths.Default()
		s := New(p)
		if err := s.acquire(); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		s.release()
		if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
			t.Fatalf("release left the pid file behind (stat err = %v)", err)
		}
	})
	t.Run("preserves a successor's claim", func(t *testing.T) {
		t.Setenv("TTORCH_HOME", t.TempDir())
		p := paths.Default()
		s := New(p)
		if err := s.acquire(); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		// A successor took over and recorded its own (different) pid; our release
		// must not delete the newcomer's claim.
		writePID(t, p, os.Getppid())
		s.release()
		if got := readPID(t, p); got != os.Getppid() {
			t.Fatalf("release clobbered a successor's claim: pid file = %d, want %d", got, os.Getppid())
		}
	})
}

// TestAcquire_ConcurrentExactlyOneWinner races many independent claimants in one
// process against a pre-seeded stale (dead-pid) file — the reclaim-under-contention
// scenario. Exactly one must win and the rest must lose with AlreadyRunningError.
// Each claimant opens its own descriptor, so flock genuinely arbitrates between
// them; the Supervisors are kept referenced so a winner's fd is not GC-closed
// mid-race.
func TestAcquire_ConcurrentExactlyOneWinner(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, p, deadPID(t)) // stale slot the racers must reclaim safely

	const n = 16
	sups := make([]*Supervisor, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sups[i] = New(p)
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = sups[i].acquire() }(i)
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		var running *AlreadyRunningError
		if !errors.As(err, &running) {
			t.Fatalf("racer %d: a loser must return AlreadyRunningError, got %v", i, err)
		}
	}
	for _, s := range sups {
		s.release() // also keeps sups (and the winner's fd) alive until here
	}
	if winners != 1 {
		t.Fatalf("want exactly one winner among %d racers, got %d", n, winners)
	}
}

// TestAcquire_ConcurrentProcessesExactlyOneWins is the real cross-process proof: it
// re-execs this test binary as several child daemons that all race acquire on a
// shared TTORCH_HOME pre-seeded with a stale dead-pid file. Exactly one must win;
// the rest must lose cleanly. The winner holds the lock for a beat so every sibling
// attempts its claim while the lock is genuinely held.
func TestAcquire_ConcurrentProcessesExactlyOneWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, p, deadPID(t)) // children race the reclaim path, not just an empty slot

	const n = 8
	env := append(os.Environ(), "TTORCH_ACQUIRE_HELPER=1") // TTORCH_HOME already names home
	cmds := make([]*exec.Cmd, n)
	for i := range cmds {
		c := exec.Command(os.Args[0], "-test.run=^TestAcquireRaceHelper$")
		c.Env = env
		cmds[i] = c
	}
	for i, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
	}

	winners := 0
	for i, c := range cmds {
		err := c.Wait()
		if err == nil {
			winners++
			continue
		}
		var ee *exec.ExitError
		// Exit 3 is a clean loss (AlreadyRunningError); any other code means the child
		// failed in an unexpected way and must surface as a test failure.
		if errors.As(err, &ee) && ee.ExitCode() == 3 {
			continue
		}
		t.Fatalf("child %d failed unexpectedly: %v", i, err)
	}
	if winners != 1 {
		t.Fatalf("want exactly one winner across %d daemons, got %d", n, winners)
	}
}

// TestAcquireRaceHelper is the child half of the cross-process race test. It is a
// no-op unless re-executed by the parent with TTORCH_ACQUIRE_HELPER set. It exits 3
// only on a clean loss (AlreadyRunningError) and 4 on any other failure, so the
// parent can tell a legitimate loser from a genuine bug.
func TestAcquireRaceHelper(t *testing.T) {
	if os.Getenv("TTORCH_ACQUIRE_HELPER") == "" {
		return
	}
	s := New(paths.Default())
	if err := s.acquire(); err != nil {
		var running *AlreadyRunningError
		if errors.As(err, &running) {
			os.Exit(3) // a clean loser
		}
		os.Exit(4) // an unexpected acquire failure
	}
	// Won: hold the lock long enough that every losing sibling, which calls acquire
	// on startup within a few ms, attempts while we still hold it. The KeepAlive
	// stops the GC from finalizing (and closing) the locked descriptor mid-hold.
	time.Sleep(time.Second)
	runtime.KeepAlive(s)
	os.Exit(0)
}

// TestLockedLiveFile covers the inode-verification guard that keeps acquire from
// holding a lock on an orphaned (unlinked or replaced) pid file.
func TestLockedLiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !lockedLiveFile(f, path) {
		t.Fatal("a freshly opened file should match its own path")
	}

	// Unlink the path: the descriptor now points at an orphaned inode.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if lockedLiveFile(f, path) {
		t.Fatal("an unlinked descriptor must not be reported as the live file")
	}

	// Recreate a different inode at the same path.
	g, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if lockedLiveFile(f, path) {
		t.Fatal("the old inode must not match a replacement at the path")
	}
	if !lockedLiveFile(g, path) {
		t.Fatal("the descriptor for the current inode should match")
	}
}

func writePID(t *testing.T, p paths.Paths, pid int) {
	t.Helper()
	writeRaw(t, p, strconv.Itoa(pid))
}

func writeRaw(t *testing.T, p paths.Paths, s string) {
	t.Helper()
	if err := os.WriteFile(p.PIDFile(), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPID(t *testing.T, p paths.Paths) int {
	t.Helper()
	pid, ok := PID(p)
	if !ok {
		t.Fatalf("pid file unreadable")
	}
	return pid
}

// deadPID spawns a short-lived process, reaps it, and returns its now-dead pid.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$") // runs no tests, exits 0
	if err := c.Start(); err != nil {
		t.Fatalf("spawn helper for dead pid: %v", err)
	}
	pid := c.Process.Pid
	_ = c.Wait() // reap; the pid is dead until the OS reuses it
	if Alive(pid) {
		t.Skipf("reaped pid %d still reports alive (reused?); skipping", pid)
	}
	return pid
}

func TestRunningFalseWhenNoPID(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	if _, ok := Running(paths.Default()); ok {
		t.Fatal("Running should be false with no pid file")
	}
}

func TestStart_NoOpWhenRunning(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live supervisor is represented by a pid file naming a running process; our
	// own pid is alive, so Start must treat the singleton as held and launch nothing.
	if err := os.WriteFile(p.PIDFile(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, started, err := Start(p)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if started {
		t.Fatal("Start launched a second supervisor while one was running")
	}
	if pid != os.Getpid() {
		t.Fatalf("Start returned pid %d, want the running pid %d", pid, os.Getpid())
	}
}

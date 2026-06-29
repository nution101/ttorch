package watch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/paths"
)

// TestAcquireFlock_FreshSlot: a fresh slot is claimed, the pid recorded, and release
// removes the pid file.
func TestAcquireFlock_FreshSlot(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	f, err := acquireFlock(p.WatchPIDFile(), "pane:4321")
	if err != nil {
		t.Fatalf("acquireFlock: %v", err)
	}
	rec, ok := readWatchRecord(p.WatchPIDFile())
	if !ok || rec.pid != os.Getpid() {
		t.Fatalf("pid file holder = %d (ok=%v), want our pid %d", rec.pid, ok, os.Getpid())
	}
	if rec.token != "pane:4321" {
		t.Fatalf("pid file token = %q, want %q", rec.token, "pane:4321")
	}
	releaseFlock(f, p.WatchPIDFile())
	if _, err := os.Stat(p.WatchPIDFile()); !os.IsNotExist(err) {
		t.Fatalf("release left the pid file behind (stat err = %v)", err)
	}
}

// TestAcquireFlock_HeldLoses: while another descriptor owns the lock, a second
// acquireFlock loses with errLockHeld (the singleton, mirroring the supervisor's
// flock test; flock arbitrates between descriptors even within one process).
func TestAcquireFlock_HeldLoses(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(p.WatchPIDFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

	if _, err := acquireFlock(p.WatchPIDFile(), ""); err != errLockHeld {
		t.Fatalf("acquireFlock while held: want errLockHeld, got %v", err)
	}
}

// TestLockedLiveFile covers the inode-verification guard (a descriptor whose path was
// unlinked or replaced must not be reported as the live file).
func TestLockedLiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.pid")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !lockedLiveFile(f, path) {
		t.Fatal("a freshly opened file should match its own path")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if lockedLiveFile(f, path) {
		t.Fatal("an unlinked descriptor must not be reported as the live file")
	}
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

// TestCommandLineIsWatch: the pid-reuse guard accepts genuine `ttorch watch`
// command lines and rejects strangers that merely contain the substrings, so
// --reset never SIGTERMs an unrelated process that reused the recorded pid.
func TestCommandLineIsWatch(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"ttorch watch", true},
		{"ttorch watch --since 42 --timeout 5m", true},
		{"/Users/x/.ttorch/bin/ttorch watch --since 1", true},
		{"ttorch report done", false},
		{"ttorch", false},
		{"", false},
		{"vim /tmp/ttorch-watch.log", false}, // basename vim, not ttorch
		{"grep watch ttorch.go", false},      // basename grep, not ttorch
		{"ttorch-watch-helper run", false},   // basename is not exactly "ttorch"
		{"/usr/bin/ttorchwatch", false},
	}
	for _, tc := range cases {
		if got := commandLineIsWatch(tc.line); got != tc.want {
			t.Errorf("commandLineIsWatch(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestInstanceToken: the start-time-pinned token is what makes the singleton robust to
// pane-pid reuse (H3). The SAME pid with two different start times MUST yield different
// tokens (so a restarted manager that inherited the dead one's pane pid is seen as a new
// incarnation), the same pid+start-time is stable (the live watcher is recognized), and
// every indeterminate input fails closed to "" (so a holder is never reaped on session
// grounds when identity is unknown). The token is always a single whitespace-free field.
func TestInstanceToken(t *testing.T) {
	a := instanceToken(5555, "Mon-Jun-29-00:00:00-2026")
	b := instanceToken(5555, "Tue-Jun-30-11:11:11-2026")
	if a == "" || b == "" {
		t.Fatalf("instanceToken returned empty for valid input (a=%q b=%q)", a, b)
	}
	if a == b {
		t.Fatalf("same pid, different start times produced the same token %q — pid reuse would go undetected", a)
	}
	if got := instanceToken(5555, "Mon-Jun-29-00:00:00-2026"); got != a {
		t.Fatalf("instanceToken not stable for identical input: %q vs %q", got, a)
	}
	if strings.ContainsAny(a, " \t\n") {
		t.Errorf("instanceToken produced token-unsafe whitespace: %q", a)
	}
	for _, tc := range []struct {
		pid       int
		startTime string
	}{
		{0, "Mon-Jun-29-00:00:00-2026"}, // unknown pane pid
		{-1, "x"},                       // invalid pid
		{5555, ""},                      // start time could not be read
	} {
		if got := instanceToken(tc.pid, tc.startTime); got != "" {
			t.Errorf("instanceToken(%d, %q) = %q, want \"\" (fail closed)", tc.pid, tc.startTime, got)
		}
	}
}

// TestProcessStartTime: our own pid is a live process, so ps must report a stable,
// whitespace-free start time; a non-positive pid is never real and fails closed to "". The
// assertion is skipped only when ps itself is unavailable — there the token legitimately
// degrades to "" (indeterminate) and the holder is given the benefit of the doubt.
func TestProcessStartTime(t *testing.T) {
	got := processStartTime(os.Getpid())
	if got == "" {
		t.Skip("ps -o lstart unavailable in this environment")
	}
	if strings.ContainsAny(got, " \t\n") {
		t.Errorf("processStartTime returned token-unsafe whitespace: %q", got)
	}
	if again := processStartTime(os.Getpid()); again != got {
		t.Errorf("processStartTime not stable across reads: %q vs %q", got, again)
	}
	// The encoding must be independent of the ambient $TZ (the ps call pins TZ=UTC): two arms
	// of the same manager under different zones must mint the SAME token for the same live
	// process, else a live watcher could look stale and be reaped (double-arm).
	t.Setenv("TZ", "America/New_York")
	east := processStartTime(os.Getpid())
	t.Setenv("TZ", "Asia/Tokyo")
	west := processStartTime(os.Getpid())
	if east != west {
		t.Errorf("processStartTime is TZ-sensitive: %q (EST) vs %q (JST) — token would differ across arms", east, west)
	}
	if v := processStartTime(0); v != "" {
		t.Errorf("processStartTime(0) = %q, want \"\"", v)
	}
	if v := processStartTime(-1); v != "" {
		t.Errorf("processStartTime(-1) = %q, want \"\"", v)
	}
}

// TestReset_NoOrphanReturnsImmediately: with no recorded watcher, --reset acquires
// the free singleton and returns, leaving no pid file behind.
func TestReset_NoOrphanReturnsImmediately(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	w := &Watcher{P: paths.Default(), lockRetry: time.Millisecond, isWatchProc: func(int) bool { return false }}
	if err := w.Reset(context.Background()); err != nil {
		t.Fatalf("Reset with no orphan: %v", err)
	}
	if _, err := os.Stat(w.P.WatchPIDFile()); !os.IsNotExist(err) {
		t.Fatalf("Reset left a pid file behind (stat err = %v)", err)
	}
}

// TestReset_WaitsForFlock: with an orphan holding the lock, --reset blocks until the
// orphan releases it, then acquires and returns (§4.5: it confirms the orphan is
// gone before returning). isWatchProc is stubbed false so the recorded pid is not
// signalled — the test exercises only the block-until-free behavior.
func TestReset_WaitsForFlock(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(p.WatchPIDFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	if _, err := holder.WriteAt([]byte(strconv.Itoa(os.Getppid())), 0); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{P: p, lockRetry: 2 * time.Millisecond, isWatchProc: func(int) bool { return false }}
	done := make(chan error, 1)
	go func() { done <- w.Reset(context.Background()) }()

	select {
	case <-done:
		t.Fatal("Reset returned while the orphan still held the lock")
	case <-time.After(40 * time.Millisecond):
	}

	// The orphan releases; Reset must now acquire and return cleanly.
	_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	_ = holder.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reset after the lock was released: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reset did not complete after the orphan released the lock")
	}
}

// TestReadWatchRecord: the pid file round-trips pid + token in the new format, still
// parses a legacy pid-only file (token empty), and rejects garbage.
func TestReadWatchRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.pid")

	if err := os.WriteFile(path, []byte(formatWatchRecord(4242, "pane:99")), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, ok := readWatchRecord(path)
	if !ok || rec.pid != 4242 || rec.token != "pane:99" {
		t.Fatalf("new-format record = %+v ok=%v", rec, ok)
	}
	if pid, ok := readWatchPID(path); !ok || pid != 4242 {
		t.Fatalf("readWatchPID(new format) = %d ok=%v, want 4242", pid, ok)
	}

	// Legacy file (a watcher armed before the token field existed): pid only, no token.
	if err := os.WriteFile(path, []byte("777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec, ok := readWatchRecord(path); !ok || rec.pid != 777 || rec.token != "" {
		t.Fatalf("legacy record = %+v ok=%v", rec, ok)
	}

	if err := os.WriteFile(path, []byte("notapid pane:1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWatchRecord(path); ok {
		t.Fatal("a garbled pid file must report !ok")
	}
}

// TestHolderIsOrphan covers the reap decision: an alive holder is an orphan only when
// the session instance it served is gone (its recorded token ≠ the live one); a dead
// holder is always an orphan; and every indeterminate case fails CLOSED (not an orphan)
// so a genuinely live watcher is never reaped.
func TestHolderIsOrphan(t *testing.T) {
	const live = "pane:1000"
	cases := []struct {
		name  string
		write string // pid file content; "" ⇒ no file
		alive bool
		token string // the live session token passed in
		want  bool
	}{
		{"alive holder, stale session token", "4242 pane:999", true, live, true},
		{"alive holder, current session token", "4242 pane:1000", true, live, false},
		// PID-reuse (the H3 case): the holder's WATCH pid is alive, but the manager PANE pid
		// it served (5555) has been reused by the restarted manager — same pane pid, later
		// start time. The start-time-pinned token differs, so the stale watcher is correctly
		// an orphan and gets reaped; the new manager arms instead of going blind.
		{"alive holder, pane pid reused with a new start time",
			"4242 pane:5555:Mon-Jun-29-00:00:00-2026", true, "pane:5555:Tue-Jun-30-11:11:11-2026", true},
		// Same pane pid AND start time ⇒ the genuinely live current watcher; never reaped.
		{"alive holder, same pane pid and start time (live)",
			"4242 pane:5555:Mon-Jun-29-00:00:00-2026", true, "pane:5555:Mon-Jun-29-00:00:00-2026", false},
		{"dead holder", "4242 pane:999", false, live, true},
		{"alive holder, legacy file (no token)", "4242", true, live, false},
		{"alive holder, no live token (no tmux)", "4242 pane:999", true, "", false},
		{"holder is this process", strconv.Itoa(os.Getpid()) + " pane:999", true, live, false},
		{"no pid file", "", true, live, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TTORCH_HOME", t.TempDir())
			p := paths.Default()
			if tc.write != "" {
				if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.WatchPIDFile(), []byte(tc.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			w := &Watcher{P: p, procAlive: func(int) bool { return tc.alive }}
			if _, got := w.holderIsOrphan(tc.token); got != tc.want {
				t.Errorf("holderIsOrphan(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// TestReapHolderIfMatches covers the classify→reap TOCTOU guard. The reap must signal
// ONLY when the pid file still names the exact (pid, token) the caller classified as an
// orphan. If a successor (a new current-session watcher) won the lock and rewrote the
// record in the gap — a different pid, OR the same pid now carrying a live token — the
// reap must NOT fire, so a genuinely live watcher is never SIGTERMed.
func TestReapHolderIfMatches(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := watchRecord{pid: os.Getppid(), token: "pane:111"}
	write := func(rec watchRecord) {
		if err := os.WriteFile(p.WatchPIDFile(), []byte(formatWatchRecord(rec.pid, rec.token)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var killed []int
	w := &Watcher{
		P:           p,
		procAlive:   func(int) bool { return true },
		isWatchProc: func(int) bool { return true },
		kill:        func(pid int) { killed = append(killed, pid) }, // spy: record, never signal
	}

	// Record unchanged since classification ⇒ signal the orphan's pid.
	write(orphan)
	if !w.reapHolderIfMatches(orphan) {
		t.Fatal("unchanged record: reap should have signalled the orphan")
	}
	if len(killed) != 1 || killed[0] != orphan.pid {
		t.Fatalf("unchanged record: killed = %v, want [%d]", killed, orphan.pid)
	}

	// A successor took the lock and rewrote the record (its own pid + a LIVE token) in the
	// classify→reap gap ⇒ reap must NOT fire.
	write(watchRecord{pid: orphan.pid + 100000, token: "pane:999"})
	killed = nil
	if w.reapHolderIfMatches(orphan) {
		t.Fatal("successor pid: reap must not signal a live successor")
	}
	if len(killed) != 0 {
		t.Fatalf("successor pid: killed = %v, want none", killed)
	}

	// Same pid, but now carrying a live (current-session) token — the pid was reused by a
	// legitimate current-session watcher ⇒ reap must NOT fire.
	write(watchRecord{pid: orphan.pid, token: "pane:222"})
	killed = nil
	if w.reapHolderIfMatches(orphan) {
		t.Fatal("changed token: reap must not signal a pid that now serves the current session")
	}
	if len(killed) != 0 {
		t.Fatalf("changed token: killed = %v, want none", killed)
	}

	// The slot cleared entirely (no pid file) ⇒ nothing to signal.
	if err := os.Remove(p.WatchPIDFile()); err != nil {
		t.Fatal(err)
	}
	killed = nil
	if w.reapHolderIfMatches(orphan) {
		t.Fatal("cleared slot: reap must not signal")
	}
	if len(killed) != 0 {
		t.Fatalf("cleared slot: killed = %v, want none", killed)
	}
}

// TestAcquire_ReapsOrphanFromDeadSession is acceptance case 1: an orphan watcher (alive
// pid, but a session token from a DEAD prior incarnation — the constant session name
// having been reused) holds the singleton. A fresh arm must NOT exit empty; it detects
// the orphan, reaps it, and acquires once the orphan releases. isWatchProc is stubbed
// false so the recorded pid is never actually signalled — the test releases the lock by
// hand to stand in for the reaped orphan exiting (mirrors TestReset_WaitsForFlock).
func TestAcquire_ReapsOrphanFromDeadSession(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(p.WatchPIDFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	if _, err := holder.WriteAt([]byte(formatWatchRecord(os.Getppid(), "pane:111")), 0); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{
		P:            p,
		lockRetry:    2 * time.Millisecond,
		briefGrace:   10 * time.Millisecond,
		resetGrace:   2 * time.Second,
		procAlive:    func(int) bool { return true },      // the holder looks alive
		isWatchProc:  func(int) bool { return false },     // but is never actually SIGTERMed
		sessionToken: func() string { return "pane:222" }, // live session ≠ recorded ⇒ orphan
	}

	type res struct {
		f   *os.File
		err error
	}
	done := make(chan res, 1)
	go func() {
		f, err := w.acquire(context.Background())
		done <- res{f, err}
	}()

	// Having identified an orphan, acquire must BLOCK for it to release — not give up
	// with errLockHeld the way it would for a live holder.
	select {
	case r := <-done:
		t.Fatalf("acquire returned while the orphan still held the lock (f=%v, err=%v)", r.f, r.err)
	case <-time.After(60 * time.Millisecond):
	}

	// The reaped orphan releases; acquire must now take over — a real lock, no error.
	_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	_ = holder.Close()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("acquire after the orphan released: %v", r.err)
		}
		if r.f == nil {
			t.Fatal("acquire returned a nil lock after reaping the orphan")
		}
		releaseFlock(r.f, p.WatchPIDFile())
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not take over after the orphan released the lock")
	}
}

// TestAcquire_LiveHolderNotReaped is acceptance case 2: a genuinely LIVE watcher serving
// the CURRENT manager session instance (its recorded token == the live one) must NOT be
// reaped — acquire returns errLockHeld (the arm exits quietly, the holder owns the wake)
// without ever attempting a SIGTERM.
func TestAcquire_LiveHolderNotReaped(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(p.WatchPIDFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	if _, err := holder.WriteAt([]byte(formatWatchRecord(os.Getppid(), "pane:777")), 0); err != nil {
		t.Fatal(err)
	}

	reapAttempted := false
	w := &Watcher{
		P:            p,
		lockRetry:    2 * time.Millisecond,
		briefGrace:   10 * time.Millisecond,
		resetGrace:   2 * time.Second,
		procAlive:    func(int) bool { return true },
		isWatchProc:  func(int) bool { reapAttempted = true; return false }, // only reapHolder reaches this
		sessionToken: func() string { return "pane:777" },                   // live session == recorded ⇒ live+valid
	}

	f, err := w.acquire(context.Background())
	if err != errLockHeld {
		if f != nil {
			releaseFlock(f, p.WatchPIDFile())
		}
		t.Fatalf("acquire against a live holder: want errLockHeld, got (f=%v, err=%v)", f, err)
	}
	if reapAttempted {
		t.Fatal("acquire tried to reap a LIVE watcher serving the current session")
	}
}

package watch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	f, err := acquireFlock(p.WatchPIDFile())
	if err != nil {
		t.Fatalf("acquireFlock: %v", err)
	}
	if pid, ok := readWatchPID(p.WatchPIDFile()); !ok || pid != os.Getpid() {
		t.Fatalf("pid file holder = %d (ok=%v), want our pid %d", pid, ok, os.Getpid())
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

	if _, err := acquireFlock(p.WatchPIDFile()); err != errLockHeld {
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

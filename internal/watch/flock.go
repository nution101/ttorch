package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// errLockHeld reports that the watch singleton flock is held by another live
// watcher. It is not a failure — the loser simply has no work to do (§4.5).
var errLockHeld = errors.New("watch: singleton already held")

// Flock retry bounds. A newly-armed watcher retries briefly so a slow orphan
// release never drops a wake; --reset blocks longer, until the reaped orphan frees
// the lock. Both are real wall-clock bounds (flock contention is real-time).
const (
	briefAcquireGrace = 1 * time.Second
	resetAcquireGrace = 10 * time.Second
	maxUnlinkRetries  = 5 // bounds the open-vs-lock unlink race (mirrors supervisor.maxAcquireAttempts)
)

// acquireFlock claims the watch singleton with an exclusive advisory lock (flock) on
// path, held until the returned *os.File is closed. It mirrors the supervisor's
// flock-as-truth pattern (the kernel arbitrates; the lock — not the file contents —
// is the truth, so there is no pid-reuse hazard and a crashed holder's lock is freed
// automatically). It records this process's pid in the file purely for observability
// and for `--reset` to find the orphan. On contention it returns errLockHeld.
func acquireFlock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxUnlinkRetries; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errLockHeld
			}
			return nil, err
		}
		if !lockedLiveFile(f, path) {
			// Locked an inode no longer at the path (a departing watcher unlinked it
			// between our open and our lock). Drop the orphan and retry the live file.
			_ = f.Close()
			continue
		}
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
		if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	return nil, errors.New("watch: could not acquire the lock (pid file kept being replaced)")
}

// releaseFlock drops the pid file (only while it still names this process, so a
// successor that already took over is left untouched) and closes the locked
// descriptor, releasing the advisory lock.
func releaseFlock(f *os.File, path string) {
	if f == nil {
		return
	}
	if pid, ok := readWatchPID(path); !ok || pid == os.Getpid() {
		_ = os.Remove(path)
	}
	_ = f.Close()
}

// lockedLiveFile reports whether f still refers to the file currently at path. flock
// binds to the inode, not the path, so a descriptor whose path was unlinked (or
// replaced) between open and lock points at an orphaned inode; acquireFlock uses this
// so it never records its pid into a file the path no longer names.
func lockedLiveFile(f *os.File, path string) bool {
	onDisk, err := os.Stat(path)
	if err != nil {
		return false
	}
	held, err := f.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(held, onDisk)
}

// readWatchPID returns the pid recorded in the watch pid file, if any.
func readWatchPID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether a process is running (signal 0 probe).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// isWatchProcess reports whether pid is, as far as we can tell, a `ttorch watch`
// process. It is the pid-reuse guard for `--reset` (§4.5): the recorded pid may have
// been reused by an unrelated process since the orphan died, so we only SIGTERM a pid
// whose command line still looks like a ttorch watch. It fails CLOSED — any doubt
// (ps unavailable/errored, command unrecognized) returns false so we never signal a
// stranger.
func isWatchProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return commandLineIsWatch(string(out))
}

// commandLineIsWatch reports whether a `ps -o command=` line is a ttorch watch
// invocation: the executable's basename is exactly "ttorch" AND its first non-flag
// argument is the "watch" subcommand. Anchoring on the basename + subcommand (rather
// than free-floating "ttorch"/"watch" substrings) rejects incidental matches that
// would otherwise let `--reset` SIGTERM a stranger that merely reused the pid —
// e.g. "vim /tmp/ttorch-watch.log" or "grep watch ttorch.go".
func commandLineIsWatch(psLine string) bool {
	fields := strings.Fields(strings.TrimSpace(psLine))
	if len(fields) < 2 {
		return false
	}
	if filepath.Base(fields[0]) != "ttorch" {
		return false
	}
	for _, a := range fields[1:] {
		if strings.HasPrefix(a, "-") {
			continue // skip any leading flags before the subcommand
		}
		return a == "watch"
	}
	return false
}

// acquireBriefly claims the flock, retrying briefly on contention so a slow orphan
// release (the moment between an old watcher's SIGTERM and its defer) never drops a
// wake. If the lock is still held after the brief grace it returns errLockHeld and
// the caller exits quietly — the live holder owns the wake (§4.5).
func (w *Watcher) acquireBriefly(ctx context.Context) (*os.File, error) {
	return w.retryAcquire(ctx, time.Now().Add(briefAcquireGrace))
}

// retryAcquire repeatedly attempts acquireFlock until it succeeds, a non-contention
// error occurs, ctx is cancelled, or the real-time deadline passes (then errLockHeld).
func (w *Watcher) retryAcquire(ctx context.Context, deadline time.Time) (*os.File, error) {
	interval := w.lockRetry
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	for {
		f, err := acquireFlock(w.P.WatchPIDFile())
		if err == nil {
			return f, nil
		}
		if err != errLockHeld {
			return nil, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, errLockHeld
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// Reset reaps an orphan watcher and confirms the singleton is free, for use on
// manager (re)start (§4.5). It SIGTERMs the recorded watch pid ONLY after verifying
// the process still looks like a `ttorch watch` (pid-reuse guard), then blocks until
// it can itself acquire the flock — proving the orphan released — and releases it
// again. It does not start watching.
func (w *Watcher) Reset(ctx context.Context) error {
	path := w.P.WatchPIDFile()
	if pid, ok := readWatchPID(path); ok && pid != os.Getpid() && processAlive(pid) && w.isWatchProc(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	f, err := w.retryAcquire(ctx, time.Now().Add(resetAcquireGrace))
	if err != nil {
		if err == errLockHeld {
			return errors.New("watch --reset: an orphan watcher did not release the lock in time")
		}
		return err
	}
	releaseFlock(f, path)
	return nil
}

// hashPane is the stable content hash liveness compares a worker pane against
// across sweeps (matches the supervisor's hash).
func hashPane(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

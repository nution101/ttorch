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
// automatically). It records this process's pid and the session token in the file
// purely for observability and for the orphan reap (`--reset` and the self-healing
// arm path in Run) to tell a live holder from a stale one. On contention it returns
// errLockHeld.
func acquireFlock(path, token string) (*os.File, error) {
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
		if _, err := f.WriteAt([]byte(formatWatchRecord(os.Getpid(), token)), 0); err != nil {
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

// watchRecord is what the singleton pid file holds: the holder's pid and a session
// token identifying the manager incarnation it serves. The token is empty in a legacy
// pid file (written before this field existed) or when the holder had no tmux to read.
type watchRecord struct {
	pid   int
	token string
}

// formatWatchRecord renders a record as "<pid>[ <token>]" — the token is omitted when
// empty so a legacy reader still parses the pid. The token never contains whitespace
// (see Watcher.sessionToken), so the two fields stay unambiguous.
func formatWatchRecord(pid int, token string) string {
	s := strconv.Itoa(pid)
	if token != "" {
		s += " " + token
	}
	return s
}

// readWatchRecord parses the watch pid file. The first whitespace-separated field is
// the holder's pid; an optional second field is its session token. A missing or
// garbled file (or a non-positive pid) reports !ok.
func readWatchRecord(path string) (watchRecord, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return watchRecord{}, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return watchRecord{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return watchRecord{}, false
	}
	rec := watchRecord{pid: pid}
	if len(fields) > 1 {
		rec.token = fields[1]
	}
	return rec, true
}

// readWatchPID returns just the pid recorded in the watch pid file (the reap target
// and the release-time ownership guard), if any.
func readWatchPID(path string) (int, bool) {
	rec, ok := readWatchRecord(path)
	if !ok {
		return 0, false
	}
	return rec.pid, true
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

// acquire claims the watch singleton, self-healing past an orphan holder. It first
// retries briefly (a slow orphan RELEASE between an old watcher's SIGTERM and its
// defer must never drop a wake). If the lock is still held after the brief grace it
// inspects the holder: a watcher serving the CURRENT manager session instance owns the
// wake, so this arm has no work and gets errLockHeld (the caller exits quietly, §4.5);
// but an ORPHAN — a holder whose pid is dead, or whose manager session instance is gone
// (a restarted manager reused the constant session name) — is reaped (pid-reuse-guarded
// SIGTERM, as `--reset`) and this arm then takes the lock, instead of exiting blind and
// leaving the manager deaf to events. Returns the locked file, errLockHeld for a live
// holder, or a ctx/other error.
func (w *Watcher) acquire(ctx context.Context) (*os.File, error) {
	live := w.token()
	f, err := w.retryAcquire(ctx, time.Now().Add(w.briefGraceOr()), live)
	if err != errLockHeld {
		return f, err // success, ctx cancellation, or a non-contention error
	}
	orphan, ok := w.holderIsOrphan(live)
	if !ok {
		return nil, errLockHeld // a live watcher for the current session — don't double-arm
	}
	// Reap the EXACT record we classified — not "whatever the file says now". Between the
	// classification above and the signal below the orphan may release and a NEW
	// current-session watcher win the lock and rewrite the record (its own pid + a LIVE
	// token); reapHolderIfMatches re-reads and signals only if the (pid, token) is still
	// the orphan's, so this self-heal can never SIGTERM that live successor (TOCTOU).
	w.reapHolderIfMatches(orphan)
	// The orphan was SIGTERMed (or is already dead, or a successor took over); block longer
	// for the lock to free, then take over. A holder we could not reap (pid-reuse guard, or
	// a successor we declined to kill) falls back to errLockHeld here — the quiet exit,
	// exactly as before this self-heal existed.
	return w.retryAcquire(ctx, time.Now().Add(w.resetGraceOr()), live)
}

// holderIsOrphan reads the current singleton holder's record and reports whether it is
// an orphan this arm may reap, given the live session token; it returns the record it
// read so the caller can pin the reap to exactly that (pid, token) — see acquire's
// TOCTOU note. A holder is an orphan when its recorded pid is dead, or when the manager
// session INSTANCE it served is gone — detected by its recorded token differing from the
// live one. The manager's tmux session NAME is a constant ("ttorch"), so a restarted
// manager reuses it and a name check cannot tell the new manager from the dead prior one;
// the instance token (Watcher.sessionToken) can. It fails CLOSED: an unidentifiable
// holder, a legacy file with no recorded token, or an indeterminate live token (no tmux)
// all report not-an-orphan, so a genuinely live watcher is never reaped.
func (w *Watcher) holderIsOrphan(live string) (watchRecord, bool) {
	rec, ok := readWatchRecord(w.P.WatchPIDFile())
	if !ok || rec.pid == os.Getpid() {
		return rec, false
	}
	if !w.alive(rec.pid) {
		return rec, true
	}
	if rec.token == "" || live == "" {
		return rec, false
	}
	return rec, rec.token != live
}

// reapHolderIfMatches SIGTERMs the recorded holder so it releases the lock — but ONLY if
// the pid file still names the exact (pid, token) record `want` the caller classified as
// an orphan, AND that pid is alive, is not this process, and still looks like a
// `ttorch watch` (the pid-reuse guard). Re-verifying the record immediately before
// signalling closes the classify→reap TOCTOU: if a successor rewrote the record in the
// gap (a different pid, or the same pid carrying a now-live token), the match fails and
// nothing is signalled, so a live current-session watcher is never killed. Returns true
// iff it signalled.
func (w *Watcher) reapHolderIfMatches(want watchRecord) bool {
	rec, ok := readWatchRecord(w.P.WatchPIDFile())
	if !ok || rec.pid != want.pid || rec.token != want.token {
		return false // a successor took over (or the slot cleared) — never signal it
	}
	if rec.pid == os.Getpid() || !w.alive(rec.pid) || !w.isWatchProc(rec.pid) {
		return false
	}
	w.reapSignal(rec.pid)
	return true
}

// reapHolder SIGTERMs the recorded singleton holder unconditionally (still behind the
// not-self / alive / pid-reuse guards) so it releases the lock. It is the FORCED reap
// used by Reset (`ttorch watch --reset`), which runs on manager restart before any
// current-session watcher is armed — so there is no live successor to protect and no
// token to pin to. The self-healing arm path uses reapHolderIfMatches instead.
func (w *Watcher) reapHolder() {
	if pid, ok := readWatchPID(w.P.WatchPIDFile()); ok && pid != os.Getpid() && w.alive(pid) && w.isWatchProc(pid) {
		w.reapSignal(pid)
	}
}

// reapSignal sends the reap signal to pid (nil seam ⇒ the real SIGTERM).
func (w *Watcher) reapSignal(pid int) {
	if w.kill != nil {
		w.kill(pid)
		return
	}
	defaultKill(pid)
}

// defaultKill is the production reap signal: SIGTERM, which a `ttorch watch` process traps
// to cancel its loop and release the flock (see cmdWatch). Tests swap the seam for a spy.
func defaultKill(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

// briefGraceOr / resetGraceOr resolve the flock-acquisition grace windows, honoring a
// per-Watcher override (tests set tiny values so the contention paths run without real
// waits) and otherwise the package defaults.
func (w *Watcher) briefGraceOr() time.Duration {
	if w.briefGrace > 0 {
		return w.briefGrace
	}
	return briefAcquireGrace
}

func (w *Watcher) resetGraceOr() time.Duration {
	if w.resetGrace > 0 {
		return w.resetGrace
	}
	return resetAcquireGrace
}

// token returns the current live session token (nil seam ⇒ "", i.e. indeterminate).
func (w *Watcher) token() string {
	if w.sessionToken != nil {
		return w.sessionToken()
	}
	return ""
}

// alive reports whether pid is running (nil seam ⇒ the real signal-0 probe).
func (w *Watcher) alive(pid int) bool {
	if w.procAlive != nil {
		return w.procAlive(pid)
	}
	return processAlive(pid)
}

// retryAcquire repeatedly attempts acquireFlock (recording token on success) until it
// succeeds, a non-contention error occurs, ctx is cancelled, or the real-time deadline
// passes (then errLockHeld).
func (w *Watcher) retryAcquire(ctx context.Context, deadline time.Time, token string) (*os.File, error) {
	interval := w.lockRetry
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	for {
		f, err := acquireFlock(w.P.WatchPIDFile(), token)
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
	w.reapHolder()
	f, err := w.retryAcquire(ctx, time.Now().Add(w.resetGraceOr()), "")
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

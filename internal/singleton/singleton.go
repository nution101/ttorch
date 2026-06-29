// Package singleton provides a minimal flock-as-truth singleton: at most one live process holds
// the lock at a time, and the kernel frees a crashed holder's lock automatically — so there is no
// pid-reuse hazard and no stale-lock cleanup to do. It backs the manager's auto-started scheduler
// daemon (one daemon per ~/.ttorch) without the watcher's orphan-reap machinery, which the
// scheduler does not need: a dead daemon's lock simply frees, and the lock file is reused, never
// unlinked, so there is no open-vs-unlink race to guard.
package singleton

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Acquire takes an exclusive, non-blocking advisory lock (flock) on path, held until the returned
// file is closed (Release) or the process exits. It records the holder's pid in the file for
// observability only — the LOCK, not the file contents, is the truth. It returns:
//
//   - (file, true, nil)  the lock was acquired (this process is the singleton);
//   - (nil, false, nil)  another live process holds it (the caller is a redundant instance and
//     should exit quietly);
//   - (nil, false, err)  a real error (could not create/open the lock file).
func Acquire(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil // a live holder has it — not an error, just not ours
		}
		return nil, false, err
	}
	// Record the holder's pid (best-effort, observability only; the flock is the real truth).
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, true, nil
}

// Held reports whether another live process currently holds the lock at path, WITHOUT keeping it:
// it attempts the same non-blocking flock and, on success, immediately releases it. A held lock
// (EWOULDBLOCK) means a live holder exists. A real error (or a free lock) reports false — the
// conservative answer for a "should I start the daemon?" probe is "not held" (try to start; the
// daemon's own Acquire is the authority and a redundant start exits quietly). Note flock
// distinguishes open file descriptions, not processes, so this correctly reports held even when
// the holder is THIS process holding it via another descriptor.
func Held(path string) bool {
	f, acquired, err := Acquire(path)
	if err != nil {
		return false
	}
	if !acquired {
		return true
	}
	Release(f)
	return false
}

// Release drops the advisory lock by closing f. The lock file itself is intentionally NOT
// unlinked — it is reused by the next Acquire (which re-flocks and overwrites the pid), which
// avoids the open-vs-unlink race entirely; a lingering file with a stale pid is harmless because
// the flock, not the file, is the truth.
func Release(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

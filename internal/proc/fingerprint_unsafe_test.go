package proc

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The fingerprint file sits in the task's data dir, where the worker can write. Whatever it
// puts there must not stall the reader: the watcher reads every task's fingerprint on every
// sweep, and ttorch status reads them too, so one blocked read wedges both for every task.

// loadWithin runs StoredAgentState on path and fails the test if it has not returned within
// d. The pane read is never reached for a file that cannot be loaded.
func loadWithin(t *testing.T, path string, d time.Duration) AgentState {
	t.Helper()
	done := make(chan AgentState, 1)
	go func() {
		st, _ := StoredAgentState(path, func() (int, error) { return 100, nil })
		done <- st
	}()
	select {
	case st := <-done:
		return st
	case <-time.After(d):
		t.Fatalf("StoredAgentState(%s) did not return within %v", path, d)
		return AgentUnknown
	}
}

// mkfifo creates a FIFO at path. Cleanup opens and closes its write end, which releases a
// reader still blocked opening it, so a failing run does not leave a goroutine parked.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
}

// A FIFO with nothing writing to it is refused at once, as unknown.
func TestStoredAgentState_FIFOReturnsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.fingerprint")
	mkfifo(t, path)
	if st := loadWithin(t, path, 5*time.Second); st != AgentUnknown {
		t.Fatalf("FIFO fingerprint: %v, want unknown", st)
	}
}

// A symlink is not followed, even to a valid fingerprint. It reads as unknown, not as
// untracked, so a link to a missing target does not pass for "no fingerprint".
func TestStoredAgentState_SymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "elsewhere")
	if err := SaveFingerprint(valid, Fingerprint{PID: 200, Start: "a", CmdHash: hashCmdline("claude")}); err != nil {
		t.Fatal(err)
	}
	fakeProcesses(t, map[int]Process{200: {PID: 200, PPID: 100, Start: "a", Cmdline: "claude"}}, nil)
	link := filepath.Join(dir, "agent.fingerprint")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if st := loadWithin(t, link, 5*time.Second); st != AgentUnknown {
		t.Fatalf("symlink to a valid fingerprint: %v, want unknown", st)
	}
	dangling := filepath.Join(dir, "dangling.fingerprint")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if st := loadWithin(t, dangling, 5*time.Second); st != AgentUnknown {
		t.Fatalf("dangling symlink: %v, want unknown", st)
	}
}

// A file larger than any real fingerprint is refused, even when it starts with valid fields.
func TestStoredAgentState_OversizeIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.fingerprint")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("pid=200\nstart=a\ncmd_sha256=" + hashCmdline("claude") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fakeProcesses(t, map[int]Process{200: {PID: 200, PPID: 100, Start: "a", Cmdline: "claude"}}, nil)
	if st := loadWithin(t, path, 5*time.Second); st != AgentUnknown {
		t.Fatalf("oversize fingerprint: %v, want unknown", st)
	}
}

package brieflint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A deadline is only a bound if it actually ends the work. os/exec kills the process it
// started, not that process's children, and with a buffer sink Run also waits on the copy
// goroutines reading the child's pipes. git ls-remote forks ssh (or git-remote-https and a
// credential helper), the fork inherits the write end of stdout, so killing git alone leaves
// the pipe open and Run never returns: the budget is declared but not enforced.
//
// The fixture is a "git" that forks a child sharing its stdout and then blocks, which is the
// same shape without needing a hung remote.
func TestGitRunKillsAHungGrandchild(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 120 &\nsleep 120\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan gitResult, 1)
	start := time.Now()
	go func() { done <- gitRun(ctx, t.TempDir(), "ls-remote", "--", "origin", "main") }()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("want an error when the budget is spent, got exit %d", res.exit)
		}
		// The deadline is 500ms and WaitDelay allows a couple of seconds to tear down.
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("gitRun took %s to return on a 500ms deadline", el)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("gitRun did not return 20s after a 500ms deadline: the deadline ended git but not its children, so the run budget is not enforced")
	}
}

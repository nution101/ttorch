package brieflint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hang fixtures re-exec this test binary as the forked child (the os/exec package tests
// its own process handling the same way). The child has to be a SINGLE process that sleeps
// and then writes a marker: a shell doing the same job forks, and when the group kill reaps
// its child the shell can run one more command in the moment before the signal reaches it,
// which makes the leak check report a false leak about one run in five.
const (
	childEnv    = "TTORCH_BRIEFLINT_TEST_CHILD" // "<sleep-duration> <marker-path>"
	childBinEnv = "TTORCH_BRIEFLINT_TEST_BIN"   // path to this test binary, for the git shim
)

// runTestChild is the child mode TestMain dispatches to.
func runTestChild(v string) {
	parts := strings.SplitN(v, " ", 2)
	d, err := time.ParseDuration(parts[0])
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(d)
	if err := os.WriteFile(parts[1], []byte("survived\n"), 0o644); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// forkingGit installs a "git" on PATH that forks a child sharing its stdout and then blocks.
// That is the shape of git ls-remote forking ssh, without needing a hung remote. The child
// writes marker after wait, so a test can see whether it outlived the deadline.
func forkingGit(t *testing.T, marker string, wait time.Duration) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\n\"$" + childBinEnv + "\" &\nsleep 120\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(childBinEnv, self)
	t.Setenv(childEnv, fmt.Sprintf("%s %s", wait, marker))
}

// A deadline is only a bound if it actually ends the work. os/exec kills the process it
// started, not that process's children, and with a buffer sink Run also waits on the copy
// goroutines reading the child's pipes. git ls-remote forks ssh (or git-remote-https and a
// credential helper), the fork inherits the write end of stdout, so killing git alone leaves
// the pipe open and Run never returns: the budget is declared but not enforced.
//
// Both halves are asserted, because they are not the same thing. Run returning promptly is
// WaitDelay's doing and holds even when the group kill is broken; only the marker shows
// whether the deadline ended the work or merely stopped waiting for it.
func TestGitRunKillsAHungGrandchild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	forkingGit(t, marker, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan gitResult, 1)
	start := time.Now()
	go func() { done <- gitRun(ctx, t.TempDir(), "ls-remote", "--", "origin", "main") }()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("want an error when the budget is spent, got exit %d", res.exit)
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("gitRun took %s to return on a 300ms deadline", el)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("gitRun did not return 20s after a 300ms deadline: the deadline ended git but not its children, so the run budget is not enforced")
	}

	// The child writes its marker a second in, more than three times the deadline. If it is
	// there, the fork outlived the kill and went on working, and the deadline bounded how
	// long the caller waited rather than the work it was supposed to stop.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git's forked child outlived the deadline and went on working: it escaped the process-group kill")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

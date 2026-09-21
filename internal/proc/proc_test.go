package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests re-exec this binary as the forked child (the os/exec package tests its own
// process handling the same way). The child must be a SINGLE process that sleeps and then
// writes a marker: a shell doing the same job forks, and when the group kill reaps its
// child, the shell can run one more command in the moment before the signal reaches it. That
// made an earlier version of this test report a leak roughly one run in five. The property
// under test is whether the child outlives the deadline, so the fixture must not be able to
// half-outlive it.
const helperEnv = "TTORCH_PROC_TEST_CHILD"

func TestMain(m *testing.M) {
	if v := os.Getenv(helperEnv); v != "" {
		// v is "[setsid ]<sleep-duration> <marker-path>".
		if rest, ok := strings.CutPrefix(v, "setsid "); ok {
			// Leave the process group, so a kill aimed at the group cannot reach this
			// process. The pipe it inherited stays open either way.
			if _, err := syscall.Setsid(); err != nil {
				os.Exit(4)
			}
			v = rest
		}
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
	os.Exit(m.Run())
}

// hangingCommand builds a command of the shape that matters: a parent that forks a child
// sharing its stdout and then blocks. Kill the parent alone and the child keeps the write end
// of the pipe open, so a Wait that reads the pipe never returns. The child writes marker
// after wait, so a test can see whether it outlived the deadline.
func hangingCommand(t *testing.T, ctx context.Context, marker string, wait time.Duration, opts ...string) (*bytes.Buffer, func() error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := Command(ctx, "sh", "-c", fmt.Sprintf("%q & sleep 120", self))
	c.Env = append(os.Environ(), fmt.Sprintf("%s=%s%s %s", helperEnv, strings.Join(opts, ""), wait, marker))
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	return &out, c.Run
}

func TestCommandKillsTheWholeGroupOnDeadline(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, run := hangingCommand(t, ctx, marker, time.Second)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error when the deadline fires")
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("Run took %s to return on a 300ms deadline", el)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return 20s after a 300ms deadline: the deadline ended the process but not its children, so Wait stayed blocked on the pipe the fork still holds")
	}

	// Releasing the caller is only half of it. The child writes its marker a second in,
	// which is three times the deadline: if it is there, the child outlived the kill and
	// went on working, and a deadline that leaks a process every time it fires bounds the
	// waiting rather than the work.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the forked child outlived the deadline and went on working: it escaped the process-group kill")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// The ordinary path must be untouched: output is captured and the exit status is the
// command's own.
func TestCommandRunsNormally(t *testing.T) {
	var out bytes.Buffer
	c := Command(context.Background(), "sh", "-c", "echo hello; exit 3")
	c.Stdout = &out
	if err := c.Run(); err == nil {
		t.Fatal("want the command's own non-zero exit")
	} else if c.ProcessState.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", c.ProcessState.ExitCode())
	}
	if got := out.String(); got != "hello\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello\n")
	}
}

// A command that finishes inside its deadline is not disturbed by any of this.
func TestCommandUnderDeadlineCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := Command(ctx, "sh", "-c", "echo done")
	c.Stdout = &out
	if err := c.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.String(); got != "done\n" {
		t.Fatalf("stdout = %q, want %q", got, "done\n")
	}
}

// WaitDelay is the backstop for the case the group kill cannot cover: a child that leaves
// the process group before the deadline, so kill(-pgid) never reaches it, while still
// holding the pipe. The group kill is powerless there and Run would block forever on the
// copy goroutines; WaitDelay is what releases the caller.
//
// The child is not reaped here, and cannot be. This asserts the bound on the caller, which
// is all WaitDelay promises, and it is why the group kill is the primary mechanism rather
// than this one.
func TestWaitDelayReleasesTheCallerWhenAChildEscapesTheGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// The escaped child must hold the pipe for far longer than WaitDelay. An earlier
	// version had it sleep 3s, so with WaitDelay removed Run still returned when the child
	// exited of its own accord, and the test passed against the mutant it exists to catch.
	const holdsPipeFor = 30 * time.Second
	_, run := hangingCommand(t, ctx, marker, holdsPipeFor, "setsid ")

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error when the deadline fires")
		}
		el := time.Since(start)
		// The deadline plus WaitDelay, with room for a loaded machine, and far short of the
		// 30s the child holds the pipe for. Without WaitDelay, Run waits out the child.
		if el > WaitDelay+5*time.Second {
			t.Fatalf("Run took %s to return on a 300ms deadline with a %s wait delay", el, WaitDelay)
		}
		if el < WaitDelay {
			t.Fatalf("Run returned in %s, before the %s wait delay: the child was reaped after all, so this is no longer testing the backstop", el, WaitDelay)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return 20s after a 300ms deadline: a child that left the process group holds the pipe, and nothing capped the wait on it")
	}
}

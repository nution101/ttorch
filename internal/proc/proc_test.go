package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests re-exec this binary as the forked child (the os/exec package tests its own
// process handling the same way). The child must be a SINGLE process that waits and then
// writes a marker: a shell doing the same job forks, and when the group kill reaps its
// child, the shell can run one more command in the moment before the signal reaches it. That
// made an earlier version of this test report a leak roughly one run in five. The property
// under test is whether the child outlives the deadline, so the fixture must not be able to
// half-outlive it.
//
// Nothing here signals a pid. Every fixture process watches a stop file and exits when it
// appears, so cleanup reaps them by writing that file rather than by kill(2). An earlier
// version sent a bare SIGKILL to a pid read from a file, which on the passing path was a pid
// the fixture no longer owned: the child is reaped at the 300ms deadline and the signal went
// out around 2.3s later, so any process the OS had since given that pid would have taken it.
const (
	// childFlag puts this binary into child mode, carrying "[setsid ]<wait-duration>
	// <marker-path>". It is an argv flag rather than an environment variable on purpose:
	// an inherited variable of the right name used to make the whole test binary exit 0
	// through the child path without running a single test, so the suite that guards this
	// package could be switched off by an export. An argv flag is not inherited and `go
	// test` never passes it.
	childFlag = "-proc-test-child"
	binEnv    = "TTORCH_PROC_TEST_BIN"  // this test binary, for the shell to fork
	stopEnv   = "TTORCH_PROC_TEST_STOP" // the stop file every fixture process watches
	lockEnv   = "TTORCH_PROC_TEST_LOCK" // the child holds a lock on this for its whole life
)

// childPoll is how often a fixture process checks for the stop file.
const childPoll = 50 * time.Millisecond

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == childFlag {
		os.Exit(runChild(os.Args[2]))
	}
	os.Exit(m.Run())
}

// runChild is the forked child: hold a lock for as long as this process lives, optionally
// leave the process group, then wait out the duration and write the marker. It gives up
// early if the stop file appears, which is how cleanup reaps it without signalling a pid.
//
// The lock is the liveness signal. It is released by the kernel when this process ends, for
// any reason including SIGKILL, so a test can tell "the child is gone" from "the child is
// still running" without naming a pid that may by then belong to someone else.
func runChild(v string) int {
	// The child creates the lock file. reapFixture refuses to open it any other way, so
	// its "the child is gone" can never be satisfied by a child that never ran.
	lock, err := os.OpenFile(os.Getenv(lockEnv), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return 5
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 6
	}
	if rest, ok := strings.CutPrefix(v, "setsid "); ok {
		// Leave the process group, so a kill aimed at the group cannot reach this process.
		// The pipe it inherited stays open either way.
		if _, err := syscall.Setsid(); err != nil {
			return 4
		}
		v = rest
	}
	parts := strings.SplitN(v, " ", 2)
	d, err := time.ParseDuration(parts[0])
	if err != nil {
		return 2
	}
	stop := os.Getenv(stopEnv)
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		if _, err := os.Stat(stop); err == nil {
			return 0
		}
		time.Sleep(childPoll)
	}
	if err := os.WriteFile(parts[1], []byte("survived\n"), 0o644); err != nil {
		return 3
	}
	return 0
}

// hangingCommand builds a command of the shape that matters: a parent that forks a child
// sharing its stdout and then blocks. Kill the parent alone and the child keeps the write
// end of the pipe open, so a Wait that reads the pipe never returns. The child writes marker
// after wait, so a test can see whether it outlived the deadline.
//
// The shell blocks in a loop over the stop file rather than in sleep(1), so cleanup can end
// it without signalling anything; the loop is bounded so that a test binary killed before
// its cleanup runs still leaves nothing behind for longer than the old sleep did.
func hangingCommand(t *testing.T, ctx context.Context, marker string, wait time.Duration, opts ...string) (*bytes.Buffer, func() error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stop := filepath.Join(dir, "stop")
	lock := filepath.Join(dir, "child.lock")

	// The binary path reaches the shell through the environment, never interpolated into
	// the script: sh expands $ and backtick inside double quotes, and %q does not escape
	// either of them.
	script := `"$` + binEnv + `" ` + childFlag + ` "$` + specEnv + `" &
i=0
while [ ! -f "$` + stopEnv + `" ] && [ "$i" -lt 120 ]; do
  sleep 1
  i=$((i+1))
done`
	c := Command(ctx, "sh", "-c", script)
	c.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s%s %s", specEnv, strings.Join(opts, ""), wait, marker),
		binEnv+"="+self,
		stopEnv+"="+stop,
		lockEnv+"="+lock,
	)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	t.Cleanup(func() { reapFixture(t, stop, lock) })
	return &out, func() error { return Run(c) }
}

// specEnv carries the child's spec to the shell, which passes it on as the childFlag
// argument. Only the flag puts the binary into child mode.
const specEnv = "TTORCH_PROC_TEST_SPEC"

// reapFixture ends everything the fixture started and proves the child is gone.
//
// It signals nothing. Writing the stop file is what ends the shell and the child, so there
// is no pid to get wrong. Taking the child's lock is what proves it: the kernel releases
// that lock when the process ends however it ends, so acquiring it means the child is gone
// and nothing else can make it look that way.
func reapFixture(t *testing.T, stop, lock string) {
	t.Helper()
	if err := os.WriteFile(stop, nil, 0o644); err != nil && !os.IsNotExist(err) {
		t.Errorf("writing the stop file: %v", err)
	}
	// No O_CREATE: the child creates this file, so a missing one means no child ever ran
	// and there is nothing here to prove. Creating it ourselves and locking it on the first
	// try would have reported "the child is gone" with no evidence one existed.
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Errorf("the fixture child never created its lock (%v), so nothing here shows it ran", err)
		return
	}
	defer f.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("the fixture child still holds its lock 10s after the stop file was written: it outlived the test")
			return
		}
		time.Sleep(childPoll)
	}
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
		if errors.Is(err, ErrDisarmed) {
			t.Fatalf("the fixture never ran, so this asserted nothing: %v", err)
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("Run took %s to return on a 300ms deadline", el)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return 20s after a 300ms deadline: the deadline ended the process but not its children, so Wait stayed blocked on the pipe the fork still holds")
	}

	// Releasing the caller is only half of it. The child writes its marker a second in,
	// more than three times the deadline: if it is there, the child outlived the kill and
	// went on working, and a deadline that leaks a process every time it fires bounds the
	// waiting rather than the work.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the forked child outlived the deadline and went on working: it escaped the process-group kill")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// WaitDelay is the backstop for the case the group kill cannot cover: a child that leaves
// the process group before the deadline, so kill(-pgid) never reaches it, while still
// holding the pipe. The group kill is powerless there and Run would block forever on the
// copy goroutines; WaitDelay is what releases the caller.
//
// The child is not reaped by the deadline here, and cannot be. This asserts the bound on the
// caller, which is all WaitDelay promises; cleanup is what ends the child, and it proves it.
func TestWaitDelayReleasesTheCallerWhenAChildEscapesTheGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// The escaped child must hold the pipe for far longer than WaitDelay. An earlier
	// version had it wait 3s, so with WaitDelay removed Run still returned when the child
	// finished of its own accord, and the test passed against the mutant it exists to catch.
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
		if errors.Is(err, ErrDisarmed) {
			t.Fatalf("the fixture never ran, so this asserted nothing: %v", err)
		}
		el := time.Since(start)
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

// The ordinary path must be untouched: output is captured and the exit status is the
// command's own.
func TestCommandRunsNormally(t *testing.T) {
	var out bytes.Buffer
	c := Command(context.Background(), "sh", "-c", "echo hello; exit 3")
	c.Stdout = &out
	if err := Run(c); err == nil {
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
	out, err := CombinedOutput(Command(ctx, "sh", "-c", "echo done"))
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}
	if string(out) != "done\n" {
		t.Fatalf("output = %q, want %q", out, "done\n")
	}
}

// --- the guarantee is checked, not just documented -------------------------------------

// Every way of disarming a command must be refused rather than run. These are the four
// fields Check names, each removed the way a caller plausibly would.
func TestDisarmedCommandsAreRefused(t *testing.T) {
	cases := map[string]struct {
		disarm func(*exec.Cmd)
		want   string
	}{
		"SysProcAttr replaced wholesale": {
			// The plausible one: adding an unrelated field by assigning a fresh struct.
			disarm: func(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Foreground: false} },
			want:   "Setpgid is false",
		},
		"SysProcAttr nil": {
			disarm: func(c *exec.Cmd) { c.SysProcAttr = nil },
			want:   "SysProcAttr is nil",
		},
		"joins an existing group": {
			disarm: func(c *exec.Cmd) { c.SysProcAttr.Pgid = 1234 },
			want:   "joins that group instead of leading its own",
		},
		"Cancel removed": {
			disarm: func(c *exec.Cmd) { c.Cancel = nil },
			want:   "Cancel is nil",
		},
		"Cancel replaced": {
			disarm: func(c *exec.Cmd) { c.Cancel = func() error { return nil } },
			want:   "not the group kill this package installed",
		},
		"WaitDelay zeroed": {
			disarm: func(c *exec.Cmd) { c.WaitDelay = 0 },
			want:   "WaitDelay is zero",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := Command(context.Background(), "sh", "-c", "exit 0")
			tc.disarm(c)

			err := Check(c)
			if err == nil {
				t.Fatal("Check accepted a disarmed command")
			}
			if !errors.Is(err, ErrDisarmed) {
				t.Fatalf("Check error = %v, want it to wrap ErrDisarmed", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check error = %q, want it to mention %q", err, tc.want)
			}
			// And the command must not run at all.
			if err := Run(c); !errors.Is(err, ErrDisarmed) {
				t.Fatalf("Run error = %v, want ErrDisarmed", err)
			}
			if err := Start(c); !errors.Is(err, ErrDisarmed) {
				t.Fatalf("Start error = %v, want ErrDisarmed", err)
			}
			if _, err := CombinedOutput(c); !errors.Is(err, ErrDisarmed) {
				t.Fatalf("CombinedOutput error = %v, want ErrDisarmed", err)
			}
			if c.Process != nil {
				t.Fatal("a refused command must not have been started")
			}
		})
	}
}

// An untouched command passes, so the check cannot be satisfied by rejecting everything.
func TestCheckAcceptsAnIntactCommand(t *testing.T) {
	if err := Check(Command(context.Background(), "sh", "-c", "exit 0")); err != nil {
		t.Fatalf("Check rejected an untouched command: %v", err)
	}
}

// Check tells the installed Cancel from a replacement by code pointer, which relies on every
// method value of one method sharing the wrapper's pointer. Pin that, because the check is
// worthless if the comparison is accidentally true or accidentally false.
func TestCancelIdentityIsStable(t *testing.T) {
	a := Command(context.Background(), "true")
	b := Command(context.Background(), "false")
	// The comparison Check actually performs. An earlier version of this test compared the
	// two commands to each other and an unrelated func to the recorded pointer, but never a
	// real command to it, so it passed while Check rejected every command in the package.
	if reflect.ValueOf(a.Cancel).Pointer() != cancelPC {
		t.Fatal("a command from Command does not carry the installed Cancel, so Check rejects valid commands")
	}
	if reflect.ValueOf(a.Cancel).Pointer() != reflect.ValueOf(b.Cancel).Pointer() {
		t.Fatal("two commands from Command carry different Cancel code pointers, so Check would reject valid commands")
	}
	if reflect.ValueOf(func() error { return nil }).Pointer() == cancelPC {
		t.Fatal("an unrelated func matches the installed Cancel, so Check would accept a replacement")
	}
}

// Identity is not enough: the installed Cancel has to belong to the command being checked.
// A struct copy and a borrowed Cancel both carry a correct-looking Cancel that closes over
// a different command, whose Process is nil, so the kill returns without signalling and the
// grandchild outlives the deadline. Both used to pass Check.
func TestCheckRejectsAMisboundCancel(t *testing.T) {
	t.Run("copy", func(t *testing.T) {
		c := Command(context.Background(), "sh", "-c", "exit 0")
		cp := *c
		assertMisbound(t, &cp)
	})
	t.Run("crossbound", func(t *testing.T) {
		c := Command(context.Background(), "sh", "-c", "exit 0")
		other := Command(context.Background(), "sh", "-c", "exit 0")
		c.Cancel = other.Cancel
		assertMisbound(t, c)
	})

	// The same two shapes with the OTHER command running. Check used to answer the binding
	// question by invoking the Cancel, which reached the group kill of whichever command
	// that Cancel really belonged to: a report that killed a live process group. Both
	// commands here are deliberately alive, and the verdict must be identical to the dead
	// cases above while the process survives.
	t.Run("crossbound from a live command", func(t *testing.T) {
		victim := startLive(t, Command(context.Background(), "sleep", "60"))
		c := Command(context.Background(), "sh", "-c", "exit 0")
		c.Cancel = victim.cmd.Cancel
		assertMisbound(t, c)
		victim.assertStillRunning(t, "the command whose Cancel was borrowed")
	})
	t.Run("copy of a live command", func(t *testing.T) {
		c := Command(context.Background(), "sleep", "60")
		cp := *c // an ordinary struct copy, taken before Start
		original := startLive(t, c)
		assertMisbound(t, &cp)
		original.assertStillRunning(t, "the command that was copied")
	})
}

// live is a started command with ONE waiter. Wait is the only way to learn that a child has
// died, and only one goroutine may call it, so the wait runs once here and both the
// assertion and the cleanup read its result from the same channel.
type live struct {
	cmd  *exec.Cmd
	done chan error
}

// startLive starts c and reaps it at the end of the test. It is never waited on before
// then, so its pid cannot be recycled underneath the assertions.
func startLive(t *testing.T, c *exec.Cmd) *live {
	t.Helper()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	p := &live{cmd: c, done: make(chan error, 1)}
	go func() { p.done <- c.Wait() }()
	t.Cleanup(func() {
		_ = c.Process.Kill()
		<-p.done
	})
	return p
}

// assertStillRunning fails if the process has died. The window is generous enough that a
// SIGKILL delivered during Check would always have landed inside it.
func (p *live) assertStillRunning(t *testing.T, what string) {
	t.Helper()
	select {
	case err := <-p.done:
		p.done <- err // leave it for the cleanup, which reads the same channel
		t.Fatalf("%s died as a side effect of Check: Wait = %v (pid %d)", what, err, p.cmd.Process.Pid)
	case <-time.After(600 * time.Millisecond):
	}
}

func assertMisbound(t *testing.T, c *exec.Cmd) {
	t.Helper()
	err := Check(c)
	if err == nil {
		t.Fatal("Check accepted a Cancel bound to a different command")
	}
	if !errors.Is(err, ErrDisarmed) {
		t.Fatalf("Check error = %v, want it to wrap ErrDisarmed", err)
	}
	if !strings.Contains(err.Error(), "belongs to a different command") {
		t.Fatalf("Check error = %q, want it to name the binding", err)
	}
	if err := Run(c); !errors.Is(err, ErrDisarmed) {
		t.Fatalf("Run error = %v, want ErrDisarmed", err)
	}
	if c.Process != nil {
		t.Fatal("a refused command must not have been started")
	}
}

// Probing must not be able to suppress a real kill, so it is answered only on the path where
// there is no process to signal. A command that has already started cannot be probed, and
// Check says that rather than guessing.
// Check reads the binding instead of firing it, so a started command is answerable too, and
// the field checks are answerable whatever the Cancel turns out to be.
func TestCheckOnAStartedCommand(t *testing.T) {
	t.Run("intact", func(t *testing.T) {
		c := Command(context.Background(), "sh", "-c", "sleep 0.2")
		if err := Start(c); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Wait() }()
		if err := Check(c); err != nil {
			t.Fatalf("Check error = %v, want nil: nothing about a started command hides its binding", err)
		}
	})

	// An early return here used to report a started command as unverifiable and drop what
	// had already been established about it. WaitDelay and SysProcAttr are plain field
	// reads; a caller told "disarmed" knows what to fix, where "unverifiable" invites a
	// retry.
	t.Run("disarmed as well as started", func(t *testing.T) {
		c := Command(context.Background(), "sh", "-c", "sleep 0.2")
		c.WaitDelay = 0
		c.SysProcAttr = nil
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Wait() }()
		err := Check(c)
		if !errors.Is(err, ErrDisarmed) {
			t.Fatalf("Check error = %v, want ErrDisarmed", err)
		}
		for _, want := range []string{"SysProcAttr is nil", "WaitDelay is zero"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Check error = %q, want it to name %q", err, want)
			}
		}
	})
}

// The binding is read out of the installed method value. That layout is not guaranteed by
// the language, so the package self-tests it at startup and refuses to certify anything if
// it ever stops holding. If this fails, Check has gone permanently unverifiable.
func TestCancelBindingIsReadableWithoutCalling(t *testing.T) {
	if !layoutOK {
		t.Fatal("the method-value self-test failed, so no binding can be read in this build")
	}
	a := Command(context.Background(), "true")
	b := Command(context.Background(), "false")
	if got := cancelOwner(a.Cancel); got != a {
		t.Errorf("cancelOwner(a.Cancel) = %p, want %p", got, a)
	}
	if got := cancelOwner(b.Cancel); got != b {
		t.Errorf("cancelOwner(b.Cancel) = %p, want %p", got, b)
	}
	if cancelOwner(a.Cancel) == cancelOwner(b.Cancel) {
		t.Error("two commands must not read as the same owner")
	}
}

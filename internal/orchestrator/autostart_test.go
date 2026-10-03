package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/singleton"
)

// TestSchedulerAutoStartEnabled: the auto-start gate DEFAULTS ON and is disabled only by an
// explicit falsey TTORCH_SCHEDULER_AUTOSTART (case/space-insensitive); any other value (including
// unset/empty) leaves it on, so a user opts OUT, never in.
func TestSchedulerAutoStartEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      true,
		"1":     true,
		"true":  true,
		"on":    true,
		"yes":   true,
		"junk":  true,
		"0":     false,
		"false": false,
		"FALSE": false,
		"off":   false,
		"no":    false,
		" 0 ":   false,
	}
	for v, want := range cases {
		t.Setenv("TTORCH_SCHEDULER_AUTOSTART", v)
		if got := schedulerAutoStartEnabled(); got != want {
			t.Errorf("TTORCH_SCHEDULER_AUTOSTART=%q: enabled=%v, want %v", v, got, want)
		}
	}
}

// TestAutoStartScheduler covers the three branches that decide whether the daemon is launched:
// default-on launches it once, the off-switch suppresses it, and a held singleton lock (a daemon
// already running for this ~/.ttorch) skips the launch — never starting a second daemon. The
// launch itself is stubbed via the schedulerDaemonLauncher seam so no real process is forked.
func TestAutoStartScheduler(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}

	orig := schedulerDaemonLauncher
	defer func() { schedulerDaemonLauncher = orig }()
	launched := 0
	schedulerDaemonLauncher = func(p paths.Paths) error { launched++; return nil }

	// Default-on (unset) + no daemon running -> launch exactly once.
	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "")
	launched = 0
	m.autoStartScheduler()
	if launched != 1 {
		t.Fatalf("default-on auto-start should launch the daemon once, launched=%d", launched)
	}

	// Off-switch -> never launch.
	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "0")
	launched = 0
	m.autoStartScheduler()
	if launched != 0 {
		t.Fatalf("TTORCH_SCHEDULER_AUTOSTART=0 must disable auto-start, launched=%d", launched)
	}

	// Re-enabled, but a daemon already holds the singleton lock -> skip (no second daemon).
	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "1")
	lock, acquired, err := singleton.Acquire(m.P.SchedulerPIDFile())
	if err != nil || !acquired {
		t.Fatalf("setup: pre-acquire the singleton: acquired=%v err=%v", acquired, err)
	}
	defer singleton.Release(lock)
	launched = 0
	m.autoStartScheduler()
	if launched != 0 {
		t.Fatalf("auto-start must not start a second daemon while the singleton is held, launched=%d", launched)
	}
}

// TestStartSchedulerReportsWhatItDid covers the exported start the peer control channel's
// ensure-up calls. It decides as autoStartScheduler does, and names the outcome, including the
// one the auto-start passes over in silence: no installed binary to launch.
func TestStartSchedulerReportsWhatItDid(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}
	orig := schedulerDaemonLauncher
	defer func() { schedulerDaemonLauncher = orig }()
	launched := 0
	var launchErr error
	schedulerDaemonLauncher = func(p paths.Paths) error { launched++; return launchErr }

	check := func(label, want string, wantLaunches int) {
		t.Helper()
		launched = 0
		got, err := m.StartScheduler()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got != want || launched != wantLaunches {
			t.Errorf("%s: StartScheduler = %q with %d launches, want %q with %d", label, got, launched, want, wantLaunches)
		}
	}

	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "")
	check("no installed binary", SchedulerNotInstalled, 0)

	if err := os.MkdirAll(filepath.Dir(m.P.Binary()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.P.Binary(), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("installed, nothing running", SchedulerStarted, 1)

	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "off")
	check("switched off", SchedulerDisabled, 0)

	t.Setenv("TTORCH_SCHEDULER_AUTOSTART", "1")
	lock, acquired, err := singleton.Acquire(m.P.SchedulerPIDFile())
	if err != nil || !acquired {
		t.Fatalf("setup: pre-acquire the singleton: acquired=%v err=%v", acquired, err)
	}
	check("a daemon holds the lock", SchedulerAlreadyRunning, 0)
	singleton.Release(lock)

	launchErr = errors.New("fork failed")
	launched = 0
	if got, err := m.StartScheduler(); err == nil || got != "" || launched != 1 {
		t.Errorf("a failed launch = %q, %v after %d launches; want the error", got, err, launched)
	}
}

// TestSchedulerDaemonArgsEnableGate pins the auto-started daemon's argument list: it must run the
// full mechanical loop — dispatch, GATE, land, supervise — under the singleton lock, so trusted
// done-work is gated hands-off and never waits for a manager turn, plus the WATCH loop, so the
// manager is woken for updates without re-arming `ttorch watch`. --gate and --watch are the
// load-bearing assertions (omitting either leaves that work manager-paced); the exact list also
// guards against a pass silently dropping out of auto-start. It reads the package var directly,
// so it needs no forked process.
func TestSchedulerDaemonArgsEnableGate(t *testing.T) {
	want := []string{"scheduler", "--singleton", "--dispatch", "--gate", "--land", "--supervise", "--watch"}
	if len(schedulerDaemonArgs) != len(want) {
		t.Fatalf("schedulerDaemonArgs = %v, want %v", schedulerDaemonArgs, want)
	}
	for i, a := range want {
		if schedulerDaemonArgs[i] != a {
			t.Fatalf("schedulerDaemonArgs[%d] = %q, want %q (full: %v)", i, schedulerDaemonArgs[i], a, schedulerDaemonArgs)
		}
	}
	// --gate ordered between --dispatch and --land, mirroring the loop's dispatch→gate→land order.
	di, gi, li := indexOf(schedulerDaemonArgs, "--dispatch"), indexOf(schedulerDaemonArgs, "--gate"), indexOf(schedulerDaemonArgs, "--land")
	if !(di >= 0 && gi > di && li > gi) {
		t.Fatalf("expected --dispatch < --gate < --land, got positions dispatch=%d gate=%d land=%d in %v", di, gi, li, schedulerDaemonArgs)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestLaunchSchedulerDaemonNoBinaryIsQuietNoop: the real launcher is a quiet no-op when no
// installed binary exists (running from source / under test), so it never forks a stray process
// in those contexts — the binary-existence guard, not just the seam, protects test runs.
func TestLaunchSchedulerDaemonNoBinaryIsQuietNoop(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	if err := launchSchedulerDaemon(paths.Default()); err != nil {
		t.Fatalf("with no installed binary, the launcher must be a quiet no-op, got: %v", err)
	}
}

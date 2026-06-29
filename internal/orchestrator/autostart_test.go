package orchestrator

import (
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

// TestLaunchSchedulerDaemonNoBinaryIsQuietNoop: the real launcher is a quiet no-op when no
// installed binary exists (running from source / under test), so it never forks a stray process
// in those contexts — the binary-existence guard, not just the seam, protects test runs.
func TestLaunchSchedulerDaemonNoBinaryIsQuietNoop(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	if err := launchSchedulerDaemon(paths.Default()); err != nil {
		t.Fatalf("with no installed binary, the launcher must be a quiet no-op, got: %v", err)
	}
}

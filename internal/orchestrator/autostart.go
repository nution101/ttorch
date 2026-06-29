package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/singleton"
)

// schedulerDaemonLauncher launches the detached scheduler daemon. It is a package var so a test
// can capture the launch (or stub it out) without forking a real process; production wiring is
// launchSchedulerDaemon.
var schedulerDaemonLauncher = launchSchedulerDaemon

// autoStartScheduler starts the deterministic scheduler daemon in the background so a normal
// `ttorch` session drives the board autonomously — dispatching ready backlog, landing
// already-gated work, and recovering crashed workers — while the LLM manager plans, gates, and
// answers decisions. It is:
//
//   - CONFIG-GATED and default-ON: it runs unless TTORCH_SCHEDULER_AUTOSTART is set falsey, so a
//     user can always fall back to manual dispatch/land (the manual `ttorch scheduler` subcommand
//     is unchanged);
//   - a SINGLETON: it skips the launch when a daemon already holds the scheduler lock for this
//     ~/.ttorch, and the launched daemon itself re-checks the lock (`scheduler --singleton`), so
//     even a race between two manager starts can never run two daemons;
//   - BEST-EFFORT: a launch failure is surfaced but never fails the manager attach — the daemon
//     is an accelerator, not a prerequisite, and the lead can always run `ttorch scheduler`.
//
// It coexists with the manager session purely through the DB (atomic claims): the daemon drives
// dispatch/land/supervise, the manager session gates and answers decisions. Its log goes to a
// file under ~/.ttorch, never the manager pane (no TTY injection).
func (m *Manager) autoStartScheduler() {
	if !schedulerAutoStartEnabled() {
		return
	}
	if singleton.Held(m.P.SchedulerPIDFile()) {
		return // a scheduler daemon is already running for this ~/.ttorch
	}
	if err := schedulerDaemonLauncher(m.P); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not auto-start the scheduler daemon (run 'ttorch scheduler' manually, or set TTORCH_SCHEDULER_AUTOSTART=0 to silence): %v\n", err)
	}
}

// schedulerAutoStartEnabled reports whether StartManager should auto-start the scheduler daemon.
// It DEFAULTS ON and is disabled only by an explicit falsey TTORCH_SCHEDULER_AUTOSTART
// (0/false/no/off) — the documented off-switch for falling back to manual dispatch/land.
func schedulerAutoStartEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_SCHEDULER_AUTOSTART"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// launchSchedulerDaemon forks `ttorch scheduler --singleton --dispatch --land --supervise` as a
// detached background process (its own session via Setsid, so it outlives the launching `ttorch`,
// which exits as soon as the lead's terminal attaches), with stdout/stderr redirected to the
// scheduler log. It launches the INSTALLED, user-owned binary (paths.Binary), not the running
// executable; when no installed binary exists (running from source / under test) it is a quiet
// no-op, so it never forks a stray process in those contexts.
func launchSchedulerDaemon(p paths.Paths) error {
	exe := p.Binary()
	if _, err := os.Stat(exe); err != nil {
		return nil // nothing installed to launch (from source / under test) — skip quietly
	}
	if err := os.MkdirAll(filepath.Dir(p.SchedulerLog()), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(p.SchedulerLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "scheduler", "--singleton", "--dispatch", "--land", "--supervise")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	return cmd.Start()
}

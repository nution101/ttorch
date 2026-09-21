package cli

import (
	"fmt"
	"os"
	"strings"
)

// The `ttorch approve` caller guard.
//
// `ttorch approve` is an authorized interface, and a worker session is a process running as
// the lead with the same filesystem authority: it can call the command, and it can write the
// approval token file directly without calling anything. Nothing here changes that. What this
// guard does is narrow the class: it refuses the two shapes an approval takes when it is NOT
// the lead sitting at a keyboard — a non-interactive invocation, and an invocation from inside
// a worker's own context.
//
// It is NOT a boundary and must not be described as one. A worker that means to get past it
// can run `env -u TTORCH_TASK_ID`, change to a directory with no `.ttorch/task` above it, and
// redirect stdin from any character device. The reason to have it is that the realistic cases
// are accidental and injected approvals — an agent wrapper, a script, a prompt-injected
// command in a worker pane — and both of those are stopped by it.

// approveGuardStdin is the file the guard inspects. It is a package seam so a test can drive
// the check against a real pipe, socket or character device instead of the test binary's own
// stdin.
var approveGuardStdin = func() *os.File { return os.Stdin }

// devNullPath is the null device the interactive test compares stdin against. It is a package
// seam so a test can point it at a path that does not stat, and prove the check fails closed
// rather than skipping the comparison.
var devNullPath = os.DevNull

// stdinIsInteractiveDevice reports whether f is a character device other than the null device.
//
// This is a character-device test, not a true isatty: it is the closest the standard library
// gets without an ioctl, and any character device satisfies it (`/dev/zero`, a pty, a real
// terminal). It excludes /dev/null specifically because that is what a non-interactive caller
// gets by default — a wrapper, a cron entry, a harness that redirects stdin — and os.Stat's
// ModeCharDevice alone reports /dev/null as a terminal, which would let exactly the case this
// guard exists for straight through. Pipes and sockets (what an agent's shell tool hands a
// child) are rejected by the mode test.
func stdinIsInteractiveDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// Fail closed on a stat error: a check that cannot evaluate must not pass. Skipping the
	// identity test here would let /dev/null stdin read as interactive, which is the exact
	// case the guard exists for.
	devnull, err := os.Stat(devNullPath)
	if err != nil {
		return false
	}
	return !os.SameFile(fi, devnull)
}

// workerContextSignal names the signal that marks this process as running inside a worker's
// context — $TTORCH_TASK_ID, then a .ttorch/task file at or above cwd, the two things the
// manager writes at spawn — or "" when neither is present. Both are checked, and the message
// says which one tripped, so a lead who hits this in a worktree shell can see why.
func workerContextSignal() string {
	if env := strings.TrimSpace(os.Getenv("TTORCH_TASK_ID")); env != "" {
		return fmt.Sprintf("$TTORCH_TASK_ID is set (%s)", env)
	}
	if id, _ := findTaskFile(); id != "" {
		return fmt.Sprintf("a .ttorch/task file at or above the current directory names task %s", id)
	}
	return ""
}

// checkApproveCaller returns the refusal for an approve that does not look like the lead
// running it by hand, or nil to let it through. A worker context is reported first: it is the
// more specific signal, and its fix is not "get a terminal".
func checkApproveCaller() error {
	if signal := workerContextSignal(); signal != "" {
		return fmt.Errorf("refusing to approve from inside a worker context (%s): approving is the lead's action. Run it from your own shell, outside the worktree", signal)
	}
	if !stdinIsInteractiveDevice(approveGuardStdin()) {
		return fmt.Errorf("refusing to approve without an interactive terminal: run 'ttorch approve' yourself at a terminal, not through a wrapper, a script, or an agent's shell tool")
	}
	return nil
}

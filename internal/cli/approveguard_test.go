package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openInteractiveDevice opens a character device that is not the null device, standing in for
// the terminal a lead approves from. /dev/zero is a character device on both platforms ttorch
// ships for, and — this is the honest part — so is a real terminal and so is a pty a worker
// allocates: the guard cannot tell them apart. Using it here makes the test say exactly what
// the check tests, which is "a character device other than /dev/null", not "a TTY".
func openInteractiveDevice(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open("/dev/zero")
	if err != nil {
		t.Skipf("no /dev/zero on this platform: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestStdinIsInteractiveDevice covers the shapes stdin actually takes. A pipe and a socket are
// what an agent's shell tool or a wrapper hands a child; /dev/null is what a non-interactive
// caller gets by default and is the case os.ModeCharDevice alone gets WRONG, which is why the
// guard tests for it by identity.
func TestStdinIsInteractiveDevice(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if stdinIsInteractiveDevice(r, os.DevNull) {
		t.Error("a pipe must not read as an interactive terminal")
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if fi, err := devnull.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		t.Fatal("precondition: /dev/null should be a character device, so the identity check is what rejects it")
	}
	if stdinIsInteractiveDevice(devnull, os.DevNull) {
		t.Error("/dev/null must not read as an interactive terminal, even though it is a character device")
	}

	closed, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if stdinIsInteractiveDevice(closed, os.DevNull) {
		t.Error("an unstattable fd must not read as an interactive terminal")
	}

	if !stdinIsInteractiveDevice(openInteractiveDevice(t), os.DevNull) {
		t.Error("a character device that is not /dev/null must read as interactive")
	}
}

// TestStdinIsInteractiveDevice_FailsClosedOnStatError: when the null device cannot be stat'd
// the identity comparison cannot run, and the check must refuse rather than skip it. Skipping
// would let /dev/null stdin read as interactive, which is the case the guard exists for.
// Reaching this for real needs control of the process's view of /dev, which already defeats
// the guard — the point is that a check which cannot evaluate does not pass.
func TestStdinIsInteractiveDevice_FailsClosedOnStatError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-null-device")
	if stdinIsInteractiveDevice(openInteractiveDevice(t), missing) {
		t.Error("a stat error on the null device must refuse, not fall through to interactive")
	}
}

// TestCheckApproveCaller_RefusesNonInteractive: an approve through a pipe — the shape a
// wrapper, a script or an agent's shell tool produces — is refused.
func TestCheckApproveCaller_RefusesNonInteractive(t *testing.T) {
	clearWorkerContext(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	err = checkApproveCaller(r)
	if err == nil {
		t.Fatal("approve through a pipe must be refused")
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("the refusal must say a terminal is required, got: %v", err)
	}
}

// TestCheckApproveCaller_RefusesWorkerEnv: $TTORCH_TASK_ID marks the caller as a worker, and
// it is refused even when stdin IS an interactive device — a worker that gets a terminal does
// not get an approval with it.
func TestCheckApproveCaller_RefusesWorkerEnv(t *testing.T) {
	clearWorkerContext(t)
	t.Setenv("TTORCH_TASK_ID", "w7")

	err := checkApproveCaller(openInteractiveDevice(t))
	if err == nil {
		t.Fatal("approve from a worker's environment must be refused")
	}
	if !strings.Contains(err.Error(), "worker context") || !strings.Contains(err.Error(), "w7") {
		t.Fatalf("the refusal must name the worker signal that tripped, got: %v", err)
	}
}

// TestCheckApproveCaller_RefusesWorkerTaskFile: the .ttorch/task file the manager writes at
// spawn marks the caller as a worker on its own, with no environment variable set — so
// `env -u TTORCH_TASK_ID ttorch approve` inside a worktree is still refused. The file is
// placed in a PARENT of cwd to prove the walk-up.
func TestCheckApproveCaller_RefusesWorkerTaskFile(t *testing.T) {
	clearWorkerContext(t)
	root := t.TempDir()
	writeTaskFile(t, root, "w9", filepath.Join(root, "state.db"))
	sub := filepath.Join(root, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	err := checkApproveCaller(openInteractiveDevice(t))
	if err == nil {
		t.Fatal("approve from inside a worktree with a .ttorch/task above cwd must be refused")
	}
	if !strings.Contains(err.Error(), ".ttorch/task") || !strings.Contains(err.Error(), "w9") {
		t.Fatalf("the refusal must name the task file that tripped, got: %v", err)
	}
}

// TestCheckApproveCaller_AllowsLeadAtATerminal is the must-not-trip case: no worker signal and
// an interactive device, which is the lead running the command by hand.
func TestCheckApproveCaller_AllowsLeadAtATerminal(t *testing.T) {
	clearWorkerContext(t)
	if err := checkApproveCaller(openInteractiveDevice(t)); err != nil {
		t.Fatalf("the lead approving at a terminal must be allowed: %v", err)
	}
}

// clearWorkerContext puts the test in a lead context: no $TTORCH_TASK_ID, and a cwd with no
// .ttorch/task anywhere above it (the repo's own tree has one when a worker runs the suite).
func clearWorkerContext(t *testing.T) {
	t.Helper()
	t.Setenv("TTORCH_TASK_ID", "")
	t.Chdir(t.TempDir())
}

// TestCmdApprove_GuardRunsBeforeAnyState proves the guard is actually WIRED INTO the command
// and runs ahead of it: from a worker context `ttorch approve` never reaches the manager, so
// it cannot mint a token whatever the task id; from a lead context at a terminal the same call
// gets through the guard and fails on the task lookup instead.
func TestCmdApprove_GuardRunsBeforeAnyState(t *testing.T) {
	clearWorkerContext(t)
	t.Setenv("TTORCH_HOME", t.TempDir())
	// cmdApprove reads os.Stdin directly, so the test swaps the real thing rather than a
	// seam: with it on a character device, the interactive test is not what refuses below.
	prev := os.Stdin
	os.Stdin = openInteractiveDevice(t)
	t.Cleanup(func() { os.Stdin = prev })

	t.Setenv("TTORCH_TASK_ID", "w3")
	err := cmdApprove([]string{"w3"})
	if err == nil || !strings.Contains(err.Error(), "worker context") {
		t.Fatalf("cmdApprove from a worker context = %v, want the caller-guard refusal", err)
	}

	t.Setenv("TTORCH_TASK_ID", "")
	err = cmdApprove([]string{"nosuchtask"})
	if err == nil || strings.Contains(err.Error(), "worker context") || strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("cmdApprove from a lead context at a terminal = %v, want it past the guard and into the task lookup", err)
	}
}

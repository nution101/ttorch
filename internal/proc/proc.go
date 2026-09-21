// Package proc starts child processes so that a deadline actually ends the work.
//
// os/exec's CommandContext kills the process it started and nothing beneath it. Real
// commands fork: a build forks a compiler, a server, a file watcher; `git ls-remote` forks
// ssh, or git-remote-https and a credential helper. Those grandchildren survive the kill,
// and when the caller collects output through a pipe they hold the write end of it open, so
// Wait goes on blocking in the copy goroutines long after the deadline fired. A timeout that
// cannot end the work it bounds is not a bound at all.
//
// Command closes both halves of that gap: the child gets its own process group and the whole
// group is killed when ctx is done, and WaitDelay caps how long Wait will linger on pipes an
// escaped process might still be holding.
//
// The guarantee lives in three fields of the returned *exec.Cmd, all of them public and
// mutable, and every way of losing it is silent. So starting the command through this
// package's Start, Run or CombinedOutput checks them first and refuses to run a command
// whose enforcement has been taken away. Calling c.Start or c.Run directly skips that check.
package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// WaitDelay is how long Wait may linger after the process is gone before it closes the
// child's pipes and gives up on them. It is the backstop for a child that left the process
// group and so survived the group kill; two seconds is long enough for an orderly flush of
// output already written and short enough that no caller notices.
const WaitDelay = 2 * time.Second

// ErrDisarmed reports a command whose timeout enforcement has been removed. Start, Run and
// CombinedOutput return it instead of running such a command, so a disarm is loud.
var ErrDisarmed = errors.New("proc: timeout enforcement is disarmed")

// Command is exec.CommandContext plus the three settings that make ctx an enforceable
// bound. Set Dir, Env, Stdin, Stdout, Stderr and the rest as usual, then start it with this
// package's Start, Run or CombinedOutput.
//
// SysProcAttr, Cancel and WaitDelay carry the guarantee. Check describes what each one is
// for and what losing it costs; it runs before every start this package performs, so a
// command that has lost one fails loudly rather than running unbounded.
func Command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, arg...)
	// A new process group, so one signal reaches everything the command forked.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = groupKill(c)
	c.WaitDelay = WaitDelay
	return c
}

// groupKill is the Cancel this package installs: it signals the child's whole process
// group rather than the child alone.
//
// Every closure this function returns shares one code pointer, which is how Check tells the
// group kill apart from a replacement (see groupKillPC). That holds only while the function
// is not inlined: inlining gives each call site its own copy of the closure, and the
// comparison then rejects commands this package built itself. Hence the directive, and
// TestCancelIdentityIsStable, which compares a real command's Cancel against groupKillPC.
//
//go:noinline
func groupKill(c *exec.Cmd) func() error {
	return func() error {
		if c.Process == nil {
			return nil
		}
		// Negative pid: the group, not just the leader.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// groupKillPC is the code pointer of the closure groupKill returns. Func values are not
// comparable in Go, but every closure produced by one func literal shares a code pointer,
// so this distinguishes "the Cancel Command installed" from "some other function".
var groupKillPC = reflect.ValueOf(groupKill(nil)).Pointer()

// Check reports whether c can still enforce its context deadline, naming everything it has
// lost. A nil error means all three settings are intact.
//
// Each of the three fails silently rather than loudly, which is why this exists:
//
//   - SysProcAttr.Setpgid gives the child a process group of its own. Without it the child
//     stays in its parent's group, no group carries the child's pid as its id, and the kill
//     below finds nothing: it returns ESRCH and the child and everything it forked survive
//     the deadline outright. (Measured: with Setpgid dropped, kill(-pid) returns "no such
//     process" and the whole tree runs on.)
//   - SysProcAttr.Pgid must stay zero, which is what makes the child lead its own group.
//     Setting it makes the child JOIN that group instead, so the kill again names a group
//     that does not exist while the group it did join holds processes this command does not
//     own.
//   - Cancel is the group kill. Replacing it with anything else, os/exec's default
//     included, goes back to killing the process alone and leaving what it forked.
//   - WaitDelay bounds the wait on pipes. Zeroing it means Wait blocks for as long as a
//     child that escaped the group holds the write end, with no deadline reaching it.
func Check(c *exec.Cmd) error {
	var lost []string
	if c.SysProcAttr == nil {
		lost = append(lost, "SysProcAttr is nil, so the child shares its parent's process group and the group kill finds nothing to kill")
	} else {
		if !c.SysProcAttr.Setpgid {
			lost = append(lost, "SysProcAttr.Setpgid is false, so the child shares its parent's process group and the group kill finds nothing to kill")
		}
		if c.SysProcAttr.Pgid != 0 {
			lost = append(lost, fmt.Sprintf("SysProcAttr.Pgid is %d, so the child joins that group instead of leading its own", c.SysProcAttr.Pgid))
		}
	}
	switch {
	case c.Cancel == nil:
		lost = append(lost, "Cancel is nil, so the deadline kills the process alone and leaves whatever it forked")
	case reflect.ValueOf(c.Cancel).Pointer() != groupKillPC:
		lost = append(lost, "Cancel is not the group kill this package installed")
	}
	if c.WaitDelay == 0 {
		lost = append(lost, "WaitDelay is zero, so Wait can block forever on a pipe an escaped child holds")
	}
	if len(lost) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDisarmed, strings.Join(lost, "; "))
}

// Start checks c and starts it. It is exec.Cmd.Start for a command from Command.
func Start(c *exec.Cmd) error {
	if err := Check(c); err != nil {
		return err
	}
	return c.Start()
}

// Run checks c and runs it to completion.
func Run(c *exec.Cmd) error {
	if err := Check(c); err != nil {
		return err
	}
	return c.Run()
}

// CombinedOutput checks c, then runs it and returns its combined output.
func CombinedOutput(c *exec.Cmd) ([]byte, error) {
	if err := Check(c); err != nil {
		return nil, err
	}
	return c.CombinedOutput()
}

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
	"sync"
	"sync/atomic"
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

// ErrUnverifiable reports a command whose enforcement could not be established either way.
// It is deliberately separate from ErrDisarmed: "this is wrong" and "I cannot tell" are
// different answers, and only the first is a finding about the caller's code. Both refuse to
// run, because a gate that runs what it cannot vouch for is worse than one that stops.
var ErrUnverifiable = errors.New("proc: timeout enforcement could not be verified")

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
	c.Cancel = (&killer{cmd: c}).cancel
	c.WaitDelay = WaitDelay
	return c
}

// killer owns the group kill for exactly ONE command. Cancel is a method value on it rather
// than a bare closure, which is what lets Check ask two different questions: whether the
// function is one of ours, and whether it belongs to the command being checked.
//
// The second question is the one that matters and the one an earlier version never asked.
// `cp := *c` and `c.Cancel = other.Cancel` both leave a Cancel that closes over a DIFFERENT
// command, whose Process is nil, so it returns without signalling and the forked grandchild
// outlives the deadline. Both passed a check that only compared code pointers.
type killer struct{ cmd *exec.Cmd }

// probe lets Check ask a Cancel which command it belongs to without killing anything.
//
// The question is only askable before the command starts, which is the only time Check is
// meant to run, and that is also what keeps a probe from ever suppressing a real kill: the
// probe is answered on the path where Process is nil and there is nothing to signal, so a
// cancel with a live process goes straight to the kill whatever any probe is doing.
var (
	probeMu   sync.Mutex // one probe at a time
	probeWant atomic.Pointer[exec.Cmd]
	probeHit  atomic.Bool
)

func (k *killer) cancel() error {
	if k.cmd.Process == nil {
		if probeWant.Load() == k.cmd {
			probeHit.Store(true)
		}
		return nil
	}
	// Negative pid: the group, not just the leader.
	return syscall.Kill(-k.cmd.Process.Pid, syscall.SIGKILL)
}

// cancelPC is the code pointer of killer.cancel as a method value. Func values are not
// comparable in Go, but every method value of one method shares the wrapper's code pointer,
// so this tells "a Cancel this package installed" from "some other function". It is checked
// first, and only so that Check never invokes a function a caller supplied.
//
// A mismatch fails closed, which is a decision and not a default. At runtime a mismatch is
// indistinguishable from a deliberate replacement, so there is nothing safer to do with one.
// The risk that argues the other way is a toolchain change moving this wrapper's code
// pointer: Check would then reject every command and leave the validate gate permanently
// red. TestCancelIdentityIsStable is the guard, comparing a real command's Cancel to this
// value, so such a change fails a build rather than a gate in the field. The closure this
// replaced needed a go:noinline pragma to keep the property; a method value keeps it with no
// pragma, under -l, -l=4, -N -l, -trimpath, -buildmode=pie, -race, -ldflags=-s -w and PGO.
var cancelPC = reflect.ValueOf((&killer{}).cancel).Pointer()

// boundTo reports whether c.Cancel is the group kill belonging to c. The caller must have
// established that c.Cancel is one of ours and that c has not started.
func boundTo(c *exec.Cmd) bool {
	probeMu.Lock()
	defer probeMu.Unlock()
	probeHit.Store(false)
	probeWant.Store(c)
	defer probeWant.Store(nil)
	_ = c.Cancel()
	return probeHit.Load()
}

// Check reports whether c can still kill its whole process group when its context ends,
// naming everything it has lost. A nil error means the group kill is intact and bound to c.
//
// What Check cannot see is the context itself: exec.Cmd keeps it unexported, so a command
// built with context.Background() passes. Check answers "when the deadline fires, does the
// whole tree die", not "is there a deadline".
//
// Call it before Start. The binding below is established by asking the installed Cancel
// which command it holds, which is only safe while there is no process to signal; on a
// started command Check returns ErrUnverifiable rather than guessing either way.
//
// Each of these fails silently rather than loudly, which is why this exists:
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
//   - Cancel must be the group kill BOUND TO THIS COMMAND. A copied exec.Cmd, or a Cancel
//     lifted from another command, carries a kill that captured a different Cmd; it finds
//     that Cmd's nil Process, returns without signalling, and the tree survives on the
//     WaitDelay backstop alone. (Measured: both shapes let a forked grandchild run to
//     completion while Run returned at the deadline.)
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
	case reflect.ValueOf(c.Cancel).Pointer() != cancelPC:
		lost = append(lost, "Cancel is not the group kill this package installed")
	case c.Process != nil:
		// The binding can only be established before the command starts. Say that rather
		// than pass a command whose kill has not been vouched for.
		return fmt.Errorf("%w: Check must run before Start, so the group kill's binding can be established without signalling anything", ErrUnverifiable)
	case !boundTo(c):
		lost = append(lost, "Cancel belongs to a different command, so the deadline would signal that command's process group and not this one's (a copied exec.Cmd, or a Cancel taken from another command, does exactly this)")
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

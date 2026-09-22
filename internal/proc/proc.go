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
// The guarantee lives in four fields of the returned *exec.Cmd, all of them public and
// mutable, and every way of losing it is silent. Three are settings Command makes:
// SysProcAttr, Cancel and WaitDelay. The fourth is Process, which the kill reads when it
// fires, so repointing it after Start aims the kill somewhere else. Starting the command
// through this package's Start, Run or CombinedOutput checks the three settings first and
// refuses a command whose enforcement has been taken away; Process cannot be checked, since
// Start is what sets it. Calling c.Start or c.Run directly skips the check entirely.
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
	"unsafe"
)

// WaitDelay is how long Wait may linger after the process is gone before it closes the
// child's pipes and gives up on them. It is the backstop for a child that left the process
// group and so survived the group kill; two seconds is long enough for an orderly flush of
// output already written and short enough that no caller notices.
const WaitDelay = 2 * time.Second

// Unix only, deliberately without a build constraint. Setpgid, Pgid and kill(-pid) have no
// Windows equivalent, so a //go:build unix line here would be honest about this file and
// useless in practice: GOOS=windows already fails to build internal/singleton and
// internal/watch on syscall.Flock as well, so constraining this package alone would move
// the first error without making the binary buildable. Makefile PLATFORMS is darwin and
// linux, and CI is ubuntu plus macos. A Windows port is its own change, and it is the port
// that would add the constraints, together with the implementations behind them.

// ErrDisarmed reports a command whose timeout enforcement has been removed. Start, Run and
// CombinedOutput return it instead of running such a command, so a disarm is loud.
var ErrDisarmed = errors.New("proc: timeout enforcement is disarmed")

// ErrUnverifiable reports a command whose enforcement could not be established either way.
// It is deliberately separate from ErrDisarmed: "this is wrong" and "I cannot tell" are
// different answers, and only the first is a finding about the caller's code. Both refuse to
// run, because a gate that runs what it cannot vouch for is worse than one that stops.
//
// One thing produces it today: the layout self-test behind cancelOwner failing, which would
// make every binding unreadable in that binary rather than only this command's. A caller
// cannot act on it by editing the command, which is exactly why it is not ErrDisarmed.
var ErrUnverifiable = errors.New("proc: timeout enforcement could not be verified")

// Command is exec.CommandContext plus the three settings that make ctx an enforceable
// bound. Set Dir, Env, Stdin, Stdout, Stderr and the rest as usual, then start it with this
// package's Start, Run or CombinedOutput. Check describes what each setting is for and what
// losing it costs.
//
// Do not mutate a Cmd after Start. Process is the fourth field the guarantee rests on and
// the only one Check cannot vouch for, because Check runs before Start: a command whose
// Process was repointed at another live command killed that command's group at its own
// deadline. Mutating a Cmd after Start is already outside os/exec's contract and nothing
// here does it, so this is recorded rather than defended against.
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
// `cp := *c` and `c.Cancel = other.Cancel` both leave a Cancel closed over a DIFFERENT
// command, so this command's group is never signalled and its forked grandchild outlives the
// deadline. Both shapes passed a check that only compared code pointers. Check's last bullet
// says what becomes of the other command.
type killer struct{ cmd *exec.Cmd }

// cancel kills the command's process group. Process nil means the command never started,
// so there is nothing to signal.
//
// It does NOT mean the command is still running. os/exec never clears Process, so after Wait
// this fires at a pid the OS has already freed, where os.Process.Kill would have refused
// with "process already finished". Measured for that case alone: the raw kill returns ESRCH,
// 500 runs of 500. On the real path it happens when a deadline lands between Wait reaping
// the child and the cancel handshake completing.
//
// A wider run of 4000 timed commands is where the interesting number is, and it does not
// support a single-errno claim: the kill fired 3873 times, delivered 3775, and returned
// EPERM 93 times against ESRCH 4. Every one of those EPERMs came from a firing where Wait
// had not yet recorded the exit, so none of them is the after-reap case above. Why the
// kernel answers EPERM there rather than delivering the signal is unexplained, and is left
// unexplained rather than guessed at.
//
// The after-reap kill stays as it is. The guard that would close it is ProcessState, which
// Wait writes on another goroutine while this one runs, so reading it here is a genuine data
// race: with the guard in place, -race reports Wait writing it against this function reading
// it. A race in the kill path is worse than a signal to a pid freed microseconds earlier, a
// window too narrow for the pid to have been reused. That narrowness is the whole argument
// and it is not available to a caller reaching for a pid reaped seconds ago; see the SIGSTOP
// note in the tests.
func (k *killer) cancel() error {
	if k.cmd.Process == nil {
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
// A mismatch fails closed, which is a decision and not a default: at runtime it cannot be
// told from a deliberate replacement. The risk the other way is a toolchain change moving
// this wrapper's code pointer, which would reject every command and leave the validate gate
// permanently red. TestCancelIdentityIsStable guards that by comparing a real command's
// Cancel to this value, so such a change fails a build rather than a gate in the field.
var cancelPC = reflect.ValueOf((&killer{}).cancel).Pointer()

// methodValue mirrors the runtime layout of a method value: the code pointer, then the
// captured receiver. (*killer).cancel captures exactly one thing, the *killer it was taken
// from, so a Cancel whose code pointer is cancelPC has this shape.
type methodValue struct {
	code uintptr
	recv *killer
}

// cancelOwner returns the command a Cancel installed by this package is bound to. It READS
// the bound receiver and calls nothing.
//
// Calling was the obvious way to ask and it was wrong: a probe that invoked a borrowed or
// copied Cancel reached the group kill of the command it really belonged to, killing a live
// process group from inside a function whose contract is to report. Nothing here signals, so
// the question is free to ask, before or after Start.
//
// The caller must have established that f's code pointer is cancelPC, and layoutOK must
// hold. Check establishes both before reaching this.
func cancelOwner(f func() error) *exec.Cmd {
	return (*methodValue)(*(*unsafe.Pointer)(unsafe.Pointer(&f))).recv.cmd
}

// layoutOK records whether the method-value layout cancelOwner depends on actually holds in
// this binary. It is a runtime self-test rather than an assumption: build a killer, take its
// method value, read the receiver back the way Check will, and require it to be the killer
// we started from.
//
// That layout is not guaranteed by the language, and this self-test is what notices if a
// toolchain changes it. It notices one of two ways, because it proves the layout by
// performing the read it is proving: an init panic from dereferencing a wrong offset, or
// layoutOK false and Check reporting ErrUnverifiable for every command. Both are loud and
// the suite catches either. What cannot happen is a binding certified from a pointer read
// out of the wrong place.
var layoutOK = verifyLayout()

func verifyLayout() bool {
	// A stand-in command. Nothing is started on it, and its Cancel is never invoked.
	stand := &exec.Cmd{}
	f := (&killer{cmd: stand}).cancel
	if reflect.ValueOf(f).Pointer() != cancelPC {
		return false
	}
	return cancelOwner(f) == stand
}

// Check reports whether c can still kill its whole process group when its context ends,
// naming everything it has lost. A nil error means the group kill is intact and bound to c.
//
// What Check cannot see is the context itself: exec.Cmd keeps it unexported, so a command
// built with context.Background() passes. Check answers "when the deadline fires, does the
// whole tree die", not "is there a deadline".
//
// Check never signals anything, and must not start to: the binding is READ out of the
// installed Cancel rather than exercised, which is answerable before or after Start and
// costs the caller nothing. See cancelOwner for what invoking it cost instead.
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
//     lifted from another command, carries a kill that captured a different Cmd, so this
//     command's group is never signalled and its tree survives on the WaitDelay backstop
//     alone. (Measured: both shapes let a forked grandchild run to completion while Run
//     returned at the deadline.) If that other command happens to be running, the deadline
//     kills ITS group instead, so the same defect also reaches a process group the caller
//     never meant to touch.
//   - WaitDelay bounds the wait on pipes. Zeroing it means Wait blocks for as long as a
//     child that escaped the group holds the write end, with no deadline reaching it.
func Check(c *exec.Cmd) error {
	// Field reads first. Every one of these is establishable on its own, so none of them is
	// skipped because some later question was harder: an early return here once reported a
	// started command as merely unverifiable when its SysProcAttr was also gone, and
	// "unverifiable" invites a retry where "disarmed" does not.
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
	// The Cancel questions. Identity comes before the binding because the binding is read
	// out of a method value, and only an identity match makes it one.
	var unverifiable string
	switch {
	case c.Cancel == nil:
		lost = append(lost, "Cancel is nil, so the deadline kills the process alone and leaves whatever it forked")
	case reflect.ValueOf(c.Cancel).Pointer() != cancelPC:
		lost = append(lost, "Cancel is not the group kill this package installed")
	case !layoutOK:
		unverifiable = "the method-value layout this package reads a Cancel's binding from does not hold in this binary, so no command's binding can be established"
	case cancelOwner(c.Cancel) != c:
		lost = append(lost, "Cancel belongs to a different command, so the deadline would signal that command's process group and not this one's (a copied exec.Cmd, or a Cancel taken from another command, does exactly this)")
	}
	if c.WaitDelay == 0 {
		lost = append(lost, "WaitDelay is zero, so Wait can block forever on a pipe an escaped child holds")
	}
	// Something definite outranks something undetermined: a caller told "disarmed" knows
	// what to fix.
	if len(lost) > 0 {
		return fmt.Errorf("%w: %s", ErrDisarmed, strings.Join(lost, "; "))
	}
	if unverifiable != "" {
		return fmt.Errorf("%w: %s", ErrUnverifiable, unverifiable)
	}
	return nil
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

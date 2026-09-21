// Package proc starts child processes so that a deadline actually ends the work.
//
// os/exec's CommandContext kills the process it started and nothing beneath it. Real
// commands fork: `git ls-remote` forks ssh, or git-remote-https and a credential helper; a
// build forks a compiler, a server, a file watcher. Those grandchildren survive the kill,
// and when the caller collects output through a pipe they hold the write end of it open, so
// Wait goes on blocking in the copy goroutines long after the deadline fired. A timeout that
// cannot end the work it bounds is not a bound at all.
//
// Command closes both halves of that gap: the child gets its own process group and the whole
// group is killed when ctx is done, and WaitDelay caps how long Wait will linger on pipes an
// escaped process might still be holding.
package proc

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay is how long Wait may linger after the process is gone before it closes the
// child's pipes and gives up on them. It is the backstop for a grandchild that outlived the
// group kill; two seconds is long enough for an orderly flush of output already written and
// short enough that no caller notices.
const WaitDelay = 2 * time.Second

// Command is exec.CommandContext plus the two settings that make ctx an enforceable bound.
// Callers use the returned *exec.Cmd exactly as they would one from exec.CommandContext.
func Command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, arg...)
	// A new process group, so one signal reaches everything the command forked.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative pid: the group, not just the leader.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = WaitDelay
	return c
}

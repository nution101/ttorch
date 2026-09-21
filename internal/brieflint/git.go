package brieflint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// waitDelay is how long Wait may linger after git is gone before it closes the pipes and
// gives up on them. It is the backstop for a child that outlived the group kill by leaving
// the group; two seconds is long enough for an orderly flush of output already written.
const waitDelay = 2 * time.Second

// gitCommand builds the git invocation so that a deadline actually ends the work.
//
// exec.CommandContext on its own kills the process it started and nothing beneath it, and
// git forks: ssh for an SSH remote, git-remote-https and a credential helper for an HTTPS
// one. Those forks inherit the write end of the stdout pipe, so killing git alone leaves
// them holding it, Run keeps blocking in the copy goroutines, and the deadline bounds
// nothing. So git gets its own process group and the whole group is killed when ctx is
// done, with WaitDelay capping the wait on any pipe a fork that left the group still holds.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", args...)
	// A new process group, so one signal reaches everything git forked.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative pid: the group, not just the leader.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = waitDelay
	return c
}

// gitTimeout bounds ONE git call. It is a ceiling per call, not a budget for the run: every
// call also derives from the run-wide deadline (see Options.Budget), and both are enforced
// by gitCommand below rather than merely set.
const gitTimeout = 20 * time.Second

// maxBlobBytes caps the blob a line count will read. Counting the lines of a path means
// reading its contents, and a brief can cite a path of any size (a vendored bundle, a
// checked-in binary, a generated fixture), so the read is capped rather than trusted. Over
// the cap the citation is reported as unevaluable, never as a violation.
const maxBlobBytes = 4 << 20 // 4 MiB

// gitResult is one git invocation. stdout is kept RAW and separate from stderr: one caller
// counts the lines of a file's contents, so trimming it or folding a diagnostic into it
// would miscount.
type gitResult struct {
	stdout string
	stderr string
	// exit is the process exit status; 0 means success. Meaningful only when err is nil.
	exit int
	// err is non-nil when git produced no exit status at all — the binary is missing, or
	// the call ran out of time. That is unevaluable, never a failed check.
	err error
}

// token is stdout as a single value (a ref, a byte count), whitespace stripped.
func (r gitResult) token() string { return strings.TrimSpace(r.stdout) }

// maxMsgBytes caps the git diagnostic carried into a finding. One line of git is a
// sentence; anything longer is a remote talking at length into a report a human reads.
const maxMsgBytes = 200

// msg is the short diagnostic to quote when git failed for a reason the caller cannot
// classify. It comes back QUOTED, because git's stderr is not ours: every git host relays
// a server's "remote:" output verbatim, so ESC, CR and BEL arrive live and a crafted line
// can erase what a manager just read and write its own. Reproduced with a remote emitting
// "remote: \x1b[2K\rEVERYTHING IS FINE\a", which reached the terminal intact.
//
// strconv.Quote is the same treatment AGENTS.md values get: it escapes on unicode.IsPrint,
// so control characters, the line and paragraph separators, and invalid UTF-8 all come out
// as text rather than as instructions to the terminal.
func (r gitResult) msg() string {
	m := strings.TrimSpace(r.stderr)
	if m == "" {
		m = strings.TrimSpace(r.stdout)
	}
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = m[:i]
	}
	if len(m) > maxMsgBytes {
		m = m[:maxMsgBytes] + "..."
	}
	return strconv.Quote(m)
}

// gitFunc runs git in dir, under ctx. Tests substitute it via Options.git to exercise the
// unevaluable paths (an unreachable remote, a missing git, an exhausted budget) without a
// network.
type gitFunc func(ctx context.Context, dir string, args ...string) gitResult

// gitRun is the real git. Its deadline is the SOONER of gitTimeout and whatever ctx already
// carries, so the run-wide budget always wins over the per-call ceiling.
func gitRun(ctx context.Context, dir string, args ...string) gitResult {
	// Fail fast rather than start a process that cannot finish: an already-exhausted budget
	// is the case that keeps a brief naming many refs from stalling the caller.
	if err := ctx.Err(); err != nil {
		return gitResult{err: fmt.Errorf("git %s not attempted: %w", args[0], err)}
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := gitCommand(ctx, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := gitResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		res.err = fmt.Errorf("git %s ran out of time: %w", args[0], ctx.Err())
		return res
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return res
	case errors.As(err, &ee):
		res.exit = ee.ExitCode()
		return res
	default:
		// git could not be started at all (not installed, dir missing).
		res.err = fmt.Errorf("running git %s: %w", args[0], err)
		return res
	}
}

// remoteBranchExists reports whether branch is a head on remote. The bool is meaningful
// only when the error is nil; a non-nil error means the question could not be answered
// (the remote was unreachable, the budget ran out, git is missing), which the caller
// reports as StatusIndeterminate — never as "the branch does not exist".
func (o Options) remoteBranchExists(ctx context.Context, remote, branch string) (bool, error) {
	// --exit-code makes "no such ref" a distinct status (2) instead of a successful empty
	// listing, so a typo'd branch cannot read the same as a healthy query. The "--" keeps
	// every brief-derived value a positional argument: the extraction regex already
	// excludes a leading "-", and this makes that the second line of defence rather than
	// the only one.
	res := o.git(ctx, o.Repo, "ls-remote", "--exit-code", "--heads", "--", remote, branch)
	switch {
	case res.err != nil:
		return false, res.err
	case res.exit == 0:
		return res.token() != "", nil
	case res.exit == 2:
		return false, nil
	default:
		return false, fmt.Errorf("git ls-remote %s %s failed (exit %d): %s", remote, branch, res.exit, res.msg())
	}
}

// revExists reports whether ref resolves to a commit in the repository. As with
// remoteBranchExists, a non-nil error means unevaluable rather than absent.
func (o Options) revExists(ctx context.Context, ref string) (bool, error) {
	res := o.git(ctx, o.Repo, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	switch {
	case res.err != nil:
		return false, res.err
	case res.exit == 0:
		return true, nil
	case res.exit == 1:
		// rev-parse --quiet exits 1 for an unknown revision.
		return false, nil
	default:
		return false, fmt.Errorf("git rev-parse %s failed (exit %d): %s", ref, res.exit, res.msg())
	}
}

// objectType returns the git object type of path at ref ("blob" for a file, "tree" for a
// directory) and whether it exists there.
//
// It asks with ls-tree rather than `cat-file -t` because ls-tree separates the two answers
// that matter: an absent path is a SUCCESSFUL query with no entry, while a query that could
// not be performed at all (a shallow clone missing the object, a corrupt object, an
// exhausted budget) is a non-zero exit. `cat-file -t` collapses both into exit 128, which
// would report a repository problem as a rule violation — inconsistent with every other
// query in this file, which treats an unclassifiable failure as unevaluable.
func (o Options) objectType(ctx context.Context, ref, path string) (string, bool, error) {
	res := o.git(ctx, o.Repo, "ls-tree", "--full-tree", "--end-of-options", ref, "--", path)
	switch {
	case res.err != nil:
		return "", false, res.err
	case res.exit != 0:
		return "", false, fmt.Errorf("git ls-tree %s -- %s failed (exit %d): %s", ref, path, res.exit, res.msg())
	}
	line := res.stdout
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	// "<mode> <type> <sha>\t<path>": absent means no entry at all, which is the query
	// succeeding with a negative answer.
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false, nil
	}
	return fields[1], true, nil
}

// lineCount returns the number of lines of path at ref, counted the way an editor and a
// reviewer count them: a trailing newline closes the last line rather than starting a new
// one, and a file whose last line is unterminated still counts.
//
// The blob's size is checked first so an oversized path is declined instead of read into
// memory; callers memoize per path so citing twenty lines of one file reads it once.
func (o Options) lineCount(ctx context.Context, ref, path string) (int, error) {
	obj := ref + ":" + path
	size := o.git(ctx, o.Repo, "cat-file", "-s", "--end-of-options", obj)
	if size.err != nil {
		return 0, size.err
	}
	if size.exit != 0 {
		return 0, fmt.Errorf("git cat-file -s %s failed (exit %d): %s", obj, size.exit, size.msg())
	}
	n, err := strconv.Atoi(size.token())
	if err != nil {
		return 0, fmt.Errorf("git cat-file -s %s returned %q, not a byte count", obj, size.token())
	}
	if n > maxBlobBytes {
		return 0, fmt.Errorf("%s is %d bytes at %s, over the %d-byte line-count cap", path, n, ref, maxBlobBytes)
	}
	res := o.git(ctx, o.Repo, "cat-file", "blob", "--end-of-options", obj)
	if res.err != nil {
		return 0, res.err
	}
	if res.exit != 0 {
		return 0, fmt.Errorf("git cat-file blob %s failed (exit %d): %s", obj, res.exit, res.msg())
	}
	body := res.stdout
	if body == "" {
		return 0, nil
	}
	lines := strings.Count(body, "\n")
	if !strings.HasSuffix(body, "\n") {
		lines++
	}
	return lines, nil
}

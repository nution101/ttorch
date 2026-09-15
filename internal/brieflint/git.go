package brieflint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout bounds every git call the linter makes. A brief lint runs in front of
// `ttorch task add`, and one of its checks talks to a remote: an unreachable remote must
// time out into an INDETERMINATE finding rather than hang the add.
const gitTimeout = 20 * time.Second

// gitResult is one git invocation. stdout is kept RAW and separate from stderr: one caller
// counts the lines of a file's contents, so trimming it or folding a diagnostic into it
// would miscount.
type gitResult struct {
	stdout string
	stderr string
	// exit is the process exit status; 0 means success. Meaningful only when err is nil.
	exit int
	// err is non-nil when git produced no exit status at all — the binary is missing, or
	// the call timed out. That is unevaluable, never a failed check.
	err error
}

// token is stdout as a single value (a ref, an object type), whitespace stripped.
func (r gitResult) token() string { return strings.TrimSpace(r.stdout) }

// msg is the short diagnostic to quote when git failed for a reason the caller cannot
// classify.
func (r gitResult) msg() string {
	m := strings.TrimSpace(r.stderr)
	if m == "" {
		m = strings.TrimSpace(r.stdout)
	}
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = m[:i]
	}
	return m
}

// gitFunc runs git in dir. Tests substitute it via Options.git to exercise the unevaluable
// paths (an unreachable remote, a missing git) without a network.
type gitFunc func(dir string, args ...string) gitResult

// gitRun is the real git.
func gitRun(dir string, args ...string) gitResult {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := gitResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		res.err = fmt.Errorf("git %s timed out after %s", args[0], gitTimeout)
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
// (the remote was unreachable, git is missing), which the caller reports as
// StatusIndeterminate — never as "the branch does not exist".
func (o Options) remoteBranchExists(remote, branch string) (bool, error) {
	// --exit-code makes "no such ref" a distinct status (2) instead of a successful empty
	// listing, so a typo'd branch cannot read the same as a healthy query.
	res := o.git(o.Repo, "ls-remote", "--exit-code", "--heads", remote, branch)
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
func (o Options) revExists(ref string) (bool, error) {
	res := o.git(o.Repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
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
// directory) and whether it exists at all. Callers must have already established that ref
// itself resolves, so a miss here is a genuinely absent path and not an unknown revision.
func (o Options) objectType(ref, path string) (string, bool, error) {
	res := o.git(o.Repo, "cat-file", "-t", ref+":"+path)
	switch {
	case res.err != nil:
		return "", false, res.err
	case res.exit == 0:
		return res.token(), true, nil
	default:
		return "", false, nil
	}
}

// lineCount returns the number of lines of path at ref, counted the way an editor and a
// reviewer count them: a trailing newline closes the last line rather than starting a new
// one, and a file whose last line is unterminated still counts.
func (o Options) lineCount(ref, path string) (int, error) {
	res := o.git(o.Repo, "show", ref+":"+path)
	if res.err != nil {
		return 0, res.err
	}
	if res.exit != 0 {
		return 0, fmt.Errorf("git show %s:%s failed (exit %d): %s", ref, path, res.exit, res.msg())
	}
	body := res.stdout
	if body == "" {
		return 0, nil
	}
	n := strings.Count(body, "\n")
	if !strings.HasSuffix(body, "\n") {
		n++
	}
	return n, nil
}

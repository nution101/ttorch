package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultNetworkTimeout bounds every git command on the fetch, land and fleet-sync paths that
// talks to a remote (Fetch, RemoteBranchSHA). A remote that accepts the connection and then
// stops answering used to hold a land, a trust prep's fetch, or fleet-sync indefinitely. Two
// minutes leaves room for an incremental fetch of a large repository over a slow link.
const DefaultNetworkTimeout = 2 * time.Minute

// NetworkTimeout is the bound gitNetwork applies, DefaultNetworkTimeout unless a test shortens
// it to exercise a stalled remote.
var NetworkTimeout = DefaultNetworkTimeout

// networkWaitDelay is how long, once the timeout has killed git, gitNetwork waits for anything
// git started (a remote helper, ssh) to close the output pipes it inherited, so a child that
// outlives git cannot hold the call open either.
const networkWaitDelay = 5 * time.Second

// gitNetwork runs a git command that talks to a remote, like gitRaw (stdout and stderr apart),
// and kills it after NetworkTimeout. A timeout is reported as the remote not answering.
func gitNetwork(args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), NetworkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = networkWaitDelay
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", stderr, fmt.Errorf("git %s: no answer from the remote within %s", escapeForTerminal(strings.Join(args, " ")), NetworkTimeout)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" {
			return "", stderr, fmt.Errorf("git %s: %w: %s", escapeForTerminal(strings.Join(args, " ")), err, escapeForTerminal(msg))
		}
		return "", stderr, fmt.Errorf("git %s: %w", escapeForTerminal(strings.Join(args, " ")), err)
	}
	return stdout, stderr, nil
}

// ResolveCommit resolves rev to the full id of the commit it names. The result is read from
// stdout alone and checked to be an object id, so a warning git prints (an ambiguous refname,
// say) can never be taken for the sha. Callers pass fully qualified refs or shas; a bare name
// resolves the way git resolves it, tags first.
func ResolveCommit(repo, rev string) (string, error) {
	out, _, err := gitRaw("-C", repo, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s does not name a commit in %s", escapeForTerminal(rev), repo)
	}
	sha := strings.TrimSpace(out)
	if !isObjectID(sha) {
		return "", fmt.Errorf("%s did not resolve to a commit id in %s", escapeForTerminal(rev), repo)
	}
	return sha, nil
}

// RemoteBranchSHA asks remote itself which commit its refs/heads/<branch> is at (git ls-remote),
// without reading any local ref. A remote-tracking ref such as refs/remotes/origin/<branch> is a
// local ref every linked worktree can write, so a caller that must know what the remote really
// holds compares against this. ok is false when the remote has no such branch. It gives up after
// NetworkTimeout.
func RemoteBranchSHA(repo, remote, branch string) (sha string, ok bool, err error) {
	ref := "refs/heads/" + branch
	out, _, err := gitNetwork("-C", repo, "ls-remote", "--refs", remote, ref)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		id, name, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || name != ref {
			continue
		}
		if !isObjectID(id) {
			return "", false, fmt.Errorf("git ls-remote %s %s returned %q, which is not a commit id", remote, ref, escapeForTerminal(id))
		}
		return id, true, nil
	}
	return "", false, nil
}

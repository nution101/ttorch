package worktree

import (
	"fmt"
	"strings"
)

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
// holds compares against this. ok is false when the remote has no such branch.
func RemoteBranchSHA(repo, remote, branch string) (sha string, ok bool, err error) {
	ref := "refs/heads/" + branch
	out, _, err := gitRaw("-C", repo, "ls-remote", "--refs", remote, ref)
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

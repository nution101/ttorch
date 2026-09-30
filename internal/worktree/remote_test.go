package worktree

import (
	"strings"
	"testing"
)

// TestResolveCommit: a fully qualified ref resolves to its branch even with a tag of the same
// short name in place, and a name that is no commit is an error rather than an empty sha.
func TestResolveCommit(t *testing.T) {
	repo, wt, mainTip, workerTip := gateBaseRepo(t)
	gitT(t, wt, "tag", "main", workerTip)
	if got, err := ResolveCommit(repo, "refs/heads/main"); err != nil || got != mainTip {
		t.Fatalf("ResolveCommit(refs/heads/main) = %q, %v; want %s", got, err, mainTip)
	}
	if got, err := ResolveCommit(repo, mainTip); err != nil || got != mainTip {
		t.Fatalf("ResolveCommit(sha) = %q, %v", got, err)
	}
	if got, err := ResolveCommit(repo, "refs/heads/nope"); err == nil {
		t.Fatalf("ResolveCommit of a missing ref = %q, want an error", got)
	}
	if got, err := ResolveCommit(repo, "--output=x"); err == nil {
		t.Fatalf("ResolveCommit of an option-shaped rev = %q, want an error", got)
	}
}

// TestRemoteBranchSHA: the answer comes from the remote, not from the local remote-tracking
// ref, so moving refs/remotes/origin/main locally does not change it.
func TestRemoteBranchSHA(t *testing.T) {
	repo, wt, mainTip, workerTip := gateBaseRepo(t)
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q", "-b", "main")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", "main")
	gitT(t, repo, "fetch", "-q", "origin")
	gitT(t, wt, "update-ref", "refs/remotes/origin/main", workerTip)
	sha, ok, err := RemoteBranchSHA(repo, "origin", "main")
	if err != nil || !ok || sha != mainTip {
		t.Fatalf("RemoteBranchSHA = %q, %v, %v; want %s from the remote, not the moved tracking ref", sha, ok, err, mainTip)
	}
	if sha, ok, err := RemoteBranchSHA(repo, "origin", "develop"); err != nil || ok {
		t.Fatalf("a branch the remote lacks: %q, %v, %v; want not found", sha, ok, err)
	}
	if _, _, err := RemoteBranchSHA(repo, "nosuchremote", "main"); err == nil || !strings.Contains(err.Error(), "nosuchremote") {
		t.Fatalf("an unknown remote must be an error, got %v", err)
	}
}

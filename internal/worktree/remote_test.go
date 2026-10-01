package worktree

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"
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

// stallingRemote listens on a loopback port, accepts every connection and never answers: a
// remote that took the connection and then stopped responding. It returns a git:// URL for it
// and a count of the connections it has accepted. Cleanup closes the listener and every
// connection, which also releases a git still waiting on it.
func stallingRemote(t *testing.T) (url string, accepted func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "git://" + ln.Addr().String() + "/repo.git", func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(conns)
	}
}

// returnsWithin runs f and fails the test if it has not returned after limit.
func returnsWithin(t *testing.T, limit time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("still waiting on the stalled remote after %s", limit)
	}
}

// TestNetworkCallsGiveUpOnAStalledRemote: a fetch or ls-remote against a remote that accepts
// the connection and never answers used to wait forever, holding a land, a trust prep or a
// fleet-sync with it. Both now give up after NetworkTimeout and say why.
func TestNetworkCallsGiveUpOnAStalledRemote(t *testing.T) {
	url, _ := stallingRemote(t)
	repo, _, _, _ := gateBaseRepo(t)
	gitT(t, repo, "remote", "add", "origin", url)
	prev := NetworkTimeout
	NetworkTimeout = time.Second
	t.Cleanup(func() { NetworkTimeout = prev })

	t.Run("Fetch", func(t *testing.T) {
		var err error
		returnsWithin(t, 20*time.Second, func() { err = Fetch(repo) })
		if err == nil || !strings.Contains(err.Error(), "no answer") {
			t.Fatalf("Fetch from a stalled remote = %v, want a timeout error", err)
		}
	})
	t.Run("RemoteBranchSHA", func(t *testing.T) {
		var err error
		returnsWithin(t, 20*time.Second, func() { _, _, err = RemoteBranchSHA(repo, "origin", "main") })
		if err == nil || !strings.Contains(err.Error(), "no answer") {
			t.Fatalf("RemoteBranchSHA from a stalled remote = %v, want a timeout error", err)
		}
	})
}

// TestOriginURL: the URL is the one git fetches from, so an insteadOf rule that redirects
// remote.origin.url is applied, and a repository with no origin gives "".
func TestOriginURL(t *testing.T) {
	repo, _, _, _ := gateBaseRepo(t)
	if got := OriginURL(repo); got != "" {
		t.Fatalf("OriginURL with no origin = %q, want empty", got)
	}
	gitT(t, repo, "remote", "add", "origin", "https://example.com/team/repo.git")
	if got := OriginURL(repo); got != "https://example.com/team/repo.git" {
		t.Fatalf("OriginURL = %q", got)
	}
	gitT(t, repo, "config", "url.https://example.com/elsewhere/.insteadOf", "https://example.com/team/")
	if got := OriginURL(repo); got != "https://example.com/elsewhere/repo.git" {
		t.Fatalf("OriginURL under an insteadOf rule = %q, want the rewritten URL", got)
	}
}

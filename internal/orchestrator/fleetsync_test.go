package orchestrator

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/gittest"
	"github.com/nution101/ttorch/internal/worktree"
)

// fleetSyncRepo builds a repository on main with an origin in sync, registered with main as its
// recorded default branch, and a linked worktree "wt" one commit ahead the way a worker's is.
func fleetSyncRepo(t *testing.T) (m *Manager, repo, wt, workerTip string) {
	t.Helper()
	s := openTestStore(t)
	m = &Manager{Store: s}
	repo = newRepoMain(t)
	bare := t.TempDir()
	gitIn(t, bare, "init", "--bare", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", bare)
	gitIn(t, repo, "push", "-q", "origin", "main")
	gitIn(t, repo, "fetch", "-q", "origin")
	root := gitIn(t, repo, "rev-parse", "--show-toplevel")
	p, err := s.UpsertProject(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefaultBranch(context.Background(), p.ID, "main", ""); err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "worker", wt)
	workerTip = commitFeature(t, wt, "feature.txt", "worker change\n")
	return m, repo, wt, workerTip
}

// TestFleetSync_TagCannotMoveTheDefaultBranch: fleet-sync fast-forwarded main with a bare
// `origin/main`, which git resolves to refs/tags/origin/main ahead of the remote-tracking ref,
// and `fetch --prune` never removes a tag. A worker's tag of that name moved the lead's main to
// the worker's commit. main must stay where origin has it.
//
// The repository has no refs/remotes/origin/HEAD, as with a git older than 2.48 or
// remote.origin.followRemoteHEAD=never. Where fetch creates one, the same tag made the old
// code's `symbolic-ref --short` answer "remotes/origin/main", so it skipped the fast-forward by
// accident rather than by design.
func TestFleetSync_TagCannotMoveTheDefaultBranch(t *testing.T) {
	m, repo, wt, workerTip := fleetSyncRepo(t)
	gitIn(t, repo, "config", "remote.origin.followRemoteHEAD", "never")
	_, _ = gittest.Command(repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD").CombinedOutput()
	gitIn(t, wt, "tag", "origin/main", workerTip)
	before := gitIn(t, repo, "rev-parse", "refs/heads/main")
	if _, err := m.FleetSync(repo); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("a tag named origin/main moved main to %s (worker tip %s)", got, workerTip)
	}
}

// TestFleetSync_UnfetchedTrackingRefCannotMoveTheDefaultBranch: the remote-tracking ref is a
// local ref too. With the fetch kept from resetting it, a worker-moved refs/remotes/origin/main
// must not be fast-forwarded to; fleet-sync takes what origin itself reports.
func TestFleetSync_UnfetchedTrackingRefCannotMoveTheDefaultBranch(t *testing.T) {
	m, repo, wt, workerTip := fleetSyncRepo(t)
	gitIn(t, wt, "config", "remote.origin.fetch", "+refs/heads/elsewhere/*:refs/remotes/origin/elsewhere/*")
	gitIn(t, wt, "update-ref", "refs/remotes/origin/main", workerTip)
	before := gitIn(t, repo, "rev-parse", "refs/heads/main")
	notes, err := m.FleetSync(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("a remote-tracking ref origin does not hold moved main to %s", got)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "origin reports") {
		t.Fatalf("fleet-sync must say why it skipped the fast-forward, got %q", notes)
	}
}

// TestFleetSync_FastForwardsToWhatOriginHolds: the ordinary case still works. origin advances,
// and fleet-sync brings main to origin's commit.
func TestFleetSync_FastForwardsToWhatOriginHolds(t *testing.T) {
	m, repo, _, _ := fleetSyncRepo(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	gitIn(t, repo, "worktree", "add", "-q", "--detach", scratch, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(scratch, "other.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, scratch, "add", "other.txt")
	gitIn(t, scratch, "commit", "-q", "-m", "on origin")
	want := gitIn(t, scratch, "rev-parse", "HEAD")
	gitIn(t, scratch, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitIn(t, repo, "worktree", "remove", "--force", scratch)
	notes, err := m.FleetSync(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/heads/main"); got != want {
		t.Fatalf("main = %s, want origin's %s (notes %q)", got, want, notes)
	}
}

// TestFleetSync_NoRecordedBranchSkipsTheFastForward: without a recorded default branch fleet-sync
// has no branch it may move, so it moves none and says so.
func TestFleetSync_NoRecordedBranchSkipsTheFastForward(t *testing.T) {
	m, repo, _, _ := fleetSyncRepo(t)
	root := gitIn(t, repo, "rev-parse", "--show-toplevel")
	p := projectByRepo(t, m.Store, root)
	if err := m.Store.SetProjectDefaultBranch(context.Background(), p.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	notes, err := m.FleetSync(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), SetBranchCommand) {
		t.Fatalf("fleet-sync must name %q when no branch is recorded, got %q", SetBranchCommand, notes)
	}
}

// stallingRemote listens on a loopback port, accepts every connection and never answers, and
// counts the connections. Cleanup closes everything, which releases a git still waiting on it.
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

// TestFleetSync_StalledFetchSkipsTheSync: with origin accepting the connection and never
// answering, fleet-sync used to wait on the fetch forever. It now gives up after
// worktree.NetworkTimeout, and, the fetch having failed, does not go back to origin for an
// ls-remote: one connection, the fetch's.
func TestFleetSync_StalledFetchSkipsTheSync(t *testing.T) {
	m, repo, _, _ := fleetSyncRepo(t)
	url, accepted := stallingRemote(t)
	gitIn(t, repo, "remote", "set-url", "origin", url)
	prev := worktree.NetworkTimeout
	worktree.NetworkTimeout = time.Second
	t.Cleanup(func() { worktree.NetworkTimeout = prev })
	before := gitIn(t, repo, "rev-parse", "refs/heads/main")

	done := make(chan struct{})
	var notes []string
	var err error
	go func() {
		defer close(done)
		notes, err = m.FleetSync(repo)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("fleet-sync is still waiting on the stalled origin after 20s")
	}
	if err != nil {
		t.Fatal(err)
	}
	if n := accepted(); n != 1 {
		t.Fatalf("after its fetch failed, fleet-sync must not ask origin again; origin saw %d connections (notes %q)", n, notes)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "fetch failed") {
		t.Fatalf("fleet-sync must say the fetch failed, got %q", notes)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("main moved to %s", got)
	}
}

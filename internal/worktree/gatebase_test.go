package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateBaseRepo builds a repository on main with one commit, and a second branch "worker" one
// commit ahead, checked out in a LINKED worktree the way a ttorch worker's is. The main
// checkout stays on main. It returns the repo, the linked worktree, and both tips.
func gateBaseRepo(t *testing.T) (repo, wt, mainTip, workerTip string) {
	t.Helper()
	repo = t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "main")
	mainTip = gitT(t, repo, "rev-parse", "refs/heads/main")
	wt = filepath.Join(t.TempDir(), "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "worker", wt)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wt, "commit", "-q", "-am", "worker")
	workerTip = gitT(t, wt, "rev-parse", "HEAD")
	return repo, wt, mainTip, workerTip
}

func mustGateBase(t *testing.T, repo, branch string) GateBase {
	t.Helper()
	b, err := ResolveGateBase(repo, branch)
	if err != nil {
		t.Fatalf("ResolveGateBase: %v", err)
	}
	return b
}

// TestResolveGateBase_TagCannotShadowTheBranch: refs are shared with every linked worktree, so
// a worker can `git tag main <its commit>` from its own checkout, and every bare-name lookup of
// "main" then resolves to the tag. The gate base must still be refs/heads/main.
func TestResolveGateBase_TagCannotShadowTheBranch(t *testing.T) {
	repo, wt, mainTip, workerTip := gateBaseRepo(t)
	gitT(t, wt, "tag", "main", workerTip)
	// Setup: the shadow is real. A bare "main" now names the worker's commit, and git's only
	// sign of it is a warning on stderr (gitT folds stderr into its output).
	if got := gitT(t, repo, "rev-parse", "main"); !strings.HasSuffix(got, workerTip) || !strings.Contains(got, "ambiguous") {
		t.Fatalf("setup: expected the tag to shadow main for a bare lookup, rev-parse main = %q", got)
	}
	if s, ok := ShowFile(repo, "main", "f.txt"); !ok || s != "worker\n" {
		t.Fatalf("setup: expected a bare-name read to see the worker's bytes, got %q", s)
	}
	b := mustGateBase(t, repo, "main")
	if b.Name != "main" || b.SHA != mainTip {
		t.Fatalf("ResolveGateBase = %+v, want main at %s (the branch, not the tag at %s)", b, mainTip, workerTip)
	}
	if s, ok := ShowFile(repo, b.SHA, "f.txt"); !ok || s != "main\n" {
		t.Fatalf("a read through the gate base must see the branch's bytes, got %q", s)
	}
}

// TestResolveGateBase_WorkerRefsDoNotChooseTheBranch: the branch is the recorded one, so
// nothing a worker can write from its worktree picks another. Here the recorded default is
// develop, and a worker repoints refs/remotes/origin/HEAD at its own branch and creates a local
// main and master at its tip, which is every ref the old derivation read.
func TestResolveGateBase_WorkerRefsDoNotChooseTheBranch(t *testing.T) {
	repo, wt, mainTip, workerTip := gateBaseRepo(t)
	gitT(t, repo, "branch", "-m", "main", "develop")
	gitT(t, wt, "branch", "main", workerTip)
	gitT(t, wt, "branch", "master", workerTip)
	gitT(t, wt, "update-ref", "refs/remotes/origin/worker", workerTip)
	gitT(t, wt, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/worker")
	if got := DefaultBranch(repo); got != "worker" {
		t.Fatalf("setup: expected DefaultBranch to follow the repointed origin/HEAD, got %q", got)
	}
	b := mustGateBase(t, repo, "develop")
	if b.Name != "develop" || b.SHA != mainTip {
		t.Fatalf("ResolveGateBase = %+v, want develop at %s, not the worker's %s", b, mainTip, workerTip)
	}
}

// TestResolveGateBase_FailsClosed: without a recorded branch, or with one that has no local
// ref, the gate has nothing to read and must refuse rather than guess.
func TestResolveGateBase_FailsClosed(t *testing.T) {
	repo, _, _, workerTip := gateBaseRepo(t)
	if b, err := ResolveGateBase(repo, ""); err == nil || !strings.Contains(err.Error(), "no default branch is recorded") {
		t.Fatalf("no recorded branch: got %+v, %v; want a refusal saying none is recorded", b, err)
	}
	if b, err := ResolveGateBase(repo, "develop"); err == nil || !strings.Contains(err.Error(), "refs/heads/develop") {
		t.Fatalf("a recorded branch with no local ref: got %+v, %v; want a refusal naming refs/heads/develop", b, err)
	}
	// A tag of the recorded name is not the branch.
	gitT(t, repo, "tag", "develop", workerTip)
	if b, err := ResolveGateBase(repo, "develop"); err == nil {
		t.Fatalf("a tag named after the recorded branch must not resolve as it, got %+v", b)
	}
}

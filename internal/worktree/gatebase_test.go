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

func mustGateBase(t *testing.T, repo string) GateBase {
	t.Helper()
	b, err := ResolveGateBase(repo)
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
	b := mustGateBase(t, repo)
	if b.Name != "main" || b.SHA != mainTip {
		t.Fatalf("ResolveGateBase = %+v, want main at %s (the branch, not the tag at %s)", b, mainTip, workerTip)
	}
	if s, ok := ShowFile(repo, b.SHA, "f.txt"); !ok || s != "main\n" {
		t.Fatalf("a read through the gate base must see the branch's bytes, got %q", s)
	}
}

// TestResolveGateBase_RepointedOriginHeadDoesNotMoveIt: refs/remotes/origin/HEAD is shared and
// can be repointed from any worktree. Pointing it at the worker's branch must not make the gate
// read that branch while the repository's own checkout is on main.
func TestResolveGateBase_RepointedOriginHeadDoesNotMoveIt(t *testing.T) {
	repo, wt, mainTip, workerTip := gateBaseRepo(t)
	gitT(t, wt, "update-ref", "refs/remotes/origin/worker", workerTip)
	gitT(t, wt, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/worker")
	if got := DefaultBranch(repo); got != "worker" {
		t.Fatalf("setup: expected DefaultBranch to follow the repointed origin/HEAD, got %q", got)
	}
	b := mustGateBase(t, repo)
	if b.Name != "main" || b.SHA != mainTip {
		t.Fatalf("ResolveGateBase = %+v, want main at %s; a repointed origin/HEAD must not move the gate onto %s", b, mainTip, workerTip)
	}
}

// TestResolveGateBase_Names covers how the name is chosen when nothing is being attacked.
func TestResolveGateBase_Names(t *testing.T) {
	t.Run("develop default with the checkout on it", func(t *testing.T) {
		repo := t.TempDir()
		gitT(t, repo, "init", "-q", "-b", "develop")
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", "-A")
		gitT(t, repo, "commit", "-q", "-m", "x")
		tip := gitT(t, repo, "rev-parse", "HEAD")
		gitT(t, repo, "update-ref", "refs/remotes/origin/develop", tip)
		gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
		if b := mustGateBase(t, repo); b.Name != "develop" || b.SHA != tip {
			t.Fatalf("ResolveGateBase = %+v, want develop at %s", b, tip)
		}
		// The same repo with the checkout moved to a feature branch: origin/HEAD alone does not
		// decide, and there is no main or master to fall back to, so it fails closed.
		gitT(t, repo, "checkout", "-q", "-b", "feature")
		if b, err := ResolveGateBase(repo); err == nil {
			t.Fatalf("with the checkout off the default and only origin/HEAD naming develop, want an error, got %+v", b)
		}
	})
	t.Run("checkout on a feature branch falls back to main", func(t *testing.T) {
		repo, _, mainTip, _ := gateBaseRepo(t)
		gitT(t, repo, "checkout", "-q", "-b", "feature")
		if b := mustGateBase(t, repo); b.Name != "main" || b.SHA != mainTip {
			t.Fatalf("ResolveGateBase = %+v, want main at %s", b, mainTip)
		}
	})
	t.Run("master", func(t *testing.T) {
		repo := t.TempDir()
		gitT(t, repo, "init", "-q", "-b", "master")
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", "-A")
		gitT(t, repo, "commit", "-q", "-m", "x")
		if b := mustGateBase(t, repo); b.Name != "master" {
			t.Fatalf("ResolveGateBase = %+v, want master", b)
		}
	})
	t.Run("no default branch at all", func(t *testing.T) {
		repo := t.TempDir()
		gitT(t, repo, "init", "-q", "-b", "trunk")
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", "-A")
		gitT(t, repo, "commit", "-q", "-m", "x")
		gitT(t, repo, "checkout", "-q", "--detach")
		b, err := ResolveGateBase(repo)
		if err == nil {
			t.Fatalf("a detached checkout with no main, master or origin/HEAD must fail closed, got %+v", b)
		}
		if !strings.Contains(err.Error(), "default branch") {
			t.Fatalf("the error must say what could not be resolved: %v", err)
		}
	})
}

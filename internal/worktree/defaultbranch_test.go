package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// branchRepo builds a repository whose checkout is on branch, with one commit.
func branchRepo(t *testing.T, branch string) (repo, tip string) {
	t.Helper()
	repo = t.TempDir()
	gitT(t, repo, "init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "x")
	return repo, gitT(t, repo, "rev-parse", "HEAD")
}

// TestDetectDefaultBranch covers the order the registration-time detection takes: the branch
// origin/HEAD names when that branch exists locally, else the branch the checkout is on.
func TestDetectDefaultBranch(t *testing.T) {
	t.Run("origin/HEAD names an existing local branch", func(t *testing.T) {
		repo, tip := branchRepo(t, "develop")
		gitT(t, repo, "branch", "release", tip)
		gitT(t, repo, "update-ref", "refs/remotes/origin/release", tip)
		gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/release")
		if b, err := DetectDefaultBranch(repo); err != nil || b != "release" {
			t.Fatalf("DetectDefaultBranch = %q, %v; want release", b, err)
		}
	})
	t.Run("origin/HEAD names a branch with no local ref", func(t *testing.T) {
		repo, tip := branchRepo(t, "develop")
		gitT(t, repo, "update-ref", "refs/remotes/origin/gone", tip)
		gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/gone")
		if b, err := DetectDefaultBranch(repo); err != nil || b != "develop" {
			t.Fatalf("DetectDefaultBranch = %q, %v; want the checkout's develop", b, err)
		}
	})
	t.Run("no origin/HEAD takes the checkout's branch", func(t *testing.T) {
		repo, _ := branchRepo(t, "trunk")
		if b, err := DetectDefaultBranch(repo); err != nil || b != "trunk" {
			t.Fatalf("DetectDefaultBranch = %q, %v; want trunk", b, err)
		}
	})
	t.Run("a detached checkout with no origin/HEAD fails", func(t *testing.T) {
		repo, _ := branchRepo(t, "trunk")
		gitT(t, repo, "checkout", "-q", "--detach")
		if b, err := DetectDefaultBranch(repo); err == nil {
			t.Fatalf("DetectDefaultBranch = %q, want an error", b)
		}
	})
	t.Run("a directory outside git fails", func(t *testing.T) {
		if b, err := DetectDefaultBranch(t.TempDir()); err == nil {
			t.Fatalf("DetectDefaultBranch = %q, want an error", b)
		}
	})
}

func TestBranchNameAndExistence(t *testing.T) {
	repo, tip := branchRepo(t, "main")
	gitT(t, repo, "tag", "only-a-tag", tip)
	for _, c := range []struct {
		name        string
		valid, have bool
	}{
		{"main", true, true},
		{"feature/x", true, false},
		{"only-a-tag", true, false}, // a tag is not a branch
		{"", false, false},
		{"-x", false, false},
		{"a..b", false, false},
		{"@{-1}", false, false},
		{"has space", false, false},
	} {
		if got := IsBranchName(repo, c.name); got != c.valid {
			t.Errorf("IsBranchName(%q) = %v, want %v", c.name, got, c.valid)
		}
		if got := BranchExists(repo, c.name); got != c.have {
			t.Errorf("BranchExists(%q) = %v, want %v", c.name, got, c.have)
		}
	}
}

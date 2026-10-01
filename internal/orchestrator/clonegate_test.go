package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestCloneTask_WorkdirKind covers how the seam reads a workdir's kind: a gitfile is a
// worktree, a git directory is a clone, a workdir that is the project itself is a worktree
// whatever its .git looks like, and anything else, a symlink at the workdir included, is
// refused rather than handed to git.
func TestCloneTask_WorkdirKind(t *testing.T) {
	repo := newRepoMain(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "task/k", wt)
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, filepath.Dir(clone), "init", "-q", clone)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(t.TempDir(), "pointer")
	if err := os.Symlink(clone, pointer); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, workdir string
		clone, err    bool
	}{
		{name: "linked worktree", workdir: wt},
		{name: "clone", workdir: clone, clone: true},
		{name: "the project itself", workdir: repo},
		{name: "symlinked .git", workdir: linked, err: true},
		{name: "symlinked workdir", workdir: pointer, err: true},
		{name: "no .git", workdir: t.TempDir(), err: true},
		{name: "no workdir", workdir: "", err: true},
	} {
		w, err := openWork(db.Task{ID: "k", Project: repo, Worktree: c.workdir})
		if (err != nil) != c.err {
			t.Errorf("%s: openWork error = %v, want error %v", c.name, err, c.err)
			continue
		}
		if err == nil && w.clone != c.clone {
			t.Errorf("%s: read as clone=%v, want %v", c.name, w.clone, c.clone)
		}
	}
}

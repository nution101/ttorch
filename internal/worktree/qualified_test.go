package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultBase_TagCannotChooseTheTaskBase: defaultBase is the commit a new task branch is cut
// from. It named origin/<default> bare, and git resolves a tag of that name ahead of the
// remote-tracking ref, so a worker's tag chose the base of every later task.
//
// The repository has no refs/remotes/origin/HEAD (git older than 2.48, or
// remote.origin.followRemoteHEAD=never). Where fetch creates one, the tag made DefaultBranch's
// `symbolic-ref --short` answer "remotes/origin/<default>", which happened to resolve correctly.
func TestDefaultBase_TagCannotChooseTheTaskBase(t *testing.T) {
	repo, _, def := makeRepoWithOrigin(t)
	gitT(t, repo, "config", "remote.origin.followRemoteHEAD", "never")
	gitT(t, repo, "fetch", "-q", "origin")
	_, _ = git("-C", repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	want := gitT(t, repo, "rev-parse", "refs/remotes/origin/"+def)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "worker", wt)
	if err := os.WriteFile(filepath.Join(wt, "w.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "commit", "-q", "-m", "worker")
	gitT(t, wt, "tag", "origin/"+def, "HEAD")
	got, err := ResolveCommit(repo, defaultBase(repo))
	if err != nil || got != want {
		t.Fatalf("defaultBase resolves to %s (%v), want origin's %s, not the tag", got, err, want)
	}
}

// TestHasUnlandedWork_TagCannotHideASlotsCommit: the pool reuses a slot only when its HEAD holds
// nothing unlanded. Compared against a bare origin/<default>, a tag of that name at the slot's own
// commit made the commit read as landed, and the slot was reset over it.
func TestHasUnlandedWork_TagCannotHideASlotsCommit(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	slot := filepath.Join(t.TempDir(), "slot")
	gitT(t, repo, "worktree", "add", "-q", "--detach", slot)
	if err := os.WriteFile(filepath.Join(slot, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, slot, "add", "-A")
	gitT(t, slot, "commit", "-q", "-m", "unlanded")
	gitT(t, slot, "tag", "origin/"+def, "HEAD")
	if !hasUnlandedWork(repo, slot) {
		t.Fatalf("a tag named origin/%s at the slot's commit must not make it read as landed", def)
	}
}

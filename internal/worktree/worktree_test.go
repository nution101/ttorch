package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return repo
}

func TestPool_ReuseAndDistinctWhenBusy(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}

	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a is in use -> a distinct new slot.
	b, err := p.Acquire(repo, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("expected a distinct slot when the first is in use")
	}
	// a is now idle and clean -> reused.
	c, err := p.Acquire(repo, []string{b})
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected reuse of %q, got %q", a, c)
	}
}

func TestPool_RespectsMax(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 1}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(repo, []string{a}); err == nil {
		t.Fatal("expected pool-full error at Max=1")
	}
}

func TestPool_SkipsSlotWithTrackedChanges(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Orphaned tracked change in slot a (not in use).
	if err := os.WriteFile(filepath.Join(a, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b == a {
		t.Fatal("must not reuse a slot with uncommitted tracked changes")
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRefRemoteResolveHelpers(t *testing.T) {
	repo := makeRepo(t)
	if RemoteExists(repo, "origin") {
		t.Fatal("a fresh repo has no origin remote")
	}
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	if !RemoteExists(repo, "origin") {
		t.Fatal("origin should exist after adding it")
	}

	head, err := ResolveRef(repo, "HEAD")
	if err != nil || head != gitT(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("ResolveRef HEAD = %q err %v", head, err)
	}
	if !RefExists(repo, "HEAD") {
		t.Fatal("HEAD should resolve")
	}
	if RefExists(repo, "refs/heads/does-not-exist") {
		t.Fatal("a bogus ref must not resolve")
	}

	// Push publishes the current commit as a branch on the bare remote.
	if err := Push(repo, "origin", "HEAD:refs/heads/pushed"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if gitT(t, bare, "rev-parse", "refs/heads/pushed") != head {
		t.Fatal("the pushed branch tip does not match the local HEAD")
	}
}

func TestRebaseCleanAndConflictAbort(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	start := gitT(t, repo, "rev-parse", "HEAD")
	// The default branch advances with a conflicting edit to f.txt.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "default advance")
	defTip := gitT(t, repo, "rev-parse", "HEAD")

	// A clean rebase: a feature on a DIFFERENT file replays on top of the new default.
	wtClean := filepath.Join(t.TempDir(), "clean")
	if err := AddDetached(repo, wtClean, start); err != nil {
		t.Fatal(err)
	}
	defer RemoveWorktree(repo, wtClean)
	if err := os.WriteFile(filepath.Join(wtClean, "feature.txt"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtClean, "add", "-A")
	gitT(t, wtClean, "commit", "-q", "-m", "feature")
	if err := Rebase(wtClean, def); err != nil {
		t.Fatalf("a non-overlapping rebase should succeed: %v", err)
	}
	if !IsAncestor(repo, defTip, gitT(t, wtClean, "rev-parse", "HEAD")) {
		t.Fatal("after a clean rebase the feature must sit on top of the new default")
	}

	// A conflicting rebase: a feature touching the SAME file aborts and restores HEAD.
	wtConflict := filepath.Join(t.TempDir(), "conflict")
	if err := AddDetached(repo, wtConflict, start); err != nil {
		t.Fatal(err)
	}
	defer RemoveWorktree(repo, wtConflict)
	if err := os.WriteFile(filepath.Join(wtConflict, "f.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wtConflict, "add", "-A")
	gitT(t, wtConflict, "commit", "-q", "-m", "feature edits f.txt")
	conflictHead := gitT(t, wtConflict, "rev-parse", "HEAD")
	if err := Rebase(wtConflict, def); err == nil {
		t.Fatal("a conflicting rebase must return an error")
	}
	if err := RebaseAbort(wtConflict); err != nil {
		t.Fatalf("RebaseAbort: %v", err)
	}
	if gitT(t, wtConflict, "rev-parse", "HEAD") != conflictHead {
		t.Fatal("RebaseAbort must restore the pre-rebase HEAD")
	}
	if d, _ := IsDirty(wtConflict); d {
		t.Fatal("the worktree must be clean after the rebase abort")
	}
}

// TestStartBranch_FreshOffDefaultDiscardsPrior proves a reused slot is rebased onto a
// fresh task branch cut from the up-to-date (fetched) default tip, with the prior
// task's branch and commit gone but untracked build caches preserved.
func TestStartBranch_FreshOffDefaultDiscardsPrior(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)

	// Give the repo an origin so StartBranch exercises the fetch + origin/<def> path.
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", def)

	p := Pool{Root: t.TempDir(), Max: 4}
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A prior task left the slot on a stale branch carrying its own commit + file.
	gitT(t, slot, "checkout", "-q", "-B", "stale/prev")
	if err := os.WriteFile(filepath.Join(slot, "stale.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, slot, "add", "-A")
	gitT(t, slot, "commit", "-q", "-m", "prior task work")
	// An untracked build cache should survive the fresh start.
	cache := filepath.Join(slot, "cache_marker")
	if err := os.WriteFile(cache, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The default branch advances on the remote — what a new worker should start from.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "default advanced")
	gitT(t, repo, "push", "-q", "origin", def)
	wantTip := gitT(t, bare, "rev-parse", "refs/heads/"+def)

	if err := StartBranch(repo, slot, "ttorch/new"); err != nil {
		t.Fatalf("StartBranch: %v", err)
	}

	if br := gitT(t, slot, "rev-parse", "--abbrev-ref", "HEAD"); br != "ttorch/new" {
		t.Fatalf("slot should be on the fresh branch, got %q", br)
	}
	if got := gitT(t, slot, "rev-parse", "HEAD"); got != wantTip {
		t.Fatalf("fresh branch should start at the fetched default tip %s, got %s", wantTip, got)
	}
	if _, err := os.Stat(filepath.Join(slot, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("the prior task's tracked file must be gone after a fresh start")
	}
	if tracked, _ := HasTrackedChanges(slot); tracked {
		t.Fatal("the slot's tracked tree must be clean after a fresh start")
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("an untracked build cache must survive a fresh start")
	}
}

// TestStartBranch_NoRemoteUsesLocalDefault confirms a remote-less repo cuts the fresh
// branch from its local default branch (no fetch required).
func TestStartBranch_NoRemoteUsesLocalDefault(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	p := Pool{Root: t.TempDir(), Max: 4}
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := StartBranch(repo, slot, "ttorch/x"); err != nil {
		t.Fatalf("StartBranch: %v", err)
	}
	if br := gitT(t, slot, "rev-parse", "--abbrev-ref", "HEAD"); br != "ttorch/x" {
		t.Fatalf("slot should be on the fresh branch, got %q", br)
	}
	if got, want := gitT(t, slot, "rev-parse", "HEAD"), gitT(t, repo, "rev-parse", def); got != want {
		t.Fatalf("fresh branch should start at the local default tip %s, got %s", want, got)
	}
}

func TestPool_KeepsUntrackedCachesOnReuse(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a build cache (untracked).
	cache := filepath.Join(a, "node_modules_marker")
	if err := os.WriteFile(cache, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reuse a (idle, no tracked changes) -> cache preserved.
	c, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected reuse of %q, got %q", a, c)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("untracked cache should survive reuse")
	}
}

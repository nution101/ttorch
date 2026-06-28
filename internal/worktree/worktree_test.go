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

// TestRelease_DetachesAndDropsTaskBranch proves a released slot is parked detached
// and its per-task branch is deleted, so a later spawn can (re)create that branch in a
// DIFFERENT slot without colliding with a stale checkout, and a fresh branch cut off
// origin/<default> does not carry origin as its upstream.
func TestRelease_DetachesAndDropsTaskBranch(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", def)

	p := Pool{Root: t.TempDir(), Max: 4}
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := StartBranch(repo, slot, "ttorch/t1"); err != nil {
		t.Fatalf("StartBranch: %v", err)
	}
	// A fresh branch cut off origin/<default> must not adopt origin/<default> upstream:
	// resolving its upstream must fail (no upstream configured).
	if _, err := git("-C", slot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "ttorch/t1@{u}"); err == nil {
		t.Fatal("task branch should have no upstream after --no-track")
	}

	if err := p.Release(repo, slot); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if br := gitT(t, slot, "rev-parse", "--abbrev-ref", "HEAD"); br != "HEAD" {
		t.Fatalf("released slot should be detached, got branch %q", br)
	}
	if RefExists(repo, "refs/heads/ttorch/t1") {
		t.Fatal("the per-task branch must be deleted when the slot is released")
	}
	// With the prior slot no longer pinning ttorch/t1, a DIFFERENT slot can take it.
	other, err := p.Acquire(repo, []string{slot})
	if err != nil {
		t.Fatal(err)
	}
	if other == slot {
		t.Fatal("expected a distinct slot for this part of the test")
	}
	if err := StartBranch(repo, other, "ttorch/t1"); err != nil {
		t.Fatalf("re-creating ttorch/t1 in another slot must succeed after release: %v", err)
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

// TestAcquire_ReusedSlotBasesOnFreshOriginWhenLocalBehind is the primary stale-base fix:
// a reused idle pool slot is reset to the freshly fetched origin/<default> tip even when
// this repo's LOCAL default branch is behind origin (it never pulled the advance). A
// recycled worktree must never start from the stale local HEAD.
func TestAcquire_ReusedSlotBasesOnFreshOriginWhenLocalBehind(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)

	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", def)

	p := Pool{Root: t.TempDir(), Max: 4}
	// Acquire then release a slot so it becomes a clean, reusable idle slot.
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Release(repo, slot); err != nil {
		t.Fatal(err)
	}

	// Advance origin/<default> from a detached linked worktree so the bare remote moves
	// ahead WITHOUT advancing this repo's local <default> branch — local is now behind.
	adv := filepath.Join(t.TempDir(), "adv")
	if err := AddDetached(repo, adv, def); err != nil {
		t.Fatal(err)
	}
	defer RemoveWorktree(repo, adv)
	if err := os.WriteFile(filepath.Join(adv, "g.txt"), []byte("origin advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, adv, "add", "-A")
	gitT(t, adv, "commit", "-q", "-m", "origin advance")
	gitT(t, adv, "push", "-q", "origin", "HEAD:"+def)
	originTip := gitT(t, bare, "rev-parse", "refs/heads/"+def)

	localTip := gitT(t, repo, "rev-parse", "refs/heads/"+def)
	if originTip == localTip {
		t.Fatal("test setup: origin must be ahead of the local default")
	}

	// Reuse the idle slot: it must be reset to the fresh origin tip, not the stale local.
	reused, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reused != slot {
		t.Fatalf("expected reuse of the idle slot %q, got %q", slot, reused)
	}
	if got := gitT(t, reused, "rev-parse", "HEAD"); got != originTip {
		t.Fatalf("reused slot should start at the fresh origin tip %s, got %s (stale local tip %s)", originTip, got, localTip)
	}
}

// TestAcquire_PreservesInUseSlotWithCommittedWork is the pool-level guarantee behind
// "an existing task worktree is untouched on resume": a slot held by a live (resumed)
// worker is listed in inUse, so Acquire hands out a DIFFERENT slot and never resets it —
// the worker's task branch and its committed-but-clean work survive. Committed work
// leaves no tracked changes, so the inUse guard (not the cleanliness check) is what
// protects it; resume reuses the same worktree and never re-runs Acquire/StartBranch.
func TestAcquire_PreservesInUseSlotWithCommittedWork(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", def)

	p := Pool{Root: t.TempDir(), Max: 4}
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A live worker: a fresh task branch carrying a clean, committed change.
	if err := StartBranch(repo, slot, "ttorch/live"); err != nil {
		t.Fatalf("StartBranch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slot, "work.txt"), []byte("worker progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, slot, "add", "-A")
	gitT(t, slot, "commit", "-q", "-m", "worker progress")
	workTip := gitT(t, slot, "rev-parse", "HEAD")
	if tracked, _ := HasTrackedChanges(slot); tracked {
		t.Fatal("committed work must leave the slot with no tracked changes")
	}

	// The worker still holds the slot (a resumed task -> inUse). Acquire must not recycle it.
	other, err := p.Acquire(repo, []string{slot})
	if err != nil {
		t.Fatal(err)
	}
	if other == slot {
		t.Fatal("Acquire must not recycle a slot held by a live worker")
	}
	if got := gitT(t, slot, "rev-parse", "HEAD"); got != workTip {
		t.Fatalf("an in-use worker's committed work must be preserved: HEAD %s != %s", got, workTip)
	}
	if br := gitT(t, slot, "rev-parse", "--abbrev-ref", "HEAD"); br != "ttorch/live" {
		t.Fatalf("an in-use worker's branch must be preserved, got %q", br)
	}
}

// TestFetchAndBase_WarnsOnFetchFailureButFallsBack proves the offline-safety requirement:
// a failed origin fetch is surfaced as a warning rather than silently leaving a stale
// base, and it never hard-fails — the base falls back to the local default.
func TestFetchAndBase_WarnsOnFetchFailureButFallsBack(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	// An origin whose URL does not exist: `git remote get-url` succeeds (so the fetch is
	// attempted) but the fetch itself fails.
	gitT(t, repo, "remote", "add", "origin", filepath.Join(t.TempDir(), "absent.git"))

	var warned int
	orig := warnf
	warnf = func(string, ...any) { warned++ }
	defer func() { warnf = orig }()

	base := fetchAndBase(repo)
	if warned == 0 {
		t.Fatal("a failed origin fetch must surface a warning")
	}
	// No origin/<default> ref was ever fetched, so the fallback is the local default.
	if base != def {
		t.Fatalf("offline fallback should use the local default %q, got %q", def, base)
	}
}

// TestFetchAndBase_NoOriginSkipsWarn confirms a repo with no origin remote does not warn
// (no fetch is attempted) and bases on the local default — the warning is reserved for an
// actual fetch FAILURE, not the legitimate remote-less case.
func TestFetchAndBase_NoOriginSkipsWarn(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	if RemoteExists(repo, "origin") {
		t.Fatal("a fresh repo has no origin remote")
	}

	var warned int
	orig := warnf
	warnf = func(string, ...any) { warned++ }
	defer func() { warnf = orig }()

	base := fetchAndBase(repo)
	if warned != 0 {
		t.Fatalf("a repo with no origin must not warn, warned %d time(s)", warned)
	}
	if base != def {
		t.Fatalf("with no origin the base should be the local default %q, got %q", def, base)
	}
}

// TestAcquire_NewSlotBasesOnFreshOrigin proves a freshly CREATED pool slot (no idle slot
// to reuse) is also anchored on the up-to-date origin/<default> tip, not the stale local
// HEAD. This is the path behind an isolated worktree (OpenCC), which has no StartBranch to
// re-base it afterward.
func TestAcquire_NewSlotBasesOnFreshOrigin(t *testing.T) {
	repo := makeRepo(t)
	def := DefaultBranch(repo)
	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)
	gitT(t, repo, "push", "-q", "origin", def)

	// Advance origin/<default> ahead of the local default via a detached linked worktree,
	// so the local branch stays behind.
	adv := filepath.Join(t.TempDir(), "adv")
	if err := AddDetached(repo, adv, def); err != nil {
		t.Fatal(err)
	}
	defer RemoveWorktree(repo, adv)
	if err := os.WriteFile(filepath.Join(adv, "g.txt"), []byte("origin advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, adv, "add", "-A")
	gitT(t, adv, "commit", "-q", "-m", "origin advance")
	gitT(t, adv, "push", "-q", "origin", "HEAD:"+def)
	originTip := gitT(t, bare, "rev-parse", "refs/heads/"+def)
	if originTip == gitT(t, repo, "rev-parse", "refs/heads/"+def) {
		t.Fatal("test setup: origin must be ahead of the local default")
	}

	// The first acquire on an empty pool CREATES a slot; it must start at the fresh tip.
	p := Pool{Root: t.TempDir(), Max: 4}
	slot, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, slot, "rev-parse", "HEAD"); got != originTip {
		t.Fatalf("a newly created slot should start at the fresh origin tip %s, got %s", originTip, got)
	}
}

// TestPoolFreeSlots: free capacity reflects pool availability — the cap (Max) minus the
// worktree slots in use, clamped at zero, with duplicate paths counted once (mirroring
// Acquire's busy-set). It is a pure projection of Max and occupancy, so it needs no repo
// on disk.
func TestPoolFreeSlots(t *testing.T) {
	p := Pool{Max: 4}
	cases := []struct {
		name  string
		inUse []string
		want  int
	}{
		{"empty pool is fully free", nil, 4},
		{"each in-use slot consumes one", []string{"/wt/0", "/wt/1"}, 2},
		{"duplicate paths count once", []string{"/wt/0", "/wt/0"}, 3},
		{"at capacity reports zero", []string{"/wt/0", "/wt/1", "/wt/2", "/wt/3"}, 0},
		{"over capacity clamps at zero", []string{"/wt/0", "/wt/1", "/wt/2", "/wt/3", "/wt/4"}, 0},
	}
	for _, c := range cases {
		if got := p.FreeSlots(c.inUse); got != c.want {
			t.Errorf("%s: FreeSlots(%v) with Max=%d = %d, want %d", c.name, c.inUse, p.Max, got, c.want)
		}
	}
}

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/clonepool"
	"github.com/nution101/ttorch/internal/gittest"
	"github.com/nution101/ttorch/internal/worktree"
)

// workdirManager builds a Manager with both pools rooted in temp dirs, for the acquire and
// release seam, which needs no session.
func workdirManager(t *testing.T) *Manager {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{
		Pool:   worktree.Pool{Root: filepath.Join(root, "worktrees"), Max: 4},
		Clones: clonepool.ClonePool{Root: filepath.Join(root, "clones"), Max: 4},
	}
}

// TestAcquireWorkdirByKind checks the spawn seam hands out a clone from the clone pool,
// with the task branch in the clone and none in main, and a worktree exactly as before,
// with ttorch/<id> cut in main.
func TestAcquireWorkdirByKind(t *testing.T) {
	repo := newRepoMain(t)
	m := workdirManager(t)

	wt, err := m.acquireWorkdir(clonepool.KindClone, repo, "c1", nil)
	if err != nil {
		t.Fatalf("acquire clone: %v", err)
	}
	if !strings.HasPrefix(wt, m.Clones.Root+string(filepath.Separator)) {
		t.Fatalf("clone %s is not under the clone pool %s", wt, m.Clones.Root)
	}
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("clone .git is not a directory: %v", err)
	}
	if got := gitIn(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/ttorch/c1" {
		t.Fatalf("clone HEAD = %s", got)
	}
	if worktree.RefExists(repo, "refs/heads/ttorch/c1") {
		t.Fatal("a clone spawn created ttorch/c1 in main")
	}

	wt2, err := m.acquireWorkdir(clonepool.KindWorktree, repo, "w1", nil)
	if err != nil {
		t.Fatalf("acquire worktree: %v", err)
	}
	if !strings.HasPrefix(wt2, m.Pool.Root+string(filepath.Separator)) {
		t.Fatalf("worktree %s is not under the worktree pool %s", wt2, m.Pool.Root)
	}
	if fi, err := os.Lstat(filepath.Join(wt2, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("worktree .git should be a gitfile: %v", err)
	}
	if !worktree.RefExists(repo, "refs/heads/ttorch/w1") {
		t.Fatal("a worktree spawn did not cut ttorch/w1 in main")
	}
}

// TestReleaseWorkdirRoutesByLocation checks a clone is released to the clone pool, which
// deletes it without running git in it, and a worktree to the worktree pool, which keeps
// it for reuse. Handing a clone to the worktree pool would run checkout and reset inside
// the worker's repository.
func TestReleaseWorkdirRoutesByLocation(t *testing.T) {
	repo := newRepoMain(t)
	m := workdirManager(t)
	clone, err := m.acquireWorkdir(clonepool.KindClone, repo, "c1", nil)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.acquireWorkdir(clonepool.KindWorktree, repo, "w1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Arm the clone so any git status or checkout run there leaves a mark.
	mark := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+mark+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "config", "core.fsmonitor", script)
	if err := os.MkdirAll(filepath.Join(clone, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+mark+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.releaseWorkdir(repo, clone); err != nil {
		t.Fatalf("release clone: %v", err)
	}
	if _, err := os.Lstat(clone); !os.IsNotExist(err) {
		t.Fatalf("released clone still exists (err %v); it went to the worktree pool", err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("releasing the clone ran git inside it")
	}

	if err := m.releaseWorkdir(repo, wt); err != nil {
		t.Fatalf("release worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("released worktree was not kept for reuse: %v", err)
	}
	if out, _ := gittest.Command(wt, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(out)) != "HEAD" {
		t.Fatalf("released worktree is on %q, want detached as Pool.Release leaves it", out)
	}
}

// TestSpawnWorkerClones drives a real spawn and teardown with TTORCH_WORKER_CLONES on, and
// checks --workdir worktree overrides it and a bad --workdir is refused before any side
// effect.
func TestSpawnWorkerClones(t *testing.T) {
	m, repo := deliveryHarness(t, "clones")
	t.Setenv(clonepool.EnvVar, "1")
	ctx := context.Background()

	task, err := m.SpawnWithEffort("k1", repo, false, "sleep 30", nil, false, "", "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !strings.HasPrefix(task.Worktree, m.P.Clones()+string(filepath.Separator)) {
		t.Fatalf("worker dir %s is not under %s", task.Worktree, m.P.Clones())
	}
	if fi, err := os.Lstat(filepath.Join(task.Worktree, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("worker dir is not a clone: %v", err)
	}
	if worktree.RefExists(repo, "refs/heads/ttorch/k1") {
		t.Fatal("a clone spawn created ttorch/k1 in main")
	}
	if _, err := m.Teardown("k1", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Lstat(task.Worktree); !os.IsNotExist(err) {
		t.Fatalf("teardown left the clone in place (err %v)", err)
	}

	m.Workdir = clonepool.KindWorktree
	task2, err := m.SpawnWithEffort("k2", repo, false, "sleep 30", nil, false, "", "")
	if err != nil {
		t.Fatalf("spawn --workdir worktree: %v", err)
	}
	if !strings.HasPrefix(task2.Worktree, m.P.Worktrees()+string(filepath.Separator)) {
		t.Fatalf("--workdir worktree gave %s, want a slot under %s", task2.Worktree, m.P.Worktrees())
	}
	_, _ = m.Teardown("k2", true)

	m.Workdir = "bogus"
	if _, err := m.SpawnWithEffort("k3", repo, false, "sleep 30", nil, false, "", ""); err == nil {
		t.Fatal("spawn accepted --workdir bogus")
	}
	if m.backend().WindowExists(m.Session, "wk-k3") {
		t.Fatal("a refused spawn left a window")
	}
	if _, ok, _ := m.Store.GetTask(ctx, "k3"); ok {
		t.Fatal("a refused spawn left a task row")
	}
}

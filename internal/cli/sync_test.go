package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// fixtureGit runs git in dir with a fixture identity and fails the test on error.
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=sync fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolateGit keeps the lead's real git config out of the test, for the fixture's git and for
// the git that syncClone runs.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// syncFixture is a lead repository on main with two commits, and a clone slot under the
// clones root provisioned the way the clone pool does it: git init, an alternates file onto
// the lead's objects, origin/main pinned at the first commit, and the task branch checked out
// there. It returns the lead repo, the slot and the first commit.
func syncFixture(t *testing.T) (repo, slot, base string) {
	t.Helper()
	isolateGit(t)
	repo = filepath.Join(realDir(t), "lead")
	fixtureGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", "f")
	fixtureGit(t, repo, "commit", "-q", "-m", "one")
	base = fixtureGit(t, repo, "rev-parse", "HEAD")

	slot = filepath.Join(paths.Default().Home, "clones", "lead-0123abcd", "1")
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, filepath.Dir(slot), "init", "-q", "-b", "main", slot)
	common := fixtureGit(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := os.WriteFile(filepath.Join(slot, ".git", "objects", "info", "alternates"), []byte(filepath.Join(common, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, slot, "update-ref", "refs/remotes/origin/main", base)
	fixtureGit(t, slot, "checkout", "-q", "-B", "ttorch/t1", base)

	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "commit", "-q", "-am", "two")
	return repo, slot, base
}

// snapshot records every path under dir with a digest of its content (or its link target), so
// a before/after comparison shows any write, including a new ref, FETCH_HEAD or a config edit.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			snap[rel] = "link:" + target
		case fi.IsDir():
			snap[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			snap[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func assertSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for k, v := range after {
		if before[k] != v {
			diffs = append(diffs, "changed or added: "+k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			diffs = append(diffs, "removed: "+k)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Errorf("the lead's repository was written:\n%s", strings.Join(diffs, "\n"))
	}
}

// TestSyncCloneFetchesBaseWithoutWritingLead: sync moves the clone's origin/main to the lead's
// current default and leaves every byte of the lead's repository as it was.
func TestSyncCloneFetchesBaseWithoutWritingLead(t *testing.T) {
	repo, slot, base := syncFixture(t)
	tip := fixtureGit(t, repo, "rev-parse", "refs/heads/main")
	before := snapshot(t, repo)

	if err := syncClone(repo, slot, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"); got != tip {
		t.Errorf("clone origin/main = %s, want the lead's main %s", got, tip)
	}
	if got := fixtureGit(t, slot, "rev-parse", "HEAD"); got != base {
		t.Errorf("sync must not move the worker's branch: HEAD = %s, want %s", got, base)
	}
	if _, err := os.Stat(filepath.Join(slot, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Errorf("sync wrote FETCH_HEAD in the clone (stat err = %v)", err)
	}
	assertSameSnapshot(t, before, snapshot(t, repo))
}

// TestSyncClonePicksLandBase: with an origin, the base is origin/main when the local default is
// an ancestor of it, and the local default when the lead has commits origin does not.
func TestSyncClonePicksLandBase(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	remote := filepath.Join(realDir(t), "remote.git")
	fixtureGit(t, repo, "init", "-q", "--bare", "-b", "main", remote)
	fixtureGit(t, repo, "remote", "add", "origin", remote)
	fixtureGit(t, repo, "push", "-q", "origin", "main")
	fixtureGit(t, repo, "fetch", "-q", "origin")

	// origin ahead of the local default: a commit pushed from elsewhere, then fetched.
	other := filepath.Join(realDir(t), "other")
	fixtureGit(t, repo, "clone", "-q", remote, other)
	fixtureGit(t, other, "commit", "-q", "--allow-empty", "-m", "three")
	fixtureGit(t, other, "push", "-q", "origin", "main")
	fixtureGit(t, repo, "fetch", "-q", "origin")
	originTip := fixtureGit(t, repo, "rev-parse", "refs/remotes/origin/main")
	if err := syncClone(repo, slot, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"); got != originTip {
		t.Errorf("origin ahead: clone origin/main = %s, want origin's %s", got, originTip)
	}

	// The local default diverged from origin: the land gate bases on the local default.
	fixtureGit(t, repo, "commit", "-q", "--allow-empty", "-m", "local only")
	localTip := fixtureGit(t, repo, "rev-parse", "refs/heads/main")
	if err := syncClone(repo, slot, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"); got != localTip {
		t.Errorf("local diverged: clone origin/main = %s, want the local default %s", got, localTip)
	}
}

// TestSyncCloneIgnoresShadowingTag: a tag named like the default branch does not stand in for
// it, because sync reads refs/heads/<default> and refs/remotes/origin/<default> by full name.
func TestSyncCloneIgnoresShadowingTag(t *testing.T) {
	repo, slot, base := syncFixture(t)
	fixtureGit(t, repo, "tag", "main", base)
	tip := fixtureGit(t, repo, "rev-parse", "refs/heads/main")
	if err := syncClone(repo, slot, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"); got != tip {
		t.Errorf("clone origin/main = %s, want the branch %s, not the tag %s", got, tip, base)
	}
}

// TestSyncCloneRefusesWorktree: in a linked worktree origin/main is the lead's own ref, so a
// fetch into it would write the lead's repository. sync refuses and writes nothing.
func TestSyncCloneRefusesWorktree(t *testing.T) {
	repo, _, base := syncFixture(t)
	wt := filepath.Join(paths.Default().Home, "clones", "lead-0123abcd", "2")
	fixtureGit(t, repo, "worktree", "add", "-q", "--detach", wt, base)
	before := snapshot(t, repo)
	err := syncClone(repo, wt, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a clone") {
		t.Errorf("sync in a linked worktree: err = %v, want a refusal", err)
	}
	assertSameSnapshot(t, before, snapshot(t, repo))
}

// TestCmdSyncFromDB: the worker's command resolves its clone and the lead's repository from
// its own task row.
func TestCmdSyncFromDB(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := store.UpsertProject(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(context.Background(), db.Task{
		ID: "t1", ProjectID: proj.ID, Worktree: slot, Status: db.StatusActive, Owner: "worker:t1",
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	store.Close()
	t.Setenv("TTORCH_TASK_ID", "t1")

	if err := cmdSync(nil); err != nil {
		t.Fatal(err)
	}
	if got, want := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"), fixtureGit(t, repo, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("clone origin/main = %s, want %s", got, want)
	}
}

// TestCmdSyncRefusesManager: outside a worker there is no task to sync, and the manager never
// runs git in a worker's clone.
func TestCmdSyncRefusesManager(t *testing.T) {
	t.Setenv("TTORCH_TASK_ID", "")
	t.Chdir(t.TempDir())
	if err := cmdSync(nil); err == nil || !strings.Contains(err.Error(), "as a worker") {
		t.Fatalf("cmdSync outside a worker: err = %v, want a refusal", err)
	}
	if err := cmdSync([]string{"extra"}); err == nil {
		t.Fatalf("cmdSync with an argument must fail")
	}
}

// sentinelProgram returns a shell command that records name in dir when git runs it.
func sentinelProgram(dir, name string) string {
	return "touch '" + filepath.Join(dir, "ran-"+name) + "'; true"
}

func assertNoSentinels(t *testing.T, dir string) {
	t.Helper()
	ran, _ := filepath.Glob(filepath.Join(dir, "ran-*"))
	if len(ran) > 0 {
		var names []string
		for _, r := range ran {
			names = append(names, strings.TrimPrefix(filepath.Base(r), "ran-"))
		}
		t.Errorf("sync ran programs the clone's config named: %s", strings.Join(names, ", "))
	}
}

// TestSyncCloneRunsNoCloneConfiguredProgram: a plain `git fetch` in a clone runs the clone's
// reference-transaction hook, its core.alternateRefsCommand and its core.fsmonitor. sync's
// fetch runs none of them and still moves the ref, so a lead who runs sync from inside a
// clone does not run the worker's programs.
func TestSyncCloneRunsNoCloneConfiguredProgram(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	marks := realDir(t)
	hook := filepath.Join(slot, ".git", "hooks", "reference-transaction")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+sentinelProgram(marks, "hook")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, slot, "config", "core.alternateRefsCommand", sentinelProgram(marks, "alternateRefsCommand"))
	fixtureGit(t, slot, "config", "core.fsmonitor", sentinelProgram(marks, "fsmonitor"))
	if err := os.RemoveAll(marks); err != nil { // the fixture's own git calls above may have run the fsmonitor
		t.Fatal(err)
	}
	if err := os.MkdirAll(marks, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := syncClone(repo, slot, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertNoSentinels(t, marks)
	if got, want := fixtureGit(t, repo, "rev-parse", "refs/heads/main"), readLooseOrPacked(t, slot, "refs/remotes/origin/main"); got != want {
		t.Errorf("clone origin/main = %s, want %s", want, got)
	}
}

// readLooseOrPacked reads a ref of the clone without running git there (which would run its
// fsmonitor and leave a sentinel behind).
func readLooseOrPacked(t *testing.T, slot, ref string) string {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(slot, ".git", ref)); err == nil {
		return strings.TrimSpace(string(b))
	}
	b, _ := os.ReadFile(filepath.Join(slot, ".git", "packed-refs"))
	for _, line := range strings.Split(string(b), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok && name == ref {
			return sha
		}
	}
	return ""
}

// TestSyncCloneRefusesTransportRewrite: a url.*.insteadOf in the clone's config rewrites the
// lead's path to an ssh URL whose core.sshCommand is the worker's program, and a plain fetch
// runs it. sync refuses every transport but a local path, so the fetch fails without running
// it, and the error does not name the lead's repository.
func TestSyncCloneRefusesTransportRewrite(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	marks := realDir(t)
	fixtureGit(t, slot, "config", "core.sshCommand", sentinelProgram(marks, "sshCommand"))
	fixtureGit(t, slot, "config", "url.ssh://rewritten.invalid/x.insteadOf", repo)
	// Without the alternates file the clone lacks the commit, so the fetch has to open a
	// transport; with it, git finds the object locally and never consults the URL.
	if err := os.Remove(filepath.Join(slot, ".git", "objects", "info", "alternates")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, repo)
	err := syncClone(repo, slot, io.Discard)
	if err == nil {
		t.Fatalf("sync through an ssh rewrite succeeded; want it refused")
	}
	if strings.Contains(err.Error(), repo) {
		t.Errorf("the error names the lead's repository: %v", err)
	}
	assertNoSentinels(t, marks)
	assertSameSnapshot(t, before, snapshot(t, repo))
}

// TestSyncCloneIgnoresCallerGitEnv: a GIT_DIR in the caller's environment would point the
// clone-side fetch at another repository. sync drops every GIT_ variable, so the clone moves
// and the other repository is untouched.
func TestSyncCloneIgnoresCallerGitEnv(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	other := filepath.Join(realDir(t), "other")
	fixtureGit(t, filepath.Dir(other), "init", "-q", other)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	err := syncClone(repo, slot, io.Discard)
	os.Unsetenv("GIT_DIR")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fixtureGit(t, slot, "rev-parse", "refs/remotes/origin/main"), fixtureGit(t, repo, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("clone origin/main = %s, want %s", got, want)
	}
	if out, err := exec.Command("git", "-C", other, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main").CombinedOutput(); err == nil {
		t.Errorf("sync wrote the repository GIT_DIR named: %s", out)
	}
}

// TestSyncCloneOutputOmitsLeadPath: the worker needs the ref and the sha, not where the lead's
// repository lives.
func TestSyncCloneOutputOmitsLeadPath(t *testing.T) {
	repo, slot, _ := syncFixture(t)
	var out strings.Builder
	if err := syncClone(repo, slot, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), repo) {
		t.Errorf("sync output names the lead's repository: %q", out.String())
	}
	if !strings.Contains(out.String(), fixtureGit(t, repo, "rev-parse", "refs/heads/main")) {
		t.Errorf("sync output should give the sha: %q", out.String())
	}
}

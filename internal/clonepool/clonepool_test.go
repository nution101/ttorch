package clonepool

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/gittest"
	"github.com/nution101/ttorch/internal/harness"
)

// TestMain clears any inherited GIT_DIR and the like (git rebase --exec exports one), so the
// git that fixtures and the code under test run acts on the temp repository it names.
func TestMain(m *testing.M) {
	gittest.Scrub()
	os.Exit(m.Run())
}

// TestAcquireProvisionsAPrivateClone checks the shape of a provisioned slot: its own git
// directory, the task branch at main's tip, a clean checkout, objects borrowed from main
// rather than copied, and no task branch created in main.
func TestAcquireProvisionsAPrivateClone(t *testing.T) {
	repo := newMain(t)
	p := newPool(t)
	base := run(t, repo, "rev-parse", "HEAD")

	slot, err := p.Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if filepath.Dir(slot) != p.Dir(repo) {
		t.Fatalf("slot %s is not in the pool dir %s", slot, p.Dir(repo))
	}
	fi, err := os.Lstat(filepath.Join(slot, ".git"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("slot .git is not a directory: %v", err)
	}
	if got := run(t, slot, "symbolic-ref", "HEAD"); got != "refs/heads/ttorch/t1" {
		t.Fatalf("HEAD = %s, want refs/heads/ttorch/t1", got)
	}
	if got := run(t, slot, "rev-parse", "HEAD"); got != base {
		t.Fatalf("clone HEAD = %s, want main's tip %s", got, base)
	}
	if got := run(t, slot, "rev-parse", "refs/remotes/origin/main"); got != base {
		t.Fatalf("clone origin/main = %s, want %s", got, base)
	}
	if got := run(t, slot, "status", "--porcelain"); got != "" {
		t.Fatalf("fresh clone is not clean:\n%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(slot, "f.txt")); string(b) != "hi\n" {
		t.Fatalf("checkout content = %q", b)
	}
	alt, err := os.ReadFile(filepath.Join(slot, ".git", "objects", "info", "alternates"))
	if err != nil || strings.TrimSpace(string(alt)) != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("alternates = %q (%v), want main's object dir", alt, err)
	}
	if got := run(t, slot, "count-objects"); !strings.HasPrefix(got, "0 objects") {
		t.Fatalf("clone holds its own objects (%s); it should borrow main's", got)
	}
	if _, err := tryRun(repo, "rev-parse", "--verify", "--quiet", "refs/heads/ttorch/t1"); err == nil {
		t.Fatal("main has refs/heads/ttorch/t1; the task branch belongs to the clone only")
	}
}

// TestAcquirePinsTheBase checks that main holds refs/ttorch/clones/<task>/base at the base
// the clone borrows, so a gc in main cannot prune it.
func TestAcquirePinsTheBase(t *testing.T) {
	repo := newMain(t)
	p := newPool(t)
	base := run(t, repo, "rev-parse", "HEAD")
	if _, err := p.Acquire(repo, "t1", nil); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got, err := tryRun(repo, "rev-parse", "--verify", "refs/ttorch/clones/t1/base"); err != nil || got != base {
		t.Fatalf("pin ref = %q (%v), want %s", got, err, base)
	}
}

// TestAcquireBasesOnFetchedOriginByQualifiedName checks the base is origin's fresh tip,
// fetched at acquire, and that a tag spelled like the remote-tracking branch does not
// stand in for it.
func TestAcquireBasesOnFetchedOriginByQualifiedName(t *testing.T) {
	repo := newMain(t)
	old := run(t, repo, "rev-parse", "HEAD")
	bare := filepath.Join(filepath.Dir(repo), "origin.git")
	run(t, filepath.Dir(repo), "clone", "-q", "--bare", repo, bare)
	run(t, repo, "remote", "add", "origin", bare)
	run(t, repo, "fetch", "-q", "origin")
	run(t, repo, "remote", "set-head", "origin", "main")
	// Someone else advances origin after main last fetched.
	other := filepath.Join(filepath.Dir(repo), "other")
	run(t, filepath.Dir(repo), "clone", "-q", bare, other)
	tip := commit(t, other, "g.txt", "g\n", "advance")
	run(t, other, "push", "-q", "origin", "HEAD:main")
	run(t, repo, "tag", "origin/main", old)

	slot, err := newPool(t).Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := run(t, slot, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("clone based on %s, want origin's fresh tip %s (old %s)", got, tip, old)
	}
}

// TestCloneHasNoRemoteToMain is E2b as a test: a clone made with `git clone` names its
// source origin, and a push to it lands in main. A provisioned clone must have no remote
// that points at main, and main's path must not appear in its config at all.
func TestCloneHasNoRemoteToMain(t *testing.T) {
	t.Run("main without origin", func(t *testing.T) {
		repo := newMain(t)
		slot, err := newPool(t).Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got := run(t, slot, "remote"); got != "" {
			t.Fatalf("clone has remotes %q; main has none to copy", got)
		}
		assertNoMainInConfig(t, repo, slot)
		commit(t, slot, "w.txt", "w\n", "worker")
		_, _ = tryRun(slot, "push", "origin", "HEAD:refs/heads/evil")
		if _, err := tryRun(repo, "rev-parse", "--verify", "--quiet", "refs/heads/evil"); err == nil {
			t.Fatal("a push from the clone created refs/heads/evil in main")
		}
	})
	t.Run("main with an origin", func(t *testing.T) {
		repo := newMain(t)
		bare := filepath.Join(filepath.Dir(repo), "origin.git")
		run(t, filepath.Dir(repo), "clone", "-q", "--bare", repo, bare)
		run(t, repo, "remote", "add", "origin", bare)
		run(t, repo, "fetch", "-q", "origin")
		slot, err := newPool(t).Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got := run(t, slot, "config", "--get", "remote.origin.url"); got != bare {
			t.Fatalf("clone origin = %q, want main's origin %q", got, bare)
		}
		assertNoMainInConfig(t, repo, slot)
		commit(t, slot, "w.txt", "w\n", "worker")
		run(t, slot, "push", "-q", "origin", "HEAD:refs/heads/evil")
		if _, err := tryRun(repo, "rev-parse", "--verify", "--quiet", "refs/heads/evil"); err == nil {
			t.Fatal("a push from the clone created refs/heads/evil in main")
		}
	})
	t.Run("main whose origin is itself", func(t *testing.T) {
		repo := newMain(t)
		run(t, repo, "remote", "add", "origin", repo)
		slot, err := newPool(t).Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got := run(t, slot, "remote"); got != "" {
			t.Fatalf("clone has remotes %q; main's origin is main, which must not be copied", got)
		}
		assertNoMainInConfig(t, repo, slot)
	})
}

func assertNoMainInConfig(t *testing.T, repo, slot string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(slot, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), repo) {
		t.Fatalf("clone config names main's path %s:\n%s", repo, b)
	}
}

// TestInfoExcludeIsTheClones is E11 as a test: in a linked worktree info/exclude resolves
// to main's shared file, so the worker's own exclusions land in main. In a clone it is the
// clone's own, and the harness's exclusion for the task file stays there.
func TestInfoExcludeIsTheClones(t *testing.T) {
	repo := newMain(t)
	mainExclude := filepath.Join(repo, ".git", "info", "exclude")
	before, _ := os.ReadFile(mainExclude)
	slot, err := newPool(t).Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	got := run(t, slot, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if want := filepath.Join(slot, ".git", "info", "exclude"); got != want {
		t.Fatalf("clone info/exclude = %s, want %s", got, want)
	}
	if got := run(t, slot, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != filepath.Join(slot, ".git") {
		t.Fatalf("clone common dir = %s, want its own .git", got)
	}
	if err := harness.WriteWorkerTaskFile(slot, "t1", "/tmp/db"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(mainExclude)
	if string(after) != string(before) {
		t.Fatalf("main's info/exclude changed:\nbefore %q\nafter  %q", before, after)
	}
	own, _ := os.ReadFile(filepath.Join(slot, ".git", "info", "exclude"))
	if !strings.Contains(string(own), ".ttorch/task") {
		t.Fatalf("clone's info/exclude = %q, want the task file excluded there", own)
	}
}

// TestConfigIsTheAllowlist checks the clone's config holds what git init writes, the
// remote, and main's values for the allowlisted keys, and nothing else from main.
func TestConfigIsTheAllowlist(t *testing.T) {
	repo := newMain(t)
	allowed := map[string]string{
		"user.name":      "Lead Person",
		"user.email":     "lead@example.com",
		"commit.gpgsign": "false",
		"core.autocrlf":  "input",
		"core.eol":       "lf",
	}
	for k, v := range allowed {
		run(t, repo, "config", k, v)
	}
	for k, v := range map[string]string{
		"alias.co":                      "checkout",
		"diff.external":                 "/bin/false",
		"credential.helper":             "store",
		"core.sshCommand":               "ssh -v",
		"include.path":                  "/nonexistent/include",
		"core.pager":                    "cat",
		"merge.ours.driver":             "true",
		"filter.lead.smudge":            "cat",
		"uploadpack.allowAnySHA1InWant": "true",
	} {
		run(t, repo, "config", k, v)
	}

	slot, err := newPool(t).Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	permitted := map[string]bool{
		// written by git init on the platforms ttorch runs on
		"core.repositoryformatversion": true, "core.bare": true, "core.logallrefupdates": true,
	}
	for _, k := range seededConfigKeys {
		permitted[strings.ToLower(k)] = true
	}
	names := run(t, slot, "config", "--file", filepath.Join(slot, ".git", "config"), "--list", "--name-only")
	var extra []string
	for _, k := range strings.Split(names, "\n") {
		if k != "" && !permitted[strings.ToLower(k)] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("clone config carries keys outside the allowlist: %v", extra)
	}
	for k, v := range allowed {
		if got := run(t, slot, "config", "--file", filepath.Join(slot, ".git", "config"), "--get", k); got != v {
			t.Fatalf("clone %s = %q, want main's %q", k, got, v)
		}
	}
}

// TestHooksAndTagsAreSeeded checks hook parity (main's hooks are copied, the samples are
// not, a relative core.hooksPath is copied and an absolute one is not) and that main's
// tags resolve in the clone while a tag made in the clone stays there.
func TestHooksAndTagsAreSeeded(t *testing.T) {
	repo := newMain(t)
	hook := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push.sample"), "#!/bin/sh\n")
	run(t, repo, "config", "core.hooksPath", ".githooks")
	run(t, repo, "tag", "v1")
	tip := commit(t, repo, "g.txt", "g\n", "second")
	run(t, repo, "tag", "-a", "v2", "-m", "release two")

	slot, err := newPool(t).Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	fi, err := os.Stat(filepath.Join(slot, ".git", "hooks", "pre-commit"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("pre-commit hook not copied executable: %v", err)
	}
	if exists(filepath.Join(slot, ".git", "hooks", "pre-push.sample")) {
		t.Fatal("a .sample hook was copied")
	}
	if got := run(t, slot, "config", "--get", "core.hooksPath"); got != ".githooks" {
		t.Fatalf("clone core.hooksPath = %q, want the relative .githooks", got)
	}
	if got := run(t, slot, "rev-parse", "v2^{commit}"); got != tip {
		t.Fatalf("v2 resolves to %s, want %s", got, tip)
	}
	if got := run(t, slot, "describe", "--tags"); got != "v2" {
		t.Fatalf("describe = %q, want v2", got)
	}
	if got := run(t, slot, "rev-parse", "v1^{commit}"); got == tip {
		t.Fatal("v1 should name the first commit")
	}
	run(t, slot, "tag", "worker-tag")
	if _, err := tryRun(repo, "rev-parse", "--verify", "--quiet", "refs/tags/worker-tag"); err == nil {
		t.Fatal("a tag made in the clone appeared in main")
	}

	t.Run("absolute hooksPath is not copied", func(t *testing.T) {
		repo := newMain(t)
		run(t, repo, "config", "core.hooksPath", filepath.Join(repo, ".githooks"))
		slot, err := newPool(t).Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got, err := tryRun(slot, "config", "--get", "core.hooksPath"); err == nil {
			t.Fatalf("clone core.hooksPath = %q; an absolute path names a directory outside the clone", got)
		}
	})
}

// TestRefusesUnsupportedRepos checks LFS, partial-clone and submodule repositories are
// refused with ErrUnsupportedRepo before anything is written: no slot, no pin ref.
func TestRefusesUnsupportedRepos(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo string)
	}{
		{"lfs attributes", func(t *testing.T, repo string) {
			commit(t, repo, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n", "lfs")
		}},
		{"lfs attributes in a subdirectory", func(t *testing.T, repo string) {
			commit(t, repo, "assets/.gitattributes", "*.png filter=lfs -text\n", "lfs")
		}},
		{"lfsconfig", func(t *testing.T, repo string) {
			commit(t, repo, ".lfsconfig", "[lfs]\n\turl = https://example.invalid/lfs\n", "lfs")
		}},
		{"gitlink", func(t *testing.T, repo string) {
			sha := run(t, repo, "rev-parse", "HEAD")
			run(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+sha+",sub")
			run(t, repo, "commit", "-q", "-m", "submodule")
		}},
		{"gitmodules", func(t *testing.T, repo string) {
			commit(t, repo, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = ../x\n", "modules")
		}},
		{"partial clone extension", func(t *testing.T, repo string) {
			run(t, repo, "config", "extensions.partialClone", "origin")
		}},
		{"promisor remote", func(t *testing.T, repo string) {
			run(t, repo, "config", "remote.origin.promisor", "true")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := newMain(t)
			c.setup(t, repo)
			p := newPool(t)
			_, err := p.Acquire(repo, "t1", nil)
			if !errors.Is(err, ErrUnsupportedRepo) {
				t.Fatalf("Acquire err = %v, want ErrUnsupportedRepo", err)
			}
			if !strings.Contains(err.Error(), "--workdir worktree") {
				t.Fatalf("refusal does not say how to proceed: %v", err)
			}
			if slots := listSlots(p.Dir(repo)); len(slots) != 0 {
				t.Fatalf("refused acquire left slots %v", slots)
			}
			if _, err := tryRun(repo, "rev-parse", "--verify", "--quiet", "refs/ttorch/clones/t1/base"); err == nil {
				t.Fatal("refused acquire left a pin ref in main")
			}
		})
	}
	t.Run("plain gitattributes is accepted", func(t *testing.T) {
		repo := newMain(t)
		commit(t, repo, ".gitattributes", "*.go text eol=lf\n", "attrs")
		if _, err := newPool(t).Acquire(repo, "t1", nil); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	})
}

// TestHostileTaskIDsNeverReachGit checks an id that cannot be spliced into a ref name is
// refused before any git command runs.
func TestHostileTaskIDsNeverReachGit(t *testing.T) {
	repo := newMain(t)
	calls, reset := gitShim(t)
	p := newPool(t)
	for _, id := range []string{"", "-x", "a..b", "a b", "a/../b", "a/b", "x.lock", ".x", "a\nb", "a:b"} {
		reset()
		if _, err := p.Acquire(repo, id, nil); err == nil {
			t.Fatalf("Acquire(%q) succeeded", id)
		}
		if c := calls(); len(c) != 0 {
			t.Fatalf("Acquire(%q) ran git: %v", id, c)
		}
	}
	if exists(p.Dir(repo)) {
		t.Fatal("a refused id created the pool dir")
	}
}

// TestPoolNeverRunsGitInAnExistingSlot is the shim test from the design: through acquire,
// release and recycle, no git process runs in a slot's repository except the commands
// that provision it after this call created it. The slot is armed the way a hostile
// worker could arm it, and none of its programs may run.
func TestPoolNeverRunsGitInAnExistingSlot(t *testing.T) {
	assertProvisionOnly := func(t *testing.T, calls []shimCall, slot string) {
		t.Helper()
		created := false
		for _, c := range calls {
			if c.initOf(slot) {
				created = true
				continue
			}
			if c.touches(slot) && !created {
				t.Fatalf("git ran against %s before this call created it: cwd=%s args=%q", slot, c.cwd, c.args)
			}
		}
	}

	t.Run("recycle of an idle slot", func(t *testing.T) {
		repo := newMain(t)
		p := newPool(t)
		slot, err := p.Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		fired := sentinelRepo(t, slot)
		calls, reset := gitShim(t)
		reset()
		// t1's record was lost, so the slot is idle; its tip is main's tip, so it is landed.
		again, err := p.Acquire(repo, "t2", nil)
		if err != nil {
			t.Fatalf("recycling Acquire: %v", err)
		}
		if again != slot {
			t.Fatalf("Acquire returned %s, want the landed idle slot %s recycled", again, slot)
		}
		assertProvisionOnly(t, calls(), slot)
		if b, err := os.ReadFile(fired); err == nil {
			t.Fatalf("a program the worker configured ran: %s", b)
		}
		if got, err := tryRun(again, "config", "--get", "core.fsmonitor"); err == nil {
			t.Fatalf("recycled slot kept the worker's core.fsmonitor %q", got)
		}
		if got := run(t, again, "symbolic-ref", "HEAD"); got != "refs/heads/ttorch/t2" {
			t.Fatalf("recycled slot HEAD = %s", got)
		}
	})

	t.Run("release then acquire", func(t *testing.T) {
		repo := newMain(t)
		p := newPool(t)
		slot, err := p.Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		commit(t, slot, "w.txt", "w\n", "worker work") // released work is the caller's call
		fired := sentinelRepo(t, slot)
		calls, reset := gitShim(t)
		reset()
		if err := p.Release(repo, slot); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if c := calls(); len(c) != 0 {
			t.Fatalf("Release ran git: %v", c)
		}
		if exists(slot) {
			t.Fatal("Release left the slot in place")
		}
		reset()
		again, err := p.Acquire(repo, "t2", nil)
		if err != nil {
			t.Fatalf("Acquire after release: %v", err)
		}
		assertProvisionOnly(t, calls(), again)
		if b, err := os.ReadFile(fired); err == nil {
			t.Fatalf("a program the worker configured ran: %s", b)
		}
	})

	t.Run("skip of an unlanded slot", func(t *testing.T) {
		repo := newMain(t)
		p := newPool(t)
		slot, err := p.Acquire(repo, "t1", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		commit(t, slot, "w.txt", "w\n", "unlanded")
		fired := sentinelRepo(t, slot)
		calls, reset := gitShim(t)
		reset()
		other, err := p.Acquire(repo, "t2", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		assertProvisionOnly(t, calls(), slot)
		assertProvisionOnly(t, calls(), other)
		if b, err := os.ReadFile(fired); err == nil {
			t.Fatalf("a program the worker configured ran: %s", b)
		}
	})
}

// TestAcquireSkipsSlotsWithUnlandedWork checks the defense behind a lost task record: an
// idle slot whose tip main does not have, or reach from a safe base, or whose tip cannot
// be read, is never recycled. Once that tip lands in main, the slot is reused.
func TestAcquireSkipsSlotsWithUnlandedWork(t *testing.T) {
	repo := newMain(t)
	p := newPool(t)
	slot1, err := p.Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	work := commit(t, slot1, "w.txt", "w\n", "unlanded")

	slot2, err := p.Acquire(repo, "t2", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if slot2 == slot1 {
		t.Fatal("Acquire recycled a slot holding an unlanded commit")
	}
	if tip, ok := observeTip(slot1); !ok || tip != work {
		t.Fatalf("slot1 tip = %q (%v), want its commit %s preserved", tip, ok, work)
	}

	// A slot whose HEAD cannot be read is left alone too.
	writeFile(t, filepath.Join(slot2, ".git", "HEAD"), "garbage\n")
	slot3, err := p.Acquire(repo, "t3", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if slot3 == slot1 || slot3 == slot2 {
		t.Fatalf("Acquire recycled %s", slot3)
	}

	// The commit lands in main: fetch it and fast-forward the default branch.
	run(t, repo, "fetch", "-q", slot1, "HEAD")
	run(t, repo, "merge", "-q", "--ff-only", "FETCH_HEAD")
	slot4, err := p.Acquire(repo, "t4", []string{slot3})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if slot4 != slot1 {
		t.Fatalf("Acquire returned %s, want slot1 %s recycled now that its tip landed", slot4, slot1)
	}
}

// TestAcquireRefusesWhenFull checks the cap, counted over slots on disk and over every
// worker directory in use for the repo, clones and worktrees together.
func TestAcquireRefusesWhenFull(t *testing.T) {
	repo := newMain(t)
	p := newPool(t)
	p.Max = 1
	slot, err := p.Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := p.Acquire(repo, "t2", []string{slot}); err == nil || !strings.Contains(err.Error(), "pool full") {
		t.Fatalf("Acquire with the only slot busy: err = %v, want pool full", err)
	}

	repo2 := newMain(t)
	p2 := newPool(t)
	p2.Max = 1
	if _, err := p2.Acquire(repo2, "t1", []string{"/elsewhere/worktrees/x/1"}); err == nil || !strings.Contains(err.Error(), "pool full") {
		t.Fatalf("Acquire with a worktree holding the only slot: err = %v, want pool full", err)
	}
	if got := p2.FreeSlots([]string{"/a", "/a", "/b"}); got != 0 {
		t.Fatalf("FreeSlots = %d, want 0", got)
	}
}

// TestReleaseRefusesForeignPaths checks Release deletes only a numbered slot directly
// under the repository's pool dir, never main, the pool dir, or a symlink's target.
func TestReleaseRefusesForeignPaths(t *testing.T) {
	repo := newMain(t)
	p := newPool(t)
	slot, err := p.Acquire(repo, "t1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "keep"), "x")
	link := filepath.Join(p.Dir(repo), "9")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(p.Dir(repo), "notaslot")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{repo, p.Dir(repo), p.Root, named, link, filepath.Join(slot, "sub")} {
		if err := p.Release(repo, bad); err == nil {
			t.Fatalf("Release(%s) succeeded", bad)
		}
	}
	if !exists(filepath.Join(repo, ".git")) || !exists(filepath.Join(target, "keep")) || !exists(named) || !exists(slot) {
		t.Fatal("a refused Release deleted something")
	}
	if err := p.Release(repo, slot); err != nil || exists(slot) {
		t.Fatalf("Release(slot) = %v, exists=%v", err, exists(slot))
	}
}

// TestOwns checks Owns matches by location under the pool root.
func TestOwns(t *testing.T) {
	p := ClonePool{Root: "/home/u/.ttorch/clones"}
	for path, want := range map[string]bool{
		"/home/u/.ttorch/clones/r-1234abcd/1":    true,
		"/home/u/.ttorch/clones":                 false,
		"/home/u/.ttorch/worktrees/r-1234abcd/1": false,
		"/home/u/.ttorch/clones-x/r/1":           false,
		"/home/u/.ttorch/clones/../worktrees/1":  false,
	} {
		if got := p.Owns(path); got != want {
			t.Fatalf("Owns(%s) = %v, want %v", path, got, want)
		}
	}
	if (ClonePool{}).Owns("/x") {
		t.Fatal("a pool with no root owns a path")
	}
}

// TestResolveKind checks the flag defaults off, reads truthy values, and that an explicit
// --workdir wins either way.
func TestResolveKind(t *testing.T) {
	for _, c := range []struct {
		env, requested, want string
	}{
		{"", "", KindWorktree},
		{"0", "", KindWorktree},
		{"off", "", KindWorktree},
		{"maybe", "", KindWorktree},
		{"1", "", KindClone},
		{"true", "", KindClone},
		{" Yes ", "", KindClone},
		{"on", "", KindClone},
		{"1", KindWorktree, KindWorktree},
		{"", KindClone, KindClone},
	} {
		t.Setenv(EnvVar, c.env)
		got, err := ResolveKind(c.requested)
		if err != nil || got != c.want {
			t.Fatalf("env=%q requested=%q: got %q (%v), want %q", c.env, c.requested, got, err, c.want)
		}
	}
	if _, err := ResolveKind("bogus"); err == nil {
		t.Fatal("ResolveKind accepted bogus")
	}
	if ValidKind("bogus") || !ValidKind("") || !ValidKind(KindClone) || !ValidKind(KindWorktree) {
		t.Fatal("ValidKind is wrong")
	}
}

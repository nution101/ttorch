package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/worktree"
)

// branchedRepo builds a git repository with one commit whose checkout is on branch, and
// returns its root as git reports it (symlinks resolved), which is how projects are keyed.
func branchedRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	root, err := worktree.RepoRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestCmdProjectAddRecordsDefaultBranch: registering a project records the branch its checkout
// is on as the default branch the trust gate reads, and says so. Registering it again from
// another branch keeps the recorded one.
func TestCmdProjectAddRecordsDefaultBranch(t *testing.T) {
	repo := branchedRepo(t, "develop")
	dbPath := withSeedDB(t, nil)
	out, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "default-branch=develop") {
		t.Fatalf("project add must print the recorded branch, got %q", out)
	}
	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if _, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) }); err != nil {
		t.Fatal(err)
	}
	p, ok, err := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo)
	if err != nil || !ok || p.DefaultBranch != "develop" || p.DefaultBranchSeed != "" {
		t.Fatalf("project = %+v ok=%v err=%v; want develop kept, nothing pending", p, ok, err)
	}
	ls, err := captureStdout(t, func() error { return cmdProjectLs(nil) })
	if err != nil || !strings.Contains(ls, "BRANCH") || !strings.Contains(ls, "develop") {
		t.Fatalf("project ls must show the recorded branch, got %q (%v)", ls, err)
	}
}

// TestCmdProjectSetBranch: the lead can change the recorded branch to another local branch.
// The command refuses a worker context, a name that is not a branch, and a branch the
// repository does not have, and changes nothing when it refuses.
func TestCmdProjectSetBranch(t *testing.T) {
	repo := branchedRepo(t, "main")
	gitInRepo(t, repo, "branch", "develop")
	gitInRepo(t, repo, "tag", "tagged")
	var id int64
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, err := s.UpsertProject(ctx, repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetProjectDefaultBranch(ctx, p.ID, "main"); err != nil {
			t.Fatal(err)
		}
		id = p.ID
	})
	clearWorkerContext(t)
	t.Setenv("TTORCH_DB", dbPath)
	prev := os.Stdin
	os.Stdin = openInteractiveDevice(t)
	t.Cleanup(func() { os.Stdin = prev })
	idArg := strconv.FormatInt(id, 10)

	t.Setenv("TTORCH_TASK_ID", "w1")
	if err := cmdProjectSetBranch([]string{idArg, "develop"}); err == nil || !strings.Contains(err.Error(), "worker context") {
		t.Fatalf("from a worker context: %v, want the caller-guard refusal", err)
	}
	t.Setenv("TTORCH_TASK_ID", "")
	for _, bad := range []string{"no-such-branch", "tagged", "a..b"} {
		if err := cmdProjectSetBranch([]string{idArg, bad}); err == nil {
			t.Fatalf("set-branch %q must be refused", bad)
		}
	}
	if p, _, _ := reopen(t, dbPath).GetProject(context.Background(), id); p.DefaultBranch != "main" {
		t.Fatalf("a refused set-branch changed the branch to %q", p.DefaultBranch)
	}
	out, err := captureStdout(t, func() error { return cmdProjectSetBranch([]string{repo, "develop"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "default-branch=develop (was main)") {
		t.Fatalf("set-branch output = %q", out)
	}
	if p, _, _ := reopen(t, dbPath).GetProject(context.Background(), id); p.DefaultBranch != "develop" {
		t.Fatalf("branch = %q, want develop", p.DefaultBranch)
	}
}

// TestCmdInitRecordsDefaultBranch: `ttorch init` in a registered repository with no recorded
// branch records the one its checkout is on, and says which branch the gate reads.
func TestCmdInitRecordsDefaultBranch(t *testing.T) {
	repo := branchedRepo(t, "trunk")
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		if _, err := s.UpsertProject(ctx, repo, ""); err != nil {
			t.Fatal(err)
		}
	})
	out, err := captureStdout(t, func() error { return cmdInit([]string{"-mode", "trusted", repo}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "default branch the trust gate reads: trunk") {
		t.Fatalf("init output must name the recorded branch, got:\n%s", out)
	}
	if p, _, _ := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo); p.DefaultBranch != "trunk" {
		t.Fatalf("branch = %q, want trunk", p.DefaultBranch)
	}
}

// TestDefaultBranchNotices: `ttorch update` and `ttorch doctor` show a seeded branch once, so a
// wrong guess is seen, and name every git project with no recorded branch each time, since the
// gate refuses those. A directory that is not a repository root is not named.
func TestDefaultBranchNotices(t *testing.T) {
	seeded := branchedRepo(t, "develop")
	missing := branchedRepo(t, "main")
	gitInRepo(t, missing, "checkout", "-q", "--detach")
	plainDir := t.TempDir()
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		for _, r := range []string{seeded, missing, plainDir} {
			p, err := s.UpsertProject(ctx, r, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, db.DefaultBranchSeedPending); err != nil {
				t.Fatal(err)
			}
		}
	})
	var first bytes.Buffer
	printGateNotices(&first, paths.Default())
	for _, want := range []string{
		"recorded develop as the default branch the trust gate reads for " + seeded,
		"no default branch is recorded for " + missing,
	} {
		if !strings.Contains(first.String(), want) {
			t.Fatalf("first notices missing %q:\n%s", want, first.String())
		}
	}
	if strings.Contains(first.String(), plainDir) {
		t.Fatalf("a directory that is not a repository must not be named:\n%s", first.String())
	}
	var second bytes.Buffer
	printGateNotices(&second, paths.Default())
	if strings.Contains(second.String(), "recorded develop") {
		t.Fatalf("a seeded branch is announced once, got it again:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "no default branch is recorded for "+missing) {
		t.Fatalf("a missing branch is named every time:\n%s", second.String())
	}
}

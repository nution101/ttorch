package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// TestCmdProjectAddRecordsDefaultBranch: registering a project, as the lead at a terminal,
// records the branch its checkout is on as the default branch the trust gate reads, and the URL
// origin resolves to beside it, and says so. Registering it again from another branch, or after
// origin has changed, keeps what was recorded.
func TestCmdProjectAddRecordsDefaultBranch(t *testing.T) {
	repo := branchedRepo(t, "develop")
	gitInRepo(t, repo, "remote", "add", "origin", "https://example.com/team/repo.git")
	dbPath := withSeedDB(t, nil)
	asLeadAtATerminal(t)
	out, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "default-branch=develop") {
		t.Fatalf("project add must print the recorded branch, got %q", out)
	}
	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	gitInRepo(t, repo, "remote", "set-url", "origin", "https://example.com/elsewhere/repo.git")
	if _, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) }); err != nil {
		t.Fatal(err)
	}
	p, ok, err := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo)
	if err != nil || !ok || p.DefaultBranch != "develop" || p.OriginURL != "https://example.com/team/repo.git" || p.DefaultBranchSeed != "" {
		t.Fatalf("project = %+v ok=%v err=%v; want develop and the first origin kept, nothing pending", p, ok, err)
	}
	ls, err := captureStdout(t, func() error { return cmdProjectLs(nil) })
	if err != nil || !strings.Contains(ls, "BRANCH") || !strings.Contains(ls, "develop") {
		t.Fatalf("project ls must show the recorded branch, got %q (%v)", ls, err)
	}
}

// TestCmdProjectSetBranch: the lead can change the recorded branch to another local branch,
// which records the URL origin resolves to now and prints it. The command refuses a worker
// context, a name that is not a branch, and a branch the repository does not have, and changes
// nothing when it refuses.
func TestCmdProjectSetBranch(t *testing.T) {
	repo := branchedRepo(t, "main")
	gitInRepo(t, repo, "remote", "add", "origin", "https://example.com/team/repo.git")
	gitInRepo(t, repo, "branch", "develop")
	gitInRepo(t, repo, "tag", "tagged")
	var id int64
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, err := s.UpsertProject(ctx, repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetProjectDefaultBranch(ctx, p.ID, "main", ""); err != nil {
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
	if !strings.Contains(out, "default-branch=develop (was main) · origin=https://example.com/team/repo.git") {
		t.Fatalf("set-branch output = %q", out)
	}
	if p, _, _ := reopen(t, dbPath).GetProject(context.Background(), id); p.DefaultBranch != "develop" || p.OriginURL != "https://example.com/team/repo.git" {
		t.Fatalf("project = %+v, want develop and the current origin", p)
	}
}

// TestCmdInitRecordsDefaultBranch: `ttorch init`, run by the lead at a terminal in a registered
// repository with no recorded branch, records the one its checkout is on, and says which branch
// the gate reads.
func TestCmdInitRecordsDefaultBranch(t *testing.T) {
	repo := branchedRepo(t, "trunk")
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		if _, err := s.UpsertProject(ctx, repo, ""); err != nil {
			t.Fatal(err)
		}
	})
	asLeadAtATerminal(t)
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

// TestDefaultBranchNotices: `ttorch update` and `ttorch doctor`, run by the lead, show a seeded
// branch once, so a wrong guess is seen, and name every git project with no recorded branch each
// time, since the gate refuses those. A directory that is not a repository root is not named.
func TestDefaultBranchNotices(t *testing.T) {
	clearWorkerContext(t)
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
	printGateNotices(&first, paths.Default(), false)
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
	printGateNotices(&second, paths.Default(), false)
	if strings.Contains(second.String(), "recorded develop") {
		t.Fatalf("a seeded branch is announced once, got it again:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "no default branch is recorded for "+missing) {
		t.Fatalf("a missing branch is named every time:\n%s", second.String())
	}
}

// asLeadAtATerminal puts the test in the lead's context with stdin on a character device, which
// is what the caller guard set-branch uses accepts.
func asLeadAtATerminal(t *testing.T) {
	t.Helper()
	clearWorkerContext(t)
	prev := os.Stdin
	os.Stdin = openInteractiveDevice(t)
	t.Cleanup(func() { os.Stdin = prev })
}

// TestRecordingTheBranchNeedsTheLead: project add and init write the recorded default branch
// only behind the caller guard set-branch uses. From a worker's context, or without a terminal,
// they still register the project and refresh the mode, leave the branch unrecorded, and say why.
func TestRecordingTheBranchNeedsTheLead(t *testing.T) {
	devNull := func(t *testing.T) {
		t.Helper()
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		prev := os.Stdin
		os.Stdin = f
		t.Cleanup(func() { os.Stdin = prev })
	}
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T)
		reason string
	}{
		{"from a worker context", func(t *testing.T) { asLeadAtATerminal(t); t.Setenv("TTORCH_TASK_ID", "w1") }, "worker context"},
		{"without a terminal", func(t *testing.T) { clearWorkerContext(t); devNull(t) }, "interactive terminal"},
	} {
		t.Run("project add "+c.name, func(t *testing.T) {
			repo := branchedRepo(t, "develop")
			dbPath := withSeedDB(t, nil)
			c.setup(t)
			out, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) })
			if err != nil {
				t.Fatalf("project add must still register the project: %v", err)
			}
			if !strings.Contains(out, "default-branch=none") || !strings.Contains(out, c.reason) {
				t.Fatalf("project add must say the branch was not recorded and why (%q), got %q", c.reason, out)
			}
			p, ok, err := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo)
			if err != nil || !ok || p.DefaultBranch != "" {
				t.Fatalf("the project must be registered with no branch: %+v ok=%v err=%v", p, ok, err)
			}
		})
		t.Run("init "+c.name, func(t *testing.T) {
			repo := branchedRepo(t, "trunk")
			dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
				if _, err := s.UpsertProject(ctx, repo, ""); err != nil {
					t.Fatal(err)
				}
			})
			c.setup(t)
			out, err := captureStdout(t, func() error { return cmdInit([]string{"-mode", "trusted", repo}) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "no default branch recorded") || !strings.Contains(out, c.reason) {
				t.Fatalf("init must say the branch was not recorded and why (%q), got:\n%s", c.reason, out)
			}
			if p, _, _ := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo); p.DefaultBranch != "" || p.DeliveryMode != "trusted" {
				t.Fatalf("init must refresh the mode and leave the branch unrecorded: %+v", p)
			}
		})
	}
}

// TestGateNotices_ChangeNothingFromAWorkerContext: `ttorch doctor` and `ttorch update` ran the
// seed and cleared the one-time notice whoever called them, so a worker that ran doctor in its
// own pane recorded a branch from refs it can write, and the notice the lead was meant to see
// was printed there and cleared. From a worker context they still print, and change nothing.
func TestGateNotices_ChangeNothingFromAWorkerContext(t *testing.T) {
	underTaskFile := func(t *testing.T) {
		wt := t.TempDir()
		if err := os.MkdirAll(filepath.Join(wt, ".ttorch"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".ttorch", "task"), []byte("task_id=w1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(wt)
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"TTORCH_TASK_ID", func(t *testing.T) { t.Setenv("TTORCH_TASK_ID", "w1") }},
		{"task file", underTaskFile},
	} {
		t.Run(c.name, func(t *testing.T) {
			pending := branchedRepo(t, "develop")
			noticed := branchedRepo(t, "trunk")
			dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
				p, err := s.UpsertProject(ctx, pending, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, db.DefaultBranchSeedPending); err != nil {
					t.Fatal(err)
				}
				n, err := s.UpsertProject(ctx, noticed, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.FillProjectDefaultBranch(ctx, n.ID, "trunk", "", true); err != nil {
					t.Fatal(err)
				}
			})
			clearWorkerContext(t)
			c.setup(t)
			var out bytes.Buffer
			printGateNotices(&out, paths.Default(), false)
			if !strings.Contains(out.String(), "recorded trunk as the default branch the trust gate reads for "+noticed) {
				t.Fatalf("the notice must still be printed:\n%s", out.String())
			}
			s := reopen(t, dbPath)
			if p, _, _ := s.GetProjectByRepo(context.Background(), pending); p.DefaultBranch != "" || p.DefaultBranchSeed != db.DefaultBranchSeedPending {
				t.Fatalf("the seed ran: %+v, want the row still pending with no branch", p)
			}
			if p, _, _ := s.GetProjectByRepo(context.Background(), noticed); p.DefaultBranchSeed != db.DefaultBranchSeedNotice {
				t.Fatalf("the notice was cleared: %+v", p)
			}
		})
	}
}

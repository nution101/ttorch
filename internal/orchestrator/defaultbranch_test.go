package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func projectByRepo(t *testing.T, s *db.Store, repo string) db.Project {
	t.Helper()
	p, ok, err := s.GetProjectByRepo(context.Background(), repo)
	if err != nil || !ok {
		t.Fatalf("GetProjectByRepo(%s): ok=%v err=%v", repo, ok, err)
	}
	return p
}

// TestSeedDefaultBranches: a project migration 0010 left awaiting a seed gets the branch its
// checkout detects, marked for the one-time notice; one whose branch cannot be detected is
// left with none and not tried again; and a project with nothing pending is not touched, even
// when its checkout has moved to another branch since.
func TestSeedDefaultBranches(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	pending := newRepoMain(t)
	gitIn(t, pending, "checkout", "-q", "-b", "develop")
	detached := newRepoMain(t)
	gitIn(t, detached, "checkout", "-q", "--detach")
	notARepo := t.TempDir()
	settled := newRepoMain(t)
	for _, r := range []string{pending, detached, notARepo} {
		p, err := s.UpsertProject(ctx, r, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, db.DefaultBranchSeedPending); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := s.UpsertProject(ctx, settled, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefaultBranch(ctx, sp.ID, "main"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, settled, "checkout", "-q", "-b", "elsewhere")

	if err := SeedDefaultBranches(ctx, s); err != nil {
		t.Fatal(err)
	}
	if p := projectByRepo(t, s, pending); p.DefaultBranch != "develop" || p.DefaultBranchSeed != db.DefaultBranchSeedNotice {
		t.Fatalf("pending project: %+v, want develop with a notice", p)
	}
	for _, r := range []string{detached, notARepo} {
		if p := projectByRepo(t, s, r); p.DefaultBranch != "" || p.DefaultBranchSeed != "" {
			t.Fatalf("undetectable project %s: %+v, want no branch and nothing pending", r, p)
		}
	}
	if p := projectByRepo(t, s, settled); p.DefaultBranch != "main" || p.DefaultBranchSeed != "" {
		t.Fatalf("a recorded branch must not be reseeded: %+v", p)
	}

	// A second run changes nothing: the undetectable rows were tried once.
	gitIn(t, detached, "checkout", "-q", "-b", "now-on-a-branch")
	if err := SeedDefaultBranches(ctx, s); err != nil {
		t.Fatal(err)
	}
	if p := projectByRepo(t, s, detached); p.DefaultBranch != "" {
		t.Fatalf("a seed must be tried once, got %+v", p)
	}
}

// TestRegisterDefaultBranch: spawn records the branch for a repository with no project row, or
// one with a row and no worker checkout yet. Once a worker has had a checkout it records
// nothing, since the refs the detection reads are ones that worker can have changed; and it
// never replaces a recorded branch.
func TestRegisterDefaultBranch(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	m := &Manager{Store: s}

	fresh := newRepoMain(t)
	m.registerDefaultBranch(ctx, fresh)
	if p := projectByRepo(t, s, fresh); p.DefaultBranch != "main" || p.DefaultBranchSeed != db.DefaultBranchSeedNotice {
		t.Fatalf("a new project must get its checkout's branch with a notice: %+v", p)
	}

	// A recorded branch stays, whatever the checkout is on now.
	gitIn(t, fresh, "checkout", "-q", "-b", "feature")
	m.registerDefaultBranch(ctx, fresh)
	if p := projectByRepo(t, s, fresh); p.DefaultBranch != "main" {
		t.Fatalf("a recorded branch must not move: %+v", p)
	}

	// A row with a worker's checkout and no branch is left for the lead.
	withWorker := newRepoMain(t)
	p, err := s.UpsertProject(ctx, withWorker, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "w1", ProjectID: p.ID, Worktree: t.TempDir(), Status: db.StatusActive, Kind: db.KindShip}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	m.registerDefaultBranch(ctx, withWorker)
	if p := projectByRepo(t, s, withWorker); p.DefaultBranch != "" {
		t.Fatalf("with a worker checkout present nothing may be recorded: %+v", p)
	}

	// A row that only a cc session created (no worker) is recorded.
	ccOnly := newRepoMain(t)
	cp, err := s.UpsertProject(ctx, ccOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "cc1", ProjectID: cp.ID, Worktree: ccOnly, Status: db.StatusActive, Kind: db.KindCC}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	m.registerDefaultBranch(ctx, ccOnly)
	if p := projectByRepo(t, s, ccOnly); p.DefaultBranch != "main" {
		t.Fatalf("a cc session is the lead's, so its row must be recorded: %+v", p)
	}
}

// TestSeedDefaultBranches_KeepsTheNoticeWhenRegistrationWins: the seed reads the project list,
// detects, then writes only if no branch is recorded. A spawn can record the branch in between,
// marking it for the one-time notice. The seed then loses the write and must leave that notice
// in place rather than clear the row's seed state, or the lead is never shown the branch.
func TestSeedDefaultBranches_KeepsTheNoticeWhenRegistrationWins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	m := &Manager{Store: s}
	repo := newRepoMain(t)
	p, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, db.DefaultBranchSeedPending); err != nil {
		t.Fatal(err)
	}
	prev := seedDetect
	t.Cleanup(func() { seedDetect = prev })
	seedDetect = func(r string) (string, error) {
		m.registerDefaultBranch(ctx, repo) // the spawn that wins the race
		return prev(r)
	}
	if err := SeedDefaultBranches(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := projectByRepo(t, s, repo); got.DefaultBranch != "main" || got.DefaultBranchSeed != db.DefaultBranchSeedNotice {
		t.Fatalf("after losing the race the seed must leave the registration's notice: %+v", got)
	}
}

// TestNew_DoesNotSeedFromAWorkerContext: every ttorch command a worker runs (report, status)
// opens a Manager, and New ran the seed, which records whatever branch the refs a worker can
// write point at. From a worker context New now leaves a pending row pending for the lead's
// next run. The pending row's checkout is on develop so a seed, had it run, would be visible.
func TestNew_DoesNotSeedFromAWorkerContext(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TTORCH_HOME", t.TempDir())
	repo := newRepoMain(t)
	gitIn(t, repo, "checkout", "-q", "-b", "develop")
	s, err := db.Open(paths.Default().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, db.DefaultBranchSeedPending); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	t.Setenv("TTORCH_TASK_ID", "w1")
	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := projectByRepo(t, m.Store, repo)
	_ = m.Close()
	if got.DefaultBranch != "" || got.DefaultBranchSeed != db.DefaultBranchSeedPending {
		t.Fatalf("New from a worker context seeded the row: %+v, want it still pending with no branch", got)
	}

	t.Setenv("TTORCH_TASK_ID", "")
	t.Chdir(t.TempDir())
	m, err = New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if got := projectByRepo(t, m.Store, repo); got.DefaultBranch != "develop" || got.DefaultBranchSeed != db.DefaultBranchSeedNotice {
		t.Fatalf("New from the lead's context must seed: %+v", got)
	}
}

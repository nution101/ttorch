package db

import (
	"context"
	"testing"
)

// TestMigration0010MarksExistingProjectsForSeeding proves the migration marks every project
// that existed before it as awaiting a seed, with no branch recorded, while a project created
// afterwards starts with nothing pending: its branch is the registration's to record.
func TestMigration0010MarksExistingProjectsForSeeding(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.MigrateDown(ctx, 9); err != nil {
		t.Fatalf("MigrateDown(9): %v", err)
	}
	// At version 9 the projects table has no default_branch column, so UpsertProject (which
	// returns the full column list) cannot run; insert the pre-0010 row by hand.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (repo_path, name, created_at, updated_at) VALUES ('/old', 'old', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	old, ok, err := s.GetProjectByRepo(ctx, "/old")
	if err != nil || !ok {
		t.Fatalf("GetProjectByRepo(/old): ok=%v err=%v", ok, err)
	}
	if old.DefaultBranch != "" || old.DefaultBranchSeed != DefaultBranchSeedPending || old.LastLandedSHA != "" || old.OriginURL != "" {
		t.Fatalf("a project from before 0010 must await a seed with no branch: %+v", old)
	}
	fresh, err := s.UpsertProject(ctx, "/new", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DefaultBranch != "" || fresh.DefaultBranchSeed != "" {
		t.Fatalf("a project created after 0010 must start with nothing pending: %+v", fresh)
	}
	if !tableExists(t, s, "review_preps") {
		t.Fatal("0010 must create review_preps")
	}
	if err := s.MigrateDown(ctx, 9); err != nil {
		t.Fatalf("MigrateDown(9) after up: %v", err)
	}
	if tableExists(t, s, "review_preps") {
		t.Fatal("0010 down must drop review_preps")
	}
}

// TestFillProjectDefaultBranch proves a fill writes only while no branch is recorded, so
// registering or seeding a project again can never move the branch the gate reads or the origin
// URL recorded with it, and that the notice flag is set only when asked for.
func TestFillProjectDefaultBranch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, err := s.UpsertProject(ctx, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := s.FillProjectDefaultBranch(ctx, p.ID, "develop", "https://example.com/a.git", true)
	if err != nil || !wrote {
		t.Fatalf("first fill: wrote=%v err=%v", wrote, err)
	}
	wrote, err = s.FillProjectDefaultBranch(ctx, p.ID, "main", "https://example.com/b.git", false)
	if err != nil || wrote {
		t.Fatalf("a second fill must not replace a recorded branch: wrote=%v err=%v", wrote, err)
	}
	got, _, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultBranch != "develop" || got.OriginURL != "https://example.com/a.git" || got.DefaultBranchSeed != DefaultBranchSeedNotice {
		t.Fatalf("got %+v, want develop and the first origin with a pending notice", got)
	}

	// The lead's set replaces it and clears the notice.
	if err := s.SetProjectDefaultBranch(ctx, p.ID, "trunk", ""); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetProject(ctx, p.ID)
	if got.DefaultBranch != "trunk" || got.OriginURL != "" || got.DefaultBranchSeed != "" {
		t.Fatalf("after set: %+v, want trunk and no origin with no notice", got)
	}
	if err := s.SetProjectDefaultBranch(ctx, 9999, "x", ""); err == nil {
		t.Fatal("setting the branch of a missing project must fail")
	}

	// A fill without a notice leaves nothing pending.
	q, err := s.UpsertProject(ctx, "/other", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FillProjectDefaultBranch(ctx, q.ID, "main", "", false); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetProject(ctx, q.ID)
	if got.DefaultBranch != "main" || got.DefaultBranchSeed != "" {
		t.Fatalf("fill without notice: %+v", got)
	}
	if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, DefaultBranchSeedPending); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetProject(ctx, p.ID)
	if got.DefaultBranchSeed != DefaultBranchSeedPending {
		t.Fatalf("seed state = %q, want pending", got.DefaultBranchSeed)
	}
}

func TestSetProjectLastLanded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.UpsertProject(ctx, "/repo", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetProjectLastLanded(ctx, "/repo", "abc"); err != nil || !ok {
		t.Fatalf("SetProjectLastLanded: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetProjectLastLanded(ctx, "/unregistered", "abc"); err != nil || ok {
		t.Fatalf("an unregistered repo must no-op: ok=%v err=%v", ok, err)
	}
	p, _, _ := s.GetProjectByRepo(ctx, "/repo")
	if p.LastLandedSHA != "abc" {
		t.Fatalf("LastLandedSHA = %q, want abc", p.LastLandedSHA)
	}
}

func TestReviewPrepRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkTaskForVerdict(t, s, "rp1")
	if _, ok, err := s.GetReviewPrep(ctx, "rp1"); err != nil || ok {
		t.Fatalf("no prep yet: ok=%v err=%v", ok, err)
	}
	if err := s.SaveReviewPrep(ctx, "rp1", "head1", "base1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReviewPrep(ctx, "rp1", "head2", "base2"); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.GetReviewPrep(ctx, "rp1")
	if err != nil || !ok {
		t.Fatalf("GetReviewPrep: ok=%v err=%v", ok, err)
	}
	if p.Head != "head2" || p.BaseSHA != "base2" {
		t.Fatalf("a later prep must replace the earlier one: %+v", p)
	}
}

func TestVerdictBaseSHARoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mkTaskForVerdict(t, s, "vb1")
	if err := s.SaveVerdict(ctx, Verdict{TaskID: "vb1", Overall: "pass", ReviewedSHA: "h", BaseSHA: "b"}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetVerdict(ctx, "vb1")
	if err != nil || !ok || v.BaseSHA != "b" {
		t.Fatalf("BaseSHA round trip: %+v ok=%v err=%v", v, ok, err)
	}
}

// TestClearPendingDefaultBranchSeed clears only a pending seed, never a notice.
func TestClearPendingDefaultBranchSeed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, err := s.UpsertProject(ctx, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ from, want string }{
		{DefaultBranchSeedPending, ""},
		{DefaultBranchSeedNotice, DefaultBranchSeedNotice},
		{"", ""},
	} {
		if err := s.SetProjectDefaultBranchSeed(ctx, p.ID, c.from); err != nil {
			t.Fatal(err)
		}
		if err := s.ClearPendingDefaultBranchSeed(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.GetProject(ctx, p.ID); got.DefaultBranchSeed != c.want {
			t.Fatalf("from %q: seed = %q, want %q", c.from, got.DefaultBranchSeed, c.want)
		}
	}
}

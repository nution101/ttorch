package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/review"
)

// originAheadRepo gives repo an origin whose <default> carries a "release" commit (file
// CHANGELOG.md) that the LOCAL default branch never pulls, reproducing the field bug:
// origin/<default> is ahead, the local default branch is behind. A worker subsequently
// spawned off this repo is cut from origin/<default> (so the release is part of its history,
// not a change it authored). Returns the path of the release-only file so callers can assert
// whether it is wrongly attributed to the worker.
func originAheadRepo(t *testing.T, repo string) string {
	t.Helper()
	c0 := gitIn(t, repo, "rev-parse", "HEAD")
	bare := t.TempDir()
	gitIn(t, bare, "init", "--bare", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", bare)
	gitIn(t, repo, "push", "-q", "origin", "main") // origin == local default == C0
	// A release lands on origin (e.g. a CHANGELOG bump) that the local default never pulls.
	if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte("## v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "CHANGELOG.md")
	gitIn(t, repo, "commit", "-q", "-m", "release v1.0.0")
	gitIn(t, repo, "push", "-q", "origin", "main") // origin advances past the local default
	gitIn(t, repo, "reset", "--hard", "-q", c0)    // local default falls behind origin
	gitIn(t, repo, "fetch", "-q", "origin")        // origin/main tracking ref is now ahead of local main
	return "CHANGELOG.md"
}

// TestTrustPrep_StaleLocalMainDiffsAgainstOriginBase is the headline of this fix. When the
// LOCAL default branch is behind origin (a release merged on origin but never pulled
// locally) and the worker was cut from origin/<default>, the release is already part of the
// worker's branch history. Diffing against the stale local default would root the three-dot
// diff before the release and surface it as the worker's OWN change — phantom scope-creep
// that wastes a re-gate. Prep must diff against the TRUE base (origin/<default>), so the
// staged diff.patch contains ONLY the worker's change.
func TestTrustPrep_StaleLocalMainDiffsAgainstOriginBase(t *testing.T) {
	m, repo := deliveryHarness(t, "prepstale")
	releaseFile := originAheadRepo(t, repo)

	task, err := m.Spawn("ps1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker was cut from origin/<default>, so the release is already part of its history
	// — the exact field condition (a stale local default behind origin).
	if _, err := os.Stat(filepath.Join(wt, releaseFile)); err != nil {
		t.Fatalf("worker should be based on origin/<default> carrying %s: %v", releaseFile, err)
	}
	commitFeature(t, wt, "feature.txt", "worker change\n")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	dir, err := m.TrustPrep("ps1")
	if err != nil {
		// The branch IS current with origin (only the LOCAL default is behind), so the
		// stale-base guard must not fire.
		t.Fatalf("prep must succeed when the branch is current with origin: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "feature.txt") {
		t.Fatalf("diff.patch must contain the worker's own change:\n%s", patch)
	}
	if strings.Contains(string(patch), releaseFile) {
		t.Fatalf("diff.patch must NOT attribute the already-on-origin release (%s) to the worker — phantom scope-creep:\n%s", releaseFile, patch)
	}
	// Decisive: the staged diff is exactly the three-dot diff against the TRUE base
	// (origin/<default>), and is NOT the diff against the stale local default (which would
	// include the release).
	originDiff, err := mergeBaseDiff(wt, "origin/main", head)
	if err != nil {
		t.Fatal(err)
	}
	localDiff, err := mergeBaseDiff(wt, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if string(patch) != originDiff {
		t.Fatalf("diff.patch must be the three-dot diff against origin/main (the true base)\n got: %q\nwant: %q", patch, originDiff)
	}
	if string(patch) == localDiff {
		t.Fatal("diff.patch must NOT be the three-dot diff against the stale local main (it would include the release)")
	}
	_, _ = m.Teardown("ps1", true)
}

// TestTrustPrep_OriginInSyncDiffUnchanged guards the normal case: with origin present and
// the local default in sync with it, prep stages exactly the worker's own three-dot diff,
// unchanged by the true-base resolution.
func TestTrustPrep_OriginInSyncDiffUnchanged(t *testing.T) {
	m, repo := deliveryHarness(t, "prepsync")
	bare := t.TempDir()
	gitIn(t, bare, "init", "--bare", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", bare)
	gitIn(t, repo, "push", "-q", "origin", "main") // origin == local default
	gitIn(t, repo, "fetch", "-q", "origin")

	task, err := m.Spawn("pi1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	commitFeature(t, wt, "feature.txt", "worker change\n")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	dir, err := m.TrustPrep("pi1")
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "feature.txt") {
		t.Fatalf("diff.patch must contain the worker's change:\n%s", patch)
	}
	want, err := mergeBaseDiff(wt, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if string(patch) != want {
		t.Fatalf("in-sync diff.patch must be the worker's own three-dot diff, unchanged\n got: %q\nwant: %q", patch, want)
	}
	_, _ = m.Teardown("pi1", true)
}

// TestTrustRecord_StaleLocalMainDiffIDPinsTrueBase locks the verdict's content identity to
// the TRUE base too. The DiffID is recomputed by carryVerdictForward against the LAND base
// (origin/<default>) at merge time, so recording it against a stale local default would
// fingerprint phantom commits and force a needless re-gate of an otherwise unchanged diff.
// It must fingerprint the worker's diff against origin/<default>, matching what prep staged.
func TestTrustRecord_StaleLocalMainDiffIDPinsTrueBase(t *testing.T) {
	m, repo := deliveryHarness(t, "recstale")
	originAheadRepo(t, repo)

	task, err := m.Spawn("rs1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "feature.txt", "worker change\n")
	writeReviewReports(t, m.P.ReviewInputsDir("rs1"), feat, nil)
	if _, err := m.TrustRecord("rs1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	v, ok := m.TrustShow("rs1")
	if !ok || v.DiffID == "" {
		t.Fatalf("recorded verdict should pin a diff identity: %+v ok=%v", v, ok)
	}
	originDiff, err := mergeBaseDiff(wt, "origin/main", feat)
	if err != nil {
		t.Fatal(err)
	}
	localDiff, err := mergeBaseDiff(wt, "main", feat)
	if err != nil {
		t.Fatal(err)
	}
	if v.DiffID != review.DiffID([]byte(originDiff)) {
		t.Fatal("verdict DiffID must fingerprint the worker's diff against the TRUE base (origin/main)")
	}
	if v.DiffID == review.DiffID([]byte(localDiff)) {
		t.Fatal("verdict DiffID must NOT fingerprint the phantom diff against the stale local main")
	}
	_, _ = m.Teardown("rs1", true)
}

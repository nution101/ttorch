package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/worktree"
)

// --- PART A: teardown refuses committed-but-unmerged work (data-loss guard) ---------

// TestTeardown_RefusesCommittedUnmergedWork is the headline data-loss fix: a worker that
// committed on its branch but never merged has a CLEAN worktree, yet tearing it down
// would delete the branch and discard the commits. Teardown must refuse without --force.
func TestTeardown_RefusesCommittedUnmergedWork(t *testing.T) {
	m, repo := deliveryHarness(t, "tdunmerged")
	task, err := m.Spawn("um1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	// Commit on the task branch and leave the worktree clean — the exact shape that the
	// old uncommitted-only guard waved through.
	commitFeature(t, task.Worktree, "feature.txt", "work\n")
	if dirty, err := worktree.IsDirty(task.Worktree); err != nil || dirty {
		t.Fatalf("the worktree must be clean after committing (dirty=%v err=%v)", dirty, err)
	}

	_, err = m.Teardown("um1", false)
	if err == nil {
		t.Fatal("teardown must refuse a branch with committed-but-unmerged work")
	}
	if !strings.Contains(err.Error(), "not merged into") {
		t.Fatalf("refusal must name the unmerged work, got: %v", err)
	}
	// The refusal has no side effects: the worker is still live.
	if !m.Live(task) {
		t.Fatal("a refused teardown must leave the worker running")
	}

	// --force overrides.
	if _, err := m.Teardown("um1", true); err != nil {
		t.Fatalf("forced teardown of unmerged work must succeed: %v", err)
	}
}

// TestTeardown_AllowsMergedBranch confirms the guard only blocks UNMERGED work: once the
// branch is an ancestor of the default branch (its commits have landed), a no-force
// teardown is allowed again.
func TestTeardown_AllowsMergedBranch(t *testing.T) {
	m, repo := deliveryHarness(t, "tdmerged")
	task, err := m.Spawn("mg1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	tip := commitFeature(t, task.Worktree, "feature.txt", "work\n")
	// Land it: fast-forward the repo's default branch to the worker's commit.
	gitIn(t, repo, "merge", "--ff-only", tip)

	if _, err := m.Teardown("mg1", false); err != nil {
		t.Fatalf("teardown of merged work must not require --force: %v", err)
	}
}

// TestTeardown_ForceLeavesRecoveryRef is the defense-in-depth: a forced teardown of
// committed-but-unmerged work stashes it under refs/ttorch/discarded/<id>-<sha> so the
// commits stay recoverable instead of being lost outright.
func TestTeardown_ForceLeavesRecoveryRef(t *testing.T) {
	m, repo := deliveryHarness(t, "tdrecover")
	task, err := m.Spawn("rc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	tip := commitFeature(t, task.Worktree, "feature.txt", "work\n")

	notes, err := m.Teardown("rc1", true)
	if err != nil {
		t.Fatalf("forced teardown: %v", err)
	}

	ref := "refs/ttorch/discarded/rc1-" + short(tip)
	if got := gitIn(t, repo, "rev-parse", ref); got != tip {
		t.Fatalf("recovery ref %s = %q, want the discarded commit %q", ref, got, tip)
	}
	// The branch itself is gone (Release deleted it), so the recovery ref is the only
	// thing keeping the work reachable.
	if worktree.RefExists(repo, taskBranch("rc1")) {
		t.Fatal("teardown should have deleted the task branch")
	}
	foundNote := false
	for _, n := range notes {
		if strings.Contains(n, ref) {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatalf("teardown should report the recovery ref in its notes, got %v", notes)
	}
}

// TestTeardown_ForceMergedBranchSavesNoRecoveryRef is the inverse of the recovery-ref
// case: when the branch's work has already landed on the default branch, a forced
// teardown must NOT stash a (redundant) recovery ref or emit a recovery note — the
// commits already live on a base ref.
func TestTeardown_ForceMergedBranchSavesNoRecoveryRef(t *testing.T) {
	m, repo := deliveryHarness(t, "tdmergeforce")
	task, err := m.Spawn("mf1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	tip := commitFeature(t, task.Worktree, "feature.txt", "work\n")
	gitIn(t, repo, "merge", "--ff-only", tip) // land it onto the default branch

	notes, err := m.Teardown("mf1", true)
	if err != nil {
		t.Fatalf("forced teardown: %v", err)
	}
	if refs := gitIn(t, repo, "for-each-ref", "refs/ttorch/discarded/"); refs != "" {
		t.Fatalf("a merged branch must not leave a recovery ref, got: %s", refs)
	}
	for _, n := range notes {
		if strings.Contains(n, "discarded") || strings.Contains(n, "recover") {
			t.Errorf("merged teardown must not emit a recovery note, got %q", n)
		}
	}
}

// --- PART B: spawn launches with the manager's brief, not the generic stub ------------

// TestWriteBrief_WritesBriefPath confirms WriteBrief stores the brief at the path Spawn
// reads, creating the data dir.
func TestWriteBrief_WritesBriefPath(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}
	const body = "# Real brief\n\nDo the thing.\n"
	if err := m.WriteBrief("wb1", body); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(m.P.BriefPath("wb1"))
	if err != nil {
		t.Fatalf("brief not written to BriefPath: %v", err)
	}
	if string(got) != body {
		t.Fatalf("brief = %q, want %q", got, body)
	}
}

// TestBriefForLaunch_KeepsManagerBrief is the core PART B behavior: when a manager brief
// was written before the spawn, the launch uses it verbatim and never overwrites it with
// the generic stub.
func TestBriefForLaunch_KeepsManagerBrief(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}
	const body = "# Real brief\n\nImplement parts A and B.\n"
	if err := m.WriteBrief("bf1", body); err != nil {
		t.Fatal(err)
	}
	path := m.briefForLaunch("bf1", "ship")
	if path != m.P.BriefPath("bf1") {
		t.Fatalf("briefForLaunch path = %q, want %q", path, m.P.BriefPath("bf1"))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("the manager brief was clobbered: got %q, want %q", got, body)
	}
}

// TestBriefForLaunch_WritesStubWhenAbsent confirms the fallback is preserved: with no
// brief written, the launch gets the generic stub.
func TestBriefForLaunch_WritesStubWhenAbsent(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}
	path := m.briefForLaunch("st1", "ship")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub not written: %v", err)
	}
	if !strings.Contains(string(got), "Your full task brief is on its way") {
		t.Fatalf("expected the generic stub, got: %s", got)
	}
}

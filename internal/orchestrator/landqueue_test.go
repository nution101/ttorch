package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
)

// readyTrustedTask spawns a worker in a trusted repo, commits one disjoint file, runs a clean
// review, and records the verdict — leaving the task done+gated with an auto-minted approval,
// exactly the state a task is in when the manager goes to land it.
func readyTrustedTask(t *testing.T, m *Manager, repo, id, file, content string) db.Task {
	t.Helper()
	task, err := m.Spawn(id, repo, false, "sleep 60")
	if err != nil {
		t.Fatalf("spawn %s: %v", id, err)
	}
	head := commitFeature(t, task.Worktree, file, content)
	writeReviewReports(t, m.P.ReviewInputsDir(id), head, nil) // clean → pass
	if _, err := m.TrustRecord(id, "", time.Minute); err != nil {
		t.Fatalf("trust record %s: %v", id, err)
	}
	if !approval.Valid(m.P.ApprovalFile(id)) {
		t.Fatalf("trusted pass + green validate should auto-mint an approval for %s", id)
	}
	return task
}

func revCount(t *testing.T, repo, ref string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(gitIn(t, repo, "rev-list", "--count", ref)))
	if err != nil {
		t.Fatalf("rev-list --count %s: %v", ref, err)
	}
	return n
}

// TestLandSet_ConcurrentDisjointTasksAllLand is the headline behavior: a batch of done, gated
// tasks in ONE repo whose footprints are disjoint all land in a single LandSet call. Only one
// can fast-forward the default at a time (git), so the rest lose the race, re-rebase onto the
// advanced tip — clean, because the files are disjoint — carry their verdicts forward with no
// re-review, and land. The end state is a linear history with every task's file present: no lost
// update, no merge commit, every gate honored.
func TestLandSet_ConcurrentDisjointTasksAllLand(t *testing.T) {
	m, repo := deliveryHarness(t, "lsdisjoint")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"la1": "a.txt", "la2": "b.txt", "la3": "c.txt"}
	ids := []string{"la1", "la2", "la3"}
	for _, id := range ids {
		readyTrustedTask(t, m, repo, id, files[id], id+"\n")
	}
	before := revCount(t, repo, "main")

	results := m.LandSet(context.Background(), ids, false)

	if len(results) != len(ids) {
		t.Fatalf("expected %d results, got %d", len(ids), len(results))
	}
	for i, r := range results {
		if r.TaskID != ids[i] {
			t.Fatalf("result %d is for %q, want %q (order must match input)", i, r.TaskID, ids[i])
		}
		if r.Err != nil {
			t.Fatalf("task %q should have landed, got error: %v", r.TaskID, r.Err)
		}
		if !strings.Contains(r.Output, "landed "+r.TaskID) || !strings.Contains(r.Output, "verified") {
			t.Fatalf("task %q has an unexpected land summary: %q", r.TaskID, r.Output)
		}
		if r.Attempts < 1 {
			t.Fatalf("task %q reports %d attempts; want at least 1", r.TaskID, r.Attempts)
		}
	}
	// Every disjoint file landed on the default branch, as a linear fast-forward chain (one new
	// commit per task, no merge commits) — proof the batch serialized only the fast-forward.
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should be present on the landed default branch: %v", f, err)
		}
	}
	if after := revCount(t, repo, "main"); after != before+len(ids) {
		t.Fatalf("default branch should have advanced by %d linear commits, got %d→%d", len(ids), before, after)
	}
	// Each task's gate tokens were consumed by its merge.
	for _, id := range ids {
		if _, ok := m.TrustShow(id); ok {
			t.Fatalf("the verdict for %q must be consumed by its merge", id)
		}
		if approval.Valid(m.P.ApprovalFile(id)) {
			t.Fatalf("the approval for %q must be consumed by its merge", id)
		}
		tk, ok, _ := m.Store.GetTask(context.Background(), id)
		if !ok || tk.Status != db.StatusDelivered {
			t.Fatalf("task %q should be delivered after landing, got %q (ok=%v)", id, tk.Status, ok)
		}
	}
	for _, id := range ids {
		_, _ = m.Teardown(id, true)
	}
}

// TestLandSet_CrossRepoConcurrency confirms tasks in DIFFERENT repos are fully independent:
// they share no fast-forward lock, so both land from one LandSet call with no cross-repo
// contention, each onto its own default branch.
func TestLandSet_CrossRepoConcurrency(t *testing.T) {
	m, repoA := deliveryHarness(t, "lscrossA")
	commitGateScript(t, repoA, "exit 0")
	if _, err := projectinit.Init(repoA, "trusted"); err != nil {
		t.Fatal(err)
	}
	repoB := newRepoMain(t)
	commitGateScript(t, repoB, "exit 0")
	if _, err := projectinit.Init(repoB, "trusted"); err != nil {
		t.Fatal(err)
	}
	readyTrustedTask(t, m, repoA, "ca1", "a.txt", "a\n")
	readyTrustedTask(t, m, repoB, "cb1", "b.txt", "b\n")

	results := m.LandSet(context.Background(), []string{"ca1", "cb1"}, false)
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("cross-repo task %q should have landed, got: %v", r.TaskID, r.Err)
		}
	}
	if _, err := os.Stat(filepath.Join(repoA, "a.txt")); err != nil {
		t.Fatalf("repoA default should carry a.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoB, "b.txt")); err != nil {
		t.Fatalf("repoB default should carry b.txt: %v", err)
	}
	_, _ = m.Teardown("ca1", true)
	_, _ = m.Teardown("cb1", true)
}

// TestLandSet_SamePackageTasksSerialize is the safety counterpart: two tasks that edit the SAME
// file cannot both fast-forward a linear history. Whichever wins the race lands; the other's
// re-rebase onto that change conflicts (real overlap), so it fails with a loud re-gate/conflict
// error rather than silently dropping or double-applying a change. Exactly one lands, and the
// default carries that one's content — overlapping work serializes, by design.
func TestLandSet_SamePackageTasksSerialize(t *testing.T) {
	m, repo := deliveryHarness(t, "lssame")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	// Seed the contended file on the default branch so both workers edit a common base.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "shared.txt")
	gitIn(t, repo, "commit", "-q", "-m", "seed shared.txt")

	readyTrustedTask(t, m, repo, "sp1", "shared.txt", "sp1 wins\n")
	readyTrustedTask(t, m, repo, "sp2", "shared.txt", "sp2 wins\n")

	results := m.LandSet(context.Background(), []string{"sp1", "sp2"}, false)

	landed, failed := 0, 0
	var winnerContent string
	for _, r := range results {
		if r.Err == nil {
			landed++
			winnerContent = map[string]string{"sp1": "sp1 wins\n", "sp2": "sp2 wins\n"}[r.TaskID]
		} else {
			failed++
			// The loser fails on its re-rebase onto the winner's change — a real overlap.
			if !strings.Contains(r.Err.Error(), "land:") {
				t.Fatalf("the serialized task %q should fail with a land error, got: %v", r.TaskID, r.Err)
			}
			// That genuine rebase conflict must carry the ErrLandRebaseConflict sentinel (so the
			// autonomous land pass can recognise it and surface a land_rebase_conflict event for the
			// manager), while still containing the detailed "land:" hand-resolution guidance above.
			if !errors.Is(r.Err, ErrLandRebaseConflict) {
				t.Fatalf("the serialized task %q's conflict must wrap ErrLandRebaseConflict, got: %v", r.TaskID, r.Err)
			}
		}
	}
	if landed != 1 || failed != 1 {
		t.Fatalf("exactly one same-file task must land and one must serialize-out; got landed=%d failed=%d", landed, failed)
	}
	got, err := os.ReadFile(filepath.Join(repo, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != winnerContent {
		t.Fatalf("the default's shared.txt should be the winner's content %q, got %q", winnerContent, string(got))
	}
	_, _ = m.Teardown("sp1", true)
	_, _ = m.Teardown("sp2", true)
}

// TestLandSet_UnknownTaskReportedInOrder confirms a bad task id is reported as a failed result
// in its input position without derailing the rest of the batch.
func TestLandSet_UnknownTaskReportedInOrder(t *testing.T) {
	m, repo := deliveryHarness(t, "lsunknown")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	readyTrustedTask(t, m, repo, "ok1", "a.txt", "a\n")

	results := m.LandSet(context.Background(), []string{"nope", "ok1"}, false)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].TaskID != "nope" || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "unknown task") {
		t.Fatalf("first result should report the unknown task: %+v", results[0])
	}
	if results[1].TaskID != "ok1" || results[1].Err != nil {
		t.Fatalf("the valid task should still land: %+v", results[1])
	}
	_, _ = m.Teardown("ok1", true)
}

// TestLandBase_PrefersFastForwardTarget locks landBase's base selection: no origin → the local
// default; origin present but the local default AHEAD of it (an unpushed local fast-forward, the
// concurrent-landing case) → the local default the merge actually advances, never the stale
// origin tip; origin equal-or-ahead → origin/<def>, the authoritative tip.
func TestLandBase_PrefersFastForwardTarget(t *testing.T) {
	// No origin: base is the local default. (newRepoMain skips if git is absent.)
	repo := newRepoMain(t)
	ref, sha, err := landBase(repo, "main", false)
	if err != nil {
		t.Fatal(err)
	}
	localSha := gitIn(t, repo, "rev-parse", "main")
	if ref != "main" || sha != localSha {
		t.Fatalf("no-origin base should be the local default %s/%s, got %s/%s", "main", short(localSha), ref, short(sha))
	}

	// Give it an origin and sync, then advance the LOCAL default past origin (an unpushed local
	// fast-forward): landBase must rebase onto the local default, not the stale origin tip.
	bare := t.TempDir()
	gitIn(t, bare, "init", "--bare", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", bare)
	gitIn(t, repo, "push", "-q", "origin", "main")
	gitIn(t, repo, "fetch", "-q", "origin")
	if err := os.WriteFile(filepath.Join(repo, "local-only.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "local-only commit (not pushed)")
	localAhead := gitIn(t, repo, "rev-parse", "main")
	ref, sha, err = landBase(repo, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "main" || sha != localAhead {
		t.Fatalf("with the local default ahead of origin, base must be the local default %s, got %s/%s", short(localAhead), ref, short(sha))
	}

	// Push so origin catches up: now origin equals local, so the authoritative origin/<def> wins.
	gitIn(t, repo, "push", "-q", "origin", "main")
	gitIn(t, repo, "fetch", "-q", "origin")
	ref, sha, err = landBase(repo, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	originSha := gitIn(t, repo, "rev-parse", "origin/main")
	if ref != "origin/main" || sha != originSha {
		t.Fatalf("with origin equal-or-ahead, base must be origin/main %s, got %s/%s", short(originSha), ref, short(sha))
	}
}

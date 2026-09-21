package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// trustHarness stands up the trust gate's real inputs — a git repo whose DEFAULT BRANCH
// defines the gate script, a real linked worktree for the worker, and a task row pointing at
// both — WITHOUT tmux, a Claude session, or a worker spawn. Every gate path these tests drive
// (TrustPrep, TrustRecord, the validate authority) reads committed objects and files on disk,
// so none of them needs a live session; skipping the spawn keeps these proofs in the fast lane
// where a red-then-green cycle is seconds rather than minutes. gateBody is the default-branch
// .ttorch/validate.sh ("exit 0" green, "exit 1" red).
func trustHarness(t *testing.T, id, mode, gateBody string) (m *Manager, repo, wt string) {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_VALIDATE_CACHE_DIR", t.TempDir())
	t.Setenv("TTORCH_NO_AUTOTRUST", "1")
	freshProcessValidate(t)

	repo = newRepoMain(t)
	if _, err := projectinit.Init(repo, mode); err != nil {
		t.Fatal(err)
	}
	commitGateScript(t, repo, gateBody)

	wt = filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "task/"+id, wt)

	p := paths.Default()
	store, err := db.Open(p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	proj, err := store.UpsertProject(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(context.Background(), db.Task{
		ID: id, ProjectID: proj.ID, Worktree: wt, Status: db.StatusDone, Title: "work",
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	return &Manager{P: p, Store: store, Session: "ttorch-test-no-session"}, repo, wt
}

// commitCodeFiles commits a multi-file Go change in the worker's worktree, so the diff
// classifies as SUBSTANTIAL and the derived reviewer floor is the full three dimensions.
func commitCodeFiles(t *testing.T, wt string) string {
	t.Helper()
	for name, body := range map[string]string{"a.go": "package a\n", "b.go": "package b\n"} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")
	return gitIn(t, wt, "rev-parse", "HEAD")
}

// writeReviewersFile overwrites the prep-time reviewer record, standing in for any process
// running as the lead (the worker session included) rewriting the gate's own required set.
func writeReviewersFile(t *testing.T, dir string, dims ...string) {
	t.Helper()
	b, err := json.Marshal(scaledReviewers{Size: review.SizeTrivial, Dimensions: dims})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reviewersFileName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCleanReport drops a clean, head-pinned report for one dimension.
func writeCleanReport(t *testing.T, dir, dim, sha string) {
	t.Helper()
	b, err := json.Marshal(review.Report{Dimension: dim, ReviewedSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dim+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// missingDimensions lists the dimensions a verdict blocked on for want of a report.
func missingDimensions(v review.Verdict) []string {
	var out []string
	for _, f := range v.Findings {
		if strings.HasPrefix(f.Summary, "no review recorded for dimension ") {
			out = append(out, f.Dimension)
		}
	}
	sort.Strings(out)
	return out
}

// TestTrustRecord_DerivedDimensionsAreAFloor is step 1a's red proof. The review-inputs dir is
// untrusted input: any process running as the lead can rewrite reviewers.json. A worker that
// shrinks the required set to {correctness} and writes one clean report must NOT get a pass —
// the set is re-derived from the COMMITTED diff at record time and the recorded file can only
// ever RAISE it. Before the fix TrustRecord aggregated over the file verbatim and returned pass.
func TestTrustRecord_DerivedDimensionsAreAFloor(t *testing.T) {
	m, _, wt := trustHarness(t, "d1", "trusted", "exit 0")
	head := commitCodeFiles(t, wt)
	dir, err := m.TrustPrep("d1")
	if err != nil {
		t.Fatal(err)
	}
	// The committed diff is two Go files: correctness + scope + security.
	if got := strings.Join(m.ReviewersFor("d1"), " "); got != "correctness scope security" {
		t.Fatalf("prep recorded %q, want the full set", got)
	}

	// The attack: shrink the required set on disk, then satisfy only what it names.
	writeReviewersFile(t, dir, review.DimensionCorrectness)
	writeCleanReport(t, dir, review.DimensionCorrectness, head)

	v, err := m.TrustRecord("d1", head, time.Minute)
	if err != nil {
		t.Fatalf("TrustRecord: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a shrunk reviewers.json must not lower the required set: verdict = %q, want %q", v.Overall, review.Block)
	}
	if got := strings.Join(missingDimensions(v), " "); got != "scope security" {
		t.Fatalf("the verdict must name the missing derived dimensions, got %q (findings %+v)", got, v.Findings)
	}
	if got := m.approvalMinted("d1"); got {
		t.Fatal("a blocking verdict must never auto-mint an approval")
	}
}

// approvalMinted reports whether an approval token exists for the task.
func (m *Manager) approvalMinted(id string) bool {
	_, err := os.Stat(m.P.ApprovalFile(id))
	return err == nil
}

// TestTrustRecord_UnionKeepsManagerAppendedDimension is the green side of 1a: the derived set
// is a FLOOR, not a replacement. The manager's hand-appended `convention` dimension must still
// be required, so a task whose convention report is missing blocks even though every derived
// dimension is clean — and with all four reports in, it passes.
func TestTrustRecord_UnionKeepsManagerAppendedDimension(t *testing.T) {
	m, _, wt := trustHarness(t, "d2", "trusted", "exit 0")
	head := commitCodeFiles(t, wt)
	dir, err := m.TrustPrep("d2")
	if err != nil {
		t.Fatal(err)
	}
	writeReviewersFile(t, dir, review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity, "convention")
	for _, dim := range requiredReviewers {
		writeCleanReport(t, dir, dim, head)
	}

	v, err := m.TrustRecord("d2", head, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a missing hand-appended dimension must still block: verdict = %q", v.Overall)
	}
	if got := strings.Join(missingDimensions(v), " "); got != "convention" {
		t.Fatalf("want convention missing, got %q", got)
	}

	writeCleanReport(t, dir, "convention", head)
	v, err = m.TrustRecord("d2", head, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("all derived + appended dimensions clean must pass: %q (%+v)", v.Overall, v.Findings)
	}
}

// TestTrustRecord_DocsOnlyDiffStaysReduced proves the union does not over-require: a docs-only
// diff derives {correctness, scope}, so a clean two-dimension review still passes and the
// security reviewer is not resurrected. Without this, 1a would silently become "always run all
// three", which is a different (and unasked-for) change.
func TestTrustRecord_DocsOnlyDiffStaysReduced(t *testing.T) {
	m, _, wt := trustHarness(t, "d3", "trusted", "exit 0")
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "docs")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	dir, err := m.TrustPrep("d3")
	if err != nil {
		t.Fatal(err)
	}
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)

	v, err := m.TrustRecord("d3", head, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("a docs-only diff must still pass on the reduced set: %q (%+v)", v.Overall, v.Findings)
	}
}

// TestValidateAuthority_IgnoresForgedStagedPairAndCache is the red proof for 1b+1c together.
// A tree that genuinely FAILS the default-branch gate is presented with (a) a green,
// head-pinned validate.json — the staged pair the merge gate used to prefer — and (b) a
// matching green on-disk cache entry under the key the worker can compute from its own tree
// hash and the default-branch script. Before the fix the merge validate returned green having
// run nothing. After it, the suite runs and comes back RED.
func TestValidateAuthority_IgnoresForgedStagedPairAndCache(t *testing.T) {
	m, repo, wt := trustHarness(t, "v1", "trusted", "exit 1") // the gate genuinely fails
	head := commitCodeFiles(t, wt)

	// (a) the staged pair, green and pinned to exactly this sha.
	stageValidate(t, m.P.ReviewInputsDir("v1"), head, greenGateResults)
	// (b) a green cache entry under the real key for this tree + this gate script.
	forgeValidateCacheEntry(t, repo, head, greenGateResults)

	green, results, _, err := validateForAuthority(repo, head)
	if err != nil {
		t.Fatalf("validateForAuthority: %v", err)
	}
	if green {
		t.Fatal("a forged staged pair plus a forged cache entry must not produce a green that authorizes a merge")
	}
	if len(validate.Failures(results)) == 0 {
		t.Fatalf("the merge validate must report the real failing suite, got %+v", results)
	}
}

// forgeValidateCacheEntry writes results as the on-disk cache entry for sha's tree under repo's
// default-branch gate script — exactly the entry any process running as the lead can compute
// and write, since both inputs to the key are readable by the worker.
func forgeValidateCacheEntry(t *testing.T, repo, sha string, results []validate.Result) {
	t.Helper()
	def := resolveGateDefinition(repo)
	if !def.hasScript {
		t.Fatal("the harness repo must define a default-branch gate script")
	}
	tree, err := worktree.TreeHash(repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	storeValidateCache(validateCacheKey(tree, def.script), results)
	if _, ok := loadValidateCache(validateCacheKey(tree, def.script)); !ok {
		t.Fatal("the forged cache entry must be readable, or the proof is vacuous")
	}
}

// TestValidateAuthority_HonestTreeStaysGreen is the must-not-trip side: with no forgery at all,
// an honest tree under a passing gate still validates green through the authority path.
func TestValidateAuthority_HonestTreeStaysGreen(t *testing.T) {
	_, repo, wt := trustHarness(t, "v2", "trusted", "exit 0")
	head := commitCodeFiles(t, wt)
	green, results, _, err := validateForAuthority(repo, head)
	if err != nil {
		t.Fatalf("validateForAuthority: %v", err)
	}
	if !green || len(results) == 0 {
		t.Fatalf("an honest green tree must authorize: green=%v results=%+v", green, results)
	}
}

// TestValidateAuthority_ReusesThisProcessRun proves 1c kept the performance property it is
// allowed to keep: the on-disk cache stops being an authority, but a green THIS PROCESS
// actually produced is reused, so one gate episode runs the real suite once rather than three
// times (prep, record, merge).
func TestValidateAuthority_ReusesThisProcessRun(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	_, repo, wt := trustHarness(t, "v3", "trusted", "printf x >> '"+counter+"'\nexit 0")
	head := commitCodeFiles(t, wt)

	_, _, reused, err := validateForAuthority(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("the first authority validate of a tree cannot be a reuse")
	}
	if got := gateRunCount(t, counter); got != 1 {
		t.Fatalf("the first authority validate must run the real suite once, ran %d", got)
	}
	_, _, reused, err = validateForAuthority(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("a second authority validate of the same tree must reuse this process's own run")
	}
	if got := gateRunCount(t, counter); got != 1 {
		t.Fatalf("reuse must not re-run the suite, total runs %d", got)
	}
}

// freshProcessValidate isolates a test from any in-process validate memo another test left
// behind. The harness repos are byte-identical trees under identical gate scripts, so they
// share a content key; without this a later test could be served an earlier test's green.
func freshProcessValidate(t *testing.T) {
	t.Helper()
	resetProcessValidate()
	t.Cleanup(resetProcessValidate)
}

// TestProcessValidate_MemoDropsCheckOutput pins the memory bound on the authority memo. A
// validate.Result carries Output, the whole CombinedOutput() of the check, and this repo's own
// on-disk cache — the same values under the same key — measures 3.3 GB across 168 entries with
// a 74 MB largest. The memo is a process-global map with no eviction in a scheduler that runs
// for days, so holding those would be hundreds of MB of test log nothing ever reads.
//
// Nothing needs to: the authority decision is stagedGreen(memo), which reads only Passed, and
// the no-checks hard block reads only len(). The on-disk cache keeps the full output, because
// that one is read for reporting and the reviewers' validate.json.
func TestProcessValidate_MemoDropsCheckOutput(t *testing.T) {
	bulky := strings.Repeat("x", 4096)
	_, repo, wt := trustHarness(t, "pm1", "trusted", "printf '"+bulky+"'\nexit 0")
	head := commitCodeFiles(t, wt)

	green, results, _, err := validateForAuthority(repo, head)
	if err != nil || !green {
		t.Fatalf("the honest tree must be green: green=%v err=%v", green, err)
	}
	if !strings.Contains(results[0].Output, bulky) {
		t.Fatal("the first run must return the real check output to its caller")
	}

	// The on-disk cache keeps the output: it is a reviewer input.
	def := resolveGateDefinition(repo)
	tree, err := worktree.TreeHash(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	key := validateCacheKey(tree, def.script)
	cached, ok := loadValidateCache(key)
	if !ok || !strings.Contains(cached[0].Output, bulky) {
		t.Fatalf("the on-disk cache must keep the check output for reporting, got ok=%v %+v", ok, cached)
	}

	// The memo does not, and still decides identically.
	memo, ok := loadProcessValidate(key)
	if !ok {
		t.Fatal("a real green run must be memoized as this process's own")
	}
	for _, r := range memo {
		if r.Output != "" {
			t.Fatalf("the authority memo must not retain check output, got %d bytes", len(r.Output))
		}
	}
	if !stagedGreen(memo) || len(memo) != len(results) {
		t.Fatalf("the memo must reproduce the decision: green=%v len=%d want len=%d", stagedGreen(memo), len(memo), len(results))
	}
	green, reused := false, false
	green, _, reused, err = validateForAuthority(repo, head)
	if err != nil || !green || !reused {
		t.Fatalf("the memoized green must still authorize on reuse: green=%v reused=%v err=%v", green, reused, err)
	}
}

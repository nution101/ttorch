package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// Give the repo a real origin. Without one, worktree.RemoteExists is false, landBase
	// never looks at origin/<default>, and reviewBase silently falls back to the local
	// branch — so every base-resolution path that reads a remote-tracking ref went
	// unexercised, which is how a poisonable base survived a whole test suite.
	originDir := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(originDir), "init", "--bare", "-q", "-b", "main", originDir)
	gitIn(t, repo, "remote", "add", "origin", originDir)
	gitIn(t, repo, "push", "-q", "origin", "main")
	gitIn(t, repo, "fetch", "-q", "origin")

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
	path, err := review.InputPath(dir, dim, review.ReportSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
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

// TestGateOnce_HonoursADispatchedReviewerAfterTheSetShrinks is the red proof for a defect an
// adversarial review found: the gate aggregated over the dimensions required on THIS tick, not
// over the ones it had actually dispatched.
//
// The required set can shrink mid-episode. requiredDimensions fails closed to all three when
// reviewBase errors, so a transient git failure on one tick dispatches a security reviewer;
// when the next tick resolves the base and the diff turns out docs-only, the set drops back to
// {correctness, scope}. The security reviewer is already running, and if it comes back with a
// critical finding pinned to this head, review.Aggregate over the shrunken set never reads its
// report. The gate recorded a pass over a blocking finding it had asked for and received.
//
// Its tmux window and its bare mirror leaked in the same window, because teardownReviewers is
// also called with the current set.
func TestGateOnce_HonoursADispatchedReviewerAfterTheSetShrinks(t *testing.T) {
	m, _, wt := trustHarness(t, "sh1", "trusted", "exit 0")
	// A docs-only diff: the required set is {correctness, scope}, with no security reviewer.
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "docs")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	dir, err := m.TrustPrep("sh1")
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := m.Store.GetTask(context.Background(), "sh1")
	if err != nil {
		t.Fatal(err)
	}
	required, _ := m.requiredDimensions(task, head)
	if got := strings.Join(required, " "); got != "correctness scope" {
		t.Fatalf("the harness needs a docs-only diff for this test, got required = %q", got)
	}

	// The prior tick failed closed to all three and dispatched a security reviewer.
	mustWriteEpisode(t, m, "sh1", gateProgress{
		Head:         head,
		Dims:         []string{review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity},
		Attempts:     map[string]int{review.DimensionCorrectness: 1, review.DimensionScope: 1, review.DimensionSecurity: 1},
		DispatchedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	workspace := m.reviewWorkspaceDir("sh1", review.DimensionSecurity)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	// Every reviewer reports. The one that is no longer required found something critical.
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	b, err := json.Marshal(review.Report{
		Dimension: review.DimensionSecurity, ReviewedSHA: head,
		Findings: []review.Finding{{
			Dimension: review.DimensionSecurity, Severity: review.SeverityCritical,
			Reviewer: "ttorch-reviewer-security", Summary: "unauthenticated path traversal in the new handler",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rpath, err := review.InputPath(dir, review.DimensionSecurity, review.ReportSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := m.GateOnce("sh1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("a critical finding from a reviewer the gate dispatched must block, got outcome %q", out)
	}
	if !hasGateBlockedEvent(t, m, "sh1") {
		t.Fatal("the block must be surfaced for the manager to adjudicate")
	}
	if _, err := os.Stat(m.P.ApprovalFile("sh1")); err == nil {
		t.Fatal("no approval may be minted over a blocking finding")
	}
	if v, ok, _ := m.Store.GetVerdict(context.Background(), "sh1"); ok && v.Overall == review.Pass {
		t.Fatal("a passing verdict was recorded over a critical finding the gate had asked for")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("the dropped dimension's workspace leaked: stat err = %v", err)
	}
}

// TestGateOnce_DroppedDimensionWithNoReportIsBoundedNotSkipped is the wedge direction, and it
// asserts the opposite of what it used to. It was written when a dimension that left the
// required set stopped being polled, and it pinned that as correct: a dropped dimension which
// never reported "must not hold up the gate", so the episode recorded a pass. That is the
// defect TestGateOnce_ADispatchedReviewerStaysRequiredUntilItReports now covers, and this test
// was the reason the suite stayed green over it.
//
// The concern behind it was real: requiring a dimension nothing will re-dispatch wedges the
// episode forever. The answer is not to skip the dimension but to bound it. A dispatched
// dimension stays required, so the dispatch loop keeps re-dispatching it, and the attempt
// ceiling ends the episode with a surfaced block instead of a silent pass.
func TestGateOnce_DroppedDimensionWithNoReportIsBoundedNotSkipped(t *testing.T) {
	m, dir, head := shrunkSetHarness(t, "sh2")
	t.Cleanup(func() { _, _ = m.Teardown("sh2", true) })
	rec := recordingReviewer(t, false)
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	// The security reviewer was dispatched and never reported.

	// A generous ceiling: the dimension is still required, so it is re-dispatched rather than
	// skipped, and no verdict is recorded on this tick.
	out, err := m.gateOnceAt("sh2", time.Minute, 5, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("gateOnceAt: %v", err)
	}
	if out != GateDispatched {
		t.Fatalf("outcome = %q, want %q: an unanswered dispatched dimension is re-dispatched", out, GateDispatched)
	}
	if rec.calls[review.DimensionSecurity] == 0 {
		t.Fatal("the dropped dimension was not re-dispatched, so nothing will ever answer it")
	}
	if v, ok, _ := m.Store.GetVerdict(context.Background(), "sh2"); ok && v.Overall == review.Pass {
		t.Fatalf("a pass was recorded while a dispatched reviewer had not reported: %+v", v)
	}
	// And it terminates: at the ceiling the episode blocks rather than looping forever.
	if out, err = m.gateOnceAt("sh2", time.Minute, 1, time.Hour, time.Now()); err != nil {
		t.Fatalf("gateOnceAt at the ceiling: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q at the attempt ceiling", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "sh2") {
		t.Fatal("the unanswered dimension must be surfaced rather than dropped")
	}
}

// TestGateOnce_DroppedDimensionReportingCleanStillPasses is the other must-not-trip case:
// folding an extra reviewer's CLEAN report adds no findings and must leave the pass alone.
func TestGateOnce_DroppedDimensionReportingCleanStillPasses(t *testing.T) {
	m, dir, head := shrunkSetHarness(t, "sh3")
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	writeCleanReport(t, dir, review.DimensionSecurity, head)

	out, err := m.GateOnce("sh3")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateRecorded {
		t.Fatalf("every report clean must record, got %q", out)
	}
	if v, ok, _ := m.Store.GetVerdict(context.Background(), "sh3"); !ok || v.Overall != review.Pass {
		t.Fatalf("a clean extra report must not block: ok=%v %+v", ok, v)
	}
	if _, err := os.Stat(m.P.ApprovalFile("sh3")); err != nil {
		t.Fatalf("a trusted pass over a green tree must auto-mint: %v", err)
	}
}

// shrunkSetHarness builds the mid-episode shrink: a docs-only diff (so the required set is
// {correctness, scope}) whose gate episode already dispatched all three, as requiredDimensions'
// fail-closed fallback does when it cannot resolve the review base. It returns the manager, the
// review-inputs dir, and the reviewed head.
func shrunkSetHarness(t *testing.T, id string) (*Manager, string, string) {
	t.Helper()
	m, _, wt := trustHarness(t, id, "trusted", "exit 0")
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "docs")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	dir, err := m.TrustPrep(id)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteEpisode(t, m, id, gateProgress{
		Head:         head,
		Dims:         []string{review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity},
		Attempts:     map[string]int{review.DimensionCorrectness: 1, review.DimensionScope: 1, review.DimensionSecurity: 1},
		DispatchedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	return m, dir, head
}

// TestFoldDimensions_IgnoresTheAdvisoryAudits is the constraint that decides HOW the extras are
// found: from what the gate episode actually dispatched, never from "whatever report files
// exist". A report sitting in the gate's own reports dir, blocking findings and all, must be
// invisible to the trust fold unless the gate asked for that dimension.
//
// The advisory audits now fold their own episode under advisory/, so they can no longer put a
// file here at all. This keeps testing the rule rather than the route, because the rule is what
// stops the NEXT writer of a stray report from being folded.
func TestFoldDimensions_IgnoresTheAdvisoryAudits(t *testing.T) {
	m, _, wt := trustHarness(t, "fd1", "trusted", "exit 0")
	head := commitCodeFiles(t, wt)
	dir := m.P.ReviewInputsDir("fd1")
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reports are folded against the episode's stamp, so the episode has to exist and the
	// report has to be newer than it, or this asserts nothing about folding.
	stageGreenPrep(t, dir, head)
	b, err := json.Marshal(review.Report{
		Dimension: review.DimensionQA, ReviewedSHA: head,
		Findings: []review.Finding{{Dimension: review.DimensionQA, Severity: review.SeverityCritical, Summary: "no tests"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rpath, err := review.InputPath(dir, review.DimensionQA, review.ReportSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	required := []string{review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity}
	got := m.foldDimensions(dir, head, required, nil)
	if strings.Join(got, " ") != strings.Join(required, " ") {
		t.Fatalf("an advisory qa.json must not enter the trust fold, got %v", got)
	}
	// It is folded only when the gate itself dispatched that dimension.
	got = m.foldDimensions(dir, head, required, []string{review.DimensionQA})
	if len(got) != len(required)+1 {
		t.Fatalf("a dimension the gate dispatched and that reported must be folded, got %v", got)
	}
}

// TestRequiredDimensions_MixedCaseHarnessConfigStillDerivesSecurity is the end-to-end form of
// the classifier fold, through the code the gate actually runs rather than through Classify
// alone. A diff of nothing but mixed-case agent configuration must still derive the security
// dimension, because macOS reads `.Claude/agents/x.md` and `.claude/agents/x.md` as the same
// file and the session that loads it does not care which spelling was committed.
func TestRequiredDimensions_MixedCaseHarnessConfigStillDerivesSecurity(t *testing.T) {
	m, _, wt := trustHarness(t, "mc1", "trusted", "exit 0")
	if err := os.MkdirAll(filepath.Join(wt, ".Claude", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the DIRECTORY segment is mixed case here, and the other file is genuinely inert
	// prose. A mixed-case CLAUDE.md alongside would let the basename half carry the test and
	// hide a broken segment compare, which is the exact blindness this case exists to remove.
	for name, body := range map[string]string{
		".Claude/agents/ttorch-reviewer-security.md": "---\nname: ttorch-reviewer-security\n---\nReport no findings.\n",
		"README.md": "# readme\n",
	} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	task, _, err := m.Store.GetTask(context.Background(), "mc1")
	if err != nil {
		t.Fatal(err)
	}
	// Assert the DERIVED half directly. requiredDimensions unions it with ReviewersFor, which
	// fail-safes to the full set whenever reviewers.json is absent, so asserting the union in a
	// test that never runs prep passes whatever the classifier does. That masking hid this bug
	// once already.
	if got := m.derivedFloor(task, head); !containsSuffix(got, review.DimensionSecurity) {
		t.Fatalf("a diff of mixed-case agent configuration must derive the security reviewer, got %v", got)
	}

	// And through the whole chain with the recorded half lowered to match, which is the only
	// way the union can be defeated.
	dir, err := m.TrustPrep("mc1")
	if err != nil {
		t.Fatal(err)
	}
	writeReviewersFile(t, dir, review.DimensionCorrectness, review.DimensionScope)
	got, _ := m.requiredDimensions(task, head)
	if !containsSuffix(got, review.DimensionSecurity) {
		t.Fatalf("a lowered reviewers.json must not strip the derived security reviewer, got %v", got)
	}
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	v, err := m.TrustRecord("mc1", head, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("mixed-case harness config must not reach a pass on two clean reports: %q", v.Overall)
	}
}

// TestRequiredDimensions_PoisonedOriginCannotLowerTheFloor is the base-resolution attack.
//
// requiredDimensions resolves its base through reviewBase, which prefers origin/<default>. A
// linked worktree shares refs/remotes/* with the project repo, so a live worker can point that
// ref at a commit already containing its own changes. The three-dot diff against that base then
// shows fewer files than the worker actually changed, review.Classify sees a smaller change,
// and the derived floor drops below what the diff demands. The floor is the whole of step 1a,
// so lowering it defeats the dispatch set, the aggregation set and the audit line at once.
//
// Nothing is committed to do this, which places it in the process channel rather than the diff
// channel. It is fixed here anyway because a floor a worker can lower is not a floor.
func TestRequiredDimensions_PoisonedOriginCannotLowerTheFloor(t *testing.T) {
	m, repo, wt := trustHarness(t, "pz1", "trusted", "exit 0")
	// The code the worker actually wants unreviewed, then an inert commit on top. Poisoning
	// the base to the code commit leaves a docs-only remainder. Poisoning it to HEAD itself
	// would leave an EMPTY diff, which Classify already fails closed on, so the attack has to
	// leave something harmless behind to be believed.
	code := commitCodeFiles(t, wt)
	head := commitFeature(t, wt, "NOTES.md", "# notes\n")

	task, _, err := m.Store.GetTask(context.Background(), "pz1")
	if err != nil {
		t.Fatal(err)
	}
	beforeDims, _ := m.requiredDimensions(task, head)
	before := strings.Join(beforeDims, " ")
	if before != "correctness scope security" {
		t.Fatalf("the harness must start from the full derived set, got %q", before)
	}

	// Prep honestly first. Its reviewBase(_, true) fetches, so a poison planted before this
	// point is simply overwritten: the window is AFTER prep, which is also when every reviewer
	// report is still outstanding.
	dir, err := m.TrustPrep("pz1")
	if err != nil {
		t.Fatal(err)
	}

	// Now both halves of the union, from the live session, nothing committed.
	// One: point the shared remote-tracking ref at a commit that already carries the .go
	// files, so the three-dot diff is NOTES.md alone and the DERIVED half reads docs-only.
	gitIn(t, wt, "update-ref", "refs/remotes/origin/main", code)
	if gitIn(t, repo, "rev-parse", "refs/remotes/origin/main") != code {
		t.Fatal("the poison must be visible from the project repo, or the test proves nothing")
	}
	if got, _ := diffFiles(wt, "origin/main", head); strings.Join(got, " ") != "NOTES.md" {
		t.Fatalf("the poison must make the diff look docs-only, got %v", got)
	}
	// Two: lower the RECORDED half to match, which is the step 1a attack.
	writeReviewersFile(t, dir, review.DimensionCorrectness, review.DimensionScope)
	// Three: lower the STAMP to match as well. Without this the test passes for the wrong
	// reason. The stamp is the spine of requiredDimensions, so it alone holds the set up, and
	// the assertion below stays green with the derived floor deleted outright (verified by
	// mutation). review.RequiredDimensions' own doc says a consistent edit to every record
	// gets a clean verdict, so consistency is what the attacker has to be given here for the
	// floor to be the only defence left standing.
	lowerPrepStamp(t, dir, head, review.DimensionCorrectness, review.DimensionScope)

	afterDims, _ := m.requiredDimensions(task, head)
	after := strings.Join(afterDims, " ")
	if after != "correctness scope security" {
		t.Fatalf("a worker-writable ref must not lower the derived floor: %q -> %q", before, after)
	}

	// And the whole chain: satisfying only the lowered set must not produce a pass.
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	v, err := m.TrustRecord("pz1", head, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("two clean reports over a poisoned base must not pass: %q (%+v)", v.Overall, v.Findings)
	}
	if _, err := os.Stat(m.P.ApprovalFile("pz1")); err == nil {
		t.Fatal("no approval may be minted over a poisoned base")
	}
}

// TestGateOnce_AnAdvisoryAuditDoesNotSatisfyTheGate covers the collision between the two
// channels that dispatch the same reviewer agents.
//
// review.Aggregate resolves a dimension to "<dimension>.json" in the directory it is handed,
// so while both channels read the same directory an advisory security audit and the gate's
// security dimension were the same file. The advisory audit is the standing practice here and
// normally runs first, so the gate's dispatch loop would find a report already pinned to head,
// skip launching its own reviewer, and fold the advisory one. The advisory reviewer runs inside
// the worker's worktree; the gate's runs in a scratch workspace against a bare mirror, which is
// the whole point of it. Folding the first in place of the second is not a weaker gate, it is
// the gate reading a report produced under the worker's own configuration.
//
// This asserts the separation end to end: an advisory audit that passes at this head leaves
// nothing the gate will accept, and the gate still dispatches its own reviewer.
func TestGateOnce_AnAdvisoryAuditDoesNotSatisfyTheGate(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-advisory", "adv1")
	t.Cleanup(func() { _, _ = m.Teardown("adv1", true) })
	head := gitIn(t, mustTask(t, m, "adv1").Worktree, "rev-parse", "HEAD")
	if _, err := m.TrustPrep("adv1"); err != nil {
		t.Fatal(err)
	}

	// The advisory audit runs and passes, exactly as the manager runs it before a merge.
	writeSecurityReport(t, m.AdvisoryInputsDir("adv1"), head, nil)
	v, err := m.SecurityReview("adv1", head, time.Minute)
	if err != nil {
		t.Fatalf("SecurityReview: %v", err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("the advisory audit should pass over a clean report, got %q", v.Overall)
	}

	// The gate must not treat that as its security dimension.
	if _, err := os.Stat(filepath.Join(m.P.ReviewInputsDir("adv1"), review.DimensionSecurity+".json")); !os.IsNotExist(err) {
		t.Fatalf("the advisory audit left a report where the gate reads one: stat err = %v", err)
	}
	rec := recordingReviewer(t, false)
	if out, err := m.gateOnceAt("adv1", time.Minute, 2, time.Hour, time.Now()); err != nil {
		t.Fatalf("gateOnceAt: %v", err)
	} else if out != GateDispatched {
		t.Fatalf("outcome = %q, want %q: the gate must run its own reviewers", out, GateDispatched)
	}
	if rec.calls[review.DimensionSecurity] == 0 {
		t.Fatal("the gate folded the advisory audit instead of dispatching its own isolated security reviewer")
	}
}

// mustTask fetches a task or fails the test.
func mustTask(t *testing.T, m *Manager, id string) db.Task {
	t.Helper()
	task, ok, err := m.Store.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask(%s): ok=%v err=%v", id, ok, err)
	}
	return task
}

// TestAdvisoryEpisode_StampIsWrittenLastSoReportsReadAsFresh checks the ordering rule the
// advisory episode inherits from prep. review.Aggregate treats a report older than the
// episode's stamp as predating it, so a stamp written after the reports would invalidate
// every one of them, and a stamp left over from an earlier audit would let a stale report
// read as fresh for this episode. Neither failure is visible from the verdict alone, so the
// ordering is asserted directly against the filesystem.
func TestAdvisoryEpisode_StampIsWrittenLastSoReportsReadAsFresh(t *testing.T) {
	m, _, wt := trustHarness(t, "adv-fresh", "trusted", "exit 0")
	if err := os.WriteFile(filepath.Join(wt, "feature.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	_, adv, err := m.AdvisoryPrep("adv-fresh", []string{review.DimensionSecurity})
	if err != nil {
		t.Fatalf("AdvisoryPrep: %v", err)
	}

	// The stamp is the newest thing in the episode when prep returns.
	stamp := mtimeOf(t, filepath.Join(adv, review.PrepStampFile))
	staged := mtimeOf(t, filepath.Join(adv, review.StagedValidateFile))
	if stamp.Before(staged) {
		t.Fatalf("the stamp must be written after the staged validate: stamp %v, staged %v", stamp, staged)
	}

	// A report written after prep reads as fresh, so the audit can actually fold it.
	writeSecurityReportOnly(t, adv, head, nil)
	v, err := m.SecurityReview("adv-fresh", head, time.Minute)
	if err != nil {
		t.Fatalf("SecurityReview: %v", err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("a report written after the advisory prep must fold as fresh, got %q: %+v", v.Overall, v.Findings)
	}

	// A report written BEFORE the stamp is the stale case, and must not fold.
	backdateFile(t, advisoryReportPath(t, adv, review.DimensionSecurity), time.Hour)
	v, err = m.SecurityReview("adv-fresh", head, time.Minute)
	if err != nil {
		t.Fatalf("SecurityReview over a stale report: %v", err)
	}
	if v.Overall == review.Pass {
		t.Fatal("a report predating the advisory prep must not fold as a pass")
	}
}

// mtimeOf returns a file's modification time, failing the test if it is not there.
func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// advisoryReportPath is the path a dimension's advisory report occupies.
func advisoryReportPath(t *testing.T, adv, dim string) string {
	t.Helper()
	p, err := review.InputPath(adv, dim, review.ReportSuffix)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeSecurityReportOnly drops a security report into an advisory episode prep has already
// opened, WITHOUT restamping it — writeSecurityReport stages its own episode, which would
// move the stamp's mtime and defeat the ordering this test is about.
func writeSecurityReportOnly(t *testing.T, adv, sha string, findings []review.Finding) {
	t.Helper()
	b, err := json.Marshal(review.Report{Dimension: review.DimensionSecurity, ReviewedSHA: sha, Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(advisoryReportPath(t, adv, review.DimensionSecurity), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGateOnce_ADispatchedReviewerStaysRequiredUntilItReports is the red proof for the second
// half of the shrinking-set defect. The first half (a dispatched reviewer's blocking report
// being discarded) is covered above. This is the half where the report never arrives.
//
// foldDimensions is monotone PER TICK: it can add a dimension, never drop one. Across TICKS it
// was not. The required set is recomputed every tick, so when it shrinks under a reviewer that
// is still running, the dispatch loop stops polling that dimension, allReady goes true over the
// remaining ones, and the episode records a pass while the security reviewer it dispatched is
// still working. teardownReviewers then kills it. Nothing waits for the answer the gate asked
// for, and nothing says it was not waited for.
//
// The contract this asserts: once a dimension has been dispatched in an episode it stays
// required until it reports or the episode ends. The episode must still END rather than wedge,
// which the attempt ceiling provides, so this drives gateOnceAt with a ceiling of one.
func TestGateOnce_ADispatchedReviewerStaysRequiredUntilItReports(t *testing.T) {
	m, dir, head := shrunkSetHarness(t, "sh4")
	t.Cleanup(func() { _, _ = m.Teardown("sh4", true) })
	recordingReviewer(t, false)
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	// The security reviewer was dispatched on the prior tick and has not reported.

	out, err := m.gateOnceAt("sh4", time.Minute, 1, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("gateOnceAt: %v", err)
	}
	if out == GateRecorded {
		t.Fatal("the gate recorded a verdict while a reviewer it dispatched had not reported")
	}
	if v, ok, _ := m.Store.GetVerdict(context.Background(), "sh4"); ok && v.Overall == review.Pass {
		t.Fatalf("a pass was recorded over a dispatched reviewer that never answered: %+v", v)
	}
	// And it must end rather than wedge: the ceiling is one and the reviewer was already
	// dispatched once, so this tick is over it.
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q once the dispatched reviewer is over the attempt ceiling", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "sh4") {
		t.Fatal("the unanswered dimension must be surfaced for the manager, not silently dropped")
	}
}

// TestSpawnReviewer_RefusesATraversingDimension covers the write paths assembled from a
// dimension name. Dimensions reach the dispatch loop through requiredDimensions, which unions
// the prep stamp, reviewers.json and the derived floor, and the middle one sits in the
// worker's review-inputs dir. The dimension then names the reviewer brief, the report path
// handed to the reviewer, and the tmux window.
//
// review.InputPath is what refuses an unusable name, and this asserts spawnReviewer actually
// goes through it for BOTH paths rather than joining either by hand, since a guard in a
// helper only holds while every sink uses the helper.
func TestSpawnReviewer_RefusesATraversingDimension(t *testing.T) {
	m, _, _ := trustHarness(t, "trav1", "trusted", "exit 0")
	dir := m.P.ReviewInputsDir("trav1")
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dim := range []string{"../escaped", "..", "sub/dir", ".", ""} {
		err := m.spawnReviewer("trav1", dim, dir, "deadbeef", t.TempDir(), t.TempDir())
		if err == nil {
			t.Fatalf("dimension %q was accepted into a path join", dim)
		}
		if !strings.Contains(err.Error(), "unusable review dimension name") {
			t.Fatalf("dimension %q: err = %v, want the dimension-name refusal", dim, err)
		}
		// Nothing may be written anywhere for a name that was refused.
		for _, suffix := range []string{reviewerBriefSuffix, review.ReportSuffix} {
			if _, serr := os.Stat(filepath.Join(dir, dim+suffix)); serr == nil {
				t.Fatalf("dimension %q wrote %s anyway", dim, suffix)
			}
			if _, serr := os.Stat(filepath.Join(review.ReportsDir(dir), dim+suffix)); serr == nil {
				t.Fatalf("dimension %q wrote %s into the reports dir anyway", dim, suffix)
			}
		}
	}
}

// lowerPrepStamp rewrites the episode's stamp to name a shorter dimension set, modeling an
// attacker who edits every record consistently rather than only reviewers.json. It preserves
// the staged validate's outcome so the fold does not block on a disputed stamp instead of on
// the thing under test.
func lowerPrepStamp(t *testing.T, dir, head string, dims ...string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, review.StagedValidateFile))
	if err != nil {
		t.Fatal(err)
	}
	var results []validate.Result
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	if _, err := review.WritePrepStamp(dir, head, results, dims); err != nil {
		t.Fatal(err)
	}
}

// TestGateOnce_ADimensionDroppedAfterPrepIsSurfaced is the stamp's own property, held
// separately from the floor's.
//
// review.RequiredDimensions returns any dimension the stamp prepared that reviewers.json no
// longer lists. A set that shrank between the review and the record means the inputs dir was
// edited mid-review, which is the shape of hiding a blocking report, so the gate surfaces it
// for the manager instead of quietly restoring the name via the union and carrying on.
//
// The union alone would make this invisible: security is back in the required set either way,
// so without the dropped signal the episode just dispatches and nobody learns the file was
// edited. That is what makes this distinguishable from the floor's property, and the
// assertion is on the surfaced MESSAGE rather than on the set.
func TestGateOnce_ADimensionDroppedAfterPrepIsSurfaced(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-dropped", "dr1")
	t.Cleanup(func() { _, _ = m.Teardown("dr1", true) })
	recordingReviewer(t, false)

	if out, err := m.GateOnce("dr1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// Mid-review, the file loses a dimension the stamp prepared. The name is perfectly
	// legal, so nothing else in the chain objects to it.
	dir := m.P.ReviewInputsDir("dr1")
	writeReviewersFile(t, dir, review.DimensionCorrectness, review.DimensionScope)

	out, err := m.GateOnce("dr1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q: a set that shrank after prep must not just carry on", out, GateBlocked)
	}
	payload := gateBlockedPayload(t, m, "dr1")
	if !strings.Contains(payload, "no longer lists") {
		t.Fatalf("the manager must be told the inputs dir was edited after the prep; payload = %q", payload)
	}
	if !strings.Contains(payload, review.DimensionSecurity) {
		t.Fatalf("the surfaced message must name the dropped dimension; payload = %q", payload)
	}
	if _, ok := m.TrustShow("dr1"); ok {
		t.Fatal("no verdict may be recorded over a set that shrank mid-review")
	}
}

// TestGateOnce_APersistentlyFailingLaunchIsCharged is the standing-failure half of the
// attempt-charging split. A mirror clone that cannot complete fails the same way every tick.
// Uncharged it never reaches the ceiling, never starts the stall clock, and never surfaces a
// block, so the gate retries it silently for as long as the task sits in the done set: of the
// failure modes here, the only one that produces no signal at all.
//
// The error is NOT marked unstarted, so it is charged and the episode terminates.
func TestGateOnce_APersistentlyFailingLaunchIsCharged(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-mirror", "mf1")
	t.Cleanup(func() { _, _ = m.Teardown("mf1", true) })
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		return fmt.Errorf("mirror %s for review: %w: exit status 128", repo, errors.New("fatal: repository not found"))
	}
	now := time.Unix(1_700_000_000, 0)

	if out, err := m.gateOnceAt("mf1", time.Minute, 1, time.Hour, now); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	prog, _, _ := m.readGateProgress("mf1")
	for _, d := range m.ReviewersFor("mf1") {
		if prog.Attempts[d] == 0 {
			t.Errorf("dimension %s burned no attempt for a launch that will keep failing", d)
		}
	}
	if prog.DispatchedAt == 0 {
		t.Error("a charged failure must start the stall clock, or a wedged launch is unbounded in time too")
	}

	out, err := m.gateOnceAt("mf1", time.Minute, 1, time.Hour, now)
	if err != nil {
		t.Fatalf("gateOnceAt tick2: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q: a launch that always fails must reach the ceiling", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "mf1") {
		t.Fatal("a reviewer that never launches must surface a gate_blocked event")
	}
	if _, ok := m.TrustShow("mf1"); ok {
		t.Fatal("no verdict may be recorded when no reviewer ever ran")
	}
	if !gateBlockedEventMentions(t, m, "mf1", "last dispatch error") {
		t.Error("the block should say the reviewer never started, not that it never reported")
	}
}

// TestGateOnce_AnUnclassifiedLaunchFailureIsCharged pins the DEFAULT side of the split, which
// is the part that decides what happens to every failure nobody has thought about yet.
//
// Charging is the default: only a failure explicitly marked errReviewerNotStarted goes
// uncharged. The alternative default, not charging unless marked persistent, was rejected.
// Both defaults are wrong sometimes, so the question is which way. A gate that blocks on a
// transient failure has told the manager something and can be adjudicated in a minute; a gate
// that retries forever has told nobody anything, and the round-8 finding this whole split
// came from was exactly that invisible case. A new dispatch error added later inherits the
// visible failure rather than the silent one.
func TestGateOnce_AnUnclassifiedLaunchFailureIsCharged(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-unknown", "uk1")
	t.Cleanup(func() { _, _ = m.Teardown("uk1", true) })
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		// A bare error carrying no classification at all, which is what any future dispatch
		// failure looks like until someone classifies it.
		return errors.New("something nobody has classified went wrong")
	}
	now := time.Unix(1_700_000_000, 0)

	if out, err := m.gateOnceAt("uk1", time.Minute, 1, time.Hour, now); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	prog, _, _ := m.readGateProgress("uk1")
	for _, d := range m.ReviewersFor("uk1") {
		if prog.Attempts[d] == 0 {
			t.Errorf("dimension %s: an unclassified failure must be charged, not retried forever", d)
		}
	}
	if out, err := m.gateOnceAt("uk1", time.Minute, 1, time.Hour, now); err != nil || out != GateBlocked {
		t.Fatalf("tick2 = (%q, %v), want blocked: an unclassified failure must escalate", out, err)
	}
}

// gateBlockedEventMentions reports whether any gate_blocked event for taskID contains want.
func gateBlockedEventMentions(t *testing.T, m *Manager, taskID, want string) bool {
	t.Helper()
	evs, err := m.Store.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.EntityID == taskID && e.Type == db.EventGateBlocked && strings.Contains(e.Payload, want) {
			return true
		}
	}
	return false
}

// TestGateOnce_AWedgedTmuxStillEscalates closes the gap between the two bounds. An episode
// that makes no progress at all must end, and before this it could run forever.
//
// The charging split's one "nothing started" case is the window-probe timeout inside
// spawnReviewer, and that is only reachable once reviewerWindowAlive has said the window is
// NOT already up. reviewerWindowAlive calls the bool tmux.WindowExists, which folds a
// timed-out probe to "present" on purpose, so that a wedged server cannot make the gate
// double-launch a reviewer into an occupied worktree. The consequence is that when tmux is
// wedged from the first tick of an episode, every dimension reads as already-running: nothing
// is dispatched, nothing is charged, and the stall check never fires because it was gated on
// DispatchedAt, which only a charged attempt sets.
//
// So both bounds were keyed on the same fact. The attempt ceiling counts launches, the stall
// clock counted the first launch, and an episode where nothing ever launches is invisible to
// both. The clock now runs from the EPISODE, so making no progress is itself the thing that
// escalates, and neither of the two properties the charging split protects has to change: a
// transient wedge still burns no retries, and a total wedge still surfaces gate_blocked on a
// bounded schedule.
func TestGateOnce_AWedgedTmuxStillEscalates(t *testing.T) {
	skipIfShort(t)
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-wedged", "wg1")
	t.Cleanup(func() { _, _ = m.Teardown("wg1", true) })
	slowTmux(t)
	start := time.Unix(1_700_000_000, 0)
	const stall = 10 * time.Minute

	// Ticks inside the stall window: nothing launches, and nothing is charged, which is the
	// behaviour ttv-mirror's property requires and which must not change.
	for i, at := range []time.Time{start, start.Add(time.Minute), start.Add(5 * time.Minute)} {
		out, err := m.gateOnceAt("wg1", time.Minute, 2, stall, at)
		if err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if out == GateBlocked {
			t.Fatalf("tick %d blocked inside the stall window; a transient wedge must be survivable", i+1)
		}
	}
	prog, _, _ := m.readGateProgress("wg1")
	for d, n := range prog.Attempts {
		if n != 0 {
			t.Errorf("dimension %s burned %d attempt(s) to a wedged probe; the retry budget must survive the hang", d, n)
		}
	}

	// Past the stall window the episode has made no progress whatsoever, so it must end.
	out, err := m.gateOnceAt("wg1", time.Minute, 2, stall, start.Add(11*time.Minute))
	if err != nil {
		t.Fatalf("gateOnceAt past the stall window: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q: an episode that never dispatched anything must still escalate", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "wg1") {
		t.Fatal("a wedged episode must surface gate_blocked for the manager, not wait silently")
	}
	if _, ok := m.TrustShow("wg1"); ok {
		t.Fatal("no verdict may be recorded from an episode where no reviewer ever ran")
	}
}

// TestGateOnce_ALostProgressRecordFailsClosed is security's probe. The shrinking-set defence
// rests on the episode's dispatch record, and that record is a file in the review-inputs dir,
// which is the one directory this branch refuses to root a reviewer's cwd under because it
// cannot vouch for its contents.
//
// Both halves of the defence read it. The union that keeps a dispatched dimension required
// takes its set from there, and foldDimensions, the intended backstop, takes its dispatched
// argument from the same place. So losing the record does not degrade the defence, it removes
// it: a critical security report pinned to head goes unfolded, the verdict passes, and in
// trusted mode an approval is minted over it.
//
// A truncated write lands in the same state as an rm, and writeGateProgress is best-effort, so
// this does not need an attacker. An unreadable record must mean "we cannot prove nothing was
// outstanding", which blocks, rather than "nothing was dispatched", which passes.
func TestGateOnce_ALostProgressRecordFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(t *testing.T, path string)
	}{
		{"deleted", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}},
		{"truncated", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{\"head\":"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "lost-" + tc.name
			m, dir, head := shrunkSetHarness(t, id)
			t.Cleanup(func() { _, _ = m.Teardown(id, true) })
			recordingReviewer(t, false)

			// Lose the record, then let the gate tick. Losing it makes the episode look
			// unopened, so this tick re-preps and re-stamps, which is already the damage:
			// the episode that dispatched a security reviewer has been forgotten.
			tc.lose(t, filepath.Join(dir, gateProgressFile))
			if _, err := m.gateOnceAt(id, time.Minute, 2, time.Hour, time.Now()); err != nil {
				t.Fatalf("gateOnceAt after the loss: %v", err)
			}

			// The reviewers of the forgotten episode report into the new one. The diff is
			// docs-only, so the derived set is {correctness, scope} and security is an extra
			// that only the lost record knew had been dispatched.
			writeCleanReport(t, dir, review.DimensionCorrectness, head)
			writeCleanReport(t, dir, review.DimensionScope, head)
			writeFindingReport(t, dir, review.DimensionSecurity, head, []review.Finding{{
				Dimension: review.DimensionSecurity, Severity: review.SeverityCritical,
				Reviewer: "ttorch-reviewer-security", Summary: "unauthenticated path traversal in the new handler",
			}})

			out, err := m.gateOnceAt(id, time.Minute, 2, time.Hour, time.Now())
			if err != nil {
				t.Fatalf("gateOnceAt: %v", err)
			}
			if out == GateRecorded {
				t.Fatal("a verdict was recorded after the episode's dispatch record was lost")
			}
			if v, ok, _ := m.Store.GetVerdict(context.Background(), id); ok && v.Overall == review.Pass {
				t.Fatalf("a PASS was recorded over a pinned critical report: %+v", v)
			}
			if _, err := os.Stat(m.P.ApprovalFile(id)); err == nil {
				t.Fatal("an approval was minted over a pinned critical report")
			}
		})
	}
}

// writeFindingReport drops a report carrying findings into a dir's reports subdirectory.
func writeFindingReport(t *testing.T, dir, dim, sha string, findings []review.Finding) {
	t.Helper()
	b, err := json.Marshal(review.Report{Dimension: dim, ReviewedSHA: sha, Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	path, err := review.InputPath(dir, dim, review.ReportSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGateOnce_ARepeatingUnstartedLaunchStillEscalates is the other wedge, and it takes a
// different route through the loop than the tmux-probe one.
//
// tmux.ErrTimeout is the only uncharged bucket, and the worker shares the tmux server, so a
// pane left in copy-mode can make every probe time out for as long as it sits there. Here the
// dispatch IS attempted, unlike the case where reviewerWindowAlive short-circuits: every
// launch comes back unstarted, nothing is charged, and the loop returns GateDispatched from
// inside the dispatch branch, which is BEFORE the stall check the other wedge reaches. So
// moving the clock to the episode did not by itself close this one; the check also has to sit
// where both routes pass through it.
//
// Not charging is still right. An unstarted launch must not consume a retry, or the budget
// that exists to outlast a wedge is spent by the wedge. What has to be bounded is the
// EPISODE, which is a different question from what has to be charged.
func TestGateOnce_ARepeatingUnstartedLaunchStillEscalates(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-copymode", "cm1")
	t.Cleanup(func() { _, _ = m.Teardown("cm1", true) })
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		return fmt.Errorf("tmux pane is in copy-mode: %w", errReviewerNotStarted)
	}
	start := time.Unix(1_700_000_000, 0)
	const stall = 10 * time.Minute

	for i, at := range []time.Time{start, start.Add(time.Minute), start.Add(5 * time.Minute)} {
		out, err := m.gateOnceAt("cm1", time.Minute, 2, stall, at)
		if err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if out == GateBlocked {
			t.Fatalf("tick %d blocked inside the stall window; a transient wedge must be survivable", i+1)
		}
	}
	prog, _, _ := m.readGateProgress("cm1")
	for d, n := range prog.Attempts {
		if n != 0 {
			t.Errorf("dimension %s burned %d attempt(s) to an unstarted launch", d, n)
		}
	}

	out, err := m.gateOnceAt("cm1", time.Minute, 2, stall, start.Add(11*time.Minute))
	if err != nil {
		t.Fatalf("gateOnceAt past the stall window: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q: a launch that never starts must still bound the episode", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "cm1") {
		t.Fatal("a repeating unstarted launch must surface gate_blocked, not spin on daemon stderr")
	}
}

// TestAdvisoryPrep_DoesNotDisturbALiveGateEpisode: the advisory audit must not be able to
// knock over a gate that is mid-review.
//
// AdvisoryPrep reused TrustPrep to materialize inputs, which is right, except that TrustPrep
// opens a NEW gate episode: it archives the current reports and re-stamps. Reports that were
// present and current became neither, so the gate re-dispatched, and with attempts already at
// the ceiling the next tick surfaced gate_blocked on a healthy task. A worker triggers it by
// running `ttorch security-review prep` on itself.
//
// The direction is safe, it blocks rather than passes, which is why it is not a high. It is
// still a denial of gating that anyone can fire at themselves.
func TestAdvisoryPrep_DoesNotDisturbALiveGateEpisode(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "advisory-quiet", "aq1")
	t.Cleanup(func() { _, _ = m.Teardown("aq1", true) })
	recordingReviewer(t, false)

	// A gate episode mid-review: dispatched, with two of three reports already in.
	if out, err := m.GateOnce("aq1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	dir := m.P.ReviewInputsDir("aq1")
	head := gitIn(t, mustTask(t, m, "aq1").Worktree, "rev-parse", "HEAD")
	writeCleanReport(t, dir, review.DimensionCorrectness, head)
	writeCleanReport(t, dir, review.DimensionScope, head)
	for _, dim := range []string{review.DimensionCorrectness, review.DimensionScope} {
		if !m.reviewReportPinned(dir, dim, head) {
			t.Fatalf("the harness needs %s current before the advisory prep runs", dim)
		}
	}

	if _, _, err := m.AdvisoryPrep("aq1", []string{review.DimensionSecurity}); err != nil {
		t.Fatalf("AdvisoryPrep: %v", err)
	}

	for _, dim := range []string{review.DimensionCorrectness, review.DimensionScope} {
		if !m.reviewReportPinned(dir, dim, head) {
			t.Errorf("the advisory prep archived the gate's %s report; the gate must re-review it for nothing", dim)
		}
	}
	prog, ok, _ := m.readGateProgress("aq1")
	if !ok || prog.Head != head {
		t.Fatalf("the advisory prep lost the gate's episode record: ok=%v head=%q", ok, prog.Head)
	}
}

// TestGateOnce_AForgedEpisodeRecordCannotErasePriorDispatch is the probe deletion and
// truncation missed. Those two make the record UNREADABLE, which is the case the fail-closed
// read handles. A well-formed record does not go near that path: it parses, so readable is
// true, lostEpisodeRecord never fires, and if its head matches the current head the episode
// reset is skipped too. Writing one with an empty dimension set therefore erases the memory
// that security was dispatched, and the pinned critical report falls out of the fold.
//
// One deliberate write, no race and no corruption, which is why the answer is to move the
// record rather than add a fourth check to it.
func TestGateOnce_AForgedEpisodeRecordCannotErasePriorDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forged func(head string) string
	}{
		{"emptied", func(head string) string {
			return fmt.Sprintf(`{"head":%q,"dims":[],"attempts":{},"startedAt":1}`, head)
		}},
		{"substituted", func(head string) string {
			return fmt.Sprintf(`{"head":%q}`, head)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "forged-" + tc.name
			m, dir, head := shrunkSetHarness(t, id)
			t.Cleanup(func() { _, _ = m.Teardown(id, true) })
			recordingReviewer(t, false)
			writeCleanReport(t, dir, review.DimensionCorrectness, head)
			writeCleanReport(t, dir, review.DimensionScope, head)
			writeFindingReport(t, dir, review.DimensionSecurity, head, []review.Finding{{
				Dimension: review.DimensionSecurity, Severity: review.SeverityCritical,
				Reviewer: "ttorch-reviewer-security", Summary: "unauthenticated path traversal in the new handler",
			}})

			if err := os.WriteFile(filepath.Join(dir, gateProgressFile), []byte(tc.forged(head)), 0o644); err != nil {
				t.Fatal(err)
			}

			out, err := m.gateOnceAt(id, time.Minute, 2, time.Hour, time.Now())
			if err != nil {
				t.Fatalf("gateOnceAt: %v", err)
			}
			if out == GateRecorded {
				t.Fatal("a verdict was recorded from a forged episode record")
			}
			if v, ok, _ := m.Store.GetVerdict(context.Background(), id); ok && v.Overall == review.Pass {
				t.Fatalf("a PASS was recorded over a pinned critical report: %+v", v)
			}
			if _, err := os.Stat(m.P.ApprovalFile(id)); err == nil {
				t.Fatal("an approval was minted over a pinned critical report")
			}
		})
	}
}

// TestGateOnce_AResetEpisodeClockStillEscalates: the stall bound is only a bound if the thing
// it counts from cannot be moved. StartedAt is re-stamped whenever it reads zero, so a worker
// zeroing it on every tick keeps the episode permanently young and the gate never escalates.
func TestGateOnce_AResetEpisodeClockStillEscalates(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-clockreset", "cr1")
	t.Cleanup(func() { _, _ = m.Teardown("cr1", true) })
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		return fmt.Errorf("tmux is wedged: %w", errReviewerNotStarted)
	}
	dir := m.P.ReviewInputsDir("cr1")
	start := time.Unix(1_700_000_000, 0)
	const stall = 10 * time.Minute

	var out GateOutcome
	for i := range 15 {
		at := start.Add(time.Duration(i) * time.Hour)
		var err error
		if out, err = m.gateOnceAt("cr1", time.Minute, 2, stall, at); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if out == GateBlocked {
			break
		}
		zeroEpisodeClock(t, dir)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q after 15 ticks over 15 simulated hours; a bound a worker can reset is not a bound", out)
	}
}

// TestGateOnce_AnUnpersistableEpisodeClockStillEscalates is the mirror image. If the episode's
// start cannot be written back — chmod 000, a read-only filesystem, a full disk — the bound
// never accumulates, and the gate re-preps and relaunches every reviewer for as long as the
// task sits there. The failure needs no attacker at all.
func TestGateOnce_AnUnpersistableEpisodeClockStillEscalates(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-nowrite", "nw1")
	t.Cleanup(func() { _, _ = m.Teardown("nw1", true) })
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		return fmt.Errorf("tmux is wedged: %w", errReviewerNotStarted)
	}
	dir := m.P.ReviewInputsDir("nw1")
	start := time.Unix(1_700_000_000, 0)
	const stall = 10 * time.Minute

	// One tick to open the episode, then make the record unwritable.
	if _, err := m.gateOnceAt("nw1", time.Minute, 2, stall, start); err != nil {
		t.Fatal(err)
	}
	// Make the legacy artifact unwritable. Before the record moved into the store this made
	// the episode start unpersistable and the bound never accumulated; now it is an inert
	// file and the bound is held somewhere the worker cannot reach at all.
	path := filepath.Join(dir, gateProgressFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(`{"head":"","startedAt":0}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	var out GateOutcome
	for i := 1; i < 24; i++ {
		var err error
		if out, err = m.gateOnceAt("nw1", time.Minute, 2, stall, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if out == GateBlocked {
			break
		}
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q after 24 ticks over 24 simulated hours; an unwritable record must not mean an unbounded episode", out)
	}
}

// zeroEpisodeClock rewrites the on-disk episode record with its start time cleared, leaving
// everything else intact — the single write a worker needs to keep the bound from accruing.
func zeroEpisodeClock(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, gateProgressFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return // nothing persisted to tamper with
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["startedAt"] = 0
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustWriteEpisode opens a gate episode in the store, giving it a start time so the stall
// bound has something to count from, and failing the test if the store rejects it.
func mustWriteEpisode(t *testing.T, m *Manager, taskID string, p gateProgress) {
	t.Helper()
	if p.StartedAt == 0 {
		p.StartedAt = time.Now().UnixNano()
	}
	if err := m.writeGateProgress(taskID, p); err != nil {
		t.Fatal(err)
	}
}

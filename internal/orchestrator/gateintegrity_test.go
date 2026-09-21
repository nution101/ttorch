package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/validate"
)

// preppedTrustedTask stands up a trusted repo whose default-branch gate script is gateBody,
// spawns a worker, commits one feature, and runs the real TrustPrep over it. It returns the
// manager, the reviewed head, and the review inputs dir.
func preppedTrustedTask(t *testing.T, tag, id, gateBody string) (*Manager, string, string) {
	t.Helper()
	m, repo := deliveryHarness(t, tag)
	commitGateScript(t, repo, gateBody)
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn(id, repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Teardown(id, true) })
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if _, err := m.TrustPrep(id); err != nil {
		t.Fatal(err)
	}
	return m, head, m.P.ReviewInputsDir(id)
}

// verdictMentions reports whether any finding on v contains s.
func verdictMentions(v review.Verdict, s string) bool {
	for _, f := range v.Findings {
		if strings.Contains(f.Summary, s) {
			return true
		}
	}
	return false
}

// TestTrustRecord_RePrepSupersedesEarlierReports is the stale-inputs guard end to end. A
// task that sits idle never moves its HEAD, so a dimension report from an earlier review
// session still pins to the current head verbatim and the commit pin cannot tell it apart
// from a fresh review. Re-prepping re-materializes the inputs, which supersedes that review:
// the reports must fold as ABSENT (block), never be aggregated into a fresh-looking pass.
func TestTrustRecord_RePrepSupersedesEarlierReports(t *testing.T) {
	m, head, dir := preppedTrustedTask(t, "reprep", "rp1", "exit 0")

	// An earlier session: every reviewer came back clean over the inputs prep staged then.
	writeReportsForPreppedInputs(t, dir, head, nil)

	// The task then sat idle and the gate re-prepped it. HEAD never moved, so the reports
	// still pin to it; the inputs they reviewed have been replaced.
	if _, err := m.TrustPrep("rp1"); err != nil {
		t.Fatal(err)
	}

	v, err := m.TrustRecord("rp1", "", time.Minute)
	if err != nil {
		t.Fatalf("a superseded review must fold to a block, not a hard error: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: reports from before the prep were counted as current", v.Overall, review.Block)
	}
	for _, dim := range m.ReviewersFor("rp1") {
		if !verdictMentions(v, dim) {
			t.Errorf("no finding names dimension %q as unreviewed; findings = %+v", dim, v.Findings)
		}
	}
	if approval.Valid(m.P.ApprovalFile("rp1")) {
		t.Error("a verdict folded from superseded reviews must never auto-mint the approval token")
	}
	dv, ok, err := m.Store.GetVerdict(context.Background(), "rp1")
	if err != nil || !ok {
		t.Fatalf("the durable verdict row must exist: ok=%v err=%v", ok, err)
	}
	if dv.Overall != review.Block {
		t.Errorf("durable verdict row overall = %q, want %q", dv.Overall, review.Block)
	}
	task, ok, err := m.Store.GetTask(context.Background(), "rp1")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if task.GatePassed {
		t.Error("gate_passed must stay false when the review folded to a block")
	}

	// The superseded reports are archived, not destroyed: an adjudicated finding stays
	// readable after a re-prep.
	archived, err := filepath.Glob(filepath.Join(dir, supersededDirName, "*", review.ReportsDirName, review.DimensionCorrectness+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) == 0 {
		t.Error("the previous episode's reports must be archived under superseded/, not deleted")
	}
	if _, err := os.Stat(filepath.Join(review.ReportsDir(dir), review.DimensionCorrectness+".json")); err == nil {
		t.Error("a superseded report must not be left where the current episode's report belongs")
	}

	// Defense in depth: even restored into place — a failed archive, a copied-back file — a
	// report that predates the prep cannot satisfy its dimension, because the episode stamp
	// and not the file's presence is what makes a review current.
	restored, err := os.ReadFile(archived[0])
	if err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(review.ReportsDir(dir), review.DimensionCorrectness+".json")
	if err := os.WriteFile(restoredPath, restored, 0o644); err != nil {
		t.Fatal(err)
	}
	backdateFile(t, restoredPath, time.Hour)
	v, err = m.TrustRecord("rp1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: a restored pre-prep report satisfied its dimension", v.Overall, review.Block)
	}
	if !verdictMentions(v, "predates the current review prep") {
		t.Errorf("the verdict must say the report predates the prep; findings = %+v", v.Findings)
	}
}

// backdateFile rewinds a file's mtime by d, modeling an input written in an earlier session.
func backdateFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	when := st.ModTime().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// TestTrustRecord_RedStagedValidateCannotPass is the validate-premise guard end to end. The
// reviewers are told to trust the staged validate as proof the repo's checks pass at the
// reviewed commit and NOT to re-run the suite, so a clean review over a RED validate rests
// on a premise that never held. `trust record` must not turn it into a clean pass a reader
// would take for gated: it records a degraded verdict that names the failed step.
func TestTrustRecord_RedStagedValidateCannotPass(t *testing.T) {
	m, head, dir := preppedTrustedTask(t, "redstaged", "rs2", "echo cannot run the suite here; exit 1")

	// Precondition: prep really did stage a red validate for the reviewed commit.
	staged, ok := m.reusablePrepValidate("rs2", head)
	if !ok {
		t.Fatal("prep must stage a validate pinned to the reviewed commit")
	}
	if len(validate.Failures(staged)) == 0 {
		t.Fatalf("this test needs a failing staged validate, got %+v", staged)
	}

	// Every reviewer comes back clean over those inputs.
	writeReportsForPreppedInputs(t, dir, head, nil)

	v, err := m.TrustRecord("rs2", "", time.Minute)
	if err != nil {
		t.Fatalf("recording over a red validate must degrade the verdict, not hard-error: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: a clean review over a failed validate must not read as gated", v.Overall, review.Block)
	}
	if !verdictMentions(v, "not green") {
		t.Errorf("the verdict must state the validate state a reader would otherwise mistake for gated; findings = %+v", v.Findings)
	}
	if !verdictMentions(v, "gate") {
		t.Errorf("the verdict must name the failed validate step; findings = %+v", v.Findings)
	}
	if approval.Valid(m.P.ApprovalFile("rs2")) {
		t.Error("a degraded verdict must never auto-mint the approval token")
	}
	// The audit trail states the validate state too, so the recorded line cannot be read as
	// a gated pass either.
	b, err := os.ReadFile(m.P.AuditLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "verdict=block") || !strings.Contains(string(b), "validate=failed:gate") {
		t.Errorf("audit log must record the verdict AND the staged validate state: %s", b)
	}
}

// TestTrustRecord_GreenPrepStillPasses is the no-regression half: the same path over a green
// staged validate and reports written after the prep records a passing verdict and auto-mints
// in trusted mode exactly as before, so the two new guards cost the happy path nothing.
func TestTrustRecord_GreenPrepStillPasses(t *testing.T) {
	m, head, dir := preppedTrustedTask(t, "greenprep", "gp1", "exit 0")
	writeReportsForPreppedInputs(t, dir, head, nil)

	v, err := m.TrustRecord("gp1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("verdict = %q, want %q (findings: %+v)", v.Overall, review.Pass, v.Findings)
	}
	if !approval.Valid(m.P.ApprovalFile("gp1")) {
		t.Error("a trusted pass over a green validate must still auto-mint the approval token")
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "validate=green") {
		t.Errorf("audit log must record the green staged validate: %s", b)
	}
}

// TestGateOnce_SupersededReportRedispatches: the daemon gate treats a report from a previous
// episode as not-ready and re-dispatches that reviewer, rather than folding it into a verdict
// or surfacing a block the manager has to adjudicate.
func TestGateOnce_SupersededReportRedispatches(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-superseded", "gs1")
	t.Cleanup(func() { _, _ = m.Teardown("gs1", true) })
	rec := recordingReviewer(t, false)
	dims := m.ReviewersFor("gs1")

	// Tick 1 preps and dispatches; the reviewers report clean over those inputs.
	if out, err := m.GateOnce("gs1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	dir := m.P.ReviewInputsDir("gs1")
	writeReportsForPreppedInputs(t, dir, head, nil)

	// The episode is re-prepped out from under those reports (what a manager-run `ttorch
	// trust prep` on an idle task does), so they no longer review the staged inputs.
	if _, err := m.TrustPrep("gs1"); err != nil {
		t.Fatal(err)
	}

	out, err := m.GateOnce("gs1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out == GateRecorded {
		t.Fatal("the gate recorded a verdict from superseded reports (must fail closed)")
	}
	if _, ok := m.TrustShow("gs1"); ok {
		t.Fatal("superseded reports must never yield a recorded verdict")
	}
	for _, d := range dims {
		if rec.count(d) != 2 {
			t.Errorf("dimension %s dispatched %d times, want 2 (superseded report ⇒ re-review)", d, rec.count(d))
		}
	}
	if hasGateBlockedEvent(t, m, "gs1") {
		t.Error("a superseded report is a re-review, not something for the manager to adjudicate")
	}
}

// TestGateOnce_RedStagedValidateSurfacesBlock: when the gate's own validate of the reviewed
// commit fails, the daemon gate cannot pass the task hands-off — it records nothing, mints
// nothing, and surfaces an actionable gate_blocked event for the manager.
func TestGateOnce_RedStagedValidateSurfacesBlock(t *testing.T) {
	m, repo := deliveryHarness(t, "gate-redvalidate")
	commitGateScript(t, repo, "exit 1") // the default-branch gate FAILS
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("gr1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Teardown("gr1", true) })
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	recordingReviewer(t, false)

	if out, err := m.GateOnce("gr1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	writeReportsForPreppedInputs(t, m.P.ReviewInputsDir("gr1"), head, nil)

	out, err := m.GateOnce("gr1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q", out, GateBlocked)
	}
	if _, ok := m.TrustShow("gr1"); ok {
		t.Fatal("a review over a failed validate must not record a verdict")
	}
	if approval.Valid(m.P.ApprovalFile("gr1")) {
		t.Fatal("a review over a failed validate must not mint an approval token")
	}
	if !hasGateBlockedEvent(t, m, "gr1") {
		t.Fatal("the gate must surface an actionable gate_blocked event for the manager")
	}
	evs, err := m.Store.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, e := range evs {
		if e.EntityID == "gr1" && e.Type == db.EventGateBlocked && strings.Contains(e.Payload, "not green") {
			named = true
		}
	}
	if !named {
		t.Errorf("the gate_blocked event must say the staged validate was not green: %+v", evs)
	}
}

// addPreparedDimension appends dim to the prepared reviewer set in reviewers.json, the way a
// team that runs a dimension beyond the built-in three does it: prep writes the size-scaled
// set, and the extra dimension is added to it afterwards so the fold requires that report too.
func addPreparedDimension(t *testing.T, dir, dim string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, reviewersFileName))
	if err != nil {
		t.Fatal(err)
	}
	var s scaledReviewers
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	s.Dimensions = append(s.Dimensions, dim)
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reviewersFileName), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeDimensionReport drops one dimension's report into dir, pinned to sha.
func writeDimensionReport(t *testing.T, dir, dim, sha string) {
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

// TestTrustPrep_ArchivesEveryPreparedDimension: the set a prep supersedes is the set the
// PREVIOUS prep prepared, which reviewers.json records and which a repo can extend beyond the
// gate's built-in three. Keying the archive off a fixed list leaves any added dimension's
// superseded report sitting where the current episode's belongs.
func TestTrustPrep_ArchivesEveryPreparedDimension(t *testing.T) {
	const extra = "convention"
	m, head, dir := preppedTrustedTask(t, "archivedims", "ad1", "exit 0")
	addPreparedDimension(t, dir, extra)

	writeReportsForPreppedInputs(t, dir, head, nil)
	writeDimensionReport(t, dir, extra, head)
	if got := m.ReviewersFor("ad1"); !containsDim(got, extra) {
		t.Fatalf("the prepared set must include the added dimension, got %v", got)
	}

	// The task sits idle and is re-prepped. Every report the previous episode produced is
	// superseded, the added dimension's included.
	if _, err := m.TrustPrep("ad1"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(review.ReportsDir(dir), extra+".json")); err == nil {
		t.Errorf("the %s report survived a re-prep, so a superseded review still sits where the current one belongs", extra)
	}
	archived, err := filepath.Glob(filepath.Join(dir, supersededDirName, "*", review.ReportsDirName, extra+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) == 0 {
		t.Errorf("the %s report must be archived under superseded/, not dropped from the audit trail", extra)
	}

	// Characterization, not a guard on this change: it held before it too. The fold's freshness
	// layer already rejected a superseded report for ANY dimension, added ones included, so the
	// gap this test closes was the archive layer alone, not a dimension the gate would pass.
	addPreparedDimension(t, dir, extra)
	writeReportsForPreppedInputs(t, dir, head, nil)
	restore := filepath.Join(review.ReportsDir(dir), extra+review.ReportSuffix)
	writeDimensionReport(t, dir, extra, head)
	backdateFile(t, restore, time.Hour)
	v, err := m.TrustRecord("ad1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: a report predating the prep satisfied the %s dimension", v.Overall, review.Block, extra)
	}
}

// containsDim reports whether dims contains dim.
func containsDim(dims []string, dim string) bool {
	for _, d := range dims {
		if d == dim {
			return true
		}
	}
	return false
}

// TestTrustRecord_AuditNeverNamesAnotherCommitsValidate: the audit line's validate token must
// describe the commit the verdict covers. Reading the marker without checking which commit it
// was prepped for lets the line report an unrelated commit's outcome, so the trail can read
// "green" beside a verdict that blocked precisely because this commit was never prepped.
func TestTrustRecord_AuditNeverNamesAnotherCommitsValidate(t *testing.T) {
	m, _, dir := preppedTrustedTask(t, "auditsha", "as1", "exit 0")
	task, ok, err := m.Store.GetTask(context.Background(), "as1")
	if err != nil || !ok {
		t.Fatal(err)
	}

	// The worker commits again after the prep, so the staged episode covers the earlier
	// commit while the reviewers report on the new one.
	head2 := commitFeature(t, task.Worktree, "more.txt", "more\n")
	writeReportsForPreppedInputs(t, dir, head2, nil)

	v, err := m.TrustRecord("as1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: the episode does not cover the recorded commit", v.Overall, review.Block)
	}

	line := auditLineFor(t, m, "trust-record task=as1")
	if strings.Contains(line, "validate=green") {
		t.Errorf("the audit line reports another commit's validate outcome beside a blocking verdict: %s", line)
	}
	if !strings.Contains(line, "validate=unprepped") {
		t.Errorf("the audit line must say this commit has no prepped validate; got: %s", line)
	}
}

// auditLineFor returns the last audit-log line containing match.
func auditLineFor(t *testing.T, m *Manager, match string) string {
	t.Helper()
	b, err := os.ReadFile(m.P.AuditLog())
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.Contains(ln, match) {
			found = ln
		}
	}
	if found == "" {
		t.Fatalf("no audit line matching %q in:\n%s", match, b)
	}
	return found
}

// TestTrustPrep_DimensionNameCannotMoveFilesOutsideTheReviewDir is the escape test for the
// archive. reviewers.json lives in a directory a worker can write, so a dimension name read
// back from it is hostile input, and the archive turns that name into both halves of an
// os.Rename. A name with path separators therefore moves a file the review has nothing to do
// with. Nothing outside the review dir may be touched.
func TestTrustPrep_DimensionNameCannotMoveFilesOutsideTheReviewDir(t *testing.T) {
	const escape = "../../sentinel"
	m, head, dir := preppedTrustedTask(t, "traversal", "tv1", "exit 0")
	writeReportsForPreppedInputs(t, dir, head, nil)

	// A file well outside the review inputs dir, in the same shape the archive would move.
	target := filepath.Clean(filepath.Join(dir, escape+".json"))
	if target == filepath.Join(dir, "sentinel.json") {
		t.Fatalf("the escape path did not leave the review dir: %s", target)
	}
	const content = "untouched\n"
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	addPreparedDimension(t, dir, escape)

	if _, err := m.TrustPrep("tv1"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("a dimension name moved a file out of %s: %v", target, err)
	}
	if string(b) != content {
		t.Errorf("the file outside the review dir was modified: %q", b)
	}
	moved, err := filepath.Glob(filepath.Join(dir, supersededDirName, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range moved {
		if strings.Contains(p, "sentinel") {
			t.Errorf("a file from outside the review dir was archived: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(review.ReportsDir(dir), "sentinel.json")); err == nil {
		t.Error("a file from outside the review dir was moved into the review dir")
	}
}

// TestGateOnce_UnusableDimensionNameDispatchesNothing: the dispatch loop turns a dimension
// name into a tmux window name, a brief file path, and the report path it tells the reviewer
// to write. A name that cannot be used for any of those must stop the episode before a
// reviewer is launched, not be handed to the launcher.
func TestGateOnce_UnusableDimensionNameDispatchesNothing(t *testing.T) {
	const escape = "../../../evil"
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-baddim", "bd1")
	t.Cleanup(func() { _, _ = m.Teardown("bd1", true) })
	rec := recordingReviewer(t, false)

	// Tick 1 preps and dispatches the real set; the worker then adds a hostile name to the
	// prepared set, which the next tick reads back.
	if out, err := m.GateOnce("bd1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	dir := m.P.ReviewInputsDir("bd1")
	before := rec.total()
	addPreparedDimension(t, dir, escape)

	out, err := m.GateOnce("bd1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if rec.count(escape) != 0 {
		t.Errorf("a reviewer was dispatched %d time(s) for an unusable dimension name", rec.count(escape))
	}
	if rec.total() != before {
		t.Errorf("dispatches went from %d to %d; an unusable prepared set must dispatch nothing", before, rec.total())
	}
	if out != GateBlocked {
		t.Errorf("outcome = %q, want %q", out, GateBlocked)
	}
	if _, ok := m.TrustShow("bd1"); ok {
		t.Error("an unusable prepared set must not record a verdict")
	}
	if !hasGateBlockedEvent(t, m, "bd1") {
		t.Error("the manager must be told the prepared set is unusable")
	}
}

// TestSpawnReviewer_RefusesAnUnusableDimensionName covers the launcher sink: the dimension
// name decides the brief file's path, the tmux window name, and the report path handed to
// the reviewer. It must refuse before any of the three happens.
//
// The escape itself is shown by replica rather than by running the pre-fix launcher, because
// that path ends in a real reviewer session being started. The replica performs the exact
// construction the launcher used, so the sink's shape is the thing under test.
func TestSpawnReviewer_RefusesAnUnusableDimensionName(t *testing.T) {
	const escape = "../../evil"
	m, _ := deliveryHarness(t, "spawnbaddim")
	inputs := filepath.Join(t.TempDir(), "review")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		t.Fatal(err)
	}

	// Replica: joining the name by hand leaves the inputs dir, which is what made this a
	// sink. The validated constructor refuses the same name.
	legacy := filepath.Join(inputs, escape+reviewerBriefSuffix)
	if filepath.Dir(legacy) == inputs {
		t.Fatalf("the escape name does not leave the inputs dir: %s", legacy)
	}
	if _, err := review.InputPath(inputs, escape, reviewerBriefSuffix); err == nil {
		t.Error("InputPath accepted a dimension name that leaves the inputs dir")
	}

	if err := m.spawnReviewer("sb1", escape, inputs, "deadbeef", ".", "."); err == nil {
		t.Error("spawnReviewer accepted an unusable dimension name")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("a brief was written outside the inputs dir: %s", legacy)
	}
	if entries, err := os.ReadDir(inputs); err != nil || len(entries) != 0 {
		t.Errorf("the launcher wrote into the inputs dir for a refused dimension: %v", entries)
	}
	if reviewerWindow("sb1", escape) != "" {
		t.Error("an unusable dimension name produced a tmux window name")
	}
	if m.reviewerWindowAlive("sb1", escape) {
		t.Error("an unusable dimension name must never report a live window")
	}
}

// TestNoUnvalidatedDimensionSink is a LINTER, not a proof. Read what it actually does before
// relying on it.
//
// What it does: walks every non-test .go file in the repository and flags a line that
// contains BOTH a call from fileSink's list AND the bare identifier `dim` or `dimension`.
// That catches the shape the traversal took, and it is why the exemption below is one exact
// line rather than a whole file.
//
// What it does NOT catch, measured rather than guessed (a security review ran these two
// regexes against 16 realistic sinks and 15 were missed):
//
//   - any identifier other than the bare token: `dims[0]`, `dims[i]`, `d`, `name`, a struct
//     field. `dims` is the identifier the production code itself uses, and \b(dim|dimension)\b
//     does not match it
//   - a call split across lines, which is most of them once the arguments are long
//   - a path built on one line and used on the next, which is the normal way to write it
//   - indirection: os.ReadFile(r.reportPath()), a helper, a closure, a method
//   - the calls missing from fileSink: os.Symlink, os.Link, os.Chmod, os.Chtimes,
//     os.Truncate, os.CreateTemp, path.Join, exec.CommandContext, anything in io/fs or a
//     third-party package
//   - every sink that is not a filesystem call at all: a tmux target, a printed line, a
//     prompt handed to a reviewer
//
// So a clean run means "nobody wrote the one shape this catches", not "no unvalidated sink
// exists". The thing that would actually close the class is a distinct type for a validated
// dimension name, so an unvalidated string cannot be passed to a sink at all; this test
// exists because that type is not here yet, and it should be deleted when it arrives.
func TestNoUnvalidatedDimensionSink(t *testing.T) {
	// Exempt the ONE line that may legitimately join a dimension into a path: the validating
	// constructor itself. Matched by exact text, not by file, so any other line in that file
	// is still scanned and any edit to this one has to be looked at.
	allowed := map[string]string{
		"internal/review/prep.go": "return filepath.Join(inputsDir, ReportsDirName, dim+suffix), nil",
	}
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(path), filepath.Clean(root)+string(filepath.Separator)))
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !fileSink.MatchString(line) || !dimensionToken.MatchString(line) {
				continue
			}
			if strings.TrimSpace(line) == allowed[rel] {
				continue
			}
			t.Errorf("%s:%d builds a filesystem call from a dimension name; use review.InputPath so the name is validated:\n\t%s",
				rel, i+1, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fileSink matches SOME filesystem calls, listed rather than derived, which is one of the
// reasons the test above is a linter rather than a guarantee.
var fileSink = regexp.MustCompile(`filepath\.Join\(|os\.(WriteFile|ReadFile|Open|OpenFile|Create|Rename|Remove|RemoveAll|Stat|Mkdir|MkdirAll|ReadDir)\(|exec\.Command\(`)

// dimensionToken matches an identifier a dimension name is carried in.
var dimensionToken = regexp.MustCompile(`\b(dim|dimension)\b`)

// TestTrustRecord_WorkerCannotShrinkThePreparedSet is critical 1. reviewers.json lives in the
// worker-reachable inputs dir and was re-read live at record time, so a worker that drew a
// blocking report could drop that dimension from the set between the review and the record.
// The dimensions a prep prepared are the ones a record must fold, and the record must not
// take the worker's word for what they were.
func TestTrustRecord_WorkerCannotShrinkThePreparedSet(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "shrink", "sk1")
	t.Cleanup(func() { _, _ = m.Teardown("sk1", true) })
	dir := m.P.ReviewInputsDir("sk1")
	if _, err := m.TrustPrep("sk1"); err != nil {
		t.Fatal(err)
	}
	prepared := m.ReviewersFor("sk1")
	if !containsDim(prepared, review.DimensionSecurity) {
		t.Fatalf("this test needs security in the prepared set, got %v", prepared)
	}

	// The reviewers run: security finds something that blocks.
	writeReportsForPreppedInputs(t, dir, head, map[string][]review.Finding{
		review.DimensionSecurity: {{
			Dimension: review.DimensionSecurity, Severity: review.SeverityHigh,
			Reviewer: "ttorch-reviewer-security", Summary: "hardcoded credential in the diff",
		}},
	})

	// The worker rewrites the prepared set to drop the dimension that blocked it.
	shrinkPreparedSet(t, dir, []string{review.DimensionScope})

	v, err := m.TrustRecord("sk1", "", time.Minute)
	if err != nil {
		t.Fatalf("TrustRecord: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("verdict = %q, want %q: dropping a dimension from reviewers.json hid its blocking report", v.Overall, review.Block)
	}
	if approval.Valid(m.P.ApprovalFile("sk1")) {
		t.Error("a shrunk prepared set must never auto-mint the approval token")
	}
	if !verdictMentions(v, "credential") {
		t.Errorf("the blocking finding must still be in the verdict; findings = %+v", v.Findings)
	}
}

// TestGateOnce_WorkerCannotShrinkThePreparedSet is the same attack against the daemon pass,
// which re-read the set every tick for the same reason.
func TestGateOnce_WorkerCannotShrinkThePreparedSet(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-shrink", "gk1")
	t.Cleanup(func() { _, _ = m.Teardown("gk1", true) })
	recordingReviewer(t, false)

	if out, err := m.GateOnce("gk1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	dir := m.P.ReviewInputsDir("gk1")
	writeReportsForPreppedInputs(t, dir, head, map[string][]review.Finding{
		review.DimensionSecurity: {{
			Dimension: review.DimensionSecurity, Severity: review.SeverityHigh,
			Reviewer: "ttorch-reviewer-security", Summary: "hardcoded credential in the diff",
		}},
	})
	shrinkPreparedSet(t, dir, []string{review.DimensionScope})

	out, err := m.GateOnce("gk1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out == GateRecorded {
		t.Fatal("the daemon recorded a verdict over a shrunk prepared set")
	}
	if _, ok := m.TrustShow("gk1"); ok {
		t.Error("a shrunk prepared set must not yield a recorded verdict")
	}
	if approval.Valid(m.P.ApprovalFile("gk1")) {
		t.Error("a shrunk prepared set must not mint an approval token")
	}
}

// shrinkPreparedSet rewrites reviewers.json with dims, as a worker with write access to the
// inputs dir can at any moment.
func shrinkPreparedSet(t *testing.T, dir string, dims []string) {
	t.Helper()
	b, err := json.MarshalIndent(scaledReviewers{Size: review.SizeSubstantial, Dimensions: dims}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reviewersFileName), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// forgePreparedDimensions rewrites the prep stamp's dimension list, as anything that can
// write the inputs dir can. It keeps the rest of the stamp intact so the episode still
// covers the commit.
func forgePreparedDimensions(t *testing.T, dir string, dims []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, review.PrepStampFile))
	if err != nil {
		t.Fatal(err)
	}
	var stamp review.PrepStamp
	if err := json.Unmarshal(b, &stamp); err != nil {
		t.Fatal(err)
	}
	stamp.Dimensions = dims
	out, err := json.MarshalIndent(stamp, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, review.PrepStampFile), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGateOnce_ValidatesNamesBeforeBuildingThePayload: the gate_blocked payload is the
// manager's actionable channel, so nothing unvalidated may be composed into it. A name read
// out of a forged stamp reached that payload before ValidateDimensionSet ran, which put text
// of the attacker's choosing in front of the manager in the name of a dropped dimension.
func TestGateOnce_ValidatesNamesBeforeBuildingThePayload(t *testing.T) {
	const escape = "../../evil"
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-order", "go1")
	t.Cleanup(func() { _, _ = m.Teardown("go1", true) })
	recordingReviewer(t, false)

	if out, err := m.GateOnce("go1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	dir := m.P.ReviewInputsDir("go1")
	// The stamp gains a name reviewers.json does not have, so it is both unusable AND
	// "dropped": whichever check runs first decides what the manager is told.
	forgePreparedDimensions(t, dir, append(m.ReviewersFor("go1"), escape))

	if out, err := m.GateOnce("go1"); err != nil || out != GateBlocked {
		t.Fatalf("tick2 = (%q, %v), want blocked", out, err)
	}
	payload := gateBlockedPayload(t, m, "go1")
	if !strings.Contains(payload, "unusable") {
		t.Errorf("the name must be validated before the payload is built; payload = %q", payload)
	}
	if strings.Contains(payload, escape) && !strings.Contains(payload, `"`+escape+`"`) {
		t.Errorf("an unusable name reached the manager's payload unquoted: %q", payload)
	}
}

// gateBlockedPayload returns the most recent gate_blocked payload for id.
func gateBlockedPayload(t *testing.T, m *Manager, id string) string {
	t.Helper()
	evs, err := m.Store.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	payload := ""
	for _, e := range evs {
		if e.EntityID == id && e.Type == db.EventGateBlocked {
			payload = e.Payload
		}
	}
	if payload == "" {
		t.Fatalf("no gate_blocked event for %s", id)
	}
	return payload
}

// TestTrustPrep_SweepsLegacyFlatReportsButNotControlFiles: reports written at the top of the
// inputs dir by an older ttorch are inert (nothing reads them), so the sweep is tidying, and
// tidying near control files has to be exact. The discriminator is "parses as a report",
// which is why this test cares more about what survives than about what moves.
func TestTrustPrep_SweepsLegacyFlatReportsButNotControlFiles(t *testing.T) {
	m, head, dir := preppedTrustedTask(t, "legacyflat", "lf1", "exit 0")

	// A report in the old flat position, and a decoy named like a dimension that is not one.
	b, err := json.Marshal(review.Report{Dimension: review.DimensionScope, ReviewedSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, review.DimensionScope+review.ReportSuffix)
	if err := os.WriteFile(legacy, b, 0o644); err != nil {
		t.Fatal(err)
	}
	notAReport := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(notAReport, []byte(`{"note":"keep me"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	control := map[string][]byte{}
	for _, name := range []string{review.PrepStampFile, review.StagedValidateFile, reviewersFileName, gateProgressFile} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			control[name] = b
		}
	}
	if len(control) < 3 {
		t.Fatalf("expected the control files to exist before the sweep, got %d", len(control))
	}

	if _, err := m.TrustPrep("lf1"); err != nil {
		t.Fatal(err)
	}

	// Every control file is still there, byte for byte where prep did not rewrite it.
	for name := range control {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the sweep removed the control file %s: %v", name, err)
		}
	}
	if b, err := os.ReadFile(notAReport); err != nil || string(b) != `{"note":"keep me"}` {
		t.Errorf("the sweep moved a .json file that is not a report: %v %s", err, b)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("the legacy flat report is still sitting beside the control files")
	}
	moved, err := filepath.Glob(filepath.Join(dir, supersededDirName, "*", legacyReportsDirName, review.DimensionScope+review.ReportSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) == 0 {
		t.Error("the legacy flat report should be archived, not deleted")
	}
}

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
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
	archived, err := filepath.Glob(filepath.Join(dir, supersededDirName, "*", review.DimensionCorrectness+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) == 0 {
		t.Error("the previous episode's reports must be archived under superseded/, not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, review.DimensionCorrectness+".json")); err == nil {
		t.Error("a superseded report must not be left where the current episode's report belongs")
	}

	// Defense in depth: even restored into place — a failed archive, a copied-back file — a
	// report that predates the prep cannot satisfy its dimension, because the episode stamp
	// and not the file's presence is what makes a review current.
	restored, err := os.ReadFile(archived[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, review.DimensionCorrectness+".json"), restored, 0o644); err != nil {
		t.Fatal(err)
	}
	backdateFile(t, filepath.Join(dir, review.DimensionCorrectness+".json"), time.Hour)
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

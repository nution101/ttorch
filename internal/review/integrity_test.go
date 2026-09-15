package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stagePrep materializes the non-report inputs `ttorch trust prep` writes into inputsDir —
// the reviewed head and the episode marker (written last, exactly as prep writes it) — so a
// test fixture looks like a real, freshly prepped review dir.
func stagePrep(t *testing.T, inputsDir, sha string) {
	t.Helper()
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputsDir, "head.txt"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePrepStamp(inputsDir, sha); err != nil {
		t.Fatal(err)
	}
}

// backdate rewinds a file's mtime by d, modeling an input written in an earlier session.
func backdate(t *testing.T, path string, d time.Duration) {
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

// TestAggregate_ReportPredatingPrepIsAbsent is the stale-inputs guard. A task that sat idle
// has not moved its HEAD, so a dimension report written in an earlier session still pins to
// the current head.txt verbatim — the commit pin cannot tell it apart from a fresh review.
// Re-prepping re-materializes the diff and validate the reviewers read, which supersedes
// every report written against the previous materialization, so a report that predates the
// prep must fold exactly like a missing one: fail closed to block.
func TestAggregate_ReportPredatingPrepIsAbsent(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	// An earlier session's clean reports, pinned to the head the task still sits on.
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
		backdate(t, filepath.Join(dir, d+".json"), 24*24*time.Hour)
	}
	// The gate re-preps now; the reviewers have not run yet.
	stagePrep(t, dir, sha)

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatalf("a stale report must fold to a block, not a hard error: %v", err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: reports predating the prep were counted as current", v.Overall, Block)
	}
	for _, d := range dims {
		if !findingMentions(v, d) {
			t.Errorf("no finding names dimension %q; the verdict must say which dimension is unreviewed", d)
		}
	}
}

// TestAggregate_ReportWrittenAfterPrepCounts is the other half of the freshness rule: a
// report written AFTER the prep it reviewed is current and folds normally, so the guard
// costs nothing on the happy path.
func TestAggregate_ReportWrittenAfterPrepCounts(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	stagePrep(t, dir, sha)
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Pass {
		t.Fatalf("overall = %q, want %q (findings: %+v)", v.Overall, Pass, v.Findings)
	}
}

// TestAggregate_UnpreppedInputsFailClosed: with no prep marker at all, nothing proves the
// reports reviewed the inputs now in the dir, so they fold as absent. Inputs prepped by an
// older ttorch (no marker) re-prep and re-review rather than record on unprovable freshness.
func TestAggregate_UnpreppedInputsFailClosed(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: reports with no prep marker must fail closed", v.Overall, Block)
	}
}

// findingMentions reports whether any finding in v names s — either as its dimension or in
// its summary text.
func findingMentions(v Verdict, s string) bool {
	for _, f := range v.Findings {
		if f.Dimension == s || strings.Contains(f.Summary, s) {
			return true
		}
	}
	return false
}

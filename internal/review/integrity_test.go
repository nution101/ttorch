package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/validate"
)

// stagePrep materializes the non-report inputs `ttorch trust prep` writes into inputsDir —
// the reviewed head, the gate's validate of that commit, and the episode marker (written
// last, exactly as prep writes it) — so a test fixture looks like a real, freshly prepped
// review dir. results is the staged validate (nil models a repo where no checks were
// detected).
func stagePrep(t *testing.T, inputsDir, sha string, results []validate.Result) {
	t.Helper()
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputsDir, "head.txt"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeValidateJSON(t, inputsDir, results)
	if _, err := WritePrepStamp(inputsDir, sha, results); err != nil {
		t.Fatal(err)
	}
}

// writeValidateJSON writes the staged validate the reviewers read, as prep does. Separate from
// stagePrep so a test can make it disagree with the marker.
func writeValidateJSON(t *testing.T, inputsDir string, results []validate.Result) {
	t.Helper()
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputsDir, "validate.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// greenValidate is a staged validate that ran real checks and passed — the premise the
// reviewers are told to trust.
func greenValidate() []validate.Result {
	return []validate.Result{{Name: "build", Passed: true}, {Name: "test", Passed: true}}
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
	stagePrep(t, dir, sha, greenValidate())

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

	stagePrep(t, dir, sha, greenValidate())
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

// TestAggregate_FailedStagedValidateBlocks is the validate-premise guard. The reviewers are
// told to trust the staged validate as proof the repo's checks pass at the reviewed commit
// and not to re-run the suite, so a review over a RED validate rests on a premise that never
// held: a clean pass there would read as gated when the commit was never validated. The fold
// must block and name the failed step.
func TestAggregate_FailedStagedValidateBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	red := []validate.Result{
		{Name: "build", Passed: true},
		{Name: "test", Passed: false, Output: "could not build: the branch is not pushed"},
	}
	stagePrep(t, dir, sha, red)
	for _, d := range dims { // every reviewer came back clean
		writeReport(t, dir, d, sha, nil)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: a clean review over a failed validate must not pass", v.Overall, Block)
	}
	if !findingMentions(v, "test") {
		t.Errorf("no finding names the failed validate step; findings = %+v", v.Findings)
	}
}

// TestAggregate_NoChecksStagedBlocks: a validate that detected no checks is not green (an
// empty set of failures must never read as a pass), so it cannot underwrite a passing
// verdict either.
func TestAggregate_NoChecksStagedBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	stagePrep(t, dir, sha, nil) // no checks detected
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: a no-checks validate must not read as green", v.Overall, Block)
	}
}

// TestAggregate_PrepForAnotherCommitBlocks: the episode counts only when it was prepared
// for the commit under review. Inputs staged for an earlier commit say nothing about this
// one, so neither the reports nor the validate that came with them can underwrite a pass.
func TestAggregate_PrepForAnotherCommitBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	stagePrep(t, dir, "OTHERSHA00000", greenValidate())
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: inputs prepped for another commit must not underwrite a pass", v.Overall, Block)
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

// TestAggregate_MarkerContradictingStagedValidateBlocks: the marker is not the only record of
// the episode's validate. It is Go-written, but it sits in the same directory as everything
// else a review reads, so a marker claiming green while the staged validate the reviewers
// actually read says otherwise means one of the two has been altered. Neither can underwrite
// the review then, so the fold blocks rather than believing the marker.
func TestAggregate_MarkerContradictingStagedValidateBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	stagePrep(t, dir, sha, greenValidate())
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}
	// The staged validate now disagrees with the marker, which still records green.
	writeValidateJSON(t, dir, []validate.Result{{Name: "gate", Passed: false, Output: "boom"}})

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: the marker was trusted against the staged validate", v.Overall, Block)
	}
	if !findingMentions(v, "disagree") {
		t.Errorf("the verdict must say the two records disagree; findings = %+v", v.Findings)
	}
}

// TestAggregate_MissingStagedValidateBlocks: with the staged validate gone there is nothing to
// corroborate the marker against, so the fold blocks instead of taking the marker's word.
func TestAggregate_MissingStagedValidateBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()

	stagePrep(t, dir, sha, greenValidate())
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}
	if err := os.Remove(filepath.Join(dir, "validate.json")); err != nil {
		t.Fatal(err)
	}

	v, err := Aggregate(dir, sha, dims)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: an uncorroborated marker must not underwrite a pass", v.Overall, Block)
	}
}

// TestAggregate_AgreeingRecordsStillPass is the other half of the cross-check: when the two
// records agree, the guard costs nothing. Both the green case (passes) and the red case
// (blocks for the validate's own sake, not for a disagreement).
func TestAggregate_AgreeingRecordsStillPass(t *testing.T) {
	const sha = "abc123def456"

	t.Run("both green", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, sha, greenValidate())
		for _, d := range dims {
			writeReport(t, dir, d, sha, nil)
		}
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Pass {
			t.Fatalf("overall = %q, want %q: the cross-check tripped on agreeing records (findings: %+v)", v.Overall, Pass, v.Findings)
		}
	})

	t.Run("both red", func(t *testing.T) {
		dir := t.TempDir()
		red := []validate.Result{{Name: "gate", Passed: false, Output: "boom"}}
		stagePrep(t, dir, sha, red)
		for _, d := range dims {
			writeReport(t, dir, d, sha, nil)
		}
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Block {
			t.Fatalf("overall = %q, want %q", v.Overall, Block)
		}
		if findingMentions(v, "disagree") {
			t.Errorf("records that agree must not be reported as a disagreement; findings = %+v", v.Findings)
		}
	})
}

// TestValidateState reports the episode's validate state for one commit, and is what the audit
// line says. It must never describe a commit the episode does not cover, and must refuse to
// call a disputed or unreadable state green.
func TestValidateState(t *testing.T) {
	const sha = "abc123def456"

	t.Run("green", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, sha, greenValidate())
		if got := ValidateState(dir, sha); got != "green" {
			t.Fatalf("ValidateState = %q, want green", got)
		}
	})

	t.Run("failed names the steps", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, sha, []validate.Result{{Name: "build", Passed: true}, {Name: "test", Passed: false}})
		if got := ValidateState(dir, sha); got != "failed:test" {
			t.Fatalf("ValidateState = %q, want failed:test", got)
		}
	})

	t.Run("no checks", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, sha, nil)
		if got := ValidateState(dir, sha); got != "none" {
			t.Fatalf("ValidateState = %q, want none", got)
		}
	})

	t.Run("an episode for another commit is not this commit's state", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, "OTHERSHA00000", greenValidate())
		if got := ValidateState(dir, sha); got != "unprepped" {
			t.Fatalf("ValidateState = %q, want unprepped: another commit's outcome was reported as this one's", got)
		}
	})

	t.Run("disputed records are not green", func(t *testing.T) {
		dir := t.TempDir()
		stagePrep(t, dir, sha, greenValidate())
		writeValidateJSON(t, dir, []validate.Result{{Name: "gate", Passed: false}})
		if got := ValidateState(dir, sha); got != "disputed" {
			t.Fatalf("ValidateState = %q, want disputed", got)
		}
	})
}

// TestAggregate_DimensionNameCannotReachOutsideTheInputsDir: a dimension name is not
// configuration, it is a value read back from reviewers.json in a directory a worker can
// write. The fold builds a file path out of it, so a name carrying path separators reaches
// files the review dir does not contain. Here a perfectly valid report sits OUTSIDE the
// inputs dir and a traversing dimension name points at it; it must not satisfy that
// dimension.
func TestAggregate_DimensionNameCannotReachOutsideTheInputsDir(t *testing.T) {
	const sha = "abc123def456"
	parent := t.TempDir()
	dir := filepath.Join(parent, "review")
	outside := "../outside"

	stagePrep(t, dir, sha, greenValidate())
	for _, d := range dims {
		writeReport(t, dir, d, sha, nil)
	}
	// A clean, correctly pinned report the fold would happily accept, one level up.
	writeReport(t, parent, "outside", sha, nil)

	v, err := Aggregate(dir, sha, append(append([]string(nil), dims...), outside))
	if err != nil {
		t.Fatalf("a rejected dimension name must fold to a block, not a hard error: %v", err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: a report outside the inputs dir satisfied a dimension", v.Overall, Block)
	}
	if !findingMentions(v, "unusable dimension name") {
		t.Errorf("the verdict must say the dimension name was rejected; findings = %+v", v.Findings)
	}
	if ReportCurrent(dir, outside, sha) {
		t.Error("ReportCurrent accepted a report reached by traversing out of the inputs dir")
	}
}

// TestAggregate_EmptyDimensionSetBlocks: nothing was reviewed, so there is nothing to pass.
// An empty required set must read like a no-checks validate does, not like a clean sweep.
func TestAggregate_EmptyDimensionSetBlocks(t *testing.T) {
	const sha = "abc123def456"
	dir := t.TempDir()
	stagePrep(t, dir, sha, greenValidate())

	v, err := Aggregate(dir, sha, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != Block {
		t.Fatalf("overall = %q, want %q: an empty dimension set read as a pass", v.Overall, Block)
	}
}

// TestValidDimensionName pins the allow-list: what a dimension MAY be, rather than what it
// must not be. Every rejected case below either reaches another directory or collides with
// another dimension's file on a case-insensitive filesystem.
func TestValidDimensionName(t *testing.T) {
	ok := []string{"correctness", "scope", "security", "qa", "convention", "perf-2", "a_b", "x"}
	bad := []string{
		"", " ", ".", "..", "../outside", "a/b", `a\b`, "/etc/passwd", "~/.claude",
		"a.json", "a b", "Scope", "qa\x00", "café",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"-lead", "2fast",
	}
	for _, n := range ok {
		if !ValidDimensionName(n) {
			t.Errorf("ValidDimensionName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidDimensionName(n) {
			t.Errorf("ValidDimensionName(%q) = true, want false", n)
		}
	}
}

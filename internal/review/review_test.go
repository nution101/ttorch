package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/validate"
)

var dims = []string{"correctness", "scope", "security"}

// writeReport drops a per-dimension findings report into inputsDir, as a reviewer
// would after `ttorch trust prep`.
func writeReport(t *testing.T, inputsDir, dim, sha string, findings []Finding) {
	t.Helper()
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Report{Dimension: dim, ReviewedSHA: sha, Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputsDir, dim+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAggregate(t *testing.T) {
	const sha = "abc123def456"

	t.Run("all clean passes", func(t *testing.T) {
		dir := t.TempDir()
		for _, d := range dims {
			writeReport(t, dir, d, sha, nil)
		}
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Pass {
			t.Fatalf("want pass, got %q (findings: %+v)", v.Overall, v.Findings)
		}
	})

	t.Run("a high finding blocks", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, nil)
		writeReport(t, dir, "scope", sha, nil)
		writeReport(t, dir, "security", sha, []Finding{
			{Severity: SeverityHigh, Reviewer: "sec", Summary: "secret in diff"},
		})
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Block {
			t.Fatalf("a high finding must block, got %q", v.Overall)
		}
	})

	t.Run("low and medium findings do not block", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, []Finding{{Severity: SeverityLow, Summary: "nit"}})
		writeReport(t, dir, "scope", sha, []Finding{{Severity: SeverityMedium, Summary: "minor"}})
		writeReport(t, dir, "security", sha, nil)
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Pass {
			t.Fatalf("low/medium must not block, got %q", v.Overall)
		}
	})

	t.Run("a missing dimension blocks", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, nil)
		writeReport(t, dir, "security", sha, nil) // scope absent
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Block {
			t.Fatalf("a missing dimension must block, got %q", v.Overall)
		}
	})

	t.Run("a malformed report blocks", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, nil)
		writeReport(t, dir, "scope", sha, nil)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "security.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Block {
			t.Fatalf("a malformed report must block, got %q", v.Overall)
		}
	})

	t.Run("an unknown severity blocks", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, nil)
		writeReport(t, dir, "scope", sha, nil)
		writeReport(t, dir, "security", sha, []Finding{{Severity: Severity("weird"), Summary: "?"}})
		v, err := Aggregate(dir, sha, dims)
		if err != nil {
			t.Fatal(err)
		}
		if v.Overall != Block {
			t.Fatalf("an unrecognized severity must fail closed, got %q", v.Overall)
		}
	})

	t.Run("a report pinned to another commit errors", func(t *testing.T) {
		dir := t.TempDir()
		writeReport(t, dir, "correctness", sha, nil)
		writeReport(t, dir, "scope", sha, nil)
		writeReport(t, dir, "security", "OTHERSHA00000", nil)
		if _, err := Aggregate(dir, sha, dims); err == nil {
			t.Fatal("a report recorded against a different commit must error")
		}
	})
}

func TestWriteLoadConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.verdict")

	if _, ok := Load(path); ok {
		t.Fatal("no verdict yet, Load should be false")
	}
	v := Verdict{Overall: Pass, ReviewedSHA: "deadbeef"}
	if err := Write(path, v, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(path)
	if !ok || got.Overall != Pass || got.ReviewedSHA != "deadbeef" {
		t.Fatalf("Load = %+v ok=%v, want the written verdict", got, ok)
	}
	// Load does not consume.
	if _, ok := Load(path); !ok {
		t.Fatal("Load must not consume the verdict")
	}
	consumed, ok := Consume(path)
	if !ok || consumed.ReviewedSHA != "deadbeef" {
		t.Fatalf("Consume = %+v ok=%v, want the verdict", consumed, ok)
	}
	if _, ok := Load(path); ok {
		t.Fatal("verdict should be gone after Consume")
	}
	if _, ok := Consume(path); ok {
		t.Fatal("second Consume should fail")
	}
}

func TestExpiredVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.verdict")
	if err := Write(path, Verdict{Overall: Pass}, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(path); ok {
		t.Fatal("an expired verdict must not Load")
	}
	if _, ok := Consume(path); ok {
		t.Fatal("consuming an expired verdict must return false")
	}
}

// TestConsumeRefusesBlock pins the fail-closed contract: a blocking verdict can never be
// consumed as an authorization, even while unexpired (it is removed but returns ok=false),
// so a verdict that turned blocking after a prior Load cannot still authorize a merge.
func TestConsumeRefusesBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.verdict")
	if err := Write(path, Verdict{Overall: Block, ReviewedSHA: "deadbeef"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := Consume(path); ok {
		t.Fatal("a blocking verdict must not be consumable as an authorization")
	}
	if _, ok := Load(path); ok {
		t.Fatal("the blocking verdict should still have been removed by Consume")
	}
}

func TestToResults(t *testing.T) {
	v := Verdict{
		Overall: Block,
		Findings: []Finding{
			{Dimension: "security", Severity: SeverityHigh, Reviewer: "sec", Summary: "secret"},
			{Dimension: "correctness", Severity: SeverityCritical, Summary: "off-by-one in interest calc"},
			{Dimension: "scope", Severity: SeverityLow, Summary: "nit"}, // non-blocking → no result
		},
	}
	res := ToResults(v)
	if len(res) != 2 {
		t.Fatalf("want one result per blocking finding (2), got %d: %+v", len(res), res)
	}
	if len(validate.Failures(res)) != 2 {
		t.Fatalf("every blocking result must be a failure: %+v", res)
	}

	// A clean verdict renders as a single passing result.
	clean := ToResults(Verdict{Overall: Pass})
	if len(clean) != 1 || !clean[0].Passed {
		t.Fatalf("a clean verdict should render one passing result, got %+v", clean)
	}
}

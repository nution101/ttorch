// Package review implements the adversarial-review verdict: a commit-pinned,
// time-boxed, consume-once record of whether parallel reviewer subagents
// (correctness / scope / security) cleared a worker's diff.
//
// Each reviewer writes a per-dimension findings report ("<dimension>.json") into a
// task's review inputs dir; Aggregate folds the required dimensions into a single
// Verdict. The verdict body is written by this Go-owned code (Write), not free-typed
// by an LLM, so a missing or malformed report fails closed to "block" rather than
// silently passing. It is kept distinct from the human approval token (see
// internal/approval) so an audit can always tell "a human read this" from "the
// reviewers passed it"; like that token it is defense in depth and an audit trail,
// not an unbreakable barrier against a fully compromised manager.
package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nution101/ttorch/internal/validate"
)

// Overall verdict values.
const (
	Pass  = "pass"
	Block = "block"
)

// Severity grades a finding. Aggregate blocks on any High (or above); an
// unrecognized or empty severity is treated as blocking so a malformed report fails
// closed.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// blocking reports whether a finding at this severity must block the merge.
func (s Severity) blocking() bool {
	switch s {
	case SeverityLow, SeverityMedium:
		return false
	case SeverityHigh, SeverityCritical:
		return true
	default:
		return true // unknown/empty severity → fail closed
	}
}

// Finding is one issue a reviewer raised about the diff.
type Finding struct {
	Dimension string   `json:"dimension"` // correctness | scope | security
	Severity  Severity `json:"severity"`  // low | medium | high | critical
	Reviewer  string   `json:"reviewer"`  // free-form reviewer label
	Summary   string   `json:"summary"`
}

// Report is the JSON a single reviewer writes into the review inputs dir, one file
// per dimension named "<dimension>.json". It is commit-pinned via ReviewedSHA so a
// report recorded against a stale commit is rejected.
type Report struct {
	Dimension   string    `json:"dimension"`
	ReviewedSHA string    `json:"reviewedSha"`
	Findings    []Finding `json:"findings"`
}

// Verdict is the aggregated, commit-pinned outcome of adversarial review.
type Verdict struct {
	Overall     string    `json:"overall"` // pass | block
	ReviewedSHA string    `json:"reviewedSha"`
	Expires     int64     `json:"expires"` // unix nano
	Findings    []Finding `json:"findings,omitempty"`
}

// Aggregate folds the per-dimension reports in inputsDir into a single verdict for
// sha. Every dimension in dimensions must be present and pinned to sha: a missing or
// malformed report, or any finding at High severity or above, yields a "block"
// verdict (fail closed). A report present but recorded against a different commit is
// a hard error (a stale or mis-targeted review), not merely a block.
func Aggregate(inputsDir, sha string, dimensions []string) (Verdict, error) {
	v := Verdict{Overall: Pass, ReviewedSHA: sha}
	for _, dim := range dimensions {
		b, err := os.ReadFile(filepath.Join(inputsDir, dim+".json"))
		if err != nil {
			v.Overall = Block
			v.Findings = append(v.Findings, Finding{
				Dimension: dim, Severity: SeverityHigh, Reviewer: "ttorch",
				Summary: "no review recorded for dimension " + dim,
			})
			continue
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			v.Overall = Block
			v.Findings = append(v.Findings, Finding{
				Dimension: dim, Severity: SeverityHigh, Reviewer: "ttorch",
				Summary: "malformed review report for dimension " + dim + ": " + err.Error(),
			})
			continue
		}
		if r.ReviewedSHA != sha {
			return Verdict{}, fmt.Errorf("review for %q was recorded against %s, not the current %s", dim, short(r.ReviewedSHA), short(sha))
		}
		for _, f := range r.Findings {
			if f.Dimension == "" {
				f.Dimension = dim
			}
			v.Findings = append(v.Findings, f)
			if f.Severity.blocking() {
				v.Overall = Block
			}
		}
	}
	return v, nil
}

// Write persists v to path, stamping its expiry ttl from now. The file is 0o600,
// matching the approval token.
func Write(path string, v Verdict, ttl time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	v.Expires = time.Now().Add(ttl).UnixNano()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Load returns the verdict at path if a non-expired one exists (without consuming
// it).
func Load(path string) (Verdict, bool) {
	v, ok := read(path)
	if !ok || time.Now().UnixNano() >= v.Expires {
		return Verdict{}, false
	}
	return v, true
}

// Consume removes the verdict file and returns it if it was still valid (unexpired).
// A stale (expired) verdict is also removed.
func Consume(path string) (Verdict, bool) {
	v, present := read(path)
	if !present {
		return Verdict{}, false
	}
	_ = os.Remove(path)
	if time.Now().UnixNano() >= v.Expires {
		return Verdict{}, false
	}
	return v, true
}

func read(path string) (Verdict, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Verdict{}, false
	}
	var v Verdict
	if err := json.Unmarshal(b, &v); err != nil {
		return Verdict{}, false
	}
	return v, true
}

// ToResults renders a verdict's blocking findings as validate.Results — one failing
// result per blocking finding — so `ttorch trust show` reuses the build/test/lint
// PASS/FAIL printer and Failures() unchanged. A verdict with nothing blocking yields
// a single passing result.
func ToResults(v Verdict) []validate.Result {
	var out []validate.Result
	for _, f := range v.Findings {
		if !f.Severity.blocking() {
			continue
		}
		summary := f.Summary
		if f.Reviewer != "" {
			summary = "[" + f.Reviewer + "] " + summary
		}
		out = append(out, validate.Result{
			Name:   f.Dimension + " (" + string(f.Severity) + ")",
			Passed: false,
			Output: summary,
		})
	}
	if len(out) == 0 {
		out = append(out, validate.Result{Name: "review", Passed: true})
	}
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

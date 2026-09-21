package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/validate"
)

// PrepStampFile is the marker `ttorch trust prep` writes into a task's review inputs dir.
// It opens a review EPISODE: every prep re-materializes the diff, brief, and validate the
// reviewers read, which supersedes the reports written against the materialization before
// it, and the marker is what tells the two apart.
const PrepStampFile = "prep.json"

// StagedValidateFile is the gate's own validate of the reviewed commit, as prep stages it for
// the reviewers to read. The fold cross-checks the marker against it, so the name lives here
// rather than being spelled out separately by every writer and reader of it.
const StagedValidateFile = "validate.json"

// validateDimension labels the Go-authored finding that reports a gap in the episode's
// staged validate. It is not a review dimension — no reviewer writes a validate.json
// report — but carrying the gap as an ordinary blocking finding means every existing
// reader (ToResults, Describe, the daemon gate's block surfacing) renders it unchanged.
const validateDimension = "validate"

// PrepStamp is the Go-owned record of one review episode: when the inputs were
// materialized, the commit they cover, and the outcome of the gate's own validate of that
// commit. Aggregate folds it as the evidence a verdict needs BESIDES the reports — that
// the reports reviewed these inputs, and that the green-suite premise the reviewers are
// told to trust actually held.
//
// PreparedAt is for the audit trail, in UTC to match the archive directory prep names from
// the same moment. The freshness comparison uses the marker FILE's mtime instead (see
// readPrep), so both sides of it come from one filesystem clock at one granularity and a
// coarse-mtime filesystem cannot report a report written after the prep as older than it.
type PrepStamp struct {
	PreparedAt time.Time `json:"preparedAt"`
	Head       string    `json:"head"`
	// ValidateGreen is StagedGreen over the validate this prep staged: real checks ran
	// and none failed. False covers both a failing check and a repo where no checks were
	// detected — neither proves the suite passes at Head.
	ValidateGreen bool `json:"validateGreen"`
	// ValidateFailed names the checks that did not pass, so a degraded verdict can say
	// which step failed rather than just that something did. Empty with ValidateGreen
	// false means no checks ran at all.
	ValidateFailed []string `json:"validateFailed,omitempty"`
}

// StagedGreen reports whether a PERSISTED validate result set is green: at least one check
// ran and none failed. An empty or nil set is a no-checks-detected block, never a pass (an
// empty set of failures must not read as green) — the same rule the merge gate applies to
// the results it re-runs, kept here so the verdict fold and the merge gate cannot drift.
func StagedGreen(results []validate.Result) bool {
	return len(results) > 0 && len(validate.Failures(results)) == 0
}

// WritePrepStamp opens a review episode for inputsDir: it records head and the outcome of
// the validate prep just staged for it, and returns the marker it wrote (so a caller can
// report the episode's validate state without re-deriving it). Callers write it LAST, after
// the rest of the inputs, so the marker's mtime is the moment the episode's inputs were
// complete.
func WritePrepStamp(inputsDir, head string, results []validate.Result) (PrepStamp, error) {
	stamp := PrepStamp{
		PreparedAt:    time.Now().UTC(),
		Head:          head,
		ValidateGreen: StagedGreen(results),
	}
	for _, f := range validate.Failures(results) {
		stamp.ValidateFailed = append(stamp.ValidateFailed, f.Name)
	}
	b, err := json.MarshalIndent(stamp, "", "  ")
	if err != nil {
		return stamp, err
	}
	return stamp, os.WriteFile(filepath.Join(inputsDir, PrepStampFile), append(b, '\n'), 0o644)
}

// Label renders the episode's staged validate outcome as one audit-line token: "green",
// "none" when no checks were detected, or "failed:<steps>". Callers reporting a commit's
// state go through ValidateState, which corroborates the marker first.
func (s PrepStamp) Label() string {
	if s.ValidateGreen {
		return "green"
	}
	if len(s.ValidateFailed) == 0 {
		return "none"
	}
	return "failed:" + strings.Join(s.ValidateFailed, ",")
}

// ValidateState returns the one-token summary of the episode's staged validate FOR sha:
// "green", "none" when no checks ran, "failed:<steps>", "unprepped" when no episode covers
// sha, "unreadable" when the staged validate is gone, or "disputed" when the two records
// disagree. It is what an audit line reports, and it is derived from the same cross-checked
// view the verdict fold uses (validateGap), so a line can never describe a commit the verdict
// does not cover, nor call a state green that the fold blocked on.
func ValidateState(inputsDir, sha string) string {
	p := readPrep(inputsDir)
	switch {
	case !p.covers(sha):
		return "unprepped"
	case !p.stagedOK:
		return "unreadable"
	case StagedGreen(p.staged) != p.stamp.ValidateGreen:
		return "disputed"
	default:
		return p.stamp.Label()
	}
}

// maxDimensionNameLen bounds a dimension name. Nothing legitimate comes close; the bound
// keeps a name from producing an unusable filename.
const maxDimensionNameLen = 32

// ValidDimensionName reports whether name may be used as a dimension. Every dimension name
// becomes a file path ("<name>.json") in the review inputs dir, and names are read back from
// reviewers.json, which lives in a directory a worker can write. So a name is untrusted
// input, and this states what one MAY be rather than what it must not be: 1 to 32 characters,
// starting with a lowercase ASCII letter, continuing with lowercase ASCII letters, digits,
// '-' or '_'.
//
// An allow-list, because the deny-list version of this check has to anticipate every way a
// string can name another file: "..", "/", "\", an absolute path, a NUL, a trailing dot or
// space, a Windows drive or stream, a Unicode character that normalizes to a separator. Miss
// one and the check is decorative. Nothing in this set can name anything but a plain child of
// the directory it is joined to.
//
// Lowercase only, because macOS and Windows filesystems are case-insensitive: allowing
// "Scope" alongside "scope" would let two dimensions share one report file, so one
// reviewer's findings could stand in for another's.
func ValidDimensionName(name string) bool {
	if name == "" || len(name) > maxDimensionNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '_'):
		default:
			return false
		}
	}
	return true
}

// prepState is the episode the reports in an inputs dir are folded against: the marker's
// content, the moment it was written (its mtime), whether it could be read at all, and the
// staged validate the reviewers themselves read, which the marker is cross-checked against.
type prepState struct {
	stamp      PrepStamp
	preparedAt time.Time
	ok         bool
	staged     []validate.Result
	stagedOK   bool
}

// readPrep loads the episode marker from inputsDir. A missing or unparseable marker yields
// ok=false, which fails every report closed: without it nothing establishes which inputs
// the reports reviewed. (Inputs prepped by an older ttorch have no marker, so they re-prep
// and re-review rather than record on freshness that cannot be shown.)
func readPrep(inputsDir string) prepState {
	path := filepath.Join(inputsDir, PrepStampFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return prepState{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return prepState{}
	}
	var stamp PrepStamp
	if err := json.Unmarshal(b, &stamp); err != nil {
		return prepState{}
	}
	p := prepState{stamp: stamp, preparedAt: st.ModTime(), ok: true}
	// The second record of the same fact: the staged validate the REVIEWERS read. Read it
	// here so the fold can corroborate the marker instead of taking its word (see
	// validateGap). Unreadable or malformed leaves stagedOK false, which fails closed.
	if vb, err := os.ReadFile(filepath.Join(inputsDir, StagedValidateFile)); err == nil {
		var results []validate.Result
		if err := json.Unmarshal(vb, &results); err == nil {
			p.staged, p.stagedOK = results, true
		}
	}
	return p
}

// covers reports whether the episode was prepared for sha — the inputs the reports were
// meant to review are the ones staged for the commit being recorded.
func (p prepState) covers(sha string) bool {
	return p.ok && p.stamp.Head == sha
}

// validateGap returns the blocking summary a verdict must carry when the episode's staged
// validate does not underwrite it, and whether there is one. The reviewers are instructed
// to trust the staged validate as proof the repo's checks pass at the reviewed commit and
// NOT to re-run the suite, so a review recorded over a red — or absent — validate rests on
// a premise that never held, and a clean "pass" there would read as gated when the commit
// was never validated.
// It cross-checks the marker against the staged validate.json rather than trusting either
// alone. Both files sit in the same directory the reviewers and the worker can reach, and the
// marker is the only one the fold would otherwise read, so a marker that claims green while
// the file the reviewers actually read says otherwise means one of the two was altered. There
// is no safe way to pick a winner, so a disagreement blocks.
func (p prepState) validateGap(sha string) (string, bool) {
	if !p.covers(sha) {
		return fmt.Sprintf("no review prep is recorded for %s, so neither the inputs the reviewers read nor the state of the repo's checks at that commit can be established; re-run the review prep and review again", short(sha)), true
	}
	if !p.stagedOK {
		return fmt.Sprintf("the staged validate of %s is missing or unreadable, so the prep marker's record of the checks cannot be corroborated; re-run the review prep", short(sha)), true
	}
	if staged := StagedGreen(p.staged); staged != p.stamp.ValidateGreen {
		return fmt.Sprintf("the prep marker and the staged validate of %s disagree about the checks (marker says %s, %s says %s), so one of them was altered and neither can underwrite this review", short(sha), greenWord(p.stamp.ValidateGreen), StagedValidateFile, greenWord(staged)), true
	}
	if p.stamp.ValidateGreen {
		return "", false
	}
	if len(p.stamp.ValidateFailed) == 0 {
		return fmt.Sprintf("the staged validate of %s detected no checks, so nothing shows the repo's checks pass at the reviewed commit; this review does not gate the change", short(sha)), true
	}
	return fmt.Sprintf("the staged validate of %s is not green (failed: %s), so the review rests on a premise that does not hold; this review does not gate the change", short(sha), strings.Join(p.stamp.ValidateFailed, ", ")), true
}

// greenWord renders a greenness bit for a finding summary.
func greenWord(green bool) string {
	if green {
		return "green"
	}
	return "not green"
}

// currentReport reads dim's report from inputsDir and reports whether it is the report
// Aggregate may fold. reason is "" when it is; otherwise reason is the blocking summary
// explaining why not (absent, malformed, or superseded by a later prep). err is returned
// only for the hard case a caller must surface rather than block on: a report pinned to a
// different commit.
func currentReport(inputsDir, dim, sha string, prep prepState) (Report, string, error) {
	// Before the name becomes a path: a dimension whose name is not a plain label could
	// address a file the inputs dir does not contain, and a report found that way must never
	// stand in for a review of this diff.
	if !ValidDimensionName(dim) {
		return Report{}, fmt.Sprintf("unusable dimension name %q in the prepared reviewer set, so no report can be read for it", dim), nil
	}
	path := filepath.Join(inputsDir, dim+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, "no review recorded for dimension " + dim, nil
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, "malformed review report for dimension " + dim + ": " + err.Error(), nil
	}
	if r.ReviewedSHA != sha {
		return Report{}, "", fmt.Errorf("review for %q was recorded against %s, not the current %s", dim, short(r.ReviewedSHA), short(sha))
	}
	// The commit pin alone cannot catch a superseded report: a task that has sat idle has
	// not moved its HEAD, so a report from an earlier session still pins to the current
	// commit verbatim. Require the report to postdate the episode marker, so a report that
	// reviewed a previous materialization of the inputs is treated as ABSENT — which the
	// fold already handles fail-closed — rather than accepted as current.
	if !prep.covers(sha) {
		return Report{}, "no review recorded for dimension " + dim + " in the current review prep", nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return Report{}, "no review recorded for dimension " + dim, nil
	}
	if st.ModTime().Before(prep.preparedAt) {
		return Report{}, "the report for dimension " + dim + " predates the current review prep, so it reviewed superseded inputs; treated as no review recorded for dimension " + dim, nil
	}
	return r, "", nil
}

// ReportCurrent reports whether dim's report in inputsDir is exactly the report Aggregate
// will fold: present, parseable, pinned to sha, and written during the current review
// episode. The daemon gate uses it to decide whether a dimension still needs a reviewer, so
// a superseded or mis-pinned report is re-dispatched rather than folded into a verdict.
func ReportCurrent(inputsDir, dim, sha string) bool {
	_, reason, err := currentReport(inputsDir, dim, sha, readPrep(inputsDir))
	return reason == "" && err == nil
}

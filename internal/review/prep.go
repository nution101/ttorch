package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PrepStampFile is the marker `ttorch trust prep` writes into a task's review inputs dir.
// It opens a review EPISODE: every prep re-materializes the diff, brief, and validate the
// reviewers read, which supersedes the reports written against the materialization before
// it, and the marker is what tells the two apart.
const PrepStampFile = "prep.json"

// PrepStamp is the Go-owned record of one review episode: when the inputs were
// materialized and the commit they cover. Aggregate folds it as the evidence a verdict
// needs BESIDES the reports — that the reports it folds reviewed THESE inputs.
//
// PreparedAt is for the audit trail. The freshness comparison uses the marker FILE's mtime
// instead (see readPrep), so both sides of it come from one filesystem clock at one
// granularity and a coarse-mtime filesystem cannot report a report written after the prep
// as older than it.
type PrepStamp struct {
	PreparedAt time.Time `json:"preparedAt"`
	Head       string    `json:"head"`
}

// WritePrepStamp opens a review episode for inputsDir, recording the head its inputs were
// materialized for. Callers write it LAST, after the rest of the inputs, so the marker's
// mtime is the moment the episode's inputs were complete.
func WritePrepStamp(inputsDir, head string) error {
	stamp := PrepStamp{PreparedAt: time.Now(), Head: head}
	b, err := json.MarshalIndent(stamp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(inputsDir, PrepStampFile), append(b, '\n'), 0o644)
}

// prepState is the episode the reports in an inputs dir are folded against: the marker's
// content, the moment it was written (its mtime), and whether it could be read at all.
type prepState struct {
	stamp      PrepStamp
	preparedAt time.Time
	ok         bool
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
	return prepState{stamp: stamp, preparedAt: st.ModTime(), ok: true}
}

// covers reports whether the episode was prepared for sha — the inputs the reports were
// meant to review are the ones staged for the commit being recorded.
func (p prepState) covers(sha string) bool {
	return p.ok && p.stamp.Head == sha
}

// currentReport reads dim's report from inputsDir and reports whether it is the report
// Aggregate may fold. reason is "" when it is; otherwise reason is the blocking summary
// explaining why not (absent, malformed, or superseded by a later prep). err is returned
// only for the hard case a caller must surface rather than block on: a report pinned to a
// different commit.
func currentReport(inputsDir, dim, sha string, prep prepState) (Report, string, error) {
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

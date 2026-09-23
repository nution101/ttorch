package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Dimensions is the reviewer set this prep prepared, recorded here because it decides
	// what a verdict must cover. reviewers.json holds the same list for the manager to read
	// and edit; this copy is the one the record and the daemon resolve from, so changing the
	// required set means changing both consistently rather than editing one file. Both live
	// in the same writable directory, so this is a cost, not a boundary. See
	// RequiredDimensions.
	Dimensions []string `json:"dimensions,omitempty"`
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
func WritePrepStamp(inputsDir, head string, results []validate.Result, dims []string) (PrepStamp, error) {
	stamp := PrepStamp{
		PreparedAt:    time.Now().UTC(),
		Head:          head,
		ValidateGreen: StagedGreen(results),
		Dimensions:    append([]string(nil), dims...),
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

// ReportSuffix is the filename suffix of a per-dimension findings report. Paths are built
// with InputPath rather than joined by hand, so the name is always validated first.
const ReportSuffix = ".json"

// ReportsDirName is the subdirectory of the review inputs dir that holds the per-dimension
// reports. Reports are named by a dimension and the control files are not, so keeping them
// in separate namespaces is what stops the two from colliding: flat, a dimension called
// "prep", "validate", "reviewers" or "gate-progress" names a control file, and the charset
// check cannot tell those apart from any other legal name. Reserving the names that exist
// today would be one more list to keep complete; a directory cannot go out of date.
const ReportsDirName = "reports"

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

// RequiredDimensions reconciles the reviewer set a verdict must fold for sha from the two
// records of it: the set the prep stamped and the set now in reviewers.json. Between those
// two, the stamp DECIDES: it is written by the code that chose the set, at the moment it
// chose it, while the file exists to be read and edited by the manager between prep and
// record.
//
// It is not a trust boundary, and the difference matters. prep.json, reviewers.json and
// validate.json are all 0o644 in the same review inputs dir, written and read by processes
// running as the same user, so a worker can write any of them. What the stamp changes is
// the cost: dropping a dimension used to take one edit to reviewers.json, and now takes
// consistent edits to reviewers.json and prep.json, plus a fresh mtime on every surviving
// report (rewriting the stamp moves its own mtime, which fails the freshness check on every
// report older than it), plus a validate.json that still agrees with the stamp's
// validateGreen. Three files and their timestamps rather than one file. Someone who does
// all of it consistently gets a clean verdict, and that has been reproduced, not assumed.
//
// A real boundary is not available here: the worker runs as the same uid on the same
// machine, so anything this code can read or write, it can too, including a signing key or
// a row in the SQLite store. Making the set unforgeable needs the worker to run somewhere
// this process does not, which is a change to how ttorch runs workers, not a change to this
// function. The honest summary is that this raises cost and leaves evidence; it does not
// make the set authentic.
//
// The file may only ADD. A repo that runs a dimension beyond the built-in three appends it
// to reviewers.json after prep, and that is a real workflow, so the returned set is the
// union: more review than was prepared is never a problem. Dropping a stamped dimension is
// the attack, so dropped names are returned separately for the caller to block on rather
// than silently restored: a set that shrank after prep says the inputs dir was edited during
// the review, which the caller should surface, not paper over.
//
// ok is false when no stamp covers sha. The caller then has no stamped set and must fall
// back to its own fail-safe (the full built-in set); the fold blocks in that case anyway,
// since an episode that does not cover the commit cannot underwrite a verdict.
func RequiredDimensions(inputsDir, sha string, onDisk []string) (required, dropped []string, ok bool) {
	p := readPrep(inputsDir)
	if !p.covers(sha) {
		return nil, nil, false
	}
	stamped := p.stamp.Dimensions
	seen := map[string]bool{}
	for _, d := range stamped {
		seen[d] = true
		required = append(required, d)
	}
	for _, d := range onDisk {
		if !seen[d] {
			seen[d] = true
			required = append(required, d)
		}
	}
	have := map[string]bool{}
	for _, d := range onDisk {
		have[d] = true
	}
	for _, d := range stamped {
		if !have[d] {
			dropped = append(dropped, d)
		}
	}
	return required, dropped, true
}

// ValidateDimensionSet checks every name in a prepared reviewer set, returning an error
// naming the first unusable one. Consumers of the set call it before acting on the set at
// all: the set is read back from reviewers.json in a worker-writable directory, and its
// names go on to become file paths, tmux targets, and instructions printed for the manager.
// The bad name is quoted in the error (%q), so a name carrying newlines or control
// characters cannot put extra lines on whatever reads the message.
func ValidateDimensionSet(dims []string) error {
	for _, d := range dims {
		if !ValidDimensionName(d) {
			return fmt.Errorf("unusable review dimension name %q in the prepared reviewer set", d)
		}
	}
	return nil
}

// InputPath returns the path of the dimension-named file "<dim><suffix>" inside a review
// inputs dir, refusing any dimension name that could name something other than a plain child
// of that dir (ValidDimensionName). Every sink that turns a dimension into a path goes
// through here: the fold, the archive, and the reviewer launcher. Dimension names come from
// reviewers.json, which sits in a directory a worker can write, so the join is the dangerous
// operation and it lives in one place rather than at each call site, where the next sink
// added would have to remember the check.
func InputPath(inputsDir, dim, suffix string) (string, error) {
	if !ValidDimensionName(dim) {
		return "", fmt.Errorf("unusable review dimension name %q", dim)
	}
	return filepath.Join(inputsDir, ReportsDirName, dim+suffix), nil
}

// ReportsDir is where a task's per-dimension reports live. Prep creates it; the reviewers
// write into it.
func ReportsDir(inputsDir string) string {
	return filepath.Join(inputsDir, ReportsDirName)
}

// PinnedReportDimensions returns every dimension that has a report in inputsDir's reports
// directory pinned to sha and current for the episode (ReportCurrent). The trust gate folds
// these whether or not its own records say the dimension was dispatched.
//
// A pinned report is not proof of who wrote it: any process that can write the directory can
// write one. It does not need to be, for the use it is put to. A report found here is only
// ever ADDED to the fold, where it can contribute findings but cannot satisfy or remove a
// requirement.
//
// Only plain "<dimension>.json" children count, and only ones ValidDimensionName accepts, so
// a file dropped into the directory under any other name is ignored rather than turned into
// a dimension. A missing directory is not an error: it means no reviewer has reported yet.
func PinnedReportDimensions(inputsDir, sha string) []string {
	entries, err := os.ReadDir(ReportsDir(inputsDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ReportSuffix) {
			continue
		}
		dim := strings.TrimSuffix(e.Name(), ReportSuffix)
		if !ValidDimensionName(dim) || !ReportCurrent(inputsDir, dim, sha) {
			continue
		}
		out = append(out, dim)
	}
	sort.Strings(out)
	return out
}

// BlockingReportsPinnedTo returns every dimension whose report in inputsDir is pinned to sha
// and carries a blocking finding (Report.Blocks), read without the freshness check. These are
// the reports a prep of sha must carry forward rather than supersede, so a re-prep can make
// the verdict stricter and never looser.
//
// A report that will not parse cannot be shown to be pinned to sha and is left to be
// superseded as before. Failing to list the directory or read a report is returned: without
// the listing prep cannot tell whether it is about to discard a blocking finding, so the
// caller must refuse rather than guess. A missing directory means there are no reports.
func BlockingReportsPinnedTo(inputsDir, sha string) ([]string, error) {
	entries, err := os.ReadDir(ReportsDir(inputsDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not list the review reports in %s: %w", inputsDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ReportSuffix) {
			continue
		}
		dim := strings.TrimSuffix(e.Name(), ReportSuffix)
		if !ValidDimensionName(dim) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(ReportsDir(inputsDir), e.Name()))
		if err != nil {
			return nil, fmt.Errorf("could not read the %s review report in %s: %w", dim, inputsDir, err)
		}
		var r Report
		if json.Unmarshal(b, &r) != nil || r.ReviewedSHA != sha || !r.Blocks() {
			continue
		}
		out = append(out, dim)
	}
	sort.Strings(out)
	return out, nil
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
	path, err := InputPath(inputsDir, dim, ReportSuffix)
	if err != nil {
		return Report{}, fmt.Sprintf("unusable dimension name %q in the prepared reviewer set, so no report can be read for it", dim), nil
	}
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
	//
	// This is a staleness check, NOT a tamper check, and the distinction matters to anyone
	// reading it as a control: mtime is settable by anything that can write the report
	// (touch, os.Chtimes), so it catches a report left behind, not one backdated on purpose.
	// Nothing here defends against a writer of the inputs dir, which can forge a report
	// outright; see the package comment.
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

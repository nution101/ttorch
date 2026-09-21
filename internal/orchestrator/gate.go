package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/worktree"
)

// requiredReviewers is the full adversarial-review set and the fail-safe default. The
// actual set a given task's verdict must cover is scaled to the diff size by
// review.Reviewers (a docs-only or trivial diff gets a reduced set) and persisted at prep
// time; this set is what a task falls back to when that record is missing — so a verdict
// can only ever be recorded against MORE reviewers than were prepared, never fewer.
var requiredReviewers = []string{review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity}

// reviewersFileName is the prep-time, diff-derived record of which review dimensions the
// trust gate requires for a task — the scaled reviewer set. TrustPrep writes it from the
// staged diff so the manager knows which reviewers to spawn, and TrustRecord reads it back
// so aggregation requires EXACTLY the set that was prepared (no drift between dispatch and
// record). It lives beside the other review inputs in ReviewInputsDir.
const reviewersFileName = "reviewers.json"

// supersededDirName holds the reports of previous review episodes, one timestamped subdir
// per prep. It keeps the audit trail of what earlier reviewers said (including findings
// that were later adjudicated away) without leaving those reports where the current
// episode's belong.
const supersededDirName = "superseded"

// legacyReportsDirName holds reports swept from the old flat layout, kept apart from the
// current episode's so the archive says which layout each file came from.
const legacyReportsDirName = "legacy-flat"

// reviewerBriefSuffix names the prompt file the daemon writes for one dimension's reviewer.
// Like the report suffix it is joined through review.InputPath, never by hand.
const reviewerBriefSuffix = ".reviewer-brief.md"

// scaledReviewers is the persisted reviewer-set decision: the change-size class and the
// dimensions the trust gate requires for it. The manager reads it to spawn exactly those
// reviewer subagents.
type scaledReviewers struct {
	Size       review.Size `json:"size"`
	Dimensions []string    `json:"dimensions"`
}

// ReviewersFor returns the reviewer set recorded in reviewers.json for taskID: the manager's
// readable, editable copy of what prep prepared, which a repo running an extra dimension
// appends to after prep. A missing or malformed record (e.g. inputs prepared by an older
// ttorch, or an empty set) falls back to the full set.
//
// It is NOT the authority on what a verdict must cover. The file lives in the review inputs
// dir, so anything that can write there can rewrite it between the review and the record;
// the authority is the set prep stamped into prep.json, and requiredDimensions reconciles
// the two (the stamp is the floor, the file may only add).
func (m *Manager) ReviewersFor(taskID string) []string {
	b, err := os.ReadFile(filepath.Join(m.P.ReviewInputsDir(taskID), reviewersFileName))
	if err != nil {
		return append([]string(nil), requiredReviewers...)
	}
	var s scaledReviewers
	if err := json.Unmarshal(b, &s); err != nil || len(s.Dimensions) == 0 {
		return append([]string(nil), requiredReviewers...)
	}
	return s.Dimensions
}

// requiredDimensions resolves the reviewer set a verdict for sha must fold, and reports any
// dimension the stamp prepared that reviewers.json no longer lists. Between the two records
// the stamp decides, and neither is out of a worker's reach: see review.RequiredDimensions
// for what that buys and what it does not. With no stamp covering sha there is nothing to
// decide from, so it fails safe to the full built-in set unioned with whatever the file
// says, never to fewer.
func (m *Manager) requiredDimensions(taskID, sha string) (required, dropped []string) {
	onDisk := m.ReviewersFor(taskID)
	required, dropped, ok := review.RequiredDimensions(m.P.ReviewInputsDir(taskID), sha, onDisk)
	if ok {
		return required, dropped
	}
	seen := map[string]bool{}
	for _, d := range append(append([]string(nil), requiredReviewers...), onDisk...) {
		if !seen[d] {
			seen[d] = true
			required = append(required, d)
		}
	}
	return required, nil
}

// droppedDimensionFinding is the blocking finding for a prepared set that shrank after prep.
// A dimension vanishing from reviewers.json between the review and the record means the
// inputs dir was edited mid-review, which is the shape of hiding a blocking report, so it is
// surfaced rather than quietly corrected by the union.
func droppedDimensionFinding(dropped []string) review.Finding {
	return review.Finding{
		Dimension: "review", Severity: review.SeverityHigh, Reviewer: "ttorch",
		Summary: fmt.Sprintf("the prepared reviewer set on disk no longer lists %s, which the prep for this commit prepared; the review inputs were edited after the prep", quotedNames(dropped)),
	}
}

// quotedNames renders dimension names for a message a person reads. The names come from
// files in the review inputs dir, so they are quoted even here, where they have usually
// already passed validation: a summary is composed before anyone knows which path produced
// it, and an unquoted name in a sentence is how the last three of these started.
func quotedNames(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, review.SafeQuote(n))
	}
	return strings.Join(out, ", ")
}

// ReviewDiff returns a worker's changes against the repo's default branch.
func (m *Manager) ReviewDiff(taskID string, stat bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	base := worktree.DefaultBranch(t.Project)
	return worktree.Diff(t.Worktree, base, stat)
}

// gitOut runs `git -C dir <args...>` and returns its raw stdout, enriching the error with
// git's stderr on failure. The trust-prep path uses it to shell `git rev-list` and `git
// diff` directly: the equivalent reads live in internal/worktree, but a sibling change owns
// that file, so the trust gate runs these here rather than collide with it.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// mergeBaseDiff returns the COMMITTED three-dot diff `git diff base...rev`: the diff from
// the merge-base of base and rev to rev, i.e. ONLY rev's own changes. The trust gate stages
// this for the reviewers so any lead base gained since rev was cut never appears — a two-dot
// `git diff base rev` renders that lead as phantom reverts, which burned a full
// three-reviewer pass and nearly masked a real bug (the cosign-strict / liveness-dwell
// near-miss). Reads committed objects only, never a working tree.
func mergeBaseDiff(dir, base, rev string) (string, error) {
	return gitOut(dir, "diff", base+"..."+rev)
}

// reviewBase resolves the ref the trust gate diffs a worker's branch against — the TRUE
// base of the branch, i.e. the up-to-date default tip the merge will fast-forward onto,
// chosen exactly as the land path picks it (see landBase). Diffing against this base rather
// than the raw local <default> branch is what keeps the reviewed three-dot diff honest:
// when the LOCAL default is BEHIND origin (a release merged on origin but never pulled
// locally), the commits already on origin are part of the worker's branch history, so a
// diff rooted at the stale local default surfaces them as the worker's OWN changes —
// phantom scope-creep that burns a re-gate and misleads reviewers. origin/<default> shares
// the branch's real fork point, so the three-dot diff there is ONLY the worker's changes.
//
// When fetch is true a best-effort `git fetch` first refreshes origin/<default> so a
// release that landed AFTER the worker spawned is seen too. The fetch is non-fatal: offline
// (or a repo with no origin) it degrades to the last-known origin/<default> and ultimately
// the local default, so review still works without a network. landBase already falls back
// to the local default when origin is absent or behind (an unpushed local fast-forward), so
// this never bases a diff on a ref the merge would not actually target; the merge gate's own
// authoritative fetch+rebase catches a base that is still behind here.
func reviewBase(repo string, fetch bool) (string, error) {
	def := worktree.DefaultBranch(repo)
	hasOrigin := worktree.RemoteExists(repo, "origin")
	if fetch && hasOrigin {
		// Best-effort: a stale origin/<default> still beats the local default, and a branch
		// that is genuinely behind is caught by the merge gate's own fetch+rebase. Unlike
		// landPrep's fetch (guarded by fetchMu against the concurrent LandSet fan-out), this
		// one is intentionally unguarded: TrustPrep/TrustRecord are serialized manager gate
		// steps, and `git fetch` takes its own ref-store locks, so a redundant concurrent
		// fetch is at worst wasted work, never corrupting.
		_ = worktree.Fetch(repo)
	}
	ref, _, err := landBase(repo, def, hasOrigin)
	if err != nil {
		return "", err
	}
	return ref, nil
}

// diffFiles returns the AUTHORITATIVE list of paths the committed three-dot diff base...rev
// touches, via `git diff --name-only -z`: NUL-separated and UNQUOTED regardless of
// core.quotePath, so a path with tabs, control characters, quotes, backslashes, or
// non-ASCII bytes — which the patch body would quote — still appears in full and is never
// silently dropped. ok is false on any git error so the caller fails closed (full reviewer
// set). This is the source of truth for size classification; the patch body is never
// scraped for filenames, because a dropped quoted path could hide a malicious code file
// behind a docs-only edit and skip the security reviewer.
func diffFiles(dir, base, rev string) (files []string, ok bool) {
	out, err := gitOut(dir, "diff", "--name-only", "-z", base+"..."+rev)
	if err != nil {
		return nil, false
	}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, true
}

// diffLineStat returns the total added+removed content lines and whether any file is
// binary for base...rev, via `git diff --numstat`. It reads only the two leading numeric
// fields of each record (added, removed) and never the path, so path quoting is
// irrelevant here. A binary file reports added/removed as "-"; that sets binary and is not
// counted. ok is false on a git error or any unparseable record, so the caller fails
// closed to the full reviewer set rather than under-counting a large change as trivial.
func diffLineStat(dir, base, rev string) (lines int, binary, ok bool) {
	out, err := gitOut(dir, "diff", "--numstat", base+"..."+rev)
	if err != nil {
		return 0, false, false
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		fields := strings.SplitN(ln, "\t", 3)
		if len(fields) < 3 {
			return 0, false, false // malformed record — fail closed
		}
		added, removed := fields[0], fields[1]
		if added == "-" || removed == "-" {
			binary = true // binary file: size unknowable, do not count
			continue
		}
		a, errA := strconv.Atoi(added)
		r, errR := strconv.Atoi(removed)
		if errA != nil || errR != nil {
			return 0, false, false // unparseable counts — fail closed
		}
		lines += a + r
	}
	return lines, binary, true
}

// TrustPrep materializes the inputs the adversarial reviewers read for taskID into
// ReviewInputsDir: the COMMITTED three-dot diff against the branch's TRUE base (diff.patch),
// the brief (brief.md, if one was written), a fresh validate of the committed sha
// (validate.json), and the reviewed HEAD (head.txt). It refuses a dirty worktree and reads
// only committed objects, so the reviewers see exactly the commit that will fast-forward —
// a worker cannot present a benign working tree while a different commit merges.
//
// Each prep opens a new review EPISODE: it archives the previous episode's per-dimension
// reports into superseded/ and stamps the dir (review.PrepStamp). Re-materializing the
// inputs invalidates any review written against the materialization before it, and the
// commit pin cannot catch that on its own — a task that has sat idle has not moved its
// HEAD, so a report from an earlier session still pins to head.txt verbatim. The stamp is
// what makes such a report fold as ABSENT (fail closed) instead of current, and it carries
// the staged validate's outcome so a verdict can never read as gated over a red suite.
//
// The diff base is the up-to-date default tip the merge actually targets (reviewBase:
// origin/<default> when current, fetched best-effort), NOT the raw local <default> branch.
// A local default that is behind origin (e.g. a release merged on origin but never pulled
// locally) would otherwise root the diff before commits already on origin and surface them
// as the worker's own changes — phantom scope-creep that burns a re-gate and misleads
// reviewers.
//
// It also refuses a STALE BASE up front: if that base carries commits the worker's HEAD
// lacks, prep fails (staging nothing) and tells the manager to rebase the worker first.
// Reviewing a stale-base branch diffs against a base that no longer matches what merges, and
// the merge gate would refuse the fast-forward anyway. It returns the inputs dir.
func (m *Manager) TrustPrep(taskID string) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	// Reviewed state must equal the committed state that will merge.
	if clean, err := worktree.IsClean(t.Worktree); err != nil || !clean {
		return "", fmt.Errorf("worktree for %q is not clean; commit or discard changes before review so the reviewers see exactly the committed diff that will merge", taskID)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
	}
	def := worktree.DefaultBranch(t.Project)
	// Resolve the branch's TRUE base — the up-to-date default tip the merge targets, not the
	// raw local <default> which may be behind origin (see reviewBase). A best-effort fetch
	// refreshes origin/<default> first so a release that landed after the worker spawned is
	// caught here too.
	base, err := reviewBase(t.Project, true)
	if err != nil {
		return "", fmt.Errorf("trust prep %q: could not resolve the branch's base against %s: %w", taskID, def, err)
	}

	// Stale-base guard — run BEFORE staging any inputs or dispatching reviewers. If the base
	// carries commits the worker's HEAD lacks, the branch was cut from an older base: the
	// merge gate would refuse the fast-forward anyway, and a base-relative review diff would
	// render the base's own lead as phantom reverts — which burned a full three-reviewer pass
	// and nearly masked a real bug (the cosign-strict / liveness-dwell near-miss). `git
	// rev-list <head>..<base>` lists exactly the commits the base has that the worker lacks;
	// any output means the base is stale. Fail loudly so the manager rebases the worker onto
	// the current default first, and stage nothing.
	behind, err := gitOut(t.Worktree, "rev-list", head+".."+base)
	if err != nil {
		return "", fmt.Errorf("trust prep %q: could not check whether the branch is based on the current %s: %w", taskID, def, err)
	}
	if behind = strings.TrimSpace(behind); behind != "" {
		return "", fmt.Errorf("trust prep: the branch for %q is %d commit(s) behind %s and its base is stale; have the worker rebase onto the current %s before review, then re-run 'ttorch trust prep %s'", taskID, len(strings.Split(behind, "\n")), def, def, taskID)
	}

	dir := m.P.ReviewInputsDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The reports live in their own subdirectory so a dimension name can never name a
	// control file (see review.ReportsDirName). Prep creates it; the reviewers write there.
	if err := os.MkdirAll(review.ReportsDir(dir), 0o755); err != nil {
		return "", err
	}
	// Open the new episode by moving the previous one's reports out of the way, so a report
	// written for superseded inputs is simply not there to be folded. Best-effort: the
	// episode stamp written below rejects a surviving report anyway, so failing to archive
	// one degrades to a re-review, never to counting it.
	m.archivePriorReports(taskID, dir)
	// The reviewers' diff is the COMMITTED three-dot diff `git diff <base>...<head>` (the
	// merge-base diff against the branch's true base), so it contains ONLY the branch's own
	// changes — never any lead the default gained since the branch was cut. The stale-base
	// guard above makes <base> an ancestor of <head> here, but the three-dot form is the
	// correct, intent-revealing way to diff a branch against its base, and is defense in
	// depth against a phantom-revert diff. Reads committed objects only, never the working
	// tree.
	diff, err := mergeBaseDiff(t.Worktree, base, head)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "diff.patch"), []byte(diff), 0o644); err != nil {
		return "", err
	}
	// Scale the reviewer set to the diff size and persist the decision, so the manager
	// spawns exactly these reviewers and TrustRecord aggregates against the same set. A
	// docs-only or trivial diff gets a reduced set; anything else (including an uncertain
	// diff) gets the full three-dimension pass. Security is dropped only for docs-only
	// changes — never for code.
	//
	// Classification reads an AUTHORITATIVE file list and stat from git (not the patch
	// body): `git diff --name-only -z` lists every path UNQUOTED, so a worker cannot hide a
	// malicious code file behind a quoted/non-ASCII name to misclassify a code diff as
	// docs-only and skip the security reviewer. Any git failure fails closed to the full
	// set via the ok flags.
	files, filesOK := diffFiles(t.Worktree, base, head)
	lines, binary, statOK := diffLineStat(t.Worktree, base, head)
	size, dims := review.Classify(files, lines, binary, filesOK && statOK)
	sb, err := json.MarshalIndent(scaledReviewers{Size: size, Dimensions: dims}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, reviewersFileName), append(sb, '\n'), 0o644); err != nil {
		return "", err
	}
	// A worker may run without a written brief; copy it only when present.
	if b, err := os.ReadFile(m.P.BriefPath(taskID)); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), b, 0o644); err != nil {
			return "", err
		}
	}
	// validate.json reflects the gate's own check of the committed sha (default-branch
	// definition, immutable checkout) — the same notion of "green" the gate enforces.
	_, results, _ := validateCommitted(t.Project, head) // nil when no checks are detected
	vb, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, review.StagedValidateFile), append(vb, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), []byte(head+"\n"), 0o644); err != nil {
		return "", err
	}
	// LAST, once every input is staged: stamp the episode. Its mtime is the line a report
	// must postdate to count as this episode's, and it records the staged validate's outcome
	// AND the reviewer set this prep prepared, so the fold reads one Go-owned marker rather
	// than trusting files the review dir's writer can rewrite between prep and record.
	stamp, err := review.WritePrepStamp(dir, head, results, dims)
	if err != nil {
		return "", err
	}
	m.audit(fmt.Sprintf("trust-prep task=%s commit=%s size=%s reviewers=%s validate=%s",
		taskID, short(head), size, strings.Join(dims, "+"), stamp.Label()))
	return dir, nil
}

// archivePriorReports moves any per-dimension reports left in dir into a timestamped
// superseded/ subdir, so the episode a prep opens starts with no report in place while the
// earlier review is still readable. Reviews have been worth going back to — an adjudicated
// finding, a reviewer that named a gap — so they are archived rather than deleted.
// Best-effort by design: correctness rests on the episode stamp (a report that predates it
// folds as absent), and this only keeps a superseded review from sitting where the current
// one belongs.
//
// The set it archives is the set the PREVIOUS episode prepared, read from the reviewers.json
// this prep has not overwritten yet, so a repo that requires a dimension beyond the gate's
// built-in three (appended to the prepared set after prep) has that dimension's superseded
// report moved too. A fixed list would leave exactly those reports sitting where the current
// episode's belong. The gate's own set and the advisory QA audit are unioned in, so a
// dimension DROPPED from the prepared set, or a missing/malformed record, is still covered.
// Names from that record are validated before they are used as paths, since the file they
// come from is worker-reachable (see review.ValidDimensionName).
func (m *Manager) archivePriorReports(taskID, dir string) {
	var dims []string
	seen := map[string]bool{}
	for _, d := range append(append(m.ReviewersFor(taskID), requiredReviewers...), review.DimensionQA) {
		if !seen[d] {
			seen[d] = true
			dims = append(dims, d)
		}
	}
	var archive string
	ensureArchive := func() bool {
		if archive != "" {
			return true
		}
		// The archive keeps the same shape as the inputs dir, reports under reports/, so a
		// restored file goes back where it came from and the namespaces stay apart.
		archive = filepath.Join(dir, supersededDirName, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.MkdirAll(review.ReportsDir(archive), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ttorch: could not archive the previous review reports in %s: %v\n", dir, err)
			archive = ""
			return false
		}
		return true
	}
	for _, dim := range dims {
		// reviewers.json sits in a directory a worker can write, so a dimension name read
		// back from it is untrusted input, and this loop would otherwise make it both halves
		// of an os.Rename. A name that is not a plain label names a file this review has
		// nothing to do with, so it is skipped rather than repaired: there is no correct
		// archive destination for it, and a worker should not be able to steer a move at all.
		report, err := review.InputPath(dir, dim, review.ReportSuffix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ttorch: ignoring %v in %s\n", err, dir)
			continue
		}
		if _, err := os.Stat(report); err != nil {
			continue
		}
		if !ensureArchive() {
			return
		}
		dest, err := review.InputPath(archive, dim, review.ReportSuffix)
		if err != nil {
			continue // unreachable: the name was validated above, and this states why
		}
		if err := os.Rename(report, dest); err != nil {
			fmt.Fprintf(os.Stderr, "ttorch: could not archive the previous %s review report in %s: %v\n", dim, dir, err)
		}
	}
	m.archiveLegacyFlatReports(dir, ensureArchive, func() string { return archive })
}

// archiveLegacyFlatReports sweeps reports left at the TOP of an inputs dir by a ttorch that
// wrote them there, before reports moved into reports/. Nothing reads them any more (the
// fold looks only in reports/, and a dimension with no report there blocks), so this is
// tidying rather than a fix: it stops superseded reviews accumulating beside the control
// files, where the next person to look has to work out which layout each file came from.
//
// A file is swept only if it PARSES as a review report carrying both a dimension and a
// reviewed sha. That is the discriminator rather than a list of control-file basenames to
// avoid, because a list of names to avoid is what put a report on top of gate-progress.json
// in the first place: prep.json, validate.json, reviewers.json, gate-progress.json and the
// advisory verdict files carry none of those fields, so none of them can match, however the
// dimension set is named.
func (m *Manager) archiveLegacyFlatReports(dir string, ensureArchive func() bool, archive func() string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), review.ReportSuffix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r review.Report
		if err := json.Unmarshal(b, &r); err != nil || r.Dimension == "" || r.ReviewedSHA == "" {
			continue // not a report: a control file, an advisory verdict, or something else
		}
		if !ensureArchive() {
			return
		}
		dest := filepath.Join(archive(), legacyReportsDirName, e.Name())
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			legacySweepError(e.Name(), dir, err)
			continue
		}
		if err := os.Rename(path, dest); err != nil {
			legacySweepError(e.Name(), dir, err)
		}
	}
}

// legacySweepError reports a sweep failure without letting the filename write the message.
// This is the one name in the gate that nothing validates: a filename may contain anything
// but a slash and a NUL, so a worker that arranges the rename to fail (a directory already
// sitting at the destination, say) chooses the text. The name is quoted and the error is
// flattened to one line, because os.Rename's error repeats both paths inside itself.
func legacySweepError(name, dir string, err error) {
	fmt.Fprintf(os.Stderr, "ttorch: could not archive the legacy review report %s in %s: %s\n",
		review.SafeQuote(name), dir, review.SafeLine(err.Error()))
}

// TrustRecord aggregates the reviewers' per-dimension reports for taskID into a
// commit-pinned verdict and persists it as a durable DB row (the authoritative source
// the merge gate later reads), recording GatePassed/ReviewedSHA on the task in the same
// transaction. The sha it covers must still be the worker's HEAD (a record-time pin
// against a commit landing after review).
//
// In every mode except trusted the verdict authorizes nothing on its own — it is
// advisory, and a merge still requires the human approval token. In trusted mode a
// PASS verdict whose worktree is also fresh-validate green auto-mints the approval
// token (ApprovedBy="auto"): this is the "merge without a human reading the diff"
// path. A no-checks-detected repo is NOT green (an empty Failures() must never read as
// a pass), so it never auto-approves; the same fail-closed re-check runs again at the
// merge gate in MergeLocal.
func (m *Manager) TrustRecord(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("review covers %s but the worker HEAD is now %s; re-run 'ttorch trust prep %s' and review again", short(sha), short(head), taskID)
	}
	required, dropped := m.requiredDimensions(taskID, sha)
	if err := review.ValidateDimensionSet(required); err != nil {
		return zero, err
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, required)
	if err != nil {
		return zero, err
	}
	if len(dropped) > 0 {
		verdict.Overall = review.Block
		verdict.Findings = append(verdict.Findings, droppedDimensionFinding(dropped))
	}
	// Pin the reviewed diff's content identity onto the verdict (the committed three-dot diff
	// the reviewers read) so a later clean rebase onto an advanced default can carry the
	// verdict forward without re-running the reviewers — see carryVerdictForward. It MUST be
	// computed against the SAME true base prep staged the reviewed diff against (reviewBase),
	// so the fingerprint identifies exactly the worker's own changes and matches what
	// carryVerdictForward recomputes against the land base at merge time; pinning against a
	// stale local <default> would fingerprint phantom commits and force a needless re-gate.
	// No fetch here: prep just refreshed origin/<default>, and the three-dot diff against it
	// is stable as origin advances (the merge-base stays the branch's fork point). Computed
	// from committed objects, so it is independent of the worktree state.
	reviewedBase, berr := reviewBase(t.Project, false)
	if berr != nil {
		return zero, fmt.Errorf("trust record %q: could not resolve the reviewed diff base: %w", taskID, berr)
	}
	patch, derr := mergeBaseDiff(t.Worktree, reviewedBase, sha)
	if derr != nil {
		return zero, fmt.Errorf("trust record %q: could not compute the reviewed diff identity: %w", taskID, derr)
	}
	verdict.DiffID = review.DiffID([]byte(patch))
	t.ReviewedSHA = sha
	t.GatePassed = verdict.Overall == review.Pass
	t.ApprovedBy = ""
	approvalSHA := ""
	// Trusted mode is the sole carve-out: a PASS verdict auto-mints the approval token so
	// the lead need not read the diff — but ONLY when the worktree is clean (reviewed
	// state == the committed HEAD that will merge), the worktree passes the gate's fresh
	// validate resolved from the DEFAULT BRANCH (not the worker's own copy), and the diff
	// does not touch the gate definition itself (changing the gate requires a human). The
	// token is bound to the reviewed sha so a later commit invalidates it. All of these
	// are re-checked at the merge in MergeLocal — minting here is an optimization, not the
	// authority. Any non-trusted mode leaves the verdict advisory.
	if verdict.Overall == review.Pass && projectinit.ReadMode(t.Project) == "trusted" {
		base := worktree.DefaultBranch(t.Project)
		clean, cerr := worktree.IsClean(t.Worktree)
		touched, _, terr := diffTouchesGateConfig(t.Project, base, sha)
		green := false
		// A trusted auto-mint's green authority MUST be the default-branch gate script,
		// never ecosystem detection on the worker's checkout (which the worker controls
		// via go.mod/package.json). Without it, leave the verdict advisory — a human must
		// approve — and skip validation entirely so no worker-defined checks run.
		if cerr == nil && terr == nil && clean && !touched && hasDefaultBranchGateScript(t.Project) {
			green, _, _ = validateCommitted(t.Project, sha)
		}
		if green {
			if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("auto", sha)); err != nil {
				return zero, err
			}
			t.ApprovedBy = "auto"
			approvalSHA = sha
		}
	}
	// Persist the DURABLE verdict row AND the flattened summary (gate/approval/sha) in ONE
	// transaction, so the authoritative verdict the merge gate reads and its summary
	// columns can never drift. The verdict is content-pinned (reviewed_sha + diff_id) and
	// carries no expiry — it stays valid until a genuine content change supersedes it, so a
	// merge is never forced to re-gate by file-TTL expiry (the documented re-review loop).
	// The review_recorded event is manager-authored and non-actionable (§1.3). t was
	// mutated above as the accumulator for the auto-mint decision.
	dv, err := toDBVerdict(taskID, verdict, t.ApprovedBy, approvalSHA)
	if err != nil {
		return zero, err
	}
	if err := m.Store.RecordDelivery(context.Background(), taskID, db.Delivery{
		GatePassed: t.GatePassed, ApprovedBy: t.ApprovedBy, ReviewedSHA: t.ReviewedSHA,
		EventType: db.EventReviewRecorded, Actor: db.ActorManager, Verdict: &dv,
	}); err != nil {
		return zero, err
	}
	autoMinted := "no"
	if t.ApprovedBy == "auto" {
		autoMinted = "yes"
	}
	// The audit line names the episode's validate state, so a reader of the trail can tell a
	// verdict recorded over a green suite from one degraded by a validate that never ran or
	// failed — the same thing the verdict's own findings say. It is resolved FOR sha through
	// the same cross-checked view the fold used, so the line cannot report another commit's
	// outcome beside this commit's verdict, nor call a state green that the fold blocked on.
	m.audit(fmt.Sprintf("trust-record task=%s commit=%s verdict=%s mode=%s auto-approved=%s validate=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project), autoMinted,
		review.ValidateState(m.P.ReviewInputsDir(taskID), sha)))
	return verdict, nil
}

// TrustShow returns the current durable verdict for taskID, if any, without consuming
// it. It reads the authoritative DB row (not a TTL'd file): the verdict is present until
// a gated merge consumes it or a re-record replaces it, never expiring by age.
func (m *Manager) TrustShow(taskID string) (review.Verdict, bool) {
	dv, ok, err := m.Store.GetVerdict(context.Background(), taskID)
	if err != nil || !ok {
		return review.Verdict{}, false
	}
	v, err := fromDBVerdict(dv)
	if err != nil {
		return review.Verdict{}, false
	}
	return v, true
}

// toDBVerdict projects a review.Verdict (plus the approval token's provenance and the
// commit it authorizes) into the storable db.Verdict, marshaling the findings to JSON.
// The db layer stores findings as an opaque blob, so the review types never leak into it.
func toDBVerdict(taskID string, v review.Verdict, approvedBy, approvalSHA string) (db.Verdict, error) {
	// A clean verdict has nil findings, which json.Marshal renders as "null"; store the
	// column's documented empty-array form ("[]") instead so the persisted shape always
	// matches the schema's JSON-array contract.
	findings := "[]"
	if len(v.Findings) > 0 {
		fb, err := json.Marshal(v.Findings)
		if err != nil {
			return db.Verdict{}, err
		}
		findings = string(fb)
	}
	return db.Verdict{
		TaskID:      taskID,
		Overall:     v.Overall,
		ReviewedSHA: v.ReviewedSHA,
		DiffID:      v.DiffID,
		Findings:    findings,
		ApprovedBy:  approvedBy,
		ApprovalSHA: approvalSHA,
	}, nil
}

// fromDBVerdict reconstructs a review.Verdict from a stored db.Verdict, unmarshaling the
// findings JSON. The verdict carries no Expires (the durable row is content-pinned, not
// time-boxed); nothing on the read path consults it.
func fromDBVerdict(dv db.Verdict) (review.Verdict, error) {
	var findings []review.Finding
	if dv.Findings != "" {
		if err := json.Unmarshal([]byte(dv.Findings), &findings); err != nil {
			return review.Verdict{}, err
		}
	}
	return review.Verdict{
		Overall:     dv.Overall,
		ReviewedSHA: dv.ReviewedSHA,
		DiffID:      dv.DiffID,
		Findings:    findings,
	}, nil
}

// securityVerdictPath is where the standalone, advisory security-everywhere verdict
// lives — beside the review inputs the security reviewer reads, and DISTINCT from the
// trust gate's durable DB verdict so the two never interfere: the advisory pass can
// never mint an approval or satisfy the trusted gate, and recording it never disturbs a
// trust verdict. It stays a file because it is purely advisory and never gates a merge.
func (m *Manager) securityVerdictPath(taskID string) string {
	return filepath.Join(m.P.ReviewInputsDir(taskID), "security-verdict.json")
}

// SecurityReview folds ONLY the security reviewer's report for taskID into a
// commit-pinned verdict and persists it as an advisory result — the security-everywhere
// pass that runs in every delivery mode, not just trusted. It reuses the same inputs
// (materialized by TrustPrep) and the same internal/review aggregation as the trust
// gate, but it is purely advisory: it never mints an approval, never touches the trust
// gate's verdict or the task's gate state, and never blocks a merge. The manager
// surfaces its findings; in non-trusted modes the human approval still governs delivery,
// and in trusted mode the full three-dimension gate (which already includes security) is
// unchanged.
//
// Like TrustRecord it is commit-pinned: the sha it covers must still be the worker's
// HEAD (so a commit landing after the security reviewer ran is rejected rather than
// silently passed). A missing or malformed security.json folds to a "block" advisory
// verdict (fail closed) telling the manager to actually run the reviewer.
func (m *Manager) SecurityReview(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("security review covers %s but the worker HEAD is now %s; re-run 'ttorch security-review prep %s' and review again", short(sha), short(head), taskID)
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, []string{review.DimensionSecurity})
	if err != nil {
		return zero, err
	}
	if err := review.Write(m.securityVerdictPath(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	// Record a typed, manager-authored, non-actionable 'security_recorded' event (§3.4).
	// It is a PURE event append — NOT RecordDelivery — because the security-everywhere
	// pass is advisory and must never touch the task's gate state (gate_passed/
	// approved_by/reviewed_sha); it only notes that the audit ran and its outcome.
	// Best-effort: the verdict is already persisted, so a failed append must not mask it.
	if _, err := m.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventSecurityRecorded, Actor: db.ActorManager,
		Payload: fmt.Sprintf("verdict=%s sha=%s", verdict.Overall, short(sha)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the security_recorded event for %s: %v\n", taskID, err)
	}
	m.audit(fmt.Sprintf("security-review task=%s commit=%s verdict=%s mode=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project)))
	return verdict, nil
}

// SecurityReviewShow returns the current valid (unexpired) advisory security verdict for
// taskID, if any, without consuming it.
func (m *Manager) SecurityReviewShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.securityVerdictPath(taskID))
}

// qaVerdictPath is where the standalone, advisory test-adequacy (QA) verdict lives — beside
// the review inputs the QA reviewer reads, and DISTINCT from both the trust gate's durable
// DB verdict and the security audit's verdict, so none of the three interfere: the QA pass
// can never mint an approval or satisfy the trusted gate, and recording it never disturbs a
// trust or security verdict.
func (m *Manager) qaVerdictPath(taskID string) string {
	return filepath.Join(m.P.ReviewInputsDir(taskID), "qa-verdict.json")
}

// QAReview folds ONLY the QA reviewer's report for taskID into a commit-pinned verdict and
// persists it as an advisory result — the optional test-adequacy audit. It reuses the same
// inputs (materialized by TrustPrep) and the same internal/review aggregation as the trust
// gate, but it is purely advisory: it never mints an approval, never touches the trust gate's
// verdict or the task's gate state, and never blocks a merge. The manager surfaces its
// findings; delivery is still governed by the human approval (or, in trusted mode, the
// unchanged three-dimension gate, which does not include QA).
//
// Like TrustRecord it is commit-pinned: the sha it covers must still be the worker's HEAD (so
// a commit landing after the QA reviewer ran is rejected rather than silently passed). A
// missing or malformed qa.json folds to a "block" advisory verdict (fail closed) telling the
// manager to actually run the reviewer.
func (m *Manager) QAReview(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("qa review covers %s but the worker HEAD is now %s; re-run 'ttorch qa-review prep %s' and review again", short(sha), short(head), taskID)
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, []string{review.DimensionQA})
	if err != nil {
		return zero, err
	}
	if err := review.Write(m.qaVerdictPath(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	// Record a typed, manager-authored, non-actionable 'qa_recorded' event (§3.4). Like the
	// security audit it is a PURE event append — NOT RecordDelivery — because the QA pass is
	// advisory and must never touch the task's gate state (gate_passed/approved_by/
	// reviewed_sha); it only notes that the audit ran and its outcome. Best-effort: the
	// verdict is already persisted, so a failed append must not mask it.
	if _, err := m.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventQARecorded, Actor: db.ActorManager,
		Payload: fmt.Sprintf("verdict=%s sha=%s", verdict.Overall, short(sha)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the qa_recorded event for %s: %v\n", taskID, err)
	}
	m.audit(fmt.Sprintf("qa-review task=%s commit=%s verdict=%s mode=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project)))
	return verdict, nil
}

// QAReviewShow returns the current valid (unexpired) advisory QA verdict for taskID, if any,
// without consuming it.
func (m *Manager) QAReviewShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.qaVerdictPath(taskID))
}

// ---------------------------------------------------------------------------------------
// Daemon gate-pass (roadmap A1): make GATING daemon-drivable.
//
// The manager's hand-run gate is a fixed choreography — `ttorch trust prep`, fan out the
// sized reviewer subagents, `ttorch trust record` — that the scheduler can drive instead, so
// a stalled or absent LLM manager no longer halts the steady-state land path. GateOnce is the
// single-task, single-tick state machine the scheduler's `--gate` pass calls; it AUTOMATES the
// orchestration only — it CALLS the unchanged TrustPrep / review.Aggregate / TrustRecord and
// never touches the merge/land authority (MergeLocal) or what makes a verdict valid.
//
// FAIL CLOSED: only an all-pass aggregate is ever recorded (via TrustRecord, exactly as the
// manager's `trust record`). A blocking finding, a prep refusal, a missing/mismatched report,
// or a stalled reviewer is NEVER recorded — the daemon surfaces an actionable gate_blocked
// event for the manager to adjudicate and leaves the task untouched. The manager's only
// remaining gate role becomes adjudicating those blocks; the all-pass happy path records and
// (via the land pass) lands hands-off.

// GateOutcome is one tick's result for a single task's daemon gate. It is advisory to the
// caller (the scheduler logs it and counts records); the durable state lives in the DB verdict
// row, the review-inputs dir, and the reviewer windows.
type GateOutcome string

const (
	// GateSkipped: not a daemon-gate candidate this tick — the repo is not trusted, the head
	// is unreadable, a verdict already covers the current head (the land pass or the manager
	// owns it), or this head was already surfaced as blocked/recorded (terminal for the head).
	GateSkipped GateOutcome = "skipped"
	// GateDispatched: reviewers were (re)dispatched this tick for one or more dimensions; the
	// gate is now waiting on their reports.
	GateDispatched GateOutcome = "dispatched"
	// GateWaiting: reviewers are running but not all reports are in yet (no new dispatch this
	// tick).
	GateWaiting GateOutcome = "waiting"
	// GateRecorded: every required dimension passed; the durable verdict was recorded through
	// the unchanged TrustRecord (and, in trusted mode, the approval token auto-minted), so the
	// land pass can land it hands-off.
	GateRecorded GateOutcome = "recorded"
	// GateBlocked: the gate could not pass this task hands-off (a blocking reviewer finding, a
	// prep refusal, or a stalled/failed reviewer). NOTHING was recorded; an actionable
	// gate_blocked event was surfaced for the manager.
	GateBlocked GateOutcome = "blocked"
)

// gate-pass tunables. Verdicts are content-pinned and never expire by age, so the TTL only
// bounds the short-lived approval token TrustRecord mints in trusted mode — it mirrors the
// `ttorch trust record` default. maxReviewerAttempts bounds how many times a reviewer that
// dies WITHOUT writing a report (window gone, no report) is respawned before the gate gives up
// and surfaces a stall — the reviewer restart-storm bound. reviewerTimeout bounds how long the
// gate waits on running reviewers before surfacing a stall, so a wedged reviewer never strands
// a done task in a silent forever-wait.
const (
	defaultGateTTL             = 30 * time.Minute
	defaultMaxReviewerAttempts = 2
	defaultReviewerTimeout     = 30 * time.Minute
	// reviewerEffort is the reasoning effort the daemon launches each reviewer at. Review is
	// load-bearing judgment over a diff that may merge unread, so it runs high (not the worker
	// default), matching the manager's own reviewer subagents.
	reviewerEffort = "high"
	// reviewerModel is the model the daemon launches each reviewer on. "" leaves claude's own
	// default (the most capable model the user configured): the adversarial review is the trust
	// gate that may authorize an unread merge in trusted mode, so it deliberately does NOT
	// cheap out on the model the way the worker tier classifier does. Pin it to force a model.
	reviewerModel = ""
)

// gateProgressFile is the daemon gate's per-task, crash-safe progress record, kept beside the
// review inputs (ReviewInputsDir) so a daemon restart re-derives exactly where the gate was:
// which head's episode is in flight, which dimensions it requires, how many times each
// reviewer has been dispatched, when the reviewers were first launched (the stall clock), and
// whether the head reached a terminal outcome (recorded / blocked). Together with the reviewer
// windows and the per-dimension report files it is the source of truth that makes the pass
// idempotent — never double-dispatching a reviewer or double-recording a verdict across a
// restart.
const gateProgressFile = "gate-progress.json"

// gate-progress terminal outcomes (the Outcome field). An empty Outcome means the episode for
// Head is still in flight.
const (
	gateOutcomeRecorded = "recorded" // an all-pass verdict was recorded for Head (land owns it)
	gateOutcomeBlocked  = "blocked"  // a block/refusal/stall was surfaced for Head (manager owns it)
)

// gateProgress is the persisted per-task gate state for one head's gating episode.
type gateProgress struct {
	Head         string         `json:"head"`         // the reviewed commit this episode gates
	Dims         []string       `json:"dims"`         // the prepared, size-scaled reviewer set
	Attempts     map[string]int `json:"attempts"`     // per-dimension reviewer (re)dispatch count
	DispatchedAt int64          `json:"dispatchedAt"` // unix nano of the first reviewer dispatch (stall clock); 0 until dispatched
	Outcome      string         `json:"outcome"`      // "" in flight | gateOutcomeRecorded | gateOutcomeBlocked
}

// reviewerDispatcher is the seam the daemon gate dispatches a reviewer through; production
// wiring is (*Manager).spawnReviewer (a real tmux + harness launch). It is a package var so a
// test can substitute a stand-in that writes a stub <dimension>.json into the inputs dir
// instead of standing up a Claude session — exactly how the acceptance tests exercise the
// happy/fail-closed/idempotent paths without a live reviewer.
var reviewerDispatcher = (*Manager).spawnReviewer

// GateOnce drives one tick of the daemon gate for taskID at the default tunables and the
// current wall clock. It is the Fleet entry point the scheduler's gate pass calls; the
// caller must already have decided taskID is a candidate (a done task in a trusted repo with
// no passing verdict) and claimed it. See gateOnceAt for the state machine.
func (m *Manager) GateOnce(taskID string) (GateOutcome, error) {
	return m.gateOnceAt(taskID, defaultGateTTL, defaultMaxReviewerAttempts, defaultReviewerTimeout, time.Now())
}

// gateOnceAt is the testable core: one tick of the daemon gate for taskID. now and the
// tunables are injected so a test can drive the stall clock deterministically. It returns the
// tick's GateOutcome and a hard error only for a board-read failure (which aborts the pass);
// every per-task obstruction (a prep refusal, a blocking verdict, a stalled reviewer) is
// surfaced via a gate_blocked event and returned as GateBlocked, never as an error.
func (m *Manager) gateOnceAt(taskID string, ttl time.Duration, maxReviewerAttempts int, reviewerTimeout time.Duration, now time.Time) (GateOutcome, error) {
	ctx := context.Background()
	t, ok, err := m.Store.GetTask(ctx, taskID)
	if err != nil {
		return GateSkipped, err
	}
	if !ok {
		return GateSkipped, nil // vanished between selection and gate — not ours to gate
	}
	// Only a trusted repo gates hands-off: there a recorded pass auto-mints the approval token
	// and the land pass merges without a human. In any other mode the verdict is advisory and a
	// human still approves, so daemon-recording it would not advance delivery — leave it for the
	// manager. (The scheduler's Gateable pre-filter already screens these out; this is the
	// fail-safe second check so gateOnceAt is correct if called directly.)
	if projectinit.ReadMode(t.Project) != "trusted" {
		return GateSkipped, nil
	}
	// The committed object the reviewers must cover and the merge will fast-forward — never the
	// mutable worktree. An unreadable head (e.g. a torn-down worktree) is not gateable this tick.
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return GateSkipped, nil
	}
	// Already gated for THIS head, by the daemon earlier or the manager by hand: a verdict row
	// pinned to head means the decision exists — a passing one is the land pass's to land, a
	// blocking one is the manager's to resolve. Either way the gate does not re-run.
	if v, ok, err := m.Store.GetVerdict(ctx, taskID); err != nil {
		return GateSkipped, err
	} else if ok && v.ReviewedSHA == head {
		return GateSkipped, nil
	}

	dir := m.P.ReviewInputsDir(taskID)
	prog := m.readGateProgress(dir)

	// Episode boundary: a first-ever gate, or the worker advanced past a prior episode's head
	// (a re-gate). Reset the episode for the new head — tear down the prior head's reviewer
	// windows, then run prep, which archives the previous episode's reports and stamps the new
	// one so a present report can only ever be this episode's. A prep refusal (dirty worktree
	// / stale base) is the worker's to fix (commit / rebase), not the daemon's: surface it
	// once and mark the head terminal so the pass does not re-prep every tick.
	if prog.Head != head {
		m.teardownReviewers(taskID, prog.Dims)
		if _, err := m.TrustPrep(taskID); err != nil {
			m.surfaceGateBlocked(taskID, head, "gate prep refused: "+err.Error())
			m.writeGateProgress(dir, gateProgress{Head: head, Outcome: gateOutcomeBlocked})
			return GateBlocked, nil
		}
		episodeDims, _ := m.requiredDimensions(taskID, head)
		prog = gateProgress{Head: head, Dims: episodeDims, Attempts: map[string]int{}}
		m.writeGateProgress(dir, prog)
	}

	// Terminal for this head: a block was already surfaced (manager owns it) or a verdict was
	// already recorded (land owns it; the early verdict check covers the not-yet-consumed case,
	// this covers the post-consume window before the task leaves the done set). Nothing to do.
	if prog.Outcome != "" {
		return GateSkipped, nil
	}

	// Re-derive the required reviewer set every tick rather than trusting the persisted
	// prog.Dims, so the daemon dispatches, polls, AND aggregates EXACTLY the set TrustRecord
	// will aggregate (which resolves it the same way). That alignment removes any chance of
	// recording a verdict over a different set than was reviewed. The set comes from the prep
	// stamp, not from reviewers.json: requiredDimensions treats the stamped set as the floor
	// and lets the file only add to it, and it fails safe to the full built-in set when no
	// stamp covers this head, so this never under-reviews.
	dims, dropped := m.requiredDimensions(taskID, head)
	// VALIDATE BEFORE COMPOSING ANYTHING FOR THE MANAGER. Both records these names come from
	// are files in the review inputs dir, and the gate_blocked payload below is the channel
	// the manager acts on, so an unusable name must be caught here rather than quoted into a
	// sentence about something else. The fold would block on it anyway; this is about what
	// reaches the manager and in what words.
	if err := review.ValidateDimensionSet(dims); err != nil {
		m.surfaceGateBlocked(taskID, head, err.Error())
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(dir, prog)
		m.teardownReviewers(taskID, dims)
		return GateBlocked, nil
	}
	if len(dropped) > 0 {
		// The set shrank after prep: the inputs dir was edited during the review, which is
		// how a blocking report gets hidden. The manager adjudicates that, not the daemon.
		m.surfaceGateBlocked(taskID, head, droppedDimensionFinding(dropped).Summary)
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(dir, prog)
		m.teardownReviewers(taskID, dims)
		return GateBlocked, nil
	}
	if prog.Attempts == nil {
		prog.Attempts = map[string]int{}
	}

	// Dispatch step. For each required dimension that has neither a pinned report nor a live
	// reviewer window, (re)dispatch a reviewer — bounded by maxReviewerAttempts so a reviewer
	// that keeps dying without producing a report cannot respawn forever. A present report or a
	// live window is left alone, which is exactly what makes the pass idempotent across a daemon
	// restart: it never double-dispatches a reviewer that is already running or already done.
	var toDispatch []string
	allReady := true
	for _, dim := range dims {
		if m.reviewReportPinned(dir, dim, head) {
			continue // this dimension's report is in and pinned to head
		}
		allReady = false
		if m.reviewerWindowAlive(taskID, dim) {
			continue // its reviewer is still running
		}
		if prog.Attempts[dim] >= maxReviewerAttempts {
			m.surfaceGateBlocked(taskID, head, fmt.Sprintf("reviewer %q produced no report after %d attempt(s)", dim, maxReviewerAttempts))
			prog.Outcome = gateOutcomeBlocked
			m.writeGateProgress(dir, prog)
			m.teardownReviewers(taskID, dims)
			return GateBlocked, nil
		}
		toDispatch = append(toDispatch, dim)
	}

	if len(toDispatch) > 0 {
		dispatched := false
		for _, dim := range toDispatch {
			if err := reviewerDispatcher(m, taskID, dim, dir, head, t.Project, t.Worktree); err != nil {
				// A launch failure this tick is non-fatal and does not burn an attempt (nothing
				// started): the next tick retries this dimension. Other dimensions still launch.
				fmt.Fprintf(os.Stderr, "ttorch: gate could not dispatch reviewer %s/%s: %v\n", taskID, dim, err)
				continue
			}
			prog.Attempts[dim]++
			dispatched = true
		}
		if dispatched && prog.DispatchedAt == 0 {
			prog.DispatchedAt = now.UnixNano()
		}
		m.writeGateProgress(dir, prog)
		return GateDispatched, nil
	}

	if !allReady {
		// Reviewers are running but not all reports are in. Bound the wait: a reviewer wedged
		// past reviewerTimeout (alive but never reporting) is surfaced as a stall rather than
		// leaving the done task in a silent forever-wait.
		if prog.DispatchedAt != 0 && reviewerTimeout > 0 && now.Sub(time.Unix(0, prog.DispatchedAt)) > reviewerTimeout {
			m.surfaceGateBlocked(taskID, head, fmt.Sprintf("reviewers did not all report within %s", reviewerTimeout))
			prog.Outcome = gateOutcomeBlocked
			m.writeGateProgress(dir, prog)
			m.teardownReviewers(taskID, dims)
			return GateBlocked, nil
		}
		return GateWaiting, nil
	}

	// Every required dimension has a report pinned to head. Aggregate to decide pass vs block.
	// review.Aggregate is the SAME deterministic fold the manager's `trust record` uses; the
	// daemon does not fork it.
	v, err := review.Aggregate(dir, head, dims)
	if err != nil {
		// Only a stale-sha mismatch makes Aggregate error, which reviewReportPinned already
		// excludes — so this is unexpected. Treat it as not-yet-ready (record nothing); the next
		// tick re-derives. Never downgrade an aggregate error to a pass.
		fmt.Fprintf(os.Stderr, "ttorch: gate aggregate for %s deferred: %v\n", taskID, err)
		return GateWaiting, nil
	}
	if v.Overall != review.Pass {
		// FAIL CLOSED: a blocking verdict is NEVER recorded by the daemon. Surface it for the
		// manager and mark the head terminal so the pass does not re-loop on the same reports.
		m.surfaceGateBlocked(taskID, head, "adversarial review blocked: "+strings.Join(review.Describe(v), "; "))
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(dir, prog)
		m.teardownReviewers(taskID, dims)
		return GateBlocked, nil
	}

	// PASS. Record the durable verdict through the UNCHANGED TrustRecord, which re-aggregates,
	// pins to head, persists the verdict row, and — in trusted mode over a still-green, clean
	// worktree — auto-mints the approval token, exactly as a manager-run `ttorch trust record`
	// would. The merge/land authority is untouched; the land pass lands it hands-off.
	if _, err := m.TrustRecord(taskID, head, ttl); err != nil {
		// A record-time refusal (most likely the worker advanced HEAD between our read and
		// TrustRecord's own re-check) is transient and recoverable — record nothing terminal, do
		// not surface a block; the next tick re-derives (a moved head re-preps cleanly).
		fmt.Fprintf(os.Stderr, "ttorch: gate trust-record for %s deferred: %v\n", taskID, err)
		return GateWaiting, nil
	}
	prog.Outcome = gateOutcomeRecorded
	m.writeGateProgress(dir, prog)
	m.teardownReviewers(taskID, dims)
	m.audit(fmt.Sprintf("gate-record task=%s commit=%s verdict=pass actor=daemon", taskID, short(head)))
	return GateRecorded, nil
}

// Gateable reports whether repo is a daemon-gate candidate: a trusted repo, where a recorded
// pass auto-authorizes the merge. The scheduler's gate pass uses it to skip claiming done tasks
// in non-trusted repos (where a daemon-recorded verdict would not advance delivery).
func (m *Manager) Gateable(repo string) bool {
	return projectinit.ReadMode(repo) == "trusted"
}

// reviewerWindow is the tmux window name for one dimension's daemon-dispatched reviewer:
// stable and deterministic so the gate can recognize a still-running reviewer (idempotent
// dispatch) and tear it down when the episode ends. Distinct prefix ("rv-") from worker
// windows ("wk-") so the two fleets never collide.
//
// It returns "" for a dimension name that is not a plain label. Window names are tmux
// TARGETS ("<session>:<window>"), so a name carrying tmux's own separators would be parsed
// as a different target than the one meant; the value comes from a worker-writable file, so
// it is not allowed to reach tmux at all. Callers treat "" as "there is no such window".
func reviewerWindow(taskID, dim string) string {
	if !review.ValidDimensionName(dim) {
		return ""
	}
	return "rv-" + taskID + "-" + dim
}

// reviewerWindowAlive reports whether a dimension's reviewer window is still present. A
// dimension with no usable window name has no window.
func (m *Manager) reviewerWindowAlive(taskID, dim string) bool {
	window := reviewerWindow(taskID, dim)
	return window != "" && tmux.WindowExists(m.Session, window)
}

// reviewReportPinned reports whether dimension dim's report in dir is the report the verdict
// fold will accept: a parseable review.Report pinned to head AND written during the current
// review episode (after the prep that staged the inputs it reviewed). A missing, unparseable,
// stale-pinned, or superseded report reads as not-ready, so the gate re-dispatches that
// reviewer rather than folding a review of inputs that no longer exist. It delegates to
// review.ReportCurrent so the daemon's readiness check and review.Aggregate can never
// disagree about what counts as a completed review.
func (m *Manager) reviewReportPinned(dir, dim, head string) bool {
	return review.ReportCurrent(dir, dim, head)
}

// teardownReviewers reaps and kills any reviewer windows for the given dimensions. Best-effort:
// a reviewer that has written its report has done its job, so a failed kill is harmless (the
// window is idle and holds no pool slot). Called when the episode reaches a terminal outcome.
func (m *Manager) teardownReviewers(taskID string, dims []string) {
	for _, dim := range dims {
		window := reviewerWindow(taskID, dim)
		if window == "" || !tmux.WindowExists(m.Session, window) {
			continue
		}
		m.killPaneProcesses(window)
		_ = tmux.KillWindow(m.Session, window)
	}
}

// surfaceGateBlocked records an ACTIONABLE gate_blocked event (actor=system) so `ttorch watch`
// wakes the manager to adjudicate — the daemon gate's only handoff back to the LLM manager. It
// is the single place the gate signals "I could not pass this hands-off"; it NEVER records a
// verdict. Best-effort on the event append (the audit line is the durable trail), so a failed
// append is logged, not fatal.
func (m *Manager) surfaceGateBlocked(taskID, head, reason string) {
	// The reason can carry reviewer-authored text (review.Describe joins finding summaries
	// into it), and the manager reads this payload as an instruction to act on, so it is one
	// line with no control bytes. Describe quotes the reviewer's own words inside it.
	payload := review.SafeLine(fmt.Sprintf("sha=%s %s", short(head), reason))
	if _, err := m.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventGateBlocked,
		Actor: db.ActorSystem, Actionable: true, Payload: payload,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the gate_blocked event for %s: %v\n", taskID, err)
	}
	m.audit(fmt.Sprintf("gate-blocked task=%s commit=%s actor=daemon reason=%q", taskID, short(head), reason))
}

// readGateProgress loads the per-task gate progress record, returning a zero gateProgress (a
// fresh episode) when it is absent or unparseable — fail-safe, since a missing/garbled record
// just re-preps and re-derives from the live reviewer windows and report files.
func (m *Manager) readGateProgress(dir string) gateProgress {
	var p gateProgress
	b, err := os.ReadFile(filepath.Join(dir, gateProgressFile))
	if err != nil {
		return gateProgress{}
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return gateProgress{}
	}
	if p.Attempts == nil {
		p.Attempts = map[string]int{}
	}
	return p
}

// writeGateProgress persists the gate progress record beside the review inputs. Best-effort: a
// failed write degrades safely — the next tick re-derives episode state from the reviewer
// windows and report files (a live window still suppresses re-dispatch), so the worst case is a
// lost attempt counter, never a double-record.
func (m *Manager) writeGateProgress(dir string, p gateProgress) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not create the gate progress dir for %s: %v\n", dir, err)
		return
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, gateProgressFile), append(b, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not write the gate progress for %s: %v\n", dir, err)
	}
}

// spawnReviewer launches one dimension's adversarial reviewer as a Claude session in a tmux
// window — the production wiring behind reviewerDispatcher. It is the daemon analogue of the
// manager fanning out a `ttorch-reviewer-<dim>` subagent: the launched session reads the same
// materialized inputs (inputsDir) and the same pinned commit (head), reviews ONLY its
// dimension, and writes the same commit-pinned <dim>.json report — so the daemon orchestrates
// the real adversarial reviewers, it does not replace them with a rubber stamp.
//
// It runs in the worker's worktree (wt), at the reviewed commit, so the reviewer can read the
// source the diff touches; it never edits the tree (review is read-only). It is idempotent —
// it no-ops when the dimension's window already exists — so a re-dispatch never doubles a
// running reviewer.
func (m *Manager) spawnReviewer(taskID, dim, inputsDir, head, repo, wt string) error {
	// First, before tmux, the harness, or any file: the dimension name decides a file path
	// written under inputsDir, the tmux window name, and the report path handed to the
	// reviewer. It arrives from a worker-writable file, so an unusable one launches nothing.
	briefPath, err := review.InputPath(inputsDir, dim, reviewerBriefSuffix)
	if err != nil {
		return err
	}
	reportPath, err := review.InputPath(inputsDir, dim, review.ReportSuffix)
	if err != nil {
		return err
	}
	if err := m.requireTmux(); err != nil {
		return err
	}
	window := reviewerWindow(taskID, dim)
	if tmux.WindowExists(m.Session, window) {
		return nil // already running — idempotent
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	h := harness.Resolve()
	sid := harness.NewSessionID()
	// Pre-accept the harness folder-trust prompt and write the trimmed worker settings (no AI
	// co-author trailer) so the reviewer runs autonomously, exactly like a worker spawn.
	_ = harness.WriteWorkerSettings(h, wt)
	harness.TrustWorktree(h, repo, wt)
	brief := reviewerBrief(taskID, dim, inputsDir, head, reportPath)
	if err := os.WriteFile(briefPath, []byte(brief), 0o644); err != nil {
		return err
	}
	if err := m.newWindow(window, wt, "review · "+dim+" · "+taskID); err != nil {
		return err
	}
	cmd := harness.BriefCommand(h, briefPath, sid, reviewerEffort, reviewerModel)
	if err := tmux.SendLine(m.Session, window, cmd); err != nil {
		m.killPaneProcesses(window)
		_ = tmux.KillWindow(m.Session, window)
		return err
	}
	m.audit(fmt.Sprintf("gate-dispatch-reviewer task=%s dim=%s commit=%s actor=daemon", taskID, dim, short(head)))
	return nil
}

// reviewerBrief is the initial prompt for a daemon-dispatched reviewer. It dispatches the real
// `ttorch-reviewer-<dim>` adversarial subagent over the materialized inputs at the pinned head
// (with a self-review fallback if that subagent is unavailable), and requires the single output
// the gate consumes: a commit-pinned <dim>.json following the findings contract. Go owns the
// verdict aggregation, so a missing or malformed report fails the gate closed regardless of
// what the session does.
func reviewerBrief(taskID, dim, inputsDir, head, reportPath string) string {
	return fmt.Sprintf(`# Adversarial trust-gate review — %s dimension (task %s)

You are the **%s** reviewer in ttorch's adversarial trust gate, dispatched by the scheduler
daemon. A passing verdict may merge this diff WITHOUT a human reading it, so your judgment is
load-bearing. Review ONLY the %s dimension — the other dimensions are other reviewers' jobs.
You NEVER edit, commit, or push code; review is a static read of the diff.

Use the Task tool to dispatch the `+"`ttorch-reviewer-%s`"+` subagent, giving it exactly:
- review inputs dir: %s
- reviewed commit (head): %s

It must read %s/{diff.patch, brief.md, validate.json, head.txt}, review only the %s dimension,
trust the green validate.json (do NOT re-run the build/test suite), and write its findings to
%s following the findings contract:

    {"dimension": "%s", "reviewedSha": "%s", "findings": [ ... ]}

where each finding is {"dimension","severity","reviewer","summary"}, severity is one of
low|medium|high|critical (high/critical block the merge; bias to high on uncertainty), a clean
review is "findings": [], and reviewedSha MUST equal %s verbatim.

If the `+"`ttorch-reviewer-%s`"+` subagent is unavailable, perform the review yourself per the
exact same contract and write %s. Either way the ONLY required output is that file. When it is
written, you are done — do not modify the repository.
`, dim, taskID, dim, dim, dim, inputsDir, head, inputsDir, dim, reportPath, dim, head, head, dim, reportPath)
}

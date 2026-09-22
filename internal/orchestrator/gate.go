package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
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
// the authority is the set prep stamped into prep.json unioned with the floor re-derived
// from the diff, and requiredDimensions reconciles all three (the stamp decides, the file
// may only add, the floor may only raise).
//
// HOW FAR THE "UNTRUSTED INPUTS" TREATMENT ACTUALLY GOES, stated here because this is where
// the claim starts and it is easy to read as more than it is. All of that machinery stops the
// recorded set LOWERING what a verdict must cover. It does not make the reports themselves
// trustworthy. review.Aggregate authenticates a report on two things only: that the file is
// present and that its reviewedSha equals the head under review. Neither is unforgeable by a
// process running as the lead. Worse, the daemon's dispatch loop skips any dimension whose
// report is already pinned to head, so a report that is simply THERE first leaves allReady
// true and no reviewer is ever launched for it. Writing one clean file per required dimension
// therefore produces a pass with nobody having reviewed anything.
//
// So the accurate claim is narrow: the review-inputs dir is treated as untrusted for the
// QUESTION OF WHICH DIMENSIONS ARE REQUIRED, and is trusted for the content of the answers.
// Closing the second half needs the reports to carry provenance the gate can check, which
// nothing here does.
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
// dimension the stamp prepared that reviewers.json no longer lists. It composes two defences
// that catch different things, with the prep stamp as the spine.
//
// The STAMP decides. review.RequiredDimensions reads the set prep recorded, lets reviewers.json
// only ADD to it, and returns separately any stamped dimension the file no longer lists: a set
// that shrank after prep means the inputs dir was edited during the review, which the caller
// surfaces rather than papers over. See that function for what the stamp buys (cost and
// evidence) and what it does not (authenticity; it is not a trust boundary).
//
// The DERIVED FLOOR is unioned on top, and it is a distinct property rather than a second
// opinion on the same one. The stamp catches an edit made after prep. Re-deriving the set from
// the committed diff catches a stamp that was wrong when it was written, which matters because
// prep.json sits 0o644 in the same worker-writable directory as reviewers.json. Neither
// subsumes the other, so both run and the result is their union: the floor can only raise the
// requirement, never lower it.
//
// With no stamp covering sha there is nothing to decide from, so it fails safe to the full
// built-in set unioned with the file and the floor, never to fewer.
func (m *Manager) requiredDimensions(t db.Task, sha string) (required, dropped []string) {
	onDisk := m.ReviewersFor(t.ID)
	derived := m.derivedFloor(t, sha)
	stamped, dropped, ok := review.RequiredDimensions(m.P.ReviewInputsDir(t.ID), sha, onDisk)
	if !ok {
		return unionDimensions(derived, unionDimensions(requiredReviewers, onDisk)), nil
	}
	return unionDimensions(derived, stamped), dropped
}

// derivedFloor re-derives the reviewer set from the committed diff, so the set a verdict must
// cover does not rest entirely on records kept in a directory the worker can write. It is the
// same review.Classify over the same authoritative git file list and line stat TrustPrep used.
//
// IT IS DERIVED OVER EVERY CANDIDATE BASE, NOT ONE. A diff is only as honest as the ref it is
// taken against, and a linked worktree shares refs/ with the project repo, so a live worker can
// point origin/<default> at a commit that already carries its own changes and leave a harmless
// remainder. Measured: two .go files plus a NOTES.md, poison origin/<default> at the code
// commit, and the derived set drops from correctness+scope+security to correctness+scope.
// Pairing that with a lowered reviewers.json defeats both halves at once. Deriving against
// every base that resolves and unioning the results means an attacker has to poison all of
// them; unpoisoned local <default> still sees the .go files and still demands security.
//
// Every failure path lands on the full three-dimension set: no base resolves, git fails, or
// review.Classify's own ok flag goes false.
//
// WHAT THIS DOES NOT CLOSE. Every candidate is a ref in a store the worker can write, so
// poisoning all of them lowers the floor again. That is the process channel, and a session that
// can write refs can write the approval token directly, which is one file and skips the gate
// entirely. This makes the floor no longer the cheapest way through; it does not make it
// unreachable.
func (m *Manager) derivedFloor(t db.Task, sha string) []string {
	var floor []string
	derived := false
	for _, base := range candidateReviewBases(t.Project) {
		files, filesOK := diffFiles(t.Worktree, base, sha)
		lines, binary, statOK := diffLineStat(t.Worktree, base, sha)
		_, dims := review.Classify(files, lines, binary, filesOK && statOK)
		floor = unionDimensions(floor, dims)
		derived = true
	}
	if !derived {
		return append([]string(nil), requiredReviewers...)
	}
	return floor
}

// candidateReviewBases lists every ref the branch could honestly be diffed against: the true
// review base the merge targets, the raw local default, and the remote-tracking default. They
// normally agree, and where they do the union is the same set a single base would give. Where
// they disagree, one of them has been moved, and taking the union means the move cannot hide a
// changed file. Only refs that resolve are returned; duplicates are dropped so a diff is not
// computed twice for the same sha.
func candidateReviewBases(repo string) []string {
	def := worktree.DefaultBranch(repo)
	var out []string
	seen := map[string]bool{}
	for _, ref := range []string{"", def, "origin/" + def} {
		if ref == "" {
			// The base the land path will actually use. Resolved without a fetch: prep has
			// already fetched for this episode, and a fetch here would be a network round trip
			// on a per-tick path for no added integrity.
			b, err := reviewBase(repo, false)
			if err != nil {
				continue
			}
			ref = b
		}
		sha, err := worktree.ResolveRef(repo, ref)
		if err != nil || seen[sha] {
			continue
		}
		seen[sha] = true
		out = append(out, sha)
	}
	return out
}

// unionDimensions merges two dimension sets, preserving order (the first set, in
// review.Classify's canonical order, then any dimension only the second names) and dropping
// duplicates and empty entries. The order is deterministic so the dispatch set, the
// aggregation set, and the audit line always agree.
func unionDimensions(derived, recorded []string) []string {
	seen := make(map[string]bool, len(derived)+len(recorded))
	out := make([]string, 0, len(derived)+len(recorded))
	for _, set := range [][]string{derived, recorded} {
		for _, d := range set {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
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

// dispatchedDimensions returns every dimension this episode has actually dispatched a reviewer
// for: prog.Dims (the set decided at the episode boundary) plus every key of prog.Attempts (a
// dimension the dispatch loop launched on a later tick, which prog.Dims does not record).
// Sorted, so teardown and aggregation are deterministic.
func dispatchedDimensions(prog gateProgress) []string {
	seen := make(map[string]bool, len(prog.Dims)+len(prog.Attempts))
	var out []string
	for _, d := range prog.Dims {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for d := range prog.Attempts {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// foldDimensions returns the dimensions review.Aggregate must fold for head: every REQUIRED
// dimension, plus any dimension this episode DISPATCHED that is no longer required but has
// left a report pinned to head.
//
// The required set can shrink mid-episode. requiredDimensions fails closed to all three when
// it cannot resolve the review base, so a transient git failure on one tick dispatches a
// security reviewer, and a later tick that resolves the base over a docs-only diff drops back
// to {correctness, scope}. Folding only the current set would discard that reviewer's report —
// so a critical finding the gate itself asked for and received would be thrown away and the
// verdict recorded as a pass.
//
// An extra is folded ONLY when its report is present and pinned to head, so a dimension that
// fell out of the set and never reported cannot wedge the gate: the dispatch loop no longer
// polls it and Aggregate is never asked to require it. Folding is therefore monotone — it can
// add findings, never drop a requirement.
//
// The extras come from the episode's own dispatch record and never from "whatever report files
// happen to exist". That bounds which dimensions may be folded; it does not by itself keep an
// advisory report out, and this comment used to claim it did. The advisory audits dispatch the
// same reviewer agents over the same inputs, so their reports have the same filenames, and the
// gate would have folded one for any dimension it had also dispatched. They are kept out by
// living in a different directory (AdvisoryInputsDir): the gate aggregates the inputs dir and
// the audits aggregate the subdirectory, so neither can read the other's artifact.
func (m *Manager) foldDimensions(dir, head string, required, dispatched []string) []string {
	out := append([]string(nil), required...)
	have := make(map[string]bool, len(required))
	for _, d := range required {
		have[d] = true
	}
	for _, d := range dispatched {
		if have[d] || !m.reviewReportPinned(dir, d, head) {
			continue
		}
		have[d] = true
		out = append(out, d)
	}
	return out
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
// set).
//
// --no-renames for the same reason worktree.ChangedFiles needs it, with a sharper consequence
// here: diff.renames defaults TRUE, so a detected rename reports only its DESTINATION, and
// renaming a .go file to a .md one would present the diff to review.Classify as DOCS-ONLY —
// which drops the security reviewer entirely. The source path has to be in the list for the
// classifier to see there is code in the change. This is the source of truth for size classification; the patch body is never
// scraped for filenames, because a dropped quoted path could hide a malicious code file
// behind a docs-only edit and skip the security reviewer.
func diffFiles(dir, base, rev string) (files []string, ok bool) {
	out, err := gitOut(dir, "diff", "--name-only", "-z", "--no-renames", base+"..."+rev)
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
//
// --no-renames for consistency with diffFiles and worktree.ChangedFiles, not because the
// undercount was reachable here: with renames detected a rename reports one record instead
// of two, but review.Classify's trivial branch needs len(files) == 1 and --no-renames
// always reports both paths, so the file count gets there first. Two commands answering the
// same question differently is how the last five input-set defects started.
func diffLineStat(dir, base, rev string) (lines int, binary, ok bool) {
	out, err := gitOut(dir, "diff", "--numstat", "--no-renames", base+"..."+rev)
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
// A file is swept only if it parses as a review report whose dimension MATCHES ITS OWN
// BASENAME, which is how a report was written in the flat layout: scope.json declared
// dimension "scope". That is the discriminator rather than a list of control-file basenames
// to avoid, because a list of names to avoid is what put a report on top of
// gate-progress.json in the first place.
//
// Matching the name to the field, rather than just requiring both fields to be present, is
// what keeps the advisory verdict files out by construction. A marshalled review.Verdict
// DOES carry a top-level reviewedSha, so it half-parses as a report already; it is spared
// today only because review.Verdict happens to have no Dimension field, and a per-dimension
// advisory verdict is a plausible thing to want. Under the name match, security-verdict.json
// would have to declare dimension "security-verdict" to be swept, which no verdict writer
// does. prep.json, validate.json, reviewers.json and gate-progress.json carry no dimension
// at all.
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
		if err := json.Unmarshal(b, &r); err != nil || r.ReviewedSHA == "" {
			continue // not a report: a control file or something else entirely
		}
		if r.Dimension != strings.TrimSuffix(e.Name(), review.ReportSuffix) {
			continue // a verdict file, or a report whose name and dimension disagree
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
	required, dropped := m.requiredDimensions(t, sha)
	if err := review.ValidateDimensionSet(required); err != nil {
		return zero, err
	}
	// The set is widened by any dimension the daemon's gate episode for THIS sha dispatched
	// that is no longer required but reported anyway (foldDimensions). Without that, a manager
	// running `ttorch trust record` by hand after the daemon blocked on such a reviewer would
	// record a pass and discard its findings; the two paths must fold the identical set. A
	// task with no gate episode (the manual flow throughout) has no extras.
	inputs := m.P.ReviewInputsDir(taskID)
	// The manual path folds the same extras as the daemon, and it reads them the same way.
	// A store failure here is NOT "no episode": read that way, a record the gate could not
	// load would silently drop a dispatched dimension out of the fold, which is the defect
	// this record was moved into the store to end.
	prog, found, err := m.readGateProgress(taskID)
	if err != nil {
		return zero, fmt.Errorf("trust record %q: could not read the gate episode: %w", taskID, err)
	}
	var dispatched []string
	if found && prog.Head == sha {
		dispatched = dispatchedDimensions(prog)
	}
	dims := m.foldDimensions(inputs, sha, required, dispatched)
	verdict, err := review.Aggregate(inputs, sha, dims)
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
		hit, terr := diffTouchesGateConfig(t.Project, base, sha)
		touched := hit != nil
		green := false
		// A trusted auto-mint's green authority MUST be the default-branch gate script,
		// never ecosystem detection on the worker's checkout (which the worker controls
		// via go.mod/package.json). Without it, leave the verdict advisory — a human must
		// approve — and skip validation entirely so no worker-defined checks run. The green
		// comes from validateForAuthority, so it is one this process ran: the on-disk
		// validate cache is a file the worker can write and cannot mint an approval.
		if cerr == nil && terr == nil && clean && !touched && hasDefaultBranchGateScript(t.Project) {
			green, _, _, _ = validateForAuthority(t.Project, sha)
		}
		if green {
			if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("auto", sha, nil)); err != nil {
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
	// It names the folded set too: the set is composed from three sources now, so a reader
	// cannot reconstruct it from any one of them.
	m.audit(fmt.Sprintf("trust-record task=%s commit=%s verdict=%s reviewers=%s mode=%s auto-approved=%s validate=%s",
		taskID, short(sha), verdict.Overall, strings.Join(required, "+"), projectinit.ReadMode(t.Project), autoMinted,
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

// AdvisoryInputsDir is the advisory audits' own review episode: a subdirectory of the task's
// review-inputs dir with the same shape as its parent, holding a prep stamp, the staged
// validate it was cross-checked against, and a reports/ dir the advisory reviewers write into.
//
// Both channels dispatch the same reviewer agents over the same diff, so their reports have
// the same filenames. While they shared a directory, the advisory security audit and the trust
// gate's security dimension were the same file. The audit is the standing practice in this
// repo and normally runs FIRST, so the gate's dispatch loop would find a report already pinned
// to head, skip launching its own reviewer, and fold the audit's in its place. That is not a
// degraded gate; it is the specific thing step 2 exists to prevent, because the advisory
// reviewer runs inside the worker's worktree, under whatever CLAUDE.md, settings hooks and
// agent definitions that tree carries. The gate's own security reviewer runs in a scratch
// workspace against a bare mirror for exactly that reason.
//
// Separation is by PATH rather than by a provenance field in the report, because a field has
// to be trusted absent as well as present: a report written without it needs a default, and
// either default is wrong (fail closed and every existing report is rejected, fail open and
// the substitution it was added to stop still works). A path cannot be defaulted.
//
// It is a whole episode rather than just a reports directory because review.Aggregate folds
// reports against the prep stamp in the directory it is given. Pointing it at a bare
// subdirectory would leave it with no episode covering the commit, which fails the fold closed
// and takes the advisory audit out of service. So the advisory side gets its own stamp, and
// prepareAdvisoryEpisode writes it LAST for the same reason prep does.
//
// This says which artifact satisfies which channel. It does NOT authenticate either
// directory's contents: a process that can write one path can write the other, and report
// authentication remains open work (see ReviewersFor).
func (m *Manager) AdvisoryInputsDir(taskID string) string {
	return filepath.Join(m.P.ReviewInputsDir(taskID), "advisory")
}

// AdvisoryPrep materializes the shared review inputs and opens the advisory audit's own
// episode over them, returning the inputs dir and the advisory dir the reviewer writes into.
// It is the entry point `ttorch security-review prep` and `ttorch qa-review prep` share; dims
// is the single dimension that channel folds.
func (m *Manager) AdvisoryPrep(taskID string, dims []string) (inputsDir, advisoryDir string, err error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", "", fmt.Errorf("unknown task %q", taskID)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", "", err
	}
	// The reviewer reads exactly the inputs trust prep materializes (diff.patch / brief.md /
	// validate.json / head.txt), so reuse them rather than duplicate the materialization. But
	// only PREP them when this commit has no episode yet.
	//
	// TrustPrep does not just materialize: it opens a new gate episode, archiving the current
	// reports and re-stamping. Running it unconditionally meant an advisory audit knocked over
	// a gate that was mid-review. Reports that were present and current became neither, the
	// gate re-dispatched, and with attempts at the ceiling the next tick surfaced gate_blocked
	// on a healthy task. A worker fires that at itself with `ttorch security-review prep`. The
	// direction is safe, it blocks rather than passes, but it is a denial of gating for free.
	//
	// Reusing an existing episode's inputs is sound because the stamp attests they were
	// materialized for this same commit. It does skip TrustPrep's dirty-worktree refusal in
	// that case, which costs nothing here: the inputs under review are the committed diff, and
	// they are the ones the gate is already reviewing.
	inputsDir = m.P.ReviewInputsDir(taskID)
	// Prep only when NOTHING says an episode already covers this commit.
	//
	// Keying that on review.ValidateState alone was the same root as the episode record: the
	// stamp it reads is prep.json, in the review-inputs dir, so removing one file turned the
	// guard off and an advisory prep archived a healthy gate's reports and drove it to
	// gate_blocked. The store episode is checked first because the worker cannot write it; the
	// stamp is still consulted for the manual flow, which has no gate episode but can equally
	// have a review in progress.
	prog, found, perr := m.readGateProgress(taskID)
	if perr != nil {
		return "", "", fmt.Errorf("advisory prep for %q: could not read the gate episode: %w", taskID, perr)
	}
	gated := found && prog.Head == head
	if !gated && review.ValidateState(inputsDir, head) == "unprepped" {
		if inputsDir, err = m.TrustPrep(taskID); err != nil {
			return "", "", err
		}
	}
	advisoryDir, err = m.prepareAdvisoryEpisode(taskID, head, dims)
	if err != nil {
		return "", "", err
	}
	return inputsDir, advisoryDir, nil
}

// prepareAdvisoryEpisode opens the advisory review episode for taskID at head, over the inputs
// TrustPrep has just materialized, and returns its directory.
//
// It copies the staged validate rather than re-running one: review.Aggregate cross-checks a
// stamp's validateGreen against the staged results beside it and reports "disputed" when they
// disagree, so the two must come from the same measurement. Copying is also what keeps the
// advisory verdict describing the same suite run the gate saw.
//
// ORDER MATTERS, and it is the freshness rule that makes it matter. Aggregate treats a report
// older than the episode's stamp as predating the prep, so the stamp must be written before
// any report and after everything else. Writing it last means a re-prep moves its mtime and
// correctly invalidates the previous episode's reports; writing it first would let a report
// left over from an earlier audit read as fresh for this one, which is the substitution this
// whole split exists to stop, arriving by another route.
func (m *Manager) prepareAdvisoryEpisode(taskID, head string, dims []string) (string, error) {
	inputs := m.P.ReviewInputsDir(taskID)
	adv := m.AdvisoryInputsDir(taskID)
	if err := os.MkdirAll(review.ReportsDir(adv), 0o755); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(inputs, review.StagedValidateFile))
	if err != nil {
		return "", fmt.Errorf("advisory prep for %q: the staged validate is missing: %w", taskID, err)
	}
	var results []validate.Result
	if err := json.Unmarshal(raw, &results); err != nil {
		return "", fmt.Errorf("advisory prep for %q: the staged validate is unreadable: %w", taskID, err)
	}
	if err := os.WriteFile(filepath.Join(adv, review.StagedValidateFile), raw, 0o644); err != nil {
		return "", err
	}
	if _, err := review.WritePrepStamp(adv, head, results, dims); err != nil {
		return "", err
	}
	return adv, nil
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
	verdict, err := review.Aggregate(m.AdvisoryInputsDir(taskID), sha, []string{review.DimensionSecurity})
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
	verdict, err := review.Aggregate(m.AdvisoryInputsDir(taskID), sha, []string{review.DimensionQA})
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

// The daemon gate's per-episode state lives in the STORE, not beside the review inputs. It
// records which head's episode is in flight, which dimensions it dispatched, how many launch
// attempts each has cost, when the episode opened, and whether the head reached a terminal
// outcome. With the reviewer windows and the report files it is what makes the pass idempotent
// across a daemon restart: never double-dispatching a reviewer, never double-recording.
//
// It used to be gate-progress.json in ReviewInputsDir, and that was the root of a critical and
// three highs at once. A worker can write that directory, and this record decides what the
// episode must still wait for. A parseable record naming no dimensions erased the memory that
// a security reviewer had been dispatched, so an already-pinned CRITICAL finding fell out of
// the fold and trusted mode minted an approval over it, from one deliberate write. Zeroing the
// start time held the stall bound open indefinitely. Making the file unwritable stopped the
// bound accumulating at all, and needed no attacker.
//
// Each shape wanted its own validity check, and the checks kept arriving one finding at a
// time. The reviewer's cwd had already been moved out of that directory on exactly this
// reasoning; this is the same move for the state the decision rests on. See
// db.GateEpisode.

// gateProgressFile is where the episode record USED to live, beside the review inputs. Nothing
// reads or writes it now: the record moved into the store because a worker can write that
// directory (see db.GateEpisode). The name survives so the archive sweep still recognises a
// leftover from an older build as a control file rather than mistaking it for a report.
const gateProgressFile = "gate-progress.json"

// gate-progress terminal outcomes (the Outcome field). An empty Outcome means the episode for
// Head is still in flight.
const (
	gateOutcomeRecorded = "recorded" // an all-pass verdict was recorded for Head (land owns it)
	gateOutcomeBlocked  = "blocked"  // a block/refusal/stall was surfaced for Head (manager owns it)
)

// gateProgress is one episode's state in the shape the gate reasons about. It is loaded from
// and saved to db.GateEpisode, which holds the durable form.
type gateProgress struct {
	Head         string         // the reviewed commit this episode gates
	Dims         []string       // the prepared, size-scaled reviewer set
	Attempts     map[string]int // per-dimension reviewer (re)dispatch count
	DispatchedAt int64          // unix nano of the first CHARGED dispatch; 0 until one
	// StartedAt is when this episode opened, and it is what the stall clock runs from. It is
	// deliberately NOT DispatchedAt: an episode can fail to dispatch anything at all, and
	// that is the case most in need of a bound (see the stall check).
	StartedAt int64
	Outcome   string // "" in flight | gateOutcomeRecorded | gateOutcomeBlocked
	// LastDispatchError is the most recent CHARGED launch failure, carried so the block the
	// attempt ceiling surfaces can say the reviewer never started rather than never reported.
	// An unstarted failure does not set it: it costs no attempt, so it never reaches a block.
	LastDispatchError string
}

// errReviewerNotStarted marks a dispatch failure where NOTHING WAS LAUNCHED and a retry is
// genuinely likely to succeed, so gateOnceAt must not charge it an attempt.
//
// The dispatch loop has two failure kinds and they need opposite treatment. A launch that
// fails for a standing reason, a mirror clone that cannot complete, a refused dimension, a
// missing harness, fails identically every tick: uncharged it never reaches the attempt
// ceiling, never starts the stall clock, and the gate spins on it silently for as long as the
// task sits in the done set. A launch that never got far enough to fail, the tmux probe timing
// out while the server is wedged, is the opposite: with maxReviewerAttempts at 2, charging it
// spends the whole budget on reviewers that never started, during the exact hang the probe
// deadline exists to survive.
//
// So the kind is decided where the error is CREATED, where it is known, and carried on the
// error for errors.Is to test. The caller never re-derives it by matching message text, which
// would make the classification a property of the wording.
//
// CHARGING IS THE DEFAULT. Only a failure explicitly marked as unstarted goes uncharged, so an
// error nobody has classified escalates to a surfaced block rather than spinning invisibly. Of
// the two ways to be wrong, a gate that blocks on a transient failure tells the manager
// something and can be adjudicated; a gate that retries forever tells nobody anything.
var errReviewerNotStarted = errors.New("reviewer launch not attempted")

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
	// A store failure is not "no episode in flight". Surface it and leave the task where it
	// is; the next tick retries. Recording anything over an episode the gate cannot read is
	// exactly what moving this record out of the worker's directory was for.
	prog, _, err := m.readGateProgress(taskID)
	if err != nil {
		return GateSkipped, err
	}

	// Episode boundary: a first-ever gate, or the worker advanced past a prior episode's head
	// (a re-gate). Reset the episode for the new head — tear down the prior head's reviewer
	// windows, then run prep, which archives the previous episode's reports and stamps the new
	// one so a present report can only ever be this episode's. A prep refusal (dirty worktree
	// / stale base) is the worker's to fix (commit / rebase), not the daemon's: surface it
	// once and mark the head terminal so the pass does not re-prep every tick.
	if prog.Head != head {
		m.teardownReviewers(taskID, dispatchedDimensions(prog))
		if _, err := m.TrustPrep(taskID); err != nil {
			m.surfaceGateBlocked(taskID, head, "gate prep refused: "+err.Error())
			if werr := m.writeGateProgress(taskID, gateProgress{Head: head, StartedAt: now.UnixNano(), Outcome: gateOutcomeBlocked}); werr != nil {
				return GateBlocked, werr
			}
			return GateBlocked, nil
		}
		episodeDims, _ := m.requiredDimensions(t, head)
		// The episode's start is stamped here and nowhere else. It used to be normalized on
		// every tick from a zero value, which made the bound resettable by anything that
		// could zero it; the record is manager-owned now, so the boundary is the one place
		// that decides when an episode began.
		prog = gateProgress{Head: head, Dims: episodeDims, Attempts: map[string]int{}, StartedAt: now.UnixNano()}
		if werr := m.writeGateProgress(taskID, prog); werr != nil {
			return GateSkipped, werr
		}
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
	// recording a verdict over a different set than was reviewed.
	//
	// The set is composed by requiredDimensions from the prep stamp, reviewers.json and the
	// floor re-derived from the committed diff. The stamp decides and the file may only add;
	// the floor is unioned on top so a stamp written wrong cannot lower what gets dispatched
	// here, the same way it cannot lower what gets required at record time. It fails safe to
	// the full built-in set when no stamp covers this head, so this never under-reviews.
	//
	// It is then unioned with every dimension THIS EPISODE HAS ALREADY DISPATCHED, which makes
	// the set monotone across ticks and not merely within one. The composed set can still
	// shrink mid-episode: the floor falls back to the full three when it cannot resolve a base,
	// so a transient git failure dispatches a security reviewer that the next tick no longer
	// asks for. Without the union the dispatch loop below stops polling that dimension,
	// allReady goes true over the remaining ones, and the episode records a pass while the
	// reviewer the gate itself asked for is still working, after which teardownReviewers kills
	// it. foldDimensions cannot cover this: it adds an extra only once that extra's report is
	// pinned, which is what has not happened yet.
	//
	// Sticky does not mean unbounded. A dimension that stays required and never reports is
	// re-dispatched up to maxReviewerAttempts and then surfaced as a block, so the episode ends
	// on an unanswered reviewer rather than wedging on one.
	required, dropped := m.requiredDimensions(t, head)
	dims := unionDimensions(required, dispatchedDimensions(prog))
	// VALIDATE BEFORE COMPOSING ANYTHING FOR THE MANAGER. Both records these names come from
	// are files in the review inputs dir, and the gate_blocked payload below is the channel
	// the manager acts on, so an unusable name must be caught here rather than quoted into a
	// sentence about something else. The fold would block on it anyway; this is about what
	// reaches the manager and in what words.
	if err := review.ValidateDimensionSet(dims); err != nil {
		m.surfaceGateBlocked(taskID, head, err.Error())
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(taskID, prog)
		m.teardownReviewers(taskID, dims)
		return GateBlocked, nil
	}
	if len(dropped) > 0 {
		// The set shrank after prep: the inputs dir was edited during the review, which is
		// how a blocking report gets hidden. The manager adjudicates that, not the daemon.
		m.surfaceGateBlocked(taskID, head, droppedDimensionFinding(dropped).Summary)
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(taskID, prog)
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
			reason := fmt.Sprintf("reviewer %q produced no report after %d attempt(s)", dim, maxReviewerAttempts)
			if prog.LastDispatchError != "" {
				// Distinguish a reviewer that ran and said nothing from one that never started.
				reason += "; last dispatch error: " + prog.LastDispatchError
			}
			m.surfaceGateBlocked(taskID, head, reason)
			prog.Outcome = gateOutcomeBlocked
			m.writeGateProgress(taskID, prog)
			m.teardownReviewers(taskID, unionDimensions(dims, dispatchedDimensions(prog)))
			return GateBlocked, nil
		}
		toDispatch = append(toDispatch, dim)
	}

	// Bound the episode BEFORE deciding what to do about it, so every route out of an
	// unfinished episode passes through this check. It used to live inside the waiting branch,
	// which two different wedges walk straight past: a dispatch whose launches all come back
	// unstarted returns from the dispatch branch below, and reaches nothing.
	//
	// The clock runs from the EPISODE rather than from the first dispatch. Keying it on
	// DispatchedAt tied both of the gate's bounds to the same fact: the attempt ceiling counts
	// launches, so an episode that never launches anything was invisible to both and ran
	// forever. That is reachable with no attacker. reviewerWindowAlive uses the bool
	// tmux.WindowExists, which folds a timed-out probe to "present" so a wedged server cannot
	// make the gate double-launch into an occupied worktree, so a server wedged from the
	// episode's first tick makes every dimension read as already-running.
	//
	// Running it from the episode leaves the charging split alone, which is the point: a
	// transient wedge still burns no retries, because the fix is not to start charging for it.
	if !allReady && reviewerTimeout > 0 && prog.StartedAt != 0 && now.Sub(time.Unix(0, prog.StartedAt)) > reviewerTimeout {
		// DispatchedAt still earns its place here: it separates "reviewers ran and went
		// quiet" from "nothing ever got off the ground", which are different problems for
		// whoever picks this up.
		reason := fmt.Sprintf("reviewers did not all report within %s", reviewerTimeout)
		if prog.DispatchedAt == 0 {
			reason = fmt.Sprintf("no reviewer was dispatched within %s; the episode made no progress", reviewerTimeout)
			if prog.LastDispatchError != "" {
				reason += "; last dispatch error: " + prog.LastDispatchError
			}
		}
		m.surfaceGateBlocked(taskID, head, reason)
		prog.Outcome = gateOutcomeBlocked
		m.writeGateProgress(taskID, prog)
		m.teardownReviewers(taskID, unionDimensions(dims, dispatchedDimensions(prog)))
		return GateBlocked, nil
	}

	if len(toDispatch) > 0 {
		charged := false
		for _, dim := range toDispatch {
			if err := reviewerDispatcher(m, taskID, dim, dir, head, t.Project, t.Worktree); err != nil {
				fmt.Fprintf(os.Stderr, "ttorch: gate could not dispatch reviewer %s/%s: %v\n", taskID, dim, err)
				if errors.Is(err, errReviewerNotStarted) {
					// Nothing started and a retry is likely to work. Costs no attempt and does
					// not start the stall clock, so a wedged tmux does not consume the budget
					// that exists to outlast it.
					continue
				}
				// A standing failure. Charged like a reviewer that started and never reported,
				// so it reaches the ceiling and surfaces instead of retrying forever.
				prog.LastDispatchError = fmt.Sprintf("%s: %v", dim, err)
			}
			prog.Attempts[dim]++
			charged = true
		}
		// The stall clock starts on the first attempt that COUNTED, whether it launched or
		// failed for a standing reason. An unstarted launch leaves it alone for the same
		// reason it leaves the attempt count alone.
		if charged && prog.DispatchedAt == 0 {
			prog.DispatchedAt = now.UnixNano()
		}
		m.writeGateProgress(taskID, prog)
		return GateDispatched, nil
	}

	if !allReady {
		return GateWaiting, nil
	}

	// Every required dimension has a report pinned to head. Aggregate to decide pass vs block,
	// over the required set PLUS any dimension this episode dispatched that is no longer
	// required but reported anyway (see foldDimensions) — a blocking finding from a reviewer
	// the gate itself asked for is never discarded because the set shrank under it.
	// review.Aggregate is the SAME deterministic fold the manager's `trust record` uses, and
	// TrustRecord derives the same extras, so the daemon does not fork the decision.
	fold := m.foldDimensions(dir, head, dims, dispatchedDimensions(prog))
	v, err := review.Aggregate(dir, head, fold)
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
		m.writeGateProgress(taskID, prog)
		m.teardownReviewers(taskID, unionDimensions(fold, dispatchedDimensions(prog)))
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
	m.writeGateProgress(taskID, prog)
	m.teardownReviewers(taskID, unionDimensions(fold, dispatchedDimensions(prog)))
	m.audit(fmt.Sprintf("gate-record task=%s commit=%s verdict=pass actor=daemon", taskID, short(head)))
	return GateRecorded, nil
}

// Gateable reports whether repo is a daemon-gate candidate: a trusted repo, where a recorded
// pass auto-authorizes the merge. The scheduler's gate pass uses it to skip claiming done tasks
// in non-trusted repos (where a daemon-recorded verdict would not advance delivery).
func (m *Manager) Gateable(repo string) bool {
	return projectinit.ReadMode(repo) == "trusted"
}

// EVERY dispatched reviewer runs OUTSIDE the worker's worktree, in a scratch workspace holding
// the staged diff and a BARE mirror of the repo. reviewerCwd is where that happens; this note
// is why it applies to all of them rather than to a list.
//
// Everything a Claude session resolves from its cwd is content the worker can commit: project
// .claude/settings.json hooks, project .claude/agents/ definitions that outrank the installed
// ones, CLAUDE.md. Guarding those paths in the diff is the move that never converged, because
// each review round found a resolver the previous list did not know about. Moving the cwd out
// of the tree stops the enumeration instead of extending it.
//
// This was a pilot on security alone, on the reasoning that a forged empty report damages that
// dimension most and the cost of losing ripgrep over a working tree should be measured on one
// dimension first. The pilot's scope was the hole. Correctness and scope still launched with
// cwd = the worktree under `claude --dangerously-skip-permissions`, so a committed Stop hook
// executed as the lead inside those two sessions, and from there could write the security
// report and kill the isolated reviewer's window. A report already pinned to head makes the
// dispatch loop skip that dimension on every later tick, so the isolated reviewer never runs
// again. The worker's own session is usually gone by gate time, which means the GATE is what
// re-executes the worker's committed configuration. Isolating one dimension while two others
// can write its report is not isolation.
//
// The measured cost is the one the pilot was there to find, and it is per reviewer rather than
// per gate: a `git clone --mirror` of a local repo hardlinks its objects, and the workspace is
// torn down with the episode, so it is three short-lived mirrors instead of one. The review
// experience is unchanged from the pilot: `git -C <bare> show` and `git grep <tree-ish>`, which
// the brief teaches.
//
// It still does not cover the skill-driven path, where the manager dispatches ttorch-reviewer-*
// subagents from its own cwd. That path's exposure is the DEFAULT BRANCH's CLAUDE.md and
// .claude/, which is a different problem and not closed here.

// reviewWorkspaceDir is the scratch cwd for an isolated dimension's reviewer: a per-dimension
// directory under the task's review-workspace root, so two isolated reviewers never share a
// mirror and the episode teardown can drop it wholesale. The root is paths.ReviewWorkspaceDir
// and NOT the review-inputs dir, because a session's cwd ancestors are on its configuration
// path, and the inputs dir is the directory whose CONTENT the gate cannot vouch for.
// The dimension is checked with review.ValidDimensionName, the same predicate
// review.InputPath enforces, so both sinks accept exactly the same names. InputPath itself
// cannot serve here: it resolves inside the review-INPUTS dir and injects the reports
// subdirectory, and this path is deliberately rooted somewhere else (see above). The join is
// exempted by name in TestNoUnvalidatedDimensionSink for that reason.
func (m *Manager) reviewWorkspaceDir(taskID, dim string) string {
	if !safePathComponent(taskID) || !review.ValidDimensionName(dim) {
		return ""
	}
	return filepath.Join(m.P.ReviewWorkspaceDir(taskID), dim)
}

// safePathComponent reports whether s can be joined into a path as a single component without
// escaping it. The workspace path is built from a task id and a dimension and then handed to
// os.RemoveAll, and nothing else in the tree validates a task id, so
// ReviewWorkspaceDir("../../../../tmp/victim") resolves outside the ttorch home and the
// episode teardown would delete whatever is there. Task ids are written by the manager rather
// than by a worker, so this is a guard against a malformed id rather than a hostile one, but a
// delete path assembled from an unvalidated string should not depend on that.
//
// A caller that gets "" must skip the operation rather than fall back to a shorter path: the
// parent of a per-dimension workspace is the per-task root, and deleting that on a bad
// component would be the same mistake one level up.
func safePathComponent(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.ContainsAny(s, `/\`) && !strings.ContainsRune(s, os.PathSeparator)
}

// reviewerCwd returns the directory a dimension's reviewer session runs in, and the bare mirror
// it reads source from ("" when there is none). An isolated dimension gets a freshly
// materialized scratch workspace (see prepareReviewWorkspace); every other dimension still runs
// in the worker's worktree at the reviewed commit.
func (m *Manager) reviewerCwd(taskID, dim, inputsDir, repo, wt, head string) (cwd, bare string, err error) {
	ws := m.reviewWorkspaceDir(taskID, dim)
	if ws == "" {
		return "", "", fmt.Errorf("refusing to build a review workspace for task %q dimension %q: not a single safe path component", taskID, dim)
	}
	return prepareReviewWorkspace(ws, inputsDir, repo, wt, head)
}

// prepareReviewWorkspace materializes the scratch cwd an isolated reviewer runs in and returns
// it together with the bare mirror inside it. The workspace holds:
//
//   - diff.patch — a copy of the committed three-dot diff TrustPrep staged, so the reviewer's
//     primary input sits in its own cwd.
//   - repo.git — a BARE mirror of the project repo. Bare means no working tree, so there is no
//     .claude/, no CLAUDE.md and no project config of any kind for the session to discover. A
//     clone writes the mirror a fresh config and never copies the source's hooks, so nothing
//     executable comes across either. The reviewer reads surrounding source out of it at the
//     reviewed commit with `git -C <bare> show <sha>:<path>` and searches it with
//     `git -C <bare> grep <pattern> <sha>`.
//
// It is rebuilt from scratch on every dispatch, so a re-dispatch after the worker advanced
// never reviews against a stale mirror. A local clone hardlinks its objects, so the rebuild
// costs little.
func prepareReviewWorkspace(dir, inputsDir, repo, wt, head string) (cwd, bare string, err error) {
	cwd = dir
	if err := os.RemoveAll(cwd); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", "", err
	}
	// Best-effort: prep always stages diff.patch before a reviewer is dispatched, and the brief
	// also names it by absolute path in the inputs dir, so a missing copy is not fatal here.
	if patch, rerr := os.ReadFile(filepath.Join(inputsDir, "diff.patch")); rerr == nil {
		if werr := os.WriteFile(filepath.Join(cwd, "diff.patch"), patch, 0o644); werr != nil {
			return "", "", werr
		}
	}
	bare = filepath.Join(cwd, "repo.git")
	if out, cerr := exec.Command("git", "clone", "--mirror", "--quiet", repo, bare).CombinedOutput(); cerr != nil {
		return "", "", fmt.Errorf("mirror %s for review: %w: %s", repo, cerr, strings.TrimSpace(string(out)))
	}
	// The reviewed commit is normally reachable from the worker's branch, which the mirror
	// copies. Fall back to fetching it from the worktree when it is not (a detached worker
	// HEAD), so the reviewer can always read the source it is judging.
	if !commitInMirror(bare, head) {
		if out, ferr := exec.Command("git", "-C", bare, "fetch", "--no-tags", "--quiet", wt, "+HEAD:refs/ttorch/reviewed").CombinedOutput(); ferr != nil {
			return "", "", fmt.Errorf("fetch the reviewed commit %s into the review mirror: %w: %s", short(head), ferr, strings.TrimSpace(string(out)))
		}
	}
	// Re-check rather than trust the fetch. It pulls the worktree's CURRENT HEAD, which is
	// only the reviewed commit while the worker has not moved; if it advanced between the
	// gate pinning head and this dispatch, the fetch succeeds and brings back the WRONG
	// commit. Reporting success there would hand the reviewer a mirror in which every
	// `git show <head>:<path>` the brief tells it to run fails, and leave nothing but the
	// brief's "say so in the report" between a broken read path and a quiet pass. A
	// workspace that cannot serve the commit under review is an error; the gate then retries
	// the dispatch next tick, by which point the moved head has re-prepped the episode.
	if !commitInMirror(bare, head) {
		return "", "", fmt.Errorf("the review mirror for %s does not contain the reviewed commit %s; the worker HEAD moved since the gate pinned it", filepath.Base(dir), short(head))
	}
	return cwd, bare, nil
}

// commitInMirror reports whether sha resolves to a commit object in the bare mirror.
func commitInMirror(bare, sha string) bool {
	return exec.Command("git", "-C", bare, "cat-file", "-e", sha+"^{commit}").Run() == nil
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
		// A reviewer's scratch workspace holds a bare mirror of the repo. It is rebuilt from
		// scratch on the next dispatch, so dropping it here keeps one mirror per in-flight
		// reviewer rather than one per task per episode.
		if ws := m.reviewWorkspaceDir(taskID, dim); ws != "" {
			_ = os.RemoveAll(ws)
		}
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

// readGateProgress loads a task's episode from the store. err is a real store failure and
// callers must NOT read it as "no episode in flight": that reading is what let a lost record
// record a verdict. found=false means there is genuinely no row, which is a fresh task.
func (m *Manager) readGateProgress(taskID string) (gateProgress, bool, error) {
	row, found, err := m.Store.GetGateEpisode(context.Background(), taskID)
	if err != nil || !found {
		return gateProgress{Attempts: map[string]int{}}, found, err
	}
	p := gateProgress{
		Head:              row.Head,
		Attempts:          map[string]int{},
		Outcome:           row.Outcome,
		LastDispatchError: row.LastDispatchError,
		StartedAt:         row.StartedAt.UnixNano(),
	}
	if !row.DispatchedAt.IsZero() {
		p.DispatchedAt = row.DispatchedAt.UnixNano()
	}
	// A row whose JSON will not parse is a store-side corruption, not worker input. Report it
	// rather than silently continuing with an empty set, which is the failure this move exists
	// to remove.
	if err := json.Unmarshal([]byte(row.Dims), &p.Dims); err != nil {
		return gateProgress{Attempts: map[string]int{}}, true, fmt.Errorf("gate episode for %q has unreadable dims: %w", taskID, err)
	}
	if err := json.Unmarshal([]byte(row.Attempts), &p.Attempts); err != nil {
		return gateProgress{Attempts: map[string]int{}}, true, fmt.Errorf("gate episode for %q has unreadable attempts: %w", taskID, err)
	}
	if p.Attempts == nil {
		p.Attempts = map[string]int{}
	}
	return p, true, nil
}

// writeGateProgress persists a task's episode. A failed write is reported rather than
// swallowed: the record is manager-owned now, so losing it is a real fault in something the
// gate controls, not a worker-reachable condition to be tolerated.
func (m *Manager) writeGateProgress(taskID string, p gateProgress) error {
	dims, err := json.Marshal(p.Dims)
	if err != nil {
		return err
	}
	attempts, err := json.Marshal(p.Attempts)
	if err != nil {
		return err
	}
	row := db.GateEpisode{
		TaskID: taskID, Head: p.Head, Dims: string(dims), Attempts: string(attempts),
		StartedAt: time.Unix(0, p.StartedAt), Outcome: p.Outcome, LastDispatchError: p.LastDispatchError,
	}
	if p.DispatchedAt != 0 {
		row.DispatchedAt = time.Unix(0, p.DispatchedAt)
	}
	return m.Store.SaveGateEpisode(context.Background(), row)
}

// spawnReviewer launches one dimension's adversarial reviewer as a Claude session in a tmux
// window — the production wiring behind reviewerDispatcher. It is the daemon analogue of the
// manager fanning out a `ttorch-reviewer-<dim>` subagent: the launched session reads the same
// materialized inputs (inputsDir) and the same pinned commit (head), reviews ONLY its
// dimension, and writes the same commit-pinned <dim>.json report — so the daemon orchestrates
// the real adversarial reviewers, it does not replace them with a rubber stamp.
//
// It runs in a scratch workspace outside the worker's tree (see reviewerCwd), reading source
// from a bare mirror rather than a checkout. It never edits anything, review is read-only. It is idempotent: it no-ops when
// the dimension's window already exists, so a re-dispatch never doubles a running reviewer.
//
// The idempotence probe uses WindowExistsErr, not the bool WindowExists, because this is the
// one call site where the bool's timeout fold is wrong. WindowExists answers "present" for a
// probe that timed out, which is right for the callers that would otherwise duplicate an
// agent, but here it would make spawnReviewer return nil having launched nothing, and the
// caller in gateOnceAt counts an attempt for every launch. Reporting the timeout instead
// leaves the retry to the next tick.
//
// Only a timeout is treated that way. Any other probe failure means the window is genuinely
// not there, including the one that matters here: list-windows fails when the shared session
// does not exist yet, and that case has to fall through to the EnsureSession below rather
// than error out, or the first reviewer could never be dispatched.
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
	exists, err := tmux.WindowExistsErr(m.Session, window)
	if errors.Is(err, tmux.ErrTimeout) {
		// A wedged tmux server: the window could not be inspected, so nothing was launched and
		// nothing can be concluded about whether a launch would have worked. Marked unstarted
		// so the retry budget survives the hang (see errReviewerNotStarted).
		return fmt.Errorf("could not tell whether reviewer window %q is already running: %w: %w", window, err, errReviewerNotStarted)
	}
	if exists {
		return nil // already running — idempotent
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	cwd, bare, err := m.reviewerCwd(taskID, dim, inputsDir, repo, wt, head)
	if err != nil {
		return err
	}
	h := harness.Resolve()
	sid := harness.NewSessionID()
	// Pre-accept the harness folder-trust prompt and write the trimmed worker settings (no AI
	// co-author trailer) for the directory the session ACTUALLY runs in, so the reviewer runs
	// autonomously without depending on the folder trust the worker's own spawn granted over
	// the worktree. That spawn's trust entry is untouched; this only stops the reviewer relying
	// on it.
	_ = harness.WriteWorkerSettings(h, cwd)
	harness.TrustWorktree(h, repo, cwd)
	brief := reviewerBrief(taskID, dim, inputsDir, head, reportPath, bare)
	if err := os.WriteFile(briefPath, []byte(brief), 0o644); err != nil {
		return err
	}
	if err := m.newWindow(window, cwd, "review · "+dim+" · "+taskID); err != nil {
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
//
// bare is the review workspace's bare mirror for an isolated dimension, or "" when the session
// runs in the worker's worktree. When it is set the brief MUST teach the git read path, because
// the session has no working tree to search and a reviewer that silently gives up on reading
// callers writes a worse report rather than an honest "I could not check this".
func reviewerBrief(taskID, dim, inputsDir, head, reportPath, bare string) string {
	return fmt.Sprintf(`# Adversarial trust-gate review — %s dimension (task %s)

You are the **%s** reviewer in ttorch's adversarial trust gate, dispatched by the scheduler
daemon. A passing verdict may merge this diff WITHOUT a human reading it, so your judgment is
load-bearing. Review ONLY the %s dimension — the other dimensions are other reviewers' jobs.
You NEVER edit, commit, or push code; review is a static read of the diff.

Use the Task tool to dispatch the `+"`ttorch-reviewer-%s`"+` subagent, giving it exactly:
- review inputs dir: %s
- reviewed commit (head): %s

It must read %s/{diff.patch, brief.md, validate.json, head.txt}, review only the %s dimension,
and write its findings to %s following the findings contract:

    {"dimension": "%s", "reviewedSha": "%s", "findings": [ ... ]}

where each finding is {"dimension","severity","reviewer","summary"}, severity is one of
low|medium|high|critical (high/critical block the merge; bias to high on uncertainty), a clean
review is "findings": [], and reviewedSha MUST equal %s verbatim.

validate.json records what the gate's own validate saw for this commit. Read it as a report,
not as proof: it is a file in a directory this session and others can write, and the gate
re-runs the suite itself before anything merges. Do not re-run the build or test suite here
either — that is not your job and a green there would not authorize anything. If a finding
depends on whether the suite really passes, say so in the finding.

If the `+"`ttorch-reviewer-%s`"+` subagent is unavailable, perform the review yourself per the
exact same contract and write %s. Either way the ONLY required output is that file. When it is
written, you are done — do not modify the repository.
%s`, dim, taskID, dim, dim, dim, inputsDir, head, inputsDir, dim, reportPath, dim, head, head, dim, reportPath, bareSourceSection(bare, head))
}

// bareSourceSection is the part of a reviewer brief that replaces a working tree. An isolated
// reviewer's cwd is a scratch workspace, so there is nothing to ripgrep; it reads and searches
// the repository through a bare mirror at the reviewed commit instead. `git grep` over a
// tree-ish recovers most of what a tree search gave (it takes a pattern, -n, and a pathspec),
// which is why the cost of moving the session out of the tree is a worse review experience
// rather than a blind one. It returns "" for a dimension that still runs in the worktree.
func bareSourceSection(bare, head string) string {
	if bare == "" {
		return ""
	}
	return fmt.Sprintf(`
## Reading source: there is no working tree here

Your working directory is a scratch workspace, NOT a checkout of the project, because a
reviewer must not run inside the tree it is judging. Nothing under your cwd is project source
and there is nothing to ripgrep. The repository is a BARE mirror at:

    %s

Read and search it AT THE REVIEWED COMMIT (%s):

    git -C %s ls-tree -r --name-only %s            # every path at the reviewed commit
    git -C %s show %s:<path>                       # one file's full contents
    git -C %s grep -n <pattern> %s -- <pathspec>   # search the whole tree
    git -C %s log --oneline -20 %s -- <path>       # how a file got this way

Use these wherever you would otherwise have searched a checkout. Reading the callers of a
changed function, the other implementations of an interface, or the tests that cover the
touched code is still your job — the mirror makes all of it available, so a finding you could
have caught by reading around the diff is still yours to catch. If some check genuinely cannot
be made this way, say so in the report rather than passing quietly.
`, bare, short(head), bare, head, bare, head, bare, head, bare, head)
}

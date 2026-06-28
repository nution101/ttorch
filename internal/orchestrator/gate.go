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
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
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

// scaledReviewers is the persisted reviewer-set decision: the change-size class and the
// dimensions the trust gate requires for it. The manager reads it to spawn exactly those
// reviewer subagents.
type scaledReviewers struct {
	Size       review.Size `json:"size"`
	Dimensions []string    `json:"dimensions"`
}

// ReviewersFor returns the review dimensions the trust gate requires for taskID, as
// recorded by TrustPrep in reviewers.json. A missing or malformed record (e.g. inputs
// prepared by an older ttorch, or an empty set) falls back to the full set — fail-safe, so
// a verdict is never recorded against fewer reviewers than were actually prepared.
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
// ReviewInputsDir: the COMMITTED three-dot diff against the default branch (diff.patch),
// the brief (brief.md, if one was written), a fresh validate of the committed sha
// (validate.json), and the reviewed HEAD (head.txt). It refuses a dirty worktree and reads
// only committed objects, so the reviewers see exactly the commit that will fast-forward —
// a worker cannot present a benign working tree while a different commit merges.
//
// It also refuses a STALE BASE up front: if the default branch carries commits the worker's
// HEAD lacks, prep fails (staging nothing) and tells the manager to rebase the worker first.
// Reviewing a stale-base branch diffs against a base that no longer matches what merges, the
// merge gate would refuse the fast-forward anyway, and the diff would otherwise surface the
// default's own lead as phantom reverts. It returns the inputs dir.
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

	// Stale-base guard — run BEFORE staging any inputs or dispatching reviewers. If the
	// default branch carries commits the worker's HEAD lacks, the branch was cut from an
	// older base: the merge gate would refuse the fast-forward anyway, and a base-relative
	// review diff would render the default's own lead as phantom reverts — which burned a
	// full three-reviewer pass and nearly masked a real bug (the cosign-strict /
	// liveness-dwell near-miss). `git rev-list <head>..<def>` lists exactly the commits the
	// default has that the worker lacks; any output means the base is stale. Fail loudly so
	// the manager rebases the worker onto the current default first, and stage nothing.
	behind, err := gitOut(t.Worktree, "rev-list", head+".."+def)
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
	// The reviewers' diff is the COMMITTED three-dot diff `git diff <def>...<head>` (the
	// merge-base diff), so it contains ONLY the branch's own changes — never any lead the
	// default gained since the branch was cut. The stale-base guard above makes <def> an
	// ancestor of <head> here, but the three-dot form is the correct, intent-revealing way
	// to diff a branch against its base, and is defense in depth against a phantom-revert
	// diff. Reads committed objects only, never the working tree.
	diff, err := mergeBaseDiff(t.Worktree, def, head)
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
	files, filesOK := diffFiles(t.Worktree, def, head)
	lines, binary, statOK := diffLineStat(t.Worktree, def, head)
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
	if err := os.WriteFile(filepath.Join(dir, "validate.json"), append(vb, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), []byte(head+"\n"), 0o644); err != nil {
		return "", err
	}
	m.audit(fmt.Sprintf("trust-prep task=%s commit=%s size=%s reviewers=%s",
		taskID, short(head), size, strings.Join(dims, "+")))
	return dir, nil
}

// TrustRecord aggregates the reviewers' per-dimension reports for taskID into a
// commit-pinned verdict and persists it, recording GatePassed/ReviewedSHA on the
// task. The sha it covers must still be the worker's HEAD (a record-time pin against
// a commit landing after review).
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
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, m.ReviewersFor(taskID))
	if err != nil {
		return zero, err
	}
	// Pin the reviewed diff's content identity onto the verdict (the committed three-dot diff
	// the reviewers read) so a later clean rebase onto an advanced default can carry the
	// verdict forward without re-running the reviewers — see carryVerdictForward. Computed from
	// committed objects, so it is independent of the worktree state.
	patch, derr := mergeBaseDiff(t.Worktree, worktree.DefaultBranch(t.Project), sha)
	if derr != nil {
		return zero, fmt.Errorf("trust record %q: could not compute the reviewed diff identity: %w", taskID, derr)
	}
	verdict.DiffID = review.DiffID([]byte(patch))
	if err := review.Write(m.P.ReviewVerdictFile(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	t.ReviewedSHA = sha
	t.GatePassed = verdict.Overall == review.Pass
	t.ApprovedBy = ""
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
		}
	}
	// Persist the verdict provenance (gate/approval/sha) in one write. The
	// review_recorded event is manager-authored and non-actionable (§1.3). t was
	// mutated above as the accumulator for the auto-mint decision.
	if err := m.Store.RecordDelivery(context.Background(), taskID, db.Delivery{
		GatePassed: t.GatePassed, ApprovedBy: t.ApprovedBy, ReviewedSHA: t.ReviewedSHA,
		EventType: db.EventReviewRecorded, Actor: db.ActorManager,
	}); err != nil {
		return zero, err
	}
	autoMinted := "no"
	if t.ApprovedBy == "auto" {
		autoMinted = "yes"
	}
	m.audit(fmt.Sprintf("trust-record task=%s commit=%s verdict=%s mode=%s auto-approved=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project), autoMinted))
	return verdict, nil
}

// TrustShow returns the current valid (unexpired) verdict for taskID, if any,
// without consuming it.
func (m *Manager) TrustShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.P.ReviewVerdictFile(taskID))
}

// securityVerdictPath is where the standalone, advisory security-everywhere verdict
// lives — beside the review inputs the security reviewer reads, and DISTINCT from the
// trust gate's ReviewVerdictFile so the two never interfere: the advisory pass can
// never mint an approval or satisfy the trusted gate, and recording it never disturbs a
// trust verdict.
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
// the review inputs the QA reviewer reads, and DISTINCT from both the trust gate's
// ReviewVerdictFile and the security audit's verdict, so none of the three interfere: the QA
// pass can never mint an approval or satisfy the trusted gate, and recording it never
// disturbs a trust or security verdict.
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

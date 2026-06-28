package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// Approve grants a short-lived approval token authorizing a merge for taskID.
// This is intended for the lead to run, not the manager.
func (m *Manager) Approve(taskID string, ttl time.Duration) error {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return err
	}
	if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("human", head)); err != nil {
		return err
	}
	// Record that THIS approval was the lead's — both in the token's provenance (the
	// authority for the merge's audit label) and in the persisted task state (overwriting
	// any prior auto-mint marker so the two never drift). RecordDelivery preserves the
	// gate/sha fields it is given; the 'approved' event is manager/lead-authored and
	// non-actionable, so it never wakes a watcher.
	if err := m.Store.RecordDelivery(context.Background(), taskID, db.Delivery{
		GatePassed: t.GatePassed, ApprovedBy: "human", ReviewedSHA: t.ReviewedSHA,
		EventType: db.EventApproved, Actor: db.ActorLead,
	}); err != nil {
		return err
	}
	m.audit(fmt.Sprintf("approve task=%s commit=%s ttl=%s", taskID, short(head), ttl))
	return nil
}

// recordDelivered marks a task delivered and appends a typed, manager-authored,
// non-actionable delivery event in one transaction (§3.4): `delivered` for a local
// fast-forward (MergeLocal, standalone or via Land's local/validated/trusted path) and
// `merged` for a PR merge (Land's pr path). It is best-effort by design: the merge has
// already happened and is irreversible, and the events row is only a best-effort mirror
// of the must-succeed audit.log (whose abort-on-failure semantics, §1105, are unchanged),
// so a failed append is surfaced — never allowed to fail a completed merge.
func (m *Manager) recordDelivered(taskID, eventType, payload string) {
	if _, err := m.Store.RecordTransition(context.Background(), taskID, db.StatusDelivered, db.TaskFields{}, eventType, db.ActorManager, payload); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the %s event for %s: %v\n", eventType, taskID, err)
	}
}

// MergeLocal fast-forwards the repo's local default branch to the worker's HEAD —
// the sole sanctioned state-changing write to a real checkout. It always requires a
// valid approval token, the default branch checked out and clean, and a clean
// fast-forward.
//
// The trust gate is layered on top: when requireVerdict is set, or the repo is in
// trusted delivery mode, the merge ADDITIONALLY requires a passing, commit-pinned
// review.Verdict and a fresh, green validate run — and treats no-checks-detected as a
// hard BLOCK (an empty Failures() must never read as a pass). The verdict, like the
// approval, is consumed only immediately before the merge and pinned to the exact
// commit being merged (verdict.ReviewedSHA==workerHead), closing the TOCTOU window
// where a commit lands after review. Every merge is recorded in the audit log.
func (m *Manager) MergeLocal(taskID string, requireVerdict bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	if !approval.Valid(m.P.ApprovalFile(taskID)) {
		return "", fmt.Errorf("no valid approval for %q; the lead must run 'ttorch approve %s' first", taskID, taskID)
	}
	repo := t.Project
	gated := requireVerdict || projectinit.ReadMode(repo) == "trusted"
	// Read the approval token's provenance (human|auto) from the token itself, not from
	// mutable task state, so a crash between minting and saving can never relabel a merge.
	tokData, _ := approval.Data(m.P.ApprovalFile(taskID))
	tokBy, _ := splitApprovalPayload(tokData)
	// Fail closed: an AUTO-minted approval is only valid through the active gate. If the
	// gate is not active here (the repo no longer reads as trusted — e.g. a degraded
	// AGENTS.md silently dropped the mode — and no --require-verdict), an auto token must
	// not merge ungated.
	if !gated && tokBy == "auto" {
		return "", fmt.Errorf("%q carries an auto-approval that is only valid through the trust gate, but the gate is not active (repo not in trusted mode and no --require-verdict); refusing to merge ungated", taskID)
	}
	// The committed object that will fast-forward. Everything the gate validates and pins
	// is THIS sha — never the mutable worktree, which a running worker could change.
	workerHead, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
	}
	def := worktree.DefaultBranch(repo)
	if gated {
		// Defense in depth: the worktree must be clean (a clean signal that no worker is
		// mid-edit), though correctness no longer depends on it — the gate validates the
		// committed sha, not the worktree.
		if clean, err := worktree.IsClean(t.Worktree); err != nil || !clean {
			return "", fmt.Errorf("trust gate: the worktree for %q is not clean; commit or discard all changes before merging", taskID)
		}
		// A trusted AUTO-merge's green authority must be the default-branch gate script —
		// never ecosystem detection on the worker's checkout (go.mod/package.json, which
		// the worker controls). Without it, require a human approval; refuse here before
		// any worker-defined validation runs. (A human-approved gated merge may use the
		// detection fallback.)
		if tokBy == "auto" && !hasDefaultBranchGateScript(repo) {
			return "", fmt.Errorf("trust gate: %q has no .ttorch/validate.sh on the default branch, so a trusted auto-merge's checks would be worker-defined; the lead must approve it explicitly with 'ttorch approve %s'", taskID, taskID)
		}
		// Require a passing, unexpired verdict (load, not yet consume — a recoverable
		// refusal below must leave it intact for a retry). Absent/expired/blocking all
		// fail closed.
		v, ok := review.Load(m.P.ReviewVerdictFile(taskID))
		if !ok {
			return "", fmt.Errorf("trust gate: no valid review verdict for %q; run 'ttorch trust prep %s', review, then 'ttorch trust record %s'", taskID, taskID, taskID)
		}
		if v.Overall != review.Pass {
			return "", fmt.Errorf("trust gate: the review verdict for %q is %q, not pass; resolve the blocking findings and re-record", taskID, v.Overall)
		}
		// Validate the COMMITTED sha (an immutable detached checkout), using the gate
		// definition from the DEFAULT BRANCH. No checks detected is a hard BLOCK. When trust
		// prep already validated this EXACT sha (its commit-pinned validate.json is still the
		// gate's notion of green for this commit), reuse that result rather than re-run the
		// identical full suite — only HEAD moving since prep, or no pinned result, forces a
		// fresh run. The merged commit is still backed by a green, commit-pinned validate.
		green, results, _, err := m.validateForMerge(repo, taskID, workerHead)
		if err != nil {
			return "", err
		}
		if !green {
			if len(results) == 0 {
				return "", fmt.Errorf("trust gate: no checks detected for %q; the gate requires a build/test/lint suite on the default branch (add .ttorch/validate.sh)", taskID)
			}
			return "", fmt.Errorf("trust gate: %d of %d checks failed for %q; fix them, re-validate, and re-record the verdict", len(validate.Failures(results)), len(results), taskID)
		}
		// A trusted AUTO-merge may not change the gate's own definition — that requires a
		// human. Checked against the COMMITTED diff, so reverting the bytes in the worktree
		// cannot hide it. (A human-approved gated merge, tokBy=="human", is allowed to.)
		if tokBy == "auto" {
			if touched, name, err := diffTouchesGateConfig(repo, def, workerHead); err != nil {
				return "", err
			} else if touched {
				return "", fmt.Errorf("trust gate: %q changes a gate-definition file (%s); a trusted auto-merge cannot alter its own gate — the lead must approve it explicitly with 'ttorch approve %s'", taskID, name, taskID)
			}
		}
		// HEAD-unchanged bracket: the worker must not have advanced HEAD during the gate,
		// so the sha we validated and pinned is still the sha that merges.
		if cur, err := worktree.Head(t.Worktree); err != nil || cur != workerHead {
			return "", fmt.Errorf("trust gate: the worker for %q advanced during review; re-prep, re-review, and re-record", taskID)
		}
	}
	cur, _ := worktree.CurrentBranch(repo)
	if cur != def {
		return "", fmt.Errorf("repo is on %q, not the default branch %q", cur, def)
	}
	// Only tracked changes block a fast-forward; untracked files (e.g. an `ttorch init`
	// AGENTS.md the developer hasn't committed) are fine, and git's own --ff-only
	// guards the rare untracked-collision case.
	if changed, _ := worktree.HasTrackedChanges(repo); changed {
		return "", fmt.Errorf("repo has uncommitted changes to tracked files; commit or stash before merging")
	}
	defHead, err := worktree.Head(repo)
	if err != nil {
		return "", err
	}
	if !worktree.IsAncestor(repo, defHead, workerHead) {
		return "", fmt.Errorf("worker %q is not a fast-forward of %q; have the worker rebase first", taskID, def)
	}
	// Consume the approval only now — immediately before the state change — so a
	// recoverable refusal above leaves it intact for a retry, and require it to
	// authorize exactly the commit being merged (no changes since the lead reviewed).
	approvedData, ok := approval.Consume(m.P.ApprovalFile(taskID))
	if !ok {
		return "", fmt.Errorf("approval for %q expired before merge; run 'ttorch approve %s' again", taskID, taskID)
	}
	approvedBy, approvedHead := splitApprovalPayload(approvedData)
	if approvedHead != workerHead {
		return "", fmt.Errorf("worker %q changed since approval (approved %s, now %s); re-review with 'ttorch review-diff %s' and approve again", taskID, short(approvedHead), short(workerHead), taskID)
	}
	if !gated {
		if err := worktree.MergeFastForward(repo, workerHead); err != nil {
			return "", err
		}
		m.audit(fmt.Sprintf("merge-local task=%s repo=%s %s -> %s", taskID, repo, def, short(workerHead)))
		m.recordDelivered(taskID, db.EventDelivered, fmt.Sprintf("%s -> %s", def, short(workerHead)))
		return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
	}
	// Consume the verdict beside the approval and pin it to the merged commit — the
	// second commit-pin, parallel to approvedHead==workerHead, so a commit pushed after
	// review can never ride in unreviewed. Consume re-checks pass, so a verdict that
	// turned blocking since the load above fails closed here too.
	cv, ok := review.Consume(m.P.ReviewVerdictFile(taskID))
	if !ok {
		return "", fmt.Errorf("trust gate: the review verdict for %q expired or is no longer passing; re-record it with 'ttorch trust record %s'", taskID, taskID)
	}
	if cv.ReviewedSHA != workerHead {
		return "", fmt.Errorf("trust gate: the verdict for %q covers %s but the worker is now %s; re-review and re-record", taskID, short(cv.ReviewedSHA), short(workerHead))
	}
	// Attribute the audit to the consumed token's provenance, and fail closed if it is
	// unknown (a legacy token with no provenance must not merge through the gate).
	var approver string
	switch approvedBy {
	case "auto", "human":
		approver = approvedBy
	default:
		return "", fmt.Errorf("trust gate: the approval for %q has no recorded provenance; re-approve with 'ttorch approve %s'", taskID, taskID)
	}
	// A trusted merge MUST be auditable (every trusted merge must be reconstructable):
	// write + flush the record BEFORE the fast-forward and abort if it cannot be
	// persisted — for finance, an unrecorded merge is not acceptable.
	auditLine := fmt.Sprintf("merge-local task=%s repo=%s %s -> %s gate=verdict approver=%s", taskID, repo, def, short(workerHead), approver)
	if err := m.writeAudit(auditLine); err != nil {
		return "", fmt.Errorf("trust gate: cannot record the merge for %q in the audit log (%v); refusing to merge unaudited", taskID, err)
	}
	if err := worktree.MergeFastForward(repo, workerHead); err != nil {
		return "", err
	}
	m.recordDelivered(taskID, db.EventDelivered, fmt.Sprintf("%s -> %s gate=verdict approver=%s", def, short(workerHead), approver))
	return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
}

// approvalPayload packs the grant provenance ("human"|"auto") with the reviewed sha into
// the approval token's opaque data, so a merge attributes the audit from the token it
// actually consumes rather than from mutable task state (which a crash could desync).
func approvalPayload(by, sha string) string { return by + " " + sha }

// splitApprovalPayload unpacks approvalPayload. A token with no provenance prefix
// (legacy/plain sha) yields by=="" so the gated path can fail closed on it.
func splitApprovalPayload(data string) (by, sha string) {
	if i := strings.IndexByte(data, ' '); i >= 0 {
		return data[:i], data[i+1:]
	}
	return "", data
}

// landIntegrate performs the mode-appropriate integration step of Land and returns the
// commit now on the local default branch's tip. It is a package-level seam so a test can
// substitute a faulty integrator and exercise Land's post-merge verification abort path
// (a clean local fast-forward can never land a tree different from the validated commit,
// so the only way to drive the mismatch alarm in-process is to inject one).
var landIntegrate = func(m *Manager, t db.Task, mode string, requireVerdict bool, rebasedHead string) (string, error) {
	return m.integrate(t, mode, requireVerdict, rebasedHead)
}

// Land turns the manual push/PR/merge/fetch/ff/verify dance into one safe, atomic command
// for taskID, closing the near-misses of doing it by hand:
//
//  1. fetch origin so the rebase targets the current default-branch tip, not a stale local
//     copy (the "forgot-fetch stale sync" near-miss);
//  2. rebase the worker's committed work onto that tip — on conflict it ABORTS the rebase
//     and reports the real overlap rather than blind-merging a far-behind branch whose diff
//     reads as a huge phantom deletion;
//  3. re-run the validation gate on the REBASED tree (the immutable committed sha, gate
//     definition from the default branch) and require it green — no checks detected is a
//     hard block, exactly like the merge gate;
//  4. integrate honoring the repo's delivery mode and the EXISTING merge gates — pr mode
//     pushes + opens + merges a PR (GitHub's review/branch-protection is the gate); every
//     other mode does an approval-gated local fast-forward via MergeLocal, whose approval
//     and (trusted / --require-verdict) verdict checks are never bypassed;
//  5. POST-MERGE VERIFY that the worker's reviewed changes landed intact — byte-identity to
//     the validated commit for the pinned local fast-forward, or every worker-touched file
//     landing verbatim for a PR merge (where the base may legitimately have advanced) —
//     else abort and alarm;
//  6. leave the local default branch fast-forwarded to the landed commit.
//
// The rebase preserves the gate's bright line that "what merges is what was reviewed": if
// the rebase moves the worker onto an advanced default, the lead's approval of the pre-rebase
// commit no longer covers what would merge, so Land refuses (consuming nothing) and asks for
// a fresh approval of the rebased commit rather than carrying a stale one forward. The common
// case — the worker already current with the default — rebases to a no-op and lands in one
// command. Every failure is loud; Land never silently no-ops.
func (m *Manager) Land(taskID string, requireVerdict bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	spec, err := m.resolveLandSpec(t, requireVerdict)
	if err != nil {
		return "", err
	}
	// A standalone land has no concurrent lander contending for the default branch, so its
	// prep and commit run back to back with a private (uncontended) fetch lock. LandSet shares
	// these same two phases across a fleet, serializing only the commit per repo.
	var fetchMu sync.Mutex
	prep, err := m.landPrep(t, spec, &fetchMu)
	if err != nil {
		return "", err
	}
	return m.landCommit(t, spec, prep)
}

// landSpec is the resolved, per-task landing plan: the repo/worktree, the delivery mode and
// its derived gate/origin facts, and the local default branch name. resolveLandSpec computes
// it once (validating the preconditions that do not depend on the rebase) so both the single
// Land and the concurrent LandSet share one definition of what landing this task means.
type landSpec struct {
	taskID, repo, wt string
	mode, def        string
	gated            bool
	requireVerdict   bool
	hasOrigin        bool
}

// resolveLandSpec validates the landing preconditions that are independent of the rebase and
// returns the resolved landSpec. It refuses an uninitialized repo (so a degraded AGENTS.md can
// never reroute a local/trusted repo onto the ungated PR path), a --require-verdict against pr
// mode (which has no local merge to gate), a dirty worktree (the rebased+validated commit must
// be exactly what merges), and a pr-mode repo with no origin to push to.
func (m *Manager) resolveLandSpec(t db.Task, requireVerdict bool) (landSpec, error) {
	var zero landSpec
	repo, wt := t.Project, t.Worktree
	if repo == "" || wt == "" {
		return zero, fmt.Errorf("land: task %q has no repo/worktree to land", t.ID)
	}
	// Land must resolve the delivery mode authoritatively before routing the integration:
	// refuse a repo with no recorded mode rather than silently defaulting to pr (a degraded
	// or absent AGENTS.md would otherwise reroute a local/trusted repo onto the ungated PR
	// path, sidestepping the merge gate). projectinit.Initialized is the authority that
	// `ttorch init` writes.
	if !projectinit.Initialized(repo) {
		return zero, fmt.Errorf("land: repo %s has no ttorch delivery mode configured; run 'ttorch init --mode <pr|local|validated|trusted>' first", repo)
	}
	mode := projectinit.ReadMode(repo)
	// --require-verdict layers the adversarial-review verdict onto a LOCAL merge gate; pr
	// mode has no local merge to gate (GitHub review/branch-protection is its gate), so
	// honor the flag loudly rather than silently dropping it.
	if mode == "pr" && requireVerdict {
		return zero, fmt.Errorf("land: --require-verdict applies to local/validated/trusted modes; repo %s is in pr mode, where GitHub review/branch-protection is the gate", repo)
	}
	// The rebased + validated commit must be exactly what merges: refuse a dirty worktree
	// up front (a worker mid-edit), the same contract trust prep enforces.
	if clean, err := worktree.IsClean(wt); err != nil || !clean {
		return zero, fmt.Errorf("land: the worktree for %q is not clean; commit or discard changes first so the rebased, validated commit is exactly what lands", t.ID)
	}
	// pr mode REQUIRES an origin to push to; other modes can land a purely local repo (no
	// remote), so a missing origin there is fine.
	hasOrigin := worktree.RemoteExists(repo, "origin")
	if mode == "pr" && !hasOrigin {
		return zero, fmt.Errorf("land: repo %s is in pr delivery mode but has no 'origin' remote to push to", repo)
	}
	return landSpec{
		taskID:         t.ID,
		repo:           repo,
		wt:             wt,
		mode:           mode,
		def:            worktree.DefaultBranch(repo),
		gated:          requireVerdict || mode == "trusted",
		requireVerdict: requireVerdict,
		hasOrigin:      hasOrigin,
	}, nil
}

// landBase picks the rebase base: the most-advanced default tip the fast-forward can target,
// returning both the ref to rebase onto and its resolved sha. After a fetch, origin/<def> is
// preferred when the LOCAL default is an ancestor of it (origin is the authoritative,
// equal-or-more-advanced tip) — matching the single-land default. But when the local default
// is AHEAD of origin (a prior local fast-forward — e.g. an earlier task in a concurrent batch
// that landed locally and was never pushed), origin/<def> is stale: rebasing onto it would
// leave the worker a non-fast-forward of the LOCAL default the merge actually advances, so the
// local default is the correct base. This is what lets a concurrent batch of local landings
// converge — each re-prep rebases onto the default the prior landing just advanced. Falls back
// to the local default when there is no origin/<def>.
func landBase(repo, def string, hasOrigin bool) (ref, sha string, err error) {
	localSha, err := worktree.ResolveRef(repo, def)
	if err != nil {
		return "", "", fmt.Errorf("could not resolve the local default %s: %w", def, err)
	}
	ref, sha = def, localSha
	if hasOrigin && worktree.RefExists(repo, "origin/"+def) {
		originRef := "origin/" + def
		originSha, err := worktree.ResolveRef(repo, originRef)
		if err != nil {
			return "", "", fmt.Errorf("could not resolve %s: %w", originRef, err)
		}
		if worktree.IsAncestor(repo, localSha, originSha) {
			ref, sha = originRef, originSha
		}
	}
	return ref, sha, nil
}

// landPrepResult is the committed-object output of landPrep that landCommit needs: the rebase
// base and its sha (for the post-merge verify), and the worker HEAD before and after the
// rebase (the after is the validated, staged commit that will fast-forward).
type landPrepResult struct {
	base        string
	baseSha     string
	preRebase   string
	rebasedHead string
}

// landPrep performs every step of landing that does NOT mutate the shared default branch, so a
// fleet of landings can run it concurrently: it fetches origin (under fetchMu, the only shared
// write here — remote-tracking refs), rebases the worker onto the current default tip, validates
// the REBASED tree, stages that validate commit-pinned to the rebased commit so the gated merge
// reuses it instead of re-running the suite under the FF lock, and — for a gated local land —
// carries the verdict forward over a clean rebase and confirms the gate covers the rebased
// commit. It operates on the task's own worktree and an immutable detached checkout, never the
// shared default, so concurrent landPreps for disjoint tasks do not block one another. Every
// failure is terminal for this attempt (rebase conflict, red validate, a verdict that cannot be
// carried) and is returned for the caller to surface; landPrep never mutates the default branch.
func (m *Manager) landPrep(t db.Task, spec landSpec, fetchMu *sync.Mutex) (landPrepResult, error) {
	var zero landPrepResult
	// (1) Fetch origin so the rebase targets the current default-branch tip, not a stale local
	// copy. Serialized per repo: a fetch updates shared remote-tracking refs, so concurrent
	// landers must not race it. pr mode REQUIRES an origin (resolveLandSpec already enforced);
	// other modes can land a purely local repo, so a missing origin just skips the fetch.
	if spec.hasOrigin {
		fetchMu.Lock()
		err := worktree.Fetch(spec.repo)
		fetchMu.Unlock()
		if err != nil {
			return zero, fmt.Errorf("land: 'git fetch origin' failed for %s: %w; refusing to land against a stale origin", spec.repo, err)
		}
	}

	// (2) Rebase the worker onto the current default tip. On conflict ABORT and report the real
	// overlap rather than blind-merging a far-behind branch whose diff reads as a phantom deletion.
	base, baseSha, err := landBase(spec.repo, spec.def, spec.hasOrigin)
	if err != nil {
		return zero, fmt.Errorf("land: %w", err)
	}
	preRebase, err := worktree.Head(spec.wt)
	if err != nil {
		return zero, err
	}
	if err := worktree.Rebase(spec.wt, base); err != nil {
		if abErr := worktree.RebaseAbort(spec.wt); abErr != nil {
			return zero, fmt.Errorf("land: rebasing %q onto %s hit conflicts AND the abort failed (%v); the worktree %s is left mid-rebase — run 'git -C %s rebase --abort' by hand, resolve the overlap in the worker, then re-run land: %w", spec.taskID, base, abErr, spec.wt, spec.wt, err)
		}
		return zero, fmt.Errorf("land: rebasing %q onto %s hit conflicts (real overlap with changes already on %s); aborted the rebase and restored the worktree — resolve the overlap in the worker, then re-run land: %w", spec.taskID, base, spec.def, err)
	}
	rebasedHead, err := worktree.Head(spec.wt)
	if err != nil {
		return zero, err
	}

	// (3) Validate the REBASED tree. Must be green; no checks detected is a hard block.
	green, results, err := validateCommitted(spec.repo, rebasedHead)
	if err != nil {
		return zero, fmt.Errorf("land: could not validate the rebased tree for %q: %w", spec.taskID, err)
	}
	if !green {
		if len(results) == 0 {
			return zero, fmt.Errorf("land: no checks detected for %q after rebasing onto %s; the gate requires a build/test/lint suite (add .ttorch/validate.sh on the default branch)", spec.taskID, base)
		}
		return zero, fmt.Errorf("land: %d of %d checks failed on the rebased tree for %q; fix them in the worker and re-run land", len(validate.Failures(results)), len(results), spec.taskID)
	}
	// Stage the rebased validate commit-pinned to the rebased commit, so the gated merge reuses
	// it (validateForMerge) rather than re-running the identical suite under the serialized FF
	// lock — the validate cost stays here, in concurrent prep, off the critical section. Only
	// gated merges re-validate, so only they need the staged result.
	if spec.gated {
		if err := m.stagePrepValidate(spec.taskID, rebasedHead, results); err != nil {
			return zero, fmt.Errorf("land: could not stage the rebased validate for %q: %w", spec.taskID, err)
		}
	}

	// (4) The approval (and, when gated, the verdict) authorize a SPECIFIC commit. If the rebase
	// moved the worker onto an advanced default, the prior approval of the pre-rebase commit no
	// longer covers what would merge. Verdict-portable fast-land: when the worker's own three-dot
	// diff is byte-identical to what the reviewers cleared, carry the passing verdict forward to
	// the rebased commit (re-pinning it, and the auto-minted approval) instead of forcing a full
	// re-gate; a rebase that changed the reviewed content is never carried — carryVerdictForward
	// returns a loud re-gate error. gateCoversRebased then confirms the (possibly carried) tokens
	// pin the rebased commit, consuming nothing — MergeLocal remains the consuming authority.
	if spec.mode != "pr" {
		if spec.gated && rebasedHead != preRebase {
			if _, err := m.carryVerdictForward(t, base, rebasedHead); err != nil {
				return zero, err
			}
		}
		if err := m.gateCoversRebased(t, rebasedHead, spec.gated); err != nil {
			return zero, err
		}
	}
	return landPrepResult{base: base, baseSha: baseSha, preRebase: preRebase, rebasedHead: rebasedHead}, nil
}

// stagePrepValidate persists results as the commit-pinned gate validate for taskID: validate.json
// plus head.txt pinned to sha, exactly as TrustPrep stages it. A later merge of EXACTLY sha then
// reuses it (validateForMerge / reusablePrepValidate) instead of re-running the suite. The sha is
// immutable, so the staged green is a faithful, commit-pinned record of the tree that merges.
func (m *Manager) stagePrepValidate(taskID, sha string, results []validate.Result) error {
	dir := m.P.ReviewInputsDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	vb, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "validate.json"), append(vb, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "head.txt"), []byte(sha+"\n"), 0o644)
}

// landCommit performs the part of landing that mutates the shared default branch — the
// integration (a local fast-forward via MergeLocal, or a PR merge) and the post-merge verify —
// and returns the human-readable land summary. This is the ONLY phase a concurrent LandSet
// serializes per repo: the default branch is a single resource, so its fast-forward cannot run
// truly in parallel (git requirement). Callers that share a repo MUST hold the repo's FF lock
// across landCommit and confirm the local default is still an ancestor of prep.rebasedHead
// (else another landing advanced it and this prep is stale — re-prep first); the single Land,
// with no concurrent lander, calls it directly.
func (m *Manager) landCommit(t db.Task, spec landSpec, prep landPrepResult) (string, error) {
	// (1) Integrate, honoring the delivery mode and the existing merge gates. pr mode pushes +
	// opens + merges a PR (GitHub's review/branch-protection is the gate); every other mode does
	// an approval-gated local fast-forward via MergeLocal, whose approval and (trusted /
	// --require-verdict) verdict checks are never bypassed.
	landed, err := landIntegrate(m, t, spec.mode, spec.requireVerdict, prep.rebasedHead)
	if err != nil {
		return "", err
	}

	// (2) POST-MERGE VERIFY that the worker's reviewed changes landed intact. The local
	// fast-forward is byte-identical to the validated commit (strict); a PR merge may sit on
	// a base that legitimately advanced, so there we require only the worker's own files to
	// have landed verbatim.
	defAfter, err := verifyLanded(spec.repo, spec.def, prep.baseSha, prep.rebasedHead, spec.mode != "pr")
	if err != nil {
		return "", fmt.Errorf("land: %q: %w", spec.taskID, err)
	}

	// (3) The local default branch is now fast-forwarded to the landed commit.
	rebaseNote := "worker already current"
	if prep.rebasedHead != prep.preRebase {
		rebaseNote = fmt.Sprintf("rebased %s→%s onto %s", short(prep.preRebase), short(prep.rebasedHead), prep.base)
	}
	m.audit(fmt.Sprintf("land task=%s repo=%s mode=%s %s -> %s verified", spec.taskID, spec.repo, spec.mode, spec.def, short(landed)))
	out := fmt.Sprintf("landed %s (%s mode): %s; %s fast-forwarded to %s and verified",
		spec.taskID, spec.mode, rebaseNote, spec.def, short(defAfter))
	// Surface the security-everywhere audit status. This is purely ADVISORY and never
	// blocks: a gated land (trusted / --require-verdict) already ran the full review gate
	// — which includes security — so it needs no extra note; the other modes get a
	// non-blocking reminder of whether a fresh security audit covers the landed commit.
	if note := m.securityAuditNote(spec.taskID, prep.rebasedHead, spec.gated); note != "" {
		out += "\n  " + note
	}
	return out, nil
}

// securityAuditNote returns a one-line, ADVISORY note on the security-everywhere audit
// for the commit being landed, or "" when none is warranted. It never blocks delivery:
// gated lands (trusted / --require-verdict) already cleared the full review gate, so they
// get no note; in every other mode it reports whether a fresh advisory security audit
// covers landedSHA, nudging the manager to run one when it does not.
func (m *Manager) securityAuditNote(taskID, landedSHA string, gated bool) string {
	if gated {
		return ""
	}
	v, ok := m.SecurityReviewShow(taskID)
	if !ok || v.ReviewedSHA != landedSHA {
		return fmt.Sprintf("advisory: no security audit covers %s — run 'ttorch security-review prep %s', review, then 'ttorch security-review record %s' (advisory, does not block delivery)",
			short(landedSHA), taskID, taskID)
	}
	if v.Overall != review.Pass {
		return fmt.Sprintf("advisory: security audit raised blocking findings for %s — review 'ttorch security-review show %s' (advisory, did not block this delivery)", short(landedSHA), taskID)
	}
	return fmt.Sprintf("advisory: security audit passed for %s", short(landedSHA))
}

// carryVerdictForward implements verdict-portable fast-land. When a gated land has rebased the
// worker onto an advanced default — moving the commit SHA the verdict was pinned to — it
// carries the existing PASSING verdict forward to the rebased commit, re-pinning the verdict
// (and, when the gate auto-minted it, the approval token) to rebasedHead WITHOUT re-running
// trust prep or the three reviewers — but ONLY when the worker's own three-dot diff against
// the rebase base is byte-identical to the diff the reviewers cleared, matched by the verdict's
// recorded content identity (review.DiffID). This is the throughput fix that stops re-gating a
// task every time an unrelated merge advances the default beneath it.
//
// A content change — anything but a clean, disjoint rebase — is NEVER carried: re-pinning a
// verdict onto changed content would be a trust hole, so it returns a loud re-gate error
// instead. When there is no passing verdict to carry, or one predating content identities (an
// empty DiffID), it carries nothing and returns (false, nil), letting gateCoversRebased issue
// the usual re-gate demand. The verdict expiry, findings, and content identity are preserved so
// a subsequent rebase can carry it again; the merge gate in MergeLocal re-validates and
// re-consumes the re-pinned tokens, so carry-forward is an optimization, never the authority.
func (m *Manager) carryVerdictForward(t db.Task, base, rebasedHead string) (bool, error) {
	v, ok := review.Load(m.P.ReviewVerdictFile(t.ID))
	if !ok || v.Overall != review.Pass {
		return false, nil // no carryable verdict — gateCoversRebased demands a fresh one
	}
	if v.ReviewedSHA == rebasedHead {
		return false, nil // already covers the rebased commit (e.g. a no-op re-run)
	}
	if v.DiffID == "" {
		return false, nil // a verdict recorded before content identities — fail safe to re-gate
	}
	patch, err := mergeBaseDiff(t.Worktree, base, rebasedHead)
	if err != nil {
		return false, fmt.Errorf("land: could not compute the rebased diff for %q to carry its verdict forward: %w", t.ID, err)
	}
	if review.DiffID([]byte(patch)) != v.DiffID {
		// The rebase onto the advanced default was not clean/disjoint: it changed the worker's
		// own diff, so the recorded verdict no longer covers what would merge. Never carry a
		// verdict onto changed content — force a full re-gate.
		return false, fmt.Errorf("land: rebasing %q onto the advanced %s changed its reviewed diff, so the recorded verdict no longer covers the rebased commit %s; re-run 'ttorch trust prep %s', review, and 'ttorch trust record %s', then re-run 'ttorch land %s'",
			t.ID, base, short(rebasedHead), t.ID, t.ID, t.ID)
	}
	// The worker's own diff is byte-identical to what the reviewers cleared: carry the verdict
	// forward by re-pinning it to the rebased commit, preserving its expiry, findings, and
	// content identity.
	reviewedSHA := v.ReviewedSHA
	ttl := time.Until(time.Unix(0, v.Expires))
	if ttl <= 0 {
		return false, nil // expired between Load and here — gateCoversRebased fails closed
	}
	v.ReviewedSHA = rebasedHead
	if err := review.Write(m.P.ReviewVerdictFile(t.ID), v, ttl); err != nil {
		return false, fmt.Errorf("land: could not re-pin the carried verdict for %q: %w", t.ID, err)
	}
	// The trusted gate auto-mints the approval token from the same review, pinned to the same
	// commit; re-pin it forward too so the merge proceeds without a human. A human-minted
	// approval is NEVER forged forward — the lead re-approves the rebased commit (a single
	// step), but the three reviewers do not re-run.
	if data, ok := approval.Data(m.P.ApprovalFile(t.ID)); ok {
		if by, _ := splitApprovalPayload(data); by == "auto" {
			if err := approval.Grant(m.P.ApprovalFile(t.ID), ttl, approvalPayload("auto", rebasedHead)); err != nil {
				return false, fmt.Errorf("land: could not re-pin the carried auto-approval for %q: %w", t.ID, err)
			}
		}
	}
	m.audit(fmt.Sprintf("fast-land task=%s carried verdict %s->%s base=%s (reviewed diff unchanged)",
		t.ID, short(reviewedSHA), short(rebasedHead), base))
	return true, nil
}

// gateCoversRebased verifies the existing approval (and, when gated, a passing review
// verdict) is pinned to the rebased commit that will actually merge — without consuming
// either (MergeLocal remains the consuming authority). It lets Land tell the lead clearly to
// approve the rebased commit when its own rebase moved the worker onto an advanced default,
// instead of MergeLocal later consuming a now-stale token and reporting a confusing generic
// mismatch.
func (m *Manager) gateCoversRebased(t db.Task, rebasedHead string, gated bool) error {
	data, ok := approval.Data(m.P.ApprovalFile(t.ID))
	_, approvedSha := splitApprovalPayload(data)
	if !ok || approvedSha != rebasedHead {
		return fmt.Errorf("land: no valid approval covers the rebased commit %s for %q (land rebased the worker onto the current default, so the prior approval no longer matches); review it with 'ttorch review-diff %s' and approve with 'ttorch approve %s', then re-run 'ttorch land %s'",
			short(rebasedHead), t.ID, t.ID, t.ID, t.ID)
	}
	if gated {
		v, vok := review.Load(m.P.ReviewVerdictFile(t.ID))
		if !vok || v.Overall != review.Pass || v.ReviewedSHA != rebasedHead {
			return fmt.Errorf("land: no passing review verdict covers the rebased commit %s for %q; re-run 'ttorch trust prep %s', review, and 'ttorch trust record %s', then re-run 'ttorch land %s'",
				short(rebasedHead), t.ID, t.ID, t.ID, t.ID)
		}
	}
	return nil
}

// integrate performs Land's mode-appropriate merge and returns the local default tip.
func (m *Manager) integrate(t db.Task, mode string, requireVerdict bool, rebasedHead string) (string, error) {
	repo := t.Project
	def := worktree.DefaultBranch(repo)
	if mode == "pr" {
		return m.integratePR(t, def, rebasedHead)
	}
	// local / validated / trusted: an approval-gated local fast-forward. MergeLocal
	// enforces the approval token and (in trusted mode or with --require-verdict) the
	// adversarial-review verdict + a fresh green validate; Land never bypasses those.
	if _, err := m.MergeLocal(t.ID, requireVerdict); err != nil {
		return "", fmt.Errorf("land: local merge gate refused %q: %w", t.ID, err)
	}
	return worktree.Head(repo)
}

// integratePR delivers via GitHub: it publishes EXACTLY the validated commit as a branch,
// opens (or reuses) a PR, merges it, then fast-forwards the local default branch to the
// merged tip. GitHub's required reviews / branch protection / status checks are the gate
// here, and a merge they block fails loudly.
func (m *Manager) integratePR(t db.Task, def, rebasedHead string) (string, error) {
	repo, wt := t.Project, t.Worktree
	// HEAD-unchanged bracket (mirrors MergeLocal): the worker must not have advanced past
	// the validated commit between validation and publish, so an unvalidated commit can
	// never reach the remote.
	if cur, err := worktree.Head(wt); err != nil || cur != rebasedHead {
		return "", fmt.Errorf("land: the worker for %q advanced past the validated commit %s before publish; re-run land", t.ID, short(rebasedHead))
	}
	branch := "ttorch/" + t.ID
	if err := worktree.Push(repo, "origin", rebasedHead+":refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("land: pushing %q to origin/%s failed: %w", t.ID, branch, err)
	}
	if err := ghEnsurePR(repo, branch, def, t.ID); err != nil {
		return "", err
	}
	if _, err := gh(repo, "pr", "merge", branch, "--merge", "--delete-branch"); err != nil {
		return "", fmt.Errorf("land: merging the PR for %q failed (required reviews / checks / branch protection?): %w", t.ID, err)
	}
	// Bring the local default branch to the merged tip.
	if err := worktree.Fetch(repo); err != nil {
		return "", fmt.Errorf("land: fetch after the PR merge of %q failed: %w", t.ID, err)
	}
	if cur, _ := worktree.CurrentBranch(repo); cur != def {
		return "", fmt.Errorf("land: repo is on %q, not the default branch %q; cannot fast-forward the local default after the PR merge", cur, def)
	}
	if changed, _ := worktree.HasTrackedChanges(repo); changed {
		return "", fmt.Errorf("land: repo has uncommitted tracked changes; cannot fast-forward the local default after the PR merge")
	}
	if err := worktree.MergeFastForward(repo, "origin/"+def); err != nil {
		return "", fmt.Errorf("land: fast-forwarding local %s to origin/%s after the PR merge failed: %w", def, def, err)
	}
	// The PR merged and the local default fast-forwarded: record the delivery as a
	// typed, non-actionable `merged` event (the PR-path counterpart of MergeLocal's
	// `delivered`, §3.4). Best-effort — the merge is already irreversible.
	m.recordDelivered(t.ID, db.EventMerged, fmt.Sprintf("pr merged: %s -> %s", branch, def))
	return worktree.Head(repo)
}

// verifyLanded asserts the worker's reviewed changes landed intact on the default branch and
// returns the new tip. When strict (a local fast-forward, where the base is pinned) it
// requires the tip to be byte-identical to the validated rebasedHead, catching any file
// changed or reverted outside the reviewed diff. When not strict (a PR merge, where the base
// may legitimately have advanced under a concurrent landing) it instead requires every file
// the worker changed (baseSha..rebasedHead) to be identical between rebasedHead and the
// landed tip — the worker's contribution landed verbatim, while concurrent changes to OTHER
// files are allowed. Either failure is a loud, file-naming alarm (a post-merge tripwire; it
// cannot un-merge, only refuse to bless).
func verifyLanded(repo, def, baseSha, rebasedHead string, strict bool) (string, error) {
	defAfter, err := worktree.ResolveRef(repo, def)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not resolve %s: %w", def, err)
	}
	if strict {
		drift, err := worktree.ChangedFiles(repo, defAfter, rebasedHead)
		if err != nil {
			return "", fmt.Errorf("post-merge verify could not diff the landed tip: %w", err)
		}
		if len(drift) > 0 {
			return "", fmt.Errorf("POST-MERGE VERIFY FAILED: %s now at %s is NOT identical to the validated commit %s; these files differ: %s — the integration changed or reverted files outside the reviewed diff; investigate before trusting this landing",
				def, short(defAfter), short(rebasedHead), strings.Join(drift, ", "))
		}
		return defAfter, nil
	}
	// PR path: every file the worker changed must have landed verbatim. Files that differ
	// between the validated commit and the landed tip but were NOT touched by the worker are
	// concurrent landings on the base and are allowed.
	workerFiles, err := worktree.ChangedFiles(repo, baseSha, rebasedHead)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not list the worker's changed files: %w", err)
	}
	landedDrift, err := worktree.ChangedFiles(repo, rebasedHead, defAfter)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not diff the landed tip: %w", err)
	}
	worker := make(map[string]bool, len(workerFiles))
	for _, f := range workerFiles {
		worker[f] = true
	}
	var bad []string
	for _, f := range landedDrift {
		if worker[f] {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("POST-MERGE VERIFY FAILED: the worker's reviewed changes did not land verbatim on %s (now %s); these worker-touched files differ from the validated commit %s: %s — the merge altered or dropped the worker's contribution; investigate before trusting this landing",
			def, short(defAfter), short(rebasedHead), strings.Join(bad, ", "))
	}
	return defAfter, nil
}

// gh runs a gh CLI command in repo, returning its trimmed combined output.
func gh(repo string, args ...string) (string, error) {
	c := exec.Command("gh", args...)
	c.Dir = repo
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("gh %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// ghEnsurePR opens a PR for branch into base, or succeeds if one is already open for it.
func ghEnsurePR(repo, branch, base, taskID string) error {
	if _, err := gh(repo, "pr", "view", branch, "--json", "number"); err == nil {
		return nil // a PR is already open for this branch
	}
	if _, err := gh(repo, "pr", "create", "--head", branch, "--base", base,
		"--title", "ttorch: land "+taskID,
		"--body", "Automated landing of ttorch task "+taskID+"."); err != nil {
		return fmt.Errorf("land: opening a PR for %q (%s -> %s) failed: %w", taskID, branch, base, err)
	}
	return nil
}

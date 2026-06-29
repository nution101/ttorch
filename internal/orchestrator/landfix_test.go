package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
)

// TestLand_FastForwardableWorkerSkipsRebaseAndKeepsApproval is the regression guard for the
// gratuitous-rebase bug (scheduler phase 4, Part D, fix 1): when the worker is ALREADY a
// fast-forward of the default (a clean FF, nothing to replay), land must take the
// fast-forward fast-path and NOT rebase. Rebasing an already-fast-forwardable worker can
// rewrite its commit sha (committer metadata / fork-point), moving it off the commit the
// verdict + approval are pinned to — which previously failed the daemon's autonomous land
// with "no valid approval covers the rebased commit". Here a landRebase spy that would record
// any rebase proves it is never invoked, and the existing verdict + auto-approval (pinned to
// the unchanged commit) carry the land through in one shot with no re-gate or re-approval.
func TestLand_FastForwardableWorkerSkipsRebaseAndKeepsApproval(t *testing.T) {
	m, repo := deliveryHarness(t, "ffskiprebase")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task := readyTrustedTask(t, m, repo, "ff1", "a.txt", "a\n") // gated: verdict + auto-approval, pinned to feat
	feat := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	// The default did NOT advance, so the worker is already a fast-forward of it (gitIn fatals
	// on a non-zero exit, so this asserts main is an ancestor of feat).
	gitIn(t, repo, "merge-base", "--is-ancestor", "main", feat)

	// A spy over the rebase seam: if land ever rebases this already-fast-forwardable worker,
	// rebaseCalled flips. The fast-forward fast-path must skip it entirely.
	origRebase := landRebase
	rebaseCalled := false
	landRebase = func(wt, base string) error {
		rebaseCalled = true
		return origRebase(wt, base)
	}
	defer func() { landRebase = origRebase }()

	// Autonomous-land semantics (requireVerdict=true), exactly as the scheduler's land pass runs.
	out, err := m.Land("ff1", true)
	if err != nil {
		t.Fatalf("an already-fast-forwardable gated worker should land in one command: %v", err)
	}
	if rebaseCalled {
		t.Fatal("an already-fast-forwardable worker must NOT be rebased (fast-forward fast-path)")
	}
	if now := gitIn(t, task.Worktree, "rev-parse", "HEAD"); now != feat {
		t.Fatalf("the worker commit must be unchanged by the FF fast-path: %s -> %s", feat, now)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != feat {
		t.Fatal("the default branch must fast-forward to the worker's unchanged commit")
	}
	if !strings.Contains(out, "worker already current") || !strings.Contains(out, "verified") {
		t.Fatalf("a FF fast-path land should report 'worker already current' and verify: %q", out)
	}
	// No carry-forward happened (the sha never moved), and the gate tokens were consumed by the
	// successful merge.
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if strings.Contains(string(auditLog), "fast-land task=ff1 carried verdict") {
		t.Fatalf("the FF fast-path must not carry a verdict forward (the commit never moved):\n%s", auditLog)
	}
	if _, ok := m.TrustShow("ff1"); ok {
		t.Fatal("the verdict must be consumed by the merge")
	}
	if approval.Valid(m.P.ApprovalFile("ff1")) {
		t.Fatal("the approval must be consumed by the merge")
	}
	_, _ = m.Teardown("ff1", true)
}

// TestMergeLocal_FailedMergePreservesGateTokens is the regression guard for the
// consumed-on-failure bug (scheduler phase 4, Part D, fix 2): the approval AND the verdict
// must be consumed ONLY on a SUCCESSFUL fast-forward. A merge that fails AFTER the gate checks
// pass must leave both tokens intact so a retry works — previously a failed land consumed them
// and the retry was refused with "no valid approval". The failure is injected at the
// must-succeed audit write (the last fallible step before the fast-forward, exactly where the
// old code had already consumed the tokens) by replacing the audit log with a directory.
func TestMergeLocal_FailedMergePreservesGateTokens(t *testing.T) {
	m, repo := deliveryHarness(t, "failkeepstokens")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task := readyTrustedTask(t, m, repo, "fk1", "a.txt", "a\n") // gated: verdict + auto-approval, both pinned to head
	worker := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	if !approval.Valid(m.P.ApprovalFile("fk1")) {
		t.Fatal("setup: the approval should be present before the (about-to-fail) merge")
	}
	if _, ok := m.TrustShow("fk1"); !ok {
		t.Fatal("setup: the verdict should be present before the merge")
	}

	// Force the trusted merge to fail at the must-succeed audit write — AFTER every recoverable
	// gate check passes, exactly where the old code had already consumed the tokens. Replacing
	// the audit log with a directory makes writeAudit's write-only open fail deterministically.
	if err := os.RemoveAll(m.P.AuditLog()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.P.AuditLog(), 0o755); err != nil {
		t.Fatal(err)
	}

	before := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := m.MergeLocal("fk1", false); err == nil {
		t.Fatal("the merge should have failed at the audit write")
	}
	// A failed merge must NOT advance the default branch...
	if after := gitIn(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("a failed merge must not advance the default branch: %s -> %s", before, after)
	}
	// ...and must leave BOTH gate tokens intact for a retry (consume-once on SUCCESS only).
	if !approval.Valid(m.P.ApprovalFile("fk1")) {
		t.Fatal("a failed merge must NOT consume the approval (the failed-land-orphans-token bug)")
	}
	if _, ok := m.TrustShow("fk1"); !ok {
		t.Fatal("a failed merge must NOT consume the verdict")
	}

	// Clear the injected failure; the retry now succeeds with the preserved tokens.
	if err := os.RemoveAll(m.P.AuditLog()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("fk1", false); err != nil {
		t.Fatalf("a retry after a failed merge should succeed with the preserved approval + verdict: %v", err)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != worker {
		t.Fatalf("the successful retry should fast-forward the default to the worker head %s, got %s", worker, got)
	}
	// Both tokens are consumed by the successful retry.
	if approval.Valid(m.P.ApprovalFile("fk1")) {
		t.Fatal("the approval must be consumed by the successful retry")
	}
	if _, ok := m.TrustShow("fk1"); ok {
		t.Fatal("the verdict must be consumed by the successful retry")
	}
	_, _ = m.Teardown("fk1", true)
}

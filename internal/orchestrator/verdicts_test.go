package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
)

// trustedRecorded spins up a trusted repo, commits one feature, runs a clean review, and
// records the verdict — leaving a durable, auto-approved verdict row pinned to head. It
// returns the manager, repo, and the reviewed head.
func trustedRecorded(t *testing.T, tag, id string) (*Manager, string, string) {
	t.Helper()
	m, repo := deliveryHarness(t, tag)
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn(id, repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir(id), head, nil) // clean → pass
	if _, err := m.TrustRecord(id, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	return m, repo, head
}

// TestDurableVerdict_PersistedContentPinned is the storage contract: `trust record`
// promotes the verdict to a durable DB row that is COMMIT-pinned (reviewed_sha) and
// CONTENT-pinned (a non-empty diff_id), carries the structured outcome, and records the
// approval token's provenance + bound sha — all readable straight from the DB, with no
// expiry on the read path (the row is valid until content changes, never by age).
func TestDurableVerdict_PersistedContentPinned(t *testing.T) {
	m, _, head := trustedRecorded(t, "durpin", "dp1")

	dv, ok, err := m.Store.GetVerdict(context.Background(), "dp1")
	if err != nil || !ok {
		t.Fatalf("verdict must be durable in the DB after trust record: ok=%v err=%v", ok, err)
	}
	if dv.Overall != review.Pass {
		t.Errorf("overall = %q, want pass", dv.Overall)
	}
	if dv.ReviewedSHA != head {
		t.Errorf("reviewed_sha = %q, want the reviewed head %q (commit pin)", dv.ReviewedSHA, head)
	}
	if dv.DiffID == "" {
		t.Error("diff_id must be set (the content pin) so a clean rebase can carry the verdict forward")
	}
	if dv.ApprovedBy != "auto" || dv.ApprovalSHA != head {
		t.Errorf("approval token not persisted on the row: approved_by=%q approval_sha=%q, want auto/%s", dv.ApprovedBy, dv.ApprovalSHA, head)
	}
	// A clean (no-findings) verdict stores the schema's empty-array form, never JSON "null".
	if dv.Findings != "[]" {
		t.Errorf("clean verdict findings = %q, want %q (the documented empty-array form)", dv.Findings, "[]")
	}
	_, _ = m.Teardown("dp1", true)
}

// TestTeardown_DeletesDurableVerdict proves teardown cleans up the durable verdict row, so a
// discarded task never strands gate state (restoring the self-cleanup the TTL'd file once had)
// and a re-spawn of the id can never inherit a stale verdict.
func TestTeardown_DeletesDurableVerdict(t *testing.T) {
	m, _, _ := trustedRecorded(t, "tddel", "td1")
	if _, ok, _ := m.Store.GetVerdict(context.Background(), "td1"); !ok {
		t.Fatal("precondition: trust record must have left a durable verdict row")
	}
	if _, err := m.Teardown("td1", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, ok, _ := m.Store.GetVerdict(context.Background(), "td1"); ok {
		t.Fatal("teardown must delete the durable verdict row (no stranded gate state)")
	}
}

// TestMergeLocal_VerdictAuthorityIsDBRow proves the gate reads its verdict from the DB,
// not a file: with a valid auto-approval still in place, deleting ONLY the durable verdict
// row makes the gated merge fail closed ("no valid review verdict"). This is the durable
// replacement for the old "verdict file gone" path — except now nothing but a genuine
// delete (or content change) can remove it; file-TTL expiry never can.
func TestMergeLocal_VerdictAuthorityIsDBRow(t *testing.T) {
	m, _, _ := trustedRecorded(t, "dbauth", "da1")
	if !approval.Valid(m.P.ApprovalFile("da1")) {
		t.Fatal("precondition: a trusted clean pass must have auto-minted an approval")
	}

	// Remove the durable verdict row only; the approval token stays valid.
	if err := m.Store.DeleteVerdict(context.Background(), "da1"); err != nil {
		t.Fatal(err)
	}
	_, err := m.MergeLocal("da1", false) // trusted ⇒ gated
	if err == nil {
		t.Fatal("the gate must fail closed when the durable verdict row is absent")
	}
	if !strings.Contains(err.Error(), "no valid review verdict") {
		t.Fatalf("expected a missing-verdict refusal sourced from the DB, got: %v", err)
	}
	// Fail-closed refusal must not consume the approval.
	if !approval.Valid(m.P.ApprovalFile("da1")) {
		t.Fatal("a gate refusal must leave the approval intact")
	}
	_, _ = m.Teardown("da1", true)
}

// TestMergeLocal_FailsClosedOnContentMismatchedRow is the content-pin guard read straight
// from the DB: if the durable verdict's reviewed_sha no longer equals the commit being
// merged, the gate refuses at consume even though the verdict is "pass" and the approval
// matches — a verdict never applies to changed content.
func TestMergeLocal_FailsClosedOnContentMismatchedRow(t *testing.T) {
	m, _, _ := trustedRecorded(t, "cmis", "cm1")

	// Simulate a verdict that no longer pins the commit that will merge (the worker's HEAD).
	dv, ok, err := m.Store.GetVerdict(context.Background(), "cm1")
	if err != nil || !ok {
		t.Fatalf("GetVerdict: ok=%v err=%v", ok, err)
	}
	dv.ReviewedSHA = "0000000000000000000000000000000000000000"
	if err := m.Store.SaveVerdict(context.Background(), dv); err != nil {
		t.Fatal(err)
	}

	_, err = m.MergeLocal("cm1", false) // trusted ⇒ gated
	if err == nil {
		t.Fatal("a verdict whose reviewed_sha no longer matches the merged commit must refuse")
	}
	if !strings.Contains(err.Error(), "re-review and re-record") {
		t.Fatalf("expected a content-pin (stale-sha) refusal, got: %v", err)
	}
	_, _ = m.Teardown("cm1", true)
}

// TestMergeLocal_DurableVerdictConsumedOnMerge proves the verdict row is the consume-once
// authority: a successful gated merge deletes it, so a replay finds nothing in the DB.
func TestMergeLocal_DurableVerdictConsumedOnMerge(t *testing.T) {
	m, repo, head := trustedRecorded(t, "durcons", "dc1")

	if _, err := m.MergeLocal("dc1", false); err != nil {
		t.Fatalf("trusted auto merge should succeed: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded to the reviewed head")
	}
	if _, ok, _ := m.Store.GetVerdict(context.Background(), "dc1"); ok {
		t.Fatal("the durable verdict row must be consumed (deleted) by the merge")
	}
	if _, ok := m.TrustShow("dc1"); ok {
		t.Fatal("TrustShow must report no verdict after the merge consumed it")
	}
	_, _ = m.Teardown("dc1", true)
}

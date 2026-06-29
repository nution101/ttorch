package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/worktree"
)

// TestLand_FastLandCarriesHumanApprovalOnCleanRebase is the headline of this change: the
// verdict-portable fast-land carries a HUMAN approval forward over a clean, disjoint rebase
// exactly as it already does the trusted gate's auto-minted one. A gated land (--require-verdict
// in local mode) is reviewed and approved by the lead; the default then advances with an
// unrelated (disjoint) commit. Land rebases the worker onto the advanced default — moving its
// commit sha — but the worker's own three-dot diff is byte-identical to what the reviewers
// cleared, so BOTH the verdict AND the human approval are carried forward (re-pinned to the
// rebased commit) and the land completes in ONE command WITHOUT re-running trust prep, the
// reviewers, or a second `ttorch approve`.
func TestLand_FastLandCarriesHumanApprovalOnCleanRebase(t *testing.T) {
	m, repo := deliveryHarness(t, "fastlandhuman")
	commitGateScript(t, repo, "exit 0") // passing gate on the default branch
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("fh1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "feature.txt", "new\n")
	// Gate the task: record a passing verdict (with a content identity), then have the LEAD
	// approve the reviewed commit by hand — local mode never auto-mints, so this is a genuine
	// human approval token.
	writeReviewReports(t, m.P.ReviewInputsDir("fh1"), feat, nil)
	if _, err := m.TrustRecord("fh1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if approval.Valid(m.P.ApprovalFile("fh1")) {
		t.Fatal("local mode must not auto-mint; the lead approves by hand")
	}
	if v, ok := m.TrustShow("fh1"); !ok || v.ReviewedSHA != feat || v.DiffID == "" {
		t.Fatalf("recorded verdict should pin the reviewed sha and a diff identity: %+v ok=%v", v, ok)
	}
	if err := m.Approve("fh1", time.Minute); err != nil { // human token, pinned to feat
		t.Fatal(err)
	}

	// The default branch advances non-conflictingly (a DIFFERENT file) after the human approval.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("concurrent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent landing")

	// One gated land: the rebase moves the worker's sha, but its diff is unchanged, so the
	// verdict AND the human approval are carried forward and the land succeeds with no re-gate
	// and no re-approval.
	out, err := m.Land("fh1", true)
	if err != nil {
		t.Fatalf("fast-land should carry the verdict AND the human approval over a clean rebase: %v", err)
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker onto the advanced default")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != rebased {
		t.Fatal("the default branch must fast-forward to the rebased commit")
	}
	if !strings.Contains(out, "landed fh1") || !strings.Contains(out, "verified") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	for _, f := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should be present on the landed default branch: %v", f, err)
		}
	}
	// The audit proves the verdict+approval were CARRIED (not re-reviewed / re-approved) and that
	// the merge still went through the trust gate as a human-approved verdict merge.
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if want := fmt.Sprintf("fast-land task=fh1 carried verdict %s->%s", short(feat), short(rebased)); !strings.Contains(string(auditLog), want) {
		t.Fatalf("audit log missing the fast-land carry record (%q):\n%s", want, auditLog)
	}
	if !strings.Contains(string(auditLog), "gate=verdict approver=human") {
		t.Fatalf("the carried-forward merge must record a gated human-approved merge: %s", auditLog)
	}
	// trust-record ran exactly once (at setup) — the fast-land did NOT re-gate the task.
	if n := strings.Count(string(auditLog), "trust-record task=fh1"); n != 1 {
		t.Fatalf("fast-land must not re-record the verdict; trust-record count = %d:\n%s", n, auditLog)
	}
	// approve ran exactly once (at setup) — the fast-land did NOT force a re-approval.
	if n := strings.Count(string(auditLog), "approve task=fh1"); n != 1 {
		t.Fatalf("fast-land must not force a re-approval; approve count = %d:\n%s", n, auditLog)
	}
	// Both tokens are consumed by the merge.
	if _, ok := m.TrustShow("fh1"); ok {
		t.Fatal("the carried verdict must be consumed by the merge")
	}
	if approval.Valid(m.P.ApprovalFile("fh1")) {
		t.Fatal("the carried approval must be consumed by the merge")
	}
	_, _ = m.Teardown("fh1", true)
}

// TestCarryVerdictForward_CarriesHumanApprovalAndVerdict drives the carry primitive directly and
// asserts the precise post-condition the task requires after collapsing the two-store approval:
// after a clean rebase whose diff matches the recorded content identity, carryVerdictForward
// re-pins the durable VERDICT row to the REBASED commit (preserving its human provenance) — the
// single merge authority — and the pre-merge gate check (gateCoversRebased) then re-mints the
// derived human approval token onto the rebased commit from that verdict and confirms the gate
// covers it, consuming nothing.
func TestCarryVerdictForward_CarriesHumanApprovalAndVerdict(t *testing.T) {
	m, repo := deliveryHarness(t, "carryunit")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("cu1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("cu1"), feat, nil)
	if _, err := m.TrustRecord("cu1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("cu1", time.Minute); err != nil { // human token pinned to feat
		t.Fatal(err)
	}

	// Advance the default disjointly and rebase the worker onto it, exactly as landPrep would.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent")
	def := worktree.DefaultBranch(repo)
	if err := worktree.Rebase(wt, def); err != nil {
		t.Fatal(err)
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("rebase should have moved the worker onto the advanced default")
	}

	reloaded, _, _ := m.Store.GetTask(context.Background(), "cu1")
	carried, err := m.carryVerdictForward(reloaded, def, rebased)
	if err != nil {
		t.Fatalf("carryVerdictForward should carry a clean human-approved rebase: %v", err)
	}
	if !carried {
		t.Fatal("carryVerdictForward should report it carried the gate forward")
	}
	// The durable verdict row — the single merge authority — was re-pinned to the rebased
	// commit (reviewed + approval sha both advance), preserving the human provenance.
	v, ok, _ := m.Store.GetVerdict(context.Background(), "cu1")
	if !ok || v.ReviewedSHA != rebased || v.ApprovalSHA != rebased || v.ApprovedBy != "human" {
		t.Fatalf("the carried verdict must pin the rebased commit as human: %+v ok=%v", v, ok)
	}
	// carryVerdictForward re-pins JUST the verdict; the derived token is re-minted at the gate
	// check, so the pre-carry human token still pins the ORIGINAL commit at this point.
	if data, ok := approval.Data(m.P.ApprovalFile("cu1")); ok {
		if _, sha := splitApprovalPayload(data); sha != feat {
			t.Fatalf("carryVerdictForward must not move the token itself; want it still pinned to %s, got %s", short(feat), short(sha))
		}
	}
	// The pre-merge gate check re-mints the human approval token onto the rebased commit from
	// the carried verdict and confirms the gate covers it, consuming nothing.
	if err := m.gateCoversRebased(reloaded, rebased, true); err != nil {
		t.Fatalf("gateCoversRebased must pass after the carry: %v", err)
	}
	data, ok := approval.Data(m.P.ApprovalFile("cu1"))
	if !ok {
		t.Fatal("the approval token must be re-minted onto the rebased commit by the gate check")
	}
	if by, sha := splitApprovalPayload(data); by != "human" || sha != rebased {
		t.Fatalf("the re-minted approval must pin the rebased commit as human (got by=%q sha=%s, want human/%s)", by, short(sha), short(rebased))
	}
	_, _ = m.Teardown("cu1", true)
}

// TestLand_FastLandRefusesChangedContentHumanApproval is the trust-boundary counterpart for a
// HUMAN approval: when the rebase is NOT clean/disjoint — the default advanced by editing the
// SAME file the worker touched, so the worker's own three-dot diff changes after the rebase —
// the human approval (like the verdict) must NOT be carried onto the changed content. Land
// refuses with a re-gate instruction, leaves the default branch untouched, and leaves BOTH the
// verdict and the human approval pinned to the original (now stale) commit.
func TestLand_FastLandRefusesChangedContentHumanApproval(t *testing.T) {
	m, repo := deliveryHarness(t, "carrychanged")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	// A shared multi-line file both the worker and the default will edit — on DIFFERENT lines,
	// so the rebase auto-merges (no conflict) yet the worker's three-dot diff still changes.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "shared.txt")
	gitIn(t, repo, "commit", "-q", "-m", "seed shared.txt")

	task, err := m.Spawn("cc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "shared.txt", "l1\nl2\nl3\nl4\nl5-worker\n") // worker edits the last line
	writeReviewReports(t, m.P.ReviewInputsDir("cc1"), feat, nil)
	if _, err := m.TrustRecord("cc1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("cc1", time.Minute); err != nil { // human token pinned to feat
		t.Fatal(err)
	}

	// The default advances by editing a NEARBY line of the SAME file — no conflict, but the
	// worker's three-dot diff (its surrounding context and base blob) changes after the rebase.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("l1\nl2\nl3-main\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "shared.txt")
	gitIn(t, repo, "commit", "-q", "-m", "default edits shared.txt")
	defBefore := gitIn(t, repo, "rev-parse", "HEAD")

	_, err = m.Land("cc1", true)
	if err == nil {
		t.Fatal("fast-land must refuse to carry a human approval onto changed content")
	}
	if !strings.Contains(err.Error(), "changed its reviewed diff") || !strings.Contains(err.Error(), "trust prep") {
		t.Fatalf("expected a re-gate instruction naming the changed diff, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defBefore {
		t.Fatal("the default branch must not move when the gate cannot be carried")
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker onto the advanced default")
	}
	// The HUMAN approval was NOT carried onto the changed content: it still pins the ORIGINAL commit.
	data, ok := approval.Data(m.P.ApprovalFile("cc1"))
	if !ok {
		t.Fatal("the approval must remain intact after a refusal")
	}
	if by, sha := splitApprovalPayload(data); by != "human" || sha != feat {
		t.Fatalf("the approval must remain pinned to the original commit (got by=%q sha=%s, want human/%s)", by, short(sha), short(feat))
	}
	// The verdict was NOT carried either: it still pins the ORIGINAL reviewed commit.
	if v, ok, _ := m.Store.GetVerdict(context.Background(), "cc1"); !ok || v.ReviewedSHA != feat {
		t.Fatalf("the verdict must remain pinned to the original commit (not carried): %+v ok=%v", v, ok)
	}
	_, _ = m.Teardown("cc1", true)
}

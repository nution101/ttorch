package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
)

// expireToken overwrites a task's approval token with an ALREADY-EXPIRED one of the same
// provenance and pin — simulating a token whose 30-minute wall-clock TTL lapsed while the
// gated task waited in a busy land queue, the failure that used to strand it forever.
func expireToken(t *testing.T, m *Manager, id, by, sha string) {
	t.Helper()
	if err := approval.Grant(m.P.ApprovalFile(id), -time.Second, approvalPayload(by, sha)); err != nil {
		t.Fatal(err)
	}
	if approval.Valid(m.P.ApprovalFile(id)) {
		t.Fatal("setup: the token should be expired")
	}
}

// setAutoMintMaxAge appends a per-repo `- auto-mint-max-age:` policy line to the repo's
// AGENTS.md (outside the ttorch-managed block, where ReadAutoMintMaxAge still finds it).
func setAutoMintMaxAge(t *testing.T, repo, dur string) {
	t.Helper()
	path := filepath.Join(repo, "AGENTS.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte("\n- auto-mint-max-age: "+dur+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hasApprovalRequiredEvent reports whether an actionable approval_required event exists for id.
func hasApprovalRequiredEvent(t *testing.T, m *Manager, id string) bool {
	t.Helper()
	evs, err := m.Store.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.EntityID == id && e.Type == eventApprovalRequired && e.Actionable {
			return true
		}
	}
	return false
}

// TestMergeLocal_ExpiredAutoTokenStillPassingVerdictLands is the headline fix: the durable,
// content+commit-pinned verdict row is the SINGLE merge authority, so a gated task whose
// auto-approval TOKEN has EXPIRED — but whose verdict still passes and still pins the worker
// head — LANDS by re-minting the token from the verdict, instead of looping forever on
// "no valid approval" (the production land-flakiness root cause). The worker is already a
// fast-forward of the default (no rebase), the exact non-rebased path that used to dead-end.
func TestMergeLocal_ExpiredAutoTokenStillPassingVerdictLands(t *testing.T) {
	m, repo := deliveryHarness(t, "expiredauto")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task := readyTrustedTask(t, m, repo, "ea1", "a.txt", "a\n")
	head := gitIn(t, task.Worktree, "rev-parse", "HEAD")

	// The auto-approval token lapses while the task waits in the land queue; the durable
	// verdict row does NOT expire (no TTL) and still pins the worker head.
	expireToken(t, m, "ea1", "auto", head)
	if _, ok := m.TrustShow("ea1"); !ok {
		t.Fatal("setup: the durable verdict must still be present after the token expired")
	}

	out, err := m.MergeLocal("ea1", false) // trusted ⇒ gated
	if err != nil {
		t.Fatalf("a still-passing, head-pinned verdict must land even with an expired token (no retry-forever): %v", err)
	}
	if !strings.Contains(out, "fast-forwarded") {
		t.Fatalf("unexpected merge summary: %q", out)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("the default branch must fast-forward to the reviewed head")
	}
	// Consume-once: a successful gated merge deletes the durable verdict and the (re-minted) token.
	if _, ok := m.TrustShow("ea1"); ok {
		t.Fatal("the durable verdict must be consumed by the merge")
	}
	if approval.Valid(m.P.ApprovalFile("ea1")) {
		t.Fatal("the re-minted token must be consumed by the merge")
	}
	// The audit records that the token was re-minted from the durable verdict, then merged.
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if !strings.Contains(string(auditLog), "remint-approval task=ea1") {
		t.Fatalf("the merge must record re-minting the approval from the durable verdict:\n%s", auditLog)
	}
	_, _ = m.Teardown("ea1", true)
}

// TestLand_ExpiredAutoTokenLandsViaCarry is the rebased counterpart: a gated task whose token
// EXPIRED in the queue, then the default advanced disjointly. Land rebases the worker (moving
// its sha), carries the still-passing verdict forward over the clean rebase (DiffID match), and
// the gate re-mints the auto token onto the rebased commit from that verdict — landing in one
// command with no re-gate, no re-approval, and no retry-forever.
func TestLand_ExpiredAutoTokenLandsViaCarry(t *testing.T) {
	m, repo := deliveryHarness(t, "expiredcarry")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task := readyTrustedTask(t, m, repo, "ec1", "a.txt", "a\n")
	feat := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	expireToken(t, m, "ec1", "auto", feat)

	// The default advances on a DIFFERENT file after the token expired.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("concurrent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent landing")

	out, err := m.Land("ec1", true) // autonomous-land semantics (requireVerdict=true)
	if err != nil {
		t.Fatalf("an expired-token, still-passing, cleanly-rebasable task must land via carry: %v", err)
	}
	rebased := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker onto the advanced default")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != rebased {
		t.Fatal("the default branch must fast-forward to the rebased commit")
	}
	if !strings.Contains(out, "landed ec1") || !strings.Contains(out, "verified") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	for _, f := range []string{"a.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should be present on the landed default branch: %v", f, err)
		}
	}
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if !strings.Contains(string(auditLog), "carried verdict") {
		t.Fatalf("the land must carry the verdict forward over the clean rebase:\n%s", auditLog)
	}
	_, _ = m.Teardown("ec1", true)
}

// TestLand_ExpiredHumanTokenLandsNoAgeBound proves a HUMAN approval is durable: a gated
// (--require-verdict, local mode) task the lead approved by hand still LANDS after its token
// expired, because the verdict row records ApprovedBy=human and a human approval carries NO
// age bound — the gate re-mints the human token from the durable verdict and merges.
func TestLand_ExpiredHumanTokenLandsNoAgeBound(t *testing.T) {
	m, repo := deliveryHarness(t, "expiredhuman")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	// Even with an aggressive staleness policy configured, a HUMAN approval is never age-bound.
	setAutoMintMaxAge(t, repo, "1ns")
	task, err := m.Spawn("eh1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("eh1"), head, nil)
	if _, err := m.TrustRecord("eh1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("eh1", time.Minute); err != nil { // human token pinned to head
		t.Fatal(err)
	}
	expireToken(t, m, "eh1", "human", head)

	out, err := m.Land("eh1", true)
	if err != nil {
		t.Fatalf("a human-approved gated task must land after its token expired (no age bound): %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("the default branch must fast-forward to the reviewed head")
	}
	if !strings.Contains(out, "landed eh1") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if !strings.Contains(string(auditLog), "gate=verdict approver=human") {
		t.Fatalf("the merge must record a human-approved verdict merge:\n%s", auditLog)
	}
	_, _ = m.Teardown("eh1", true)
}

// TestMergeLocal_ExpiredTokenStaleVerdictFailsClosed is the trust boundary: an expired token
// must NOT be re-minted when the verdict no longer covers the commit being merged. Here the
// worker ADVANCES past the reviewed commit (the verdict's reviewed_sha is now stale), so the
// gate must refuse — never re-mint a token for unreviewed content — and the default must not move.
func TestMergeLocal_ExpiredTokenStaleVerdictFailsClosed(t *testing.T) {
	m, repo := deliveryHarness(t, "expiredstale")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task := readyTrustedTask(t, m, repo, "es1", "a.txt", "a\n")
	reviewed := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	expireToken(t, m, "es1", "auto", reviewed)

	// The worker commits MORE after review — the verdict's reviewed_sha no longer covers HEAD.
	newHead := commitFeature(t, task.Worktree, "a.txt", "a\nmore-unreviewed\n")
	if newHead == reviewed {
		t.Fatal("setup: the worker should have advanced past the reviewed commit")
	}
	before := gitIn(t, repo, "rev-parse", "HEAD")

	_, err := m.MergeLocal("es1", false) // trusted ⇒ gated
	if err == nil {
		t.Fatal("the gate must fail closed when the verdict no longer covers the worker head")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != before {
		t.Fatal("a refused merge must not advance the default branch")
	}
	// No token was re-minted for the unreviewed head (re-mint only follows an authorizing verdict).
	if approval.Valid(m.P.ApprovalFile("es1")) {
		t.Fatal("the gate must not re-mint a token for a commit the verdict does not cover")
	}
	_, _ = m.Teardown("es1", true)
}

// TestMergeLocal_OverAgeAutoMintRefusedWithEvent covers the configurable autonomous staleness
// bound: a trusted auto-pass older than the repo's auto-mint-max-age is NOT auto-landed — the
// gate refuses with a clear "needs a human" error and surfaces an ACTIONABLE approval_required
// event (so the now-stale task is never silently stuck) — then a human `ttorch approve` lands
// it, because a human approval has no age bound.
func TestMergeLocal_OverAgeAutoMintRefusedWithEvent(t *testing.T) {
	m, repo := deliveryHarness(t, "overage")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	setAutoMintMaxAge(t, repo, "24h")
	if d, ok := projectinit.ReadAutoMintMaxAge(repo); !ok || d != 24*time.Hour {
		t.Fatalf("ReadAutoMintMaxAge = %v ok=%v, want 24h/true", d, ok)
	}
	task := readyTrustedTask(t, m, repo, "oa1", "a.txt", "a\n")
	head := gitIn(t, task.Worktree, "rev-parse", "HEAD")

	// Age the freshly-recorded verdict far past the 24h bound (the verdict's created_at is ~now).
	orig := nowFunc
	nowFunc = func() time.Time { return orig().Add(100 * 24 * time.Hour) }
	defer func() { nowFunc = orig }()

	before := gitIn(t, repo, "rev-parse", "HEAD")
	_, err := m.MergeLocal("oa1", false) // trusted ⇒ gated
	if err == nil {
		t.Fatal("an over-age autonomous auto-mint must be refused (needs a human)")
	}
	if !strings.Contains(err.Error(), "auto-mint-max-age") {
		t.Fatalf("expected an over-age refusal naming the policy, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != before {
		t.Fatal("an over-age refusal must not advance the default branch")
	}
	if !hasApprovalRequiredEvent(t, m, "oa1") {
		t.Fatal("an over-age refusal must surface an actionable approval_required event")
	}
	// The durable verdict is intact (refusal consumes nothing): a human approval now lands it,
	// since a human approval carries no age bound even with the staleness policy in force.
	if _, ok := m.TrustShow("oa1"); !ok {
		t.Fatal("the over-age refusal must not consume the durable verdict")
	}
	if err := m.Approve("oa1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("oa1", false); err != nil {
		t.Fatalf("a human approval must land regardless of the auto-mint-max-age policy: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("the human-approved merge must fast-forward the default to the reviewed head")
	}
	_, _ = m.Teardown("oa1", true)
}

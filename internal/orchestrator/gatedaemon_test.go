package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/tmux"
)

// trustedTaskWithSubstantialDiff stands up a trusted repo with a passing default-branch gate
// script, spawns a worker, and commits a multi-file code change so the gate scales to the full
// {correctness, scope, security} reviewer set. It returns the manager, the task id, and the
// reviewed HEAD.
func trustedTaskWithSubstantialDiff(t *testing.T, tag, id string) (*Manager, string) {
	t.Helper()
	m, repo := deliveryHarness(t, tag)
	commitGateScript(t, repo, "exit 0") // default branch defines a passing gate
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn(id, repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	// Two files ⇒ a substantial change ⇒ the full three-dimension reviewer set.
	for name, body := range map[string]string{"a.go": "package a\n", "b.go": "package b\n"} {
		if err := os.WriteFile(filepath.Join(task.Worktree, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, task.Worktree, "add", "-A")
	gitIn(t, task.Worktree, "commit", "-q", "-m", "work")
	head := gitIn(t, task.Worktree, "rev-parse", "HEAD")
	return m, head
}

// recordingReviewer replaces reviewerDispatcher with a stub that counts per-dimension dispatches
// and, when makeWindow is set, opens a real (empty) tmux window so reviewerWindowAlive reads true
// next tick — letting a test model a reviewer that is still running without a live Claude session.
// It restores the production dispatcher on cleanup.
func recordingReviewer(t *testing.T, makeWindow bool) *reviewerRec {
	t.Helper()
	rec := &reviewerRec{calls: map[string]int{}}
	prev := reviewerDispatcher
	t.Cleanup(func() { reviewerDispatcher = prev })
	reviewerDispatcher = func(m *Manager, taskID, dim, dir, head, repo, wt string) error {
		rec.mu.Lock()
		rec.calls[dim]++
		rec.mu.Unlock()
		if makeWindow {
			return tmux.NewWindow(m.Session, reviewerWindow(taskID, dim), wt)
		}
		return nil
	}
	return rec
}

type reviewerRec struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *reviewerRec) count(dim string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[dim]
}

func (r *reviewerRec) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

// hasGateBlockedEvent reports whether an actionable gate_blocked event was surfaced for id.
func hasGateBlockedEvent(t *testing.T, m *Manager, id string) bool {
	t.Helper()
	evs, err := m.Store.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.EntityID == id && e.Type == db.EventGateBlocked && e.Actionable && e.Actor == db.ActorSystem {
			return true
		}
	}
	return false
}

// TestGateOnce_HappyPathRecordsHandsOff is the headline happy path: with all reviewer reports in
// and clean, the daemon gate records a passing verdict through the unchanged trust-record path
// (auto-minting the approval in trusted mode) with NO manager action — and is idempotent
// afterward (a verdict covering head short-circuits, never re-dispatching or re-recording).
func TestGateOnce_HappyPathRecordsHandsOff(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-ok", "g1")
	t.Cleanup(func() { _, _ = m.Teardown("g1", true) })
	rec := recordingReviewer(t, false)
	dims := m.ReviewersFor("g1")

	// Tick 1: no reports, no windows ⇒ dispatch every required dimension exactly once.
	out, err := m.GateOnce("g1")
	if err != nil {
		t.Fatalf("GateOnce tick1: %v", err)
	}
	if out != GateDispatched {
		t.Fatalf("tick1 outcome = %q, want %q", out, GateDispatched)
	}
	for _, d := range dims {
		if rec.count(d) != 1 {
			t.Fatalf("dimension %s dispatched %d times, want 1", d, rec.count(d))
		}
	}

	// Reviewers complete: drop clean reports pinned to head.
	writeReviewReports(t, m.P.ReviewInputsDir("g1"), head, nil)

	// Tick 2: all reports present and pinned ⇒ aggregate pass ⇒ record + auto-mint.
	out, err = m.GateOnce("g1")
	if err != nil {
		t.Fatalf("GateOnce tick2: %v", err)
	}
	if out != GateRecorded {
		t.Fatalf("tick2 outcome = %q, want %q", out, GateRecorded)
	}
	v, ok := m.TrustShow("g1")
	if !ok || v.Overall != review.Pass {
		t.Fatalf("expected a recorded passing verdict, got ok=%v v=%+v", ok, v)
	}
	if v.ReviewedSHA != head {
		t.Fatalf("verdict pinned to %q, want head %q", v.ReviewedSHA, head)
	}
	if !approval.Valid(m.P.ApprovalFile("g1")) {
		t.Fatal("a trusted pass must auto-mint the approval token (hands-off land)")
	}

	// Idempotent: a verdict now covers head, so a re-run skips — no re-dispatch, no re-record.
	out, err = m.GateOnce("g1")
	if err != nil {
		t.Fatalf("GateOnce tick3: %v", err)
	}
	if out != GateSkipped {
		t.Fatalf("tick3 outcome = %q, want %q (already gated)", out, GateSkipped)
	}
	if rec.total() != len(dims) {
		t.Fatalf("reviewers re-dispatched after recording: total=%d, want %d", rec.total(), len(dims))
	}
}

// TestGateOnce_RunningReviewerNotReDispatched proves the in-flight idempotency that makes a
// daemon restart safe: while a dimension's reviewer window is alive (report not yet written) the
// gate waits and does NOT re-dispatch it.
func TestGateOnce_RunningReviewerNotReDispatched(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-running", "g2")
	t.Cleanup(func() { _, _ = m.Teardown("g2", true) })
	rec := recordingReviewer(t, true) // stub opens a real reviewer window
	dims := m.ReviewersFor("g2")

	if out, err := m.GateOnce("g2"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// Tick 2: windows alive, no reports ⇒ waiting, and no dimension re-dispatched.
	if out, err := m.GateOnce("g2"); err != nil || out != GateWaiting {
		t.Fatalf("tick2 = (%q, %v), want waiting", out, err)
	}
	for _, d := range dims {
		if rec.count(d) != 1 {
			t.Fatalf("dimension %s re-dispatched (%d) while its reviewer was still running", d, rec.count(d))
		}
	}
}

// TestGateOnce_BlockingFindingNeverRecords is the headline FAIL-CLOSED test: a high-severity
// finding makes aggregation block, so the daemon records NOTHING and surfaces an actionable
// gate_blocked event for the manager — it never auto-records a failing gate.
func TestGateOnce_BlockingFindingNeverRecords(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-block", "b1")
	t.Cleanup(func() { _, _ = m.Teardown("b1", true) })
	recordingReviewer(t, false)

	if out, err := m.GateOnce("b1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// A blocking security finding lands in the reports (the other dimensions are clean).
	writeReviewReports(t, m.P.ReviewInputsDir("b1"), head, map[string][]review.Finding{
		review.DimensionSecurity: {{
			Dimension: review.DimensionSecurity, Severity: review.SeverityHigh,
			Reviewer: "ttorch-reviewer-security", Summary: "hardcoded secret",
		}},
	})
	out, err := m.GateOnce("b1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q", out, GateBlocked)
	}
	if _, ok := m.TrustShow("b1"); ok {
		t.Fatal("a blocking gate must NOT record a verdict")
	}
	if approval.Valid(m.P.ApprovalFile("b1")) {
		t.Fatal("a blocking gate must NOT mint an approval token")
	}
	if !hasGateBlockedEvent(t, m, "b1") {
		t.Fatal("a blocking gate must surface an actionable gate_blocked event for the manager")
	}
	// Re-run: the head is terminal-for-this-episode, so it stays skipped (no re-loop, no record).
	if out, err := m.GateOnce("b1"); err != nil || out != GateSkipped {
		t.Fatalf("re-run after block = (%q, %v), want skipped", out, err)
	}
	if _, ok := m.TrustShow("b1"); ok {
		t.Fatal("a blocking gate must still not record on re-run")
	}
}

// TestGateOnce_MissingReportNeverRecordsPass proves the gate never passes on a partial report
// set: with one dimension's report absent it re-dispatches that reviewer and records nothing —
// an all-pass requires EVERY required report present and pinned.
func TestGateOnce_MissingReportNeverRecordsPass(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-missing", "mr1")
	t.Cleanup(func() { _, _ = m.Teardown("mr1", true) })
	rec := recordingReviewer(t, false)
	dims := m.ReviewersFor("mr1")

	if out, err := m.GateOnce("mr1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// Write clean reports for all dimensions EXCEPT security, then remove security's so it is
	// genuinely missing (writeReviewReports always writes the full set).
	writeReviewReports(t, m.P.ReviewInputsDir("mr1"), head, nil)
	if err := os.Remove(filepath.Join(m.P.ReviewInputsDir("mr1"), review.DimensionSecurity+".json")); err != nil {
		t.Fatal(err)
	}
	out, err := m.GateOnce("mr1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out == GateRecorded {
		t.Fatal("the gate recorded a verdict with a missing reviewer report (must fail closed)")
	}
	if _, ok := m.TrustShow("mr1"); ok {
		t.Fatal("a missing report must never yield a recorded verdict")
	}
	// The missing dimension is re-dispatched (no report, no window); the present ones are not.
	if rec.count(review.DimensionSecurity) != 2 {
		t.Fatalf("security re-dispatched %d times, want 2 (missing report ⇒ retry)", rec.count(review.DimensionSecurity))
	}
	for _, d := range dims {
		if d == review.DimensionSecurity {
			continue
		}
		if rec.count(d) != 1 {
			t.Fatalf("dimension %s with a present report was re-dispatched (%d)", d, rec.count(d))
		}
	}
}

// TestGateOnce_StaleShaReportNeverRecordsPass proves a report pinned to the WRONG commit is
// treated as not-ready (re-dispatched), never aggregated into a pass.
func TestGateOnce_StaleShaReportNeverRecordsPass(t *testing.T) {
	m, head := trustedTaskWithSubstantialDiff(t, "gate-stale", "st1")
	t.Cleanup(func() { _, _ = m.Teardown("st1", true) })
	recordingReviewer(t, false)

	if out, err := m.GateOnce("st1"); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// All reports present but pinned to a DIFFERENT sha than head.
	writeReviewReports(t, m.P.ReviewInputsDir("st1"), head+"-stale", nil)
	out, err := m.GateOnce("st1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out == GateRecorded {
		t.Fatal("the gate recorded a verdict from stale-pinned reports (must fail closed)")
	}
	if _, ok := m.TrustShow("st1"); ok {
		t.Fatal("stale-pinned reports must never yield a recorded verdict")
	}
}

// TestGateOnce_PrepRefusalSurfaces proves a prep refusal (a dirty worktree the worker must
// resolve) surfaces a gate_blocked event and records nothing — never silently records or
// dispatches reviewers against an unprepped diff.
func TestGateOnce_PrepRefusalSurfaces(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-dirty", "d1")
	t.Cleanup(func() { _, _ = m.Teardown("d1", true) })
	rec := recordingReviewer(t, false)
	// Dirty the worktree so TrustPrep refuses (committed != reviewed state).
	task, _, _ := m.Store.GetTask(context.Background(), "d1")
	if err := os.WriteFile(filepath.Join(task.Worktree, "uncommitted.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := m.GateOnce("d1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q", out, GateBlocked)
	}
	if rec.total() != 0 {
		t.Fatalf("reviewers dispatched (%d) against a refused prep", rec.total())
	}
	if _, ok := m.TrustShow("d1"); ok {
		t.Fatal("a prep refusal must not record a verdict")
	}
	if !hasGateBlockedEvent(t, m, "d1") {
		t.Fatal("a prep refusal must surface a gate_blocked event for the manager")
	}
}

// TestGateOnce_NonTrustedRepoSkips proves the daemon gate only acts in trusted repos: a non-trusted
// repo is skipped without prep, dispatch, or record (a daemon verdict there would not advance
// delivery — a human still approves).
func TestGateOnce_NonTrustedRepoSkips(t *testing.T) {
	m, repo := deliveryHarness(t, "gate-nontrusted")
	t.Cleanup(func() { _, _ = m.Teardown("nt1", true) })
	commitGateScript(t, repo, "exit 0") // a gate script, but the repo is NOT in trusted mode
	task, err := m.Spawn("nt1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "a.go", "package a\n")
	rec := recordingReviewer(t, false)

	out, err := m.GateOnce("nt1")
	if err != nil {
		t.Fatalf("GateOnce: %v", err)
	}
	if out != GateSkipped {
		t.Fatalf("outcome = %q, want %q (non-trusted repo)", out, GateSkipped)
	}
	if rec.total() != 0 {
		t.Fatalf("reviewers dispatched (%d) in a non-trusted repo", rec.total())
	}
	if _, ok := m.TrustShow("nt1"); ok {
		t.Fatal("a non-trusted repo must not get a daemon-recorded verdict")
	}
}

// TestGateOnce_ReviewerRetryCeilingBlocks proves the reviewer restart-storm bound: a reviewer
// that produces no report (and leaves no live window) is re-dispatched up to maxReviewerAttempts,
// then the gate gives up and surfaces a block rather than respawning forever.
func TestGateOnce_ReviewerRetryCeilingBlocks(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-ceiling", "rc1")
	t.Cleanup(func() { _, _ = m.Teardown("rc1", true) })
	recordingReviewer(t, false) // never writes a report, never leaves a window
	now := time.Unix(1_700_000_000, 0)

	// maxReviewerAttempts=1: tick 1 dispatches (attempt 1), tick 2 finds it over the ceiling.
	if out, err := m.gateOnceAt("rc1", time.Minute, 1, time.Hour, now); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	out, err := m.gateOnceAt("rc1", time.Minute, 1, time.Hour, now)
	if err != nil {
		t.Fatalf("gateOnceAt tick2: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q (retry ceiling)", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "rc1") {
		t.Fatal("a reviewer retry ceiling must surface a gate_blocked event")
	}
	if _, ok := m.TrustShow("rc1"); ok {
		t.Fatal("the retry ceiling must not record a verdict")
	}
}

// TestGateOnce_ReviewerTimeoutBlocks proves a reviewer that stays alive but never reports is
// surfaced as a stall once it exceeds the reviewer timeout, rather than leaving the done task in
// a silent forever-wait.
func TestGateOnce_ReviewerTimeoutBlocks(t *testing.T) {
	m, _ := trustedTaskWithSubstantialDiff(t, "gate-timeout", "to1")
	t.Cleanup(func() { _, _ = m.Teardown("to1", true) })
	recordingReviewer(t, true) // opens live reviewer windows, never writes reports
	start := time.Unix(1_700_000_000, 0)

	if out, err := m.gateOnceAt("to1", time.Minute, 5, 10*time.Minute, start); err != nil || out != GateDispatched {
		t.Fatalf("tick1 = (%q, %v), want dispatched", out, err)
	}
	// Within the timeout: still waiting (windows alive, no reports).
	if out, err := m.gateOnceAt("to1", time.Minute, 5, 10*time.Minute, start.Add(time.Minute)); err != nil || out != GateWaiting {
		t.Fatalf("within timeout = (%q, %v), want waiting", out, err)
	}
	// Past the timeout: surfaced as a stall.
	out, err := m.gateOnceAt("to1", time.Minute, 5, 10*time.Minute, start.Add(11*time.Minute))
	if err != nil {
		t.Fatalf("gateOnceAt past timeout: %v", err)
	}
	if out != GateBlocked {
		t.Fatalf("outcome = %q, want %q (reviewer timeout)", out, GateBlocked)
	}
	if !hasGateBlockedEvent(t, m, "to1") {
		t.Fatal("a reviewer timeout must surface a gate_blocked event")
	}
	if _, ok := m.TrustShow("to1"); ok {
		t.Fatal("a reviewer timeout must not record a verdict")
	}
}

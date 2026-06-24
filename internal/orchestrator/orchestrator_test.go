package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/tmux"
)

// TestMain disables native worker terminal views for the whole package. The
// integration tests below exercise Spawn, which best-effort opens a terminal tab
// or window on macOS; without this, running these tests would spawn real GUI
// windows on a developer's Mac. It also disables the auto-ensure of the background
// supervisor so a Spawn test never detaches a stray `daemon run` process.
func TestMain(m *testing.M) {
	os.Setenv("TTORCH_WORKER_TABS", "off")
	os.Setenv("TTORCH_NO_SUPERVISOR", "1")
	os.Exit(m.Run())
}

func TestDeriveState(t *testing.T) {
	cases := []struct {
		name string
		live bool
		pane string
		want string
	}{
		{"dead window", false, "", "gone"},
		{"dead wins over busy text", false, "esc to interrupt", "gone"},
		{"live empty pane", true, "", "idle"},
		{"live at a prompt", true, "all set\n> ", "idle"},
		{"busy interrupt", true, "✻ Working… (esc to interrupt)", "working"},
		{"busy thinking", true, "Thinking about the change", "working"},
	}
	for _, tc := range cases {
		if got := DeriveState(tc.live, tc.pane); got != tc.want {
			t.Errorf("%s: DeriveState(%v, %q) = %q, want %q", tc.name, tc.live, tc.pane, got, tc.want)
		}
	}
}

// TestWindowLabel pins the friendly tab titles: scouts are tagged, ship and cc
// tasks read as just the id, and no internal "wk-"/"ttv-" prefix leaks through.
func TestWindowLabel(t *testing.T) {
	cases := []struct {
		kind, id, want string
	}{
		{"scout", "tab-names", "scout · tab-names"},
		{"ship", "tab-names", "tab-names"},
		{"cc", "cc-184205", "cc-184205"},
		{"", "x", "x"}, // unknown kind defaults to the plain id
	}
	for _, c := range cases {
		if got := windowLabel(c.kind, c.id); got != c.want {
			t.Errorf("windowLabel(%q, %q) = %q, want %q", c.kind, c.id, got, c.want)
		}
		if strings.Contains(windowLabel(c.kind, c.id), "wk-") {
			t.Errorf("windowLabel(%q, %q) leaked a wk- prefix", c.kind, c.id)
		}
	}
}

// TestSpawnPeekTeardown exercises the real runtime against tmux + git. It is
// skipped where tmux is unavailable (e.g. CI without tmux installed).
func TestSpawnPeekTeardown(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	repo := t.TempDir()
	runGit := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "init")

	session := fmt.Sprintf("ttorch-test-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())

	task, err := m.Spawn("t1", repo, false, "printf 'TTORCH_MARKER\\n'; sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if task.Window != "wk-t1" {
		t.Fatalf("window = %q, want wk-t1", task.Window)
	}
	if !m.Live(task) {
		t.Fatal("window is not live after spawn")
	}
	if fi, err := os.Stat(task.Worktree); err != nil || !fi.IsDir() {
		t.Fatalf("worktree missing: %v", err)
	}
	if tasks, _ := m.Status(); len(tasks) != 1 {
		t.Fatalf("status returned %d tasks, want 1", len(tasks))
	}

	// Poll the pane for the marker (tolerates startup timing).
	var out string
	for i := 0; i < 20; i++ {
		out, _ = m.Peek("t1", 50)
		if strings.Contains(out, "TTORCH_MARKER") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(out, "TTORCH_MARKER") {
		t.Fatalf("marker not found in pane output: %q", out)
	}

	// Clean worktree -> teardown succeeds without --force.
	if _, err := m.Teardown("t1", false); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if m.Live(task) {
		t.Fatal("window still alive after teardown")
	}
	if _, err := os.Stat(task.Worktree); err != nil {
		t.Fatal("worktree should be kept in the pool for reuse after teardown")
	}
	if tasks, _ := m.Status(); len(tasks) != 0 {
		t.Fatalf("status returned %d tasks after teardown, want 0", len(tasks))
	}

	// A new task reuses the now-idle pooled worktree.
	task2, err := m.Spawn("t2", repo, false, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if task2.Worktree != task.Worktree {
		t.Fatalf("expected worktree reuse: %q vs %q", task2.Worktree, task.Worktree)
	}
	_, _ = m.Teardown("t2", true)
}

func TestTeardownRefusesDirtyWorktree(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644)
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "init")

	session := fmt.Sprintf("ttorch-test-dirty-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())
	task, err := m.Spawn("d1", repo, false, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	// Make the worktree dirty.
	if err := os.WriteFile(filepath.Join(task.Worktree, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Teardown("d1", false); err == nil {
		t.Fatal("teardown should refuse a dirty worktree without --force")
	}
	// --force overrides.
	if _, err := m.Teardown("d1", true); err != nil {
		t.Fatalf("forced teardown failed: %v", err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=w", "GIT_AUTHOR_EMAIL=w@example.com",
		"GIT_COMMITTER_NAME=w", "GIT_COMMITTER_EMAIL=w@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepoMain(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func TestDeliveryLifecycle(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	repo := newRepoMain(t)

	session := fmt.Sprintf("ttorch-deliver-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())
	task, err := m.Spawn("d1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree

	// Simulate the worker committing a change in its worktree.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "add feature")

	// review-diff surfaces the change.
	diff, err := m.ReviewDiff("d1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Fatalf("review-diff missing the change: %q", diff)
	}

	// merge-local refuses without an approval token.
	if _, err := m.MergeLocal("d1", false); err == nil {
		t.Fatal("merge-local must refuse without approval")
	}

	// Approve, then merge: the default branch fast-forwards to the worker's HEAD.
	if err := m.Approve("d1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("d1", false); err != nil {
		t.Fatalf("merge-local: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != gitIn(t, wt, "rev-parse", "HEAD") {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	// Approval is single-use.
	if _, err := m.MergeLocal("d1", false); err == nil {
		t.Fatal("approval should be consumed after one merge")
	}
	_, _ = m.Teardown("d1", true)

	// promote: a scout task becomes a ship task.
	scout, err := m.Spawn("s9", repo, true, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	if scout.Kind != "scout" {
		t.Fatalf("expected scout kind, got %q", scout.Kind)
	}
	if err := m.Promote("s9"); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := m.Store.Load("s9")
	if reloaded.Kind != "ship" {
		t.Fatalf("promote did not flip kind: %q", reloaded.Kind)
	}
	_, _ = m.Teardown("s9", true)
}

func TestMergeLocal_ApprovalBinding(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	repo := newRepoMain(t)
	session := fmt.Sprintf("ttorch-bind-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())
	task, err := m.Spawn("b1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("1\n"), 0o644)
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work1")

	// A recoverable refusal (uncommitted tracked changes) must NOT consume the approval.
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("locally changed\n"), 0o644) // f.txt is tracked
	if err := m.Approve("b1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("b1", false); err == nil {
		t.Fatal("merge should refuse with uncommitted tracked changes")
	}
	if !approval.Valid(m.P.ApprovalFile("b1")) {
		t.Fatal("a recoverable refusal must leave the approval intact")
	}
	gitIn(t, repo, "checkout", "--", "f.txt") // restore the tracked file

	// An UNTRACKED file (e.g. an `ttorch init` AGENTS.md) must NOT block the merge.
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("notes\n"), 0o644)

	// The worker changes after approval -> merge must reject (and consume the stale token).
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("2\n"), 0o644)
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work2")
	if _, err := m.MergeLocal("b1", false); err == nil {
		t.Fatal("merge should reject work that changed since approval")
	}
	if approval.Valid(m.P.ApprovalFile("b1")) {
		t.Fatal("a stale (commit-mismatched) approval should be consumed")
	}

	// Re-approve the current commit, then merge succeeds.
	if err := m.Approve("b1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("b1", false); err != nil {
		t.Fatalf("merge after re-approval (untracked files present): %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != gitIn(t, wt, "rev-parse", "HEAD") {
		t.Fatal("default branch was not fast-forwarded after re-approval")
	}
	_, _ = m.Teardown("b1", true)
}

// deliveryHarness spins up a Manager against a fresh main-branch repo and a unique
// tmux session, registering teardown. It mirrors the inline setup the other
// delivery tests use.
func deliveryHarness(t *testing.T, tag string) (*Manager, string) {
	t.Helper()
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	repo := newRepoMain(t)
	session := fmt.Sprintf("ttorch-%s-%d", tag, os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })
	return New(paths.Default()), repo
}

// writeReviewReports drops one per-dimension report per required reviewer into dir,
// pinned to sha, as the reviewer subagents would after `ttorch trust prep`.
func writeReviewReports(t *testing.T, dir, sha string, perDim map[string][]review.Finding) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dim := range requiredReviewers {
		b, err := json.Marshal(review.Report{Dimension: dim, ReviewedSHA: sha, Findings: perDim[dim]})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dim+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTrustPrep(t *testing.T) {
	m, repo := deliveryHarness(t, "prep")
	task, err := m.Spawn("pp1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new\n"), 0o644)
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "add feature")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	dir, err := m.TrustPrep("pp1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"diff.patch", "validate.json", "head.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("trust prep did not write %s: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "diff.patch")); !strings.Contains(string(b), "feature.txt") {
		t.Fatalf("diff.patch missing the change: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "head.txt")); strings.TrimSpace(string(b)) != head {
		t.Fatalf("head.txt = %q, want %q", b, head)
	}
	_, _ = m.Teardown("pp1", true)
}

func TestTrustRecord_RefusesStaleSha(t *testing.T) {
	m, repo := deliveryHarness(t, "stale")
	if _, err := m.Spawn("sr1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TrustRecord("sr1", "deadbeefdeadbeef", time.Minute); err == nil {
		t.Fatal("trust record must refuse a sha that is not the worker HEAD")
	}
	_, _ = m.Teardown("sr1", true)
}

// TestTrustRecord_PrModeUnaffectedByVerdict is the regression guard: in pr mode a
// recorded verdict neither auto-mints an approval nor authorizes a merge. The
// merge path stays exactly as today — approval-only.
func TestTrustRecord_PrModeUnaffectedByVerdict(t *testing.T) {
	m, repo := deliveryHarness(t, "pr")
	task, err := m.Spawn("p1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new\n"), 0o644)
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	writeReviewReports(t, m.P.ReviewInputsDir("p1"), head, nil) // all clean → pass
	v, err := m.TrustRecord("p1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("clean reports should pass, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("p1")) {
		t.Fatal("pr mode must NOT auto-mint an approval token")
	}
	reloaded, _ := m.Store.Load("p1")
	if !reloaded.GatePassed || reloaded.ReviewedSHA != head || reloaded.ApprovedBy != "" {
		t.Fatalf("provenance wrong in pr mode: %+v", reloaded)
	}
	// A verdict alone must not authorize a merge in pr mode.
	if _, err := m.MergeLocal("p1", false); err == nil {
		t.Fatal("pr-mode merge must still require an approval token")
	}
	// Identical to today: approve, then merge succeeds.
	if err := m.Approve("p1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("p1", false); err != nil {
		t.Fatalf("approved pr-mode merge: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	_, _ = m.Teardown("p1", true)
}

// commitValidateScript drops a .ttorch/validate.sh into the worktree (so validate.Detect
// finds a single "custom" check), commits it, and returns the new HEAD. body is the
// script's contents: "exit 0" makes the gate's fresh validate pass, "exit 1" makes it
// fail. It is how the gate tests give a worktree a real, controllable check suite.
// commitGateScript commits .ttorch/validate.sh to repo's CURRENT (default) branch with
// the given body, so the trust gate — which resolves its validation DEFINITION from the
// default branch, not the worker's worktree copy — uses it. "exit 0" passes the gate,
// "exit 1" fails it. Returns the new default-branch HEAD. Call it before Spawn so the
// worker's worktree branches from a commit that already carries the gate definition.
func commitGateScript(t *testing.T, repo, body string) string {
	t.Helper()
	dir := filepath.Join(repo, ".ttorch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "validate.sh"), []byte(body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".ttorch/validate.sh")
	gitIn(t, repo, "commit", "-q", "-m", "add gate validate.sh")
	return gitIn(t, repo, "rev-parse", "HEAD")
}

// commitFeature writes name=content in the worker's worktree and commits it (a normal
// deliverable that does NOT touch the gate definition), returning the new worktree HEAD.
func commitFeature(t *testing.T, wt, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")
	return gitIn(t, wt, "rev-parse", "HEAD")
}

// TestMergeLocal_TrustedAutoApproveHappyPath is the headline trust-gate behavior: a
// trusted repo whose worker is verdict-pass AND fresh-validate green merges with NO
// separate `ttorch approve` — the "merge without a human reading the diff" path —
// while staying fully auditable (gate=verdict approver=auto) and consume-once.
func TestMergeLocal_TrustedAutoApproveHappyPath(t *testing.T) {
	m, repo := deliveryHarness(t, "trustok")
	commitGateScript(t, repo, "exit 0") // default branch defines a passing gate
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ta1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("ta1"), head, nil) // clean → pass

	v, err := m.TrustRecord("ta1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("clean reports should pass, got %q", v.Overall)
	}
	if !approval.Valid(m.P.ApprovalFile("ta1")) {
		t.Fatal("trusted + pass verdict + green validate must auto-mint an approval")
	}
	reloaded, _ := m.Store.Load("ta1")
	if reloaded.ApprovedBy != "auto" || !reloaded.GatePassed || reloaded.ReviewedSHA != head {
		t.Fatalf("auto-approve provenance wrong: %+v", reloaded)
	}

	// No `ttorch approve`: the gate is satisfied by the auto-minted token + verdict.
	if _, err := m.MergeLocal("ta1", false); err != nil {
		t.Fatalf("trusted merge should succeed without a manual approval: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "gate=verdict approver=auto") {
		t.Fatalf("audit log missing the trusted auto-merge record: %s", b)
	}
	// Both tokens are single-use: a replay finds nothing to consume.
	if approval.Valid(m.P.ApprovalFile("ta1")) {
		t.Fatal("the approval must be consumed by the merge")
	}
	if _, ok := m.TrustShow("ta1"); ok {
		t.Fatal("the verdict must be consumed by the merge")
	}
	_, _ = m.Teardown("ta1", true)
}

// TestMergeLocal_TrustedHumanApproveOverridesAutoLabel guards the audit's human-vs-auto
// distinction — the reason the verdict and approval tokens are kept separate. If the lead
// explicitly runs `ttorch approve` in a trusted repo after an auto-mint, the merge consumes
// the human token and MUST be recorded as approver=human, derived from the token itself.
func TestMergeLocal_TrustedHumanApproveOverridesAutoLabel(t *testing.T) {
	m, repo := deliveryHarness(t, "humanoverride")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ho1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("ho1"), head, nil)
	if _, err := m.TrustRecord("ho1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// The auto-mint happened first...
	if reloaded, _ := m.Store.Load("ho1"); reloaded.ApprovedBy != "auto" {
		t.Fatalf("expected an auto-mint first, got ApprovedBy=%q", reloaded.ApprovedBy)
	}
	// ...but the lead then explicitly approves the same commit, which must take over the
	// token's provenance so the merge is attributed to a human, not the AI gate.
	if err := m.Approve("ho1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if reloaded, _ := m.Store.Load("ho1"); reloaded.ApprovedBy != "human" {
		t.Fatalf("an explicit approve must record a human approver, got %q", reloaded.ApprovedBy)
	}
	if _, err := m.MergeLocal("ho1", false); err != nil {
		t.Fatalf("gated merge after a human approve should succeed: %v", err)
	}
	b, _ := os.ReadFile(m.P.AuditLog())
	if !strings.Contains(string(b), "gate=verdict approver=human") {
		t.Fatalf("a human-approved trusted merge must audit as approver=human: %s", b)
	}
	if strings.Contains(string(b), "approver=auto") {
		t.Fatalf("the human-approved merge must not be mislabeled auto: %s", b)
	}
	_, _ = m.Teardown("ho1", true)
}

// TestMergeLocal_TrustedNoChecksHardBlock locks in the finance-critical fail-open
// correction: a repo with NO detectable build/test/lint is never green, so a pass
// verdict neither auto-approves nor satisfies the gate — even a manual approval is
// blocked, and the refusal leaves that approval intact.
func TestMergeLocal_TrustedNoChecksHardBlock(t *testing.T) {
	m, repo := deliveryHarness(t, "nochecks")
	// No gate script on the default branch and no go.mod → no checks detected.
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("nc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("nc1"), head, nil) // clean → pass

	v, err := m.TrustRecord("nc1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("clean reports should pass, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("nc1")) {
		t.Fatal("no-checks-detected must never auto-mint, even with a pass verdict in trusted mode")
	}
	if reloaded, _ := m.Store.Load("nc1"); reloaded.ApprovedBy != "" {
		t.Fatalf("no-checks repo must not record an auto approver: %+v", reloaded)
	}

	if err := m.Approve("nc1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("nc1", false) // trusted ⇒ gated
	if err == nil {
		t.Fatal("the trust gate must block a no-checks-detected repo")
	}
	if !strings.Contains(err.Error(), "no checks detected") {
		t.Fatalf("expected a no-checks-detected block, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("nc1")) {
		t.Fatal("a gate refusal must not consume the approval")
	}
	_, _ = m.Teardown("nc1", true)
}

// TestMergeLocal_RequireVerdictRefusesMissingVerdict shows --require-verdict opts a
// non-trusted repo into the gate for one merge, and that gate fails closed when no
// verdict has been recorded — without consuming the approval.
func TestMergeLocal_RequireVerdictRefusesMissingVerdict(t *testing.T) {
	m, repo := deliveryHarness(t, "noverdict")
	task, err := m.Spawn("mv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	commitFeature(t, task.Worktree, "feature.txt", "new\n") // clean worktree, but no verdict
	if err := m.Approve("mv1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("mv1", true)
	if err == nil {
		t.Fatal("--require-verdict must refuse without a recorded verdict")
	}
	if !strings.Contains(err.Error(), "no valid review verdict") {
		t.Fatalf("expected a missing-verdict refusal, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("mv1")) {
		t.Fatal("a gate refusal must not consume the approval")
	}
	_, _ = m.Teardown("mv1", true)
}

// TestMergeLocal_GateRefusesBlockedVerdict: a High finding blocks the verdict, and a
// blocking verdict refuses the merge even when validate is green and the lead approved.
func TestMergeLocal_GateRefusesBlockedVerdict(t *testing.T) {
	m, repo := deliveryHarness(t, "blocked")
	commitGateScript(t, repo, "exit 0")
	task, err := m.Spawn("bv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("bv1"), head, map[string][]review.Finding{
		"correctness": {{Severity: review.SeverityHigh, Reviewer: "corr", Summary: "off-by-one in interest calc"}},
	})
	if _, err := m.TrustRecord("bv1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("bv1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("bv1", true)
	if err == nil {
		t.Fatal("a blocking verdict must refuse the merge")
	}
	if !strings.Contains(err.Error(), "not pass") {
		t.Fatalf("expected a blocked-verdict refusal, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("bv1")) {
		t.Fatal("a gate refusal must not consume the approval")
	}
	_, _ = m.Teardown("bv1", true)
}

// TestMergeLocal_GateRefusesStaleVerdictSha is the TOCTOU guard for the verdict pin: a
// commit that lands AFTER the verdict was recorded must not ride in, even when the lead
// re-approves the new commit (so the approval pin passes but the verdict pin catches it).
func TestMergeLocal_GateRefusesStaleVerdictSha(t *testing.T) {
	m, repo := deliveryHarness(t, "toctou")
	commitGateScript(t, repo, "exit 0")
	task, err := m.Spawn("tc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	sha1 := commitFeature(t, wt, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("tc1"), sha1, nil) // pass, pinned to sha1
	if _, err := m.TrustRecord("tc1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A commit lands after the verdict was recorded.
	sha2 := commitFeature(t, wt, "after.txt", "late\n")
	if sha1 == sha2 {
		t.Fatal("expected a new commit after review")
	}
	// The lead approves the NEW commit — the approval pin is satisfied, so the verdict
	// pin is what must reject the unreviewed commit.
	if err := m.Approve("tc1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("tc1", true)
	if err == nil {
		t.Fatal("a verdict pinned to a superseded commit must refuse the merge")
	}
	if !strings.Contains(err.Error(), "re-review and re-record") {
		t.Fatalf("expected a stale-verdict-sha refusal, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") == sha2 {
		t.Fatal("the unreviewed commit must not have merged")
	}
	_, _ = m.Teardown("tc1", true)
}

// TestMergeLocal_GateRefusesFailingValidate: a fresh validate that fails refuses the
// merge even with a pass verdict and a valid approval, and leaves the approval intact.
func TestMergeLocal_GateRefusesFailingValidate(t *testing.T) {
	m, repo := deliveryHarness(t, "redvalidate")
	commitGateScript(t, repo, "echo boom; exit 1") // the default-branch gate FAILS
	task, err := m.Spawn("rv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("rv1"), head, nil)
	if _, err := m.TrustRecord("rv1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("rv1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("rv1", true)
	if err == nil {
		t.Fatal("a failing fresh validate must refuse the merge even with a pass verdict")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("expected a failing-validate refusal, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("rv1")) {
		t.Fatal("a gate refusal must not consume the approval")
	}
	_, _ = m.Teardown("rv1", true)
}

// TestMergeLocal_RequireVerdictHumanApprover is the gated-but-human path: --require-verdict
// on a pr repo still needs the lead's explicit approval (no auto-mint), and a green
// gate then merges, recorded as approver=human.
func TestMergeLocal_RequireVerdictHumanApprover(t *testing.T) {
	m, repo := deliveryHarness(t, "reqhuman")
	commitGateScript(t, repo, "exit 0")
	task, err := m.Spawn("rh1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("rh1"), head, nil)
	if _, err := m.TrustRecord("rh1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if approval.Valid(m.P.ApprovalFile("rh1")) {
		t.Fatal("pr mode must not auto-mint even though the merge will be gated")
	}
	if err := m.Approve("rh1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("rh1", true); err != nil {
		t.Fatalf("require-verdict merge with a pass verdict + green validate should succeed: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded")
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "gate=verdict approver=human") {
		t.Fatalf("audit log should record a human-approved trust-gated merge: %s", b)
	}
	_, _ = m.Teardown("rh1", true)
}

// TestMergeLocal_GateUsesDefaultBranchScriptNotWorker is the lock-down guard: the gate's
// validation authority is the .ttorch/validate.sh on the DEFAULT BRANCH, not the worker's
// copy. The default branch's gate FAILS; the worker rewrites validate.sh on its own branch
// to pass; the gate must still run the default-branch (failing) script. A human approval
// isolates this from the gate-config-change rule (which only blocks AUTO merges).
func TestMergeLocal_GateUsesDefaultBranchScriptNotWorker(t *testing.T) {
	m, repo := deliveryHarness(t, "defbranch")
	commitGateScript(t, repo, "exit 1") // default-branch gate FAILS
	task, err := m.Spawn("db1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker tries to weaken its own gate by rewriting the script on its branch.
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "validate.sh"), []byte("exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "weaken gate")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("db1"), head, nil)
	if _, err := m.TrustRecord("db1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve("db1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("db1", true)
	if err == nil {
		t.Fatal("the gate must run the default-branch validate.sh (exit 1), not the worker's weakened copy")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("expected the default-branch script to fail the gate, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("db1")) {
		t.Fatal("a gate refusal must not consume the approval")
	}
	_, _ = m.Teardown("db1", true)
}

// TestMergeLocal_TrustedAutoMergeRefusesGateConfigChange: a worker's diff that touches the
// gate definition (.ttorch/validate.sh) must NOT auto-merge in trusted mode — changing the
// gate requires a human. With an explicit human approval the same change is allowed.
func TestMergeLocal_TrustedAutoMergeRefusesGateConfigChange(t *testing.T) {
	m, repo := deliveryHarness(t, "gateconfig")
	commitGateScript(t, repo, "exit 0") // a passing default-branch gate
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("gc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker's diff changes the gate definition itself.
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "validate.sh"), []byte("exit 0\n# tweaked by worker\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "edit gate")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("gc1"), head, nil)
	if _, err := m.TrustRecord("gc1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A diff touching the gate definition must NOT auto-approve.
	if approval.Valid(m.P.ApprovalFile("gc1")) {
		t.Fatal("a diff touching the gate definition must not auto-approve in trusted mode")
	}
	if reloaded, _ := m.Store.Load("gc1"); reloaded.ApprovedBy != "" {
		t.Fatalf("a gate-config change must not record an auto approver: %+v", reloaded)
	}
	// With an explicit human approval, the gate-config change may merge (a human reviewed it).
	if err := m.Approve("gc1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("gc1", false); err != nil {
		t.Fatalf("a human-approved gate-config change should merge: %v", err)
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "approver=human") {
		t.Fatalf("the human-approved gate-config merge must audit as approver=human: %s", b)
	}
	_, _ = m.Teardown("gc1", true)
}

// TestMergeLocal_RefusesAutoApprovalWhenGateInactive is the ungated-auto guard: an
// auto-minted approval is only valid through the active gate. If the gate goes inactive
// (the repo no longer reads as trusted, e.g. a degraded AGENTS.md), the auto token must
// not merge ungated — it fails closed instead of silently falling back to approval-only.
func TestMergeLocal_RefusesAutoApprovalWhenGateInactive(t *testing.T) {
	m, repo := deliveryHarness(t, "autoungated")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ag1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("ag1"), head, nil)
	if _, err := m.TrustRecord("ag1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !approval.Valid(m.P.ApprovalFile("ag1")) {
		t.Fatal("trusted record should auto-mint")
	}
	// The gate goes inactive (mode is no longer trusted / AGENTS.md degraded).
	if _, err := projectinit.Init(repo, "pr"); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("ag1", false) // not gated now
	if err == nil {
		t.Fatal("an auto-approval must not merge once the gate is inactive")
	}
	if !strings.Contains(err.Error(), "auto-approval") {
		t.Fatalf("expected an ungated-auto refusal, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("the auto-approved commit must not merge ungated")
	}
	_, _ = m.Teardown("ag1", true)
}

// TestMergeLocal_TrustedAbortsWhenAuditUnwritable: a trusted merge MUST be auditable. If
// the audit record cannot be persisted, the merge aborts BEFORE the fast-forward — an
// unrecorded finance merge is not acceptable.
func TestMergeLocal_TrustedAbortsWhenAuditUnwritable(t *testing.T) {
	m, repo := deliveryHarness(t, "auditfail")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("af1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("af1"), head, nil)
	if _, err := m.TrustRecord("af1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Make the audit log unwritable so the merge's required audit write fails. TrustRecord
	// already created the file, so make it read-only.
	if err := os.Chmod(m.P.AuditLog(), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(m.P.AuditLog(), 0o644) })

	_, err = m.MergeLocal("af1", false)
	if err == nil {
		t.Fatal("a trusted merge must abort when its audit record cannot be written")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Fatalf("expected an audit-write failure, got: %v", err)
	}
	// The merge must NOT have happened — no record, no state change.
	if gitIn(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("the default branch must not move when the audit write fails")
	}
	_, _ = m.Teardown("af1", true)
}

// TestTrustPrep_CommittedDiffDefeatsBenignWorktreeAttack is the headline guard for the
// reviewed-state == committed-state invariant. A worker commits a malicious HEAD, then
// reverts those bytes in the WORKING TREE so a worktree-vs-base diff would look benign.
// Prep must refuse the dirty worktree; and once clean, the reviewers' diff is the
// COMMITTED diff, which still exposes the malicious change — there is nowhere to hide it.
func TestTrustPrep_CommittedDiffDefeatsBenignWorktreeAttack(t *testing.T) {
	m, repo := deliveryHarness(t, "hideattack")
	task, err := m.Spawn("ha1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// A malicious COMMITTED head.
	if err := os.WriteFile(filepath.Join(wt, "payload.txt"), []byte("MALICIOUS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "looks innocent")
	// Revert the bytes in the working tree so a working-tree-vs-base diff shows nothing.
	if err := os.Remove(filepath.Join(wt, "payload.txt")); err != nil {
		t.Fatal(err)
	}

	// Prep refuses: reviewed state must equal the committed state that merges.
	if _, err := m.TrustPrep("ha1"); err == nil {
		t.Fatal("prep must refuse a dirty worktree so reviewers cannot be shown a benign tree while a malicious commit merges")
	}

	// Restore the worktree to match HEAD (clean again). The committed diff still exposes
	// the malicious change.
	gitIn(t, wt, "checkout", "--", ".")
	dir, err := m.TrustPrep("ha1")
	if err != nil {
		t.Fatalf("prep on a clean worktree should succeed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "diff.patch")); !strings.Contains(string(b), "MALICIOUS") {
		t.Fatalf("the reviewers' diff must be the committed diff exposing the change, got: %s", b)
	}
	_, _ = m.Teardown("ha1", true)
}

// TestMergeLocal_GateRefusesDirtyWorktreeAtMerge: even after an auto-mint, a live worker
// that dirties the worktree before the merge is refused at the gate (defense in depth on
// top of committed-object validation), and the refusal leaves the approval intact.
func TestMergeLocal_GateRefusesDirtyWorktreeAtMerge(t *testing.T) {
	m, repo := deliveryHarness(t, "dirtymerge")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("dm1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	head := commitFeature(t, wt, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("dm1"), head, nil)
	if _, err := m.TrustRecord("dm1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !approval.Valid(m.P.ApprovalFile("dm1")) {
		t.Fatal("expected an auto-mint over the clean, green worktree")
	}
	// A live worker dirties the worktree after review.
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("dm1", false)
	if err == nil {
		t.Fatal("a gated merge must refuse a dirty worktree")
	}
	if !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("expected a not-clean refusal, got: %v", err)
	}
	if !approval.Valid(m.P.ApprovalFile("dm1")) {
		t.Fatal("a recoverable refusal must not consume the approval")
	}
	_, _ = m.Teardown("dm1", true)
}

// TestMergeLocal_GateRefusesLegacyBareToken: a gated merge must fail closed on an approval
// token that carries no provenance (a legacy bare-sha token), because such a merge cannot
// be attributed (human vs auto) in the audit.
func TestMergeLocal_GateRefusesLegacyBareToken(t *testing.T) {
	m, repo := deliveryHarness(t, "baretoken")
	commitGateScript(t, repo, "exit 0")
	task, err := m.Spawn("lt1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("lt1"), head, nil)
	if _, err := m.TrustRecord("lt1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A legacy approval token carrying only a bare sha (no "human "/"auto " prefix).
	if err := approval.Grant(m.P.ApprovalFile("lt1"), time.Minute, head); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("lt1", true) // --require-verdict
	if err == nil {
		t.Fatal("a gated merge must fail closed on a token with no recorded provenance")
	}
	if !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("expected a provenance refusal, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("the unattributable merge must not have happened")
	}
	_, _ = m.Teardown("lt1", true)
}

// TestMergeLocal_TrustedAutoRequiresDefaultBranchScript mirrors the reproduced bypass:
// with NO .ttorch/validate.sh on the default branch, gateValidate would fall back to
// ecosystem detection on the worker's checkout — which the worker controls. Here the
// worker deletes go.mod and adds a package.json whose test is a no-op, alongside a broken
// payload. The trusted auto path must refuse (no auto-mint, merge refused) rather than
// trust the worker-defined check.
func TestMergeLocal_TrustedAutoRequiresDefaultBranchScript(t *testing.T) {
	m, repo := deliveryHarness(t, "noscriptauto")
	// Default branch has a go.mod but NO .ttorch/validate.sh.
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "go.mod")
	gitIn(t, repo, "commit", "-q", "-m", "add go.mod")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ns1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker swaps the ecosystem to a no-op test and slips in a broken payload.
	if err := os.Remove(filepath.Join(wt, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "package.json"), []byte(`{"scripts":{"test":"exit 0"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "payload.txt"), []byte("BROKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "swap ecosystem")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("ns1"), head, nil) // verdict passes

	if _, err := m.TrustRecord("ns1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// The bypass is closed: no default-branch gate script ⇒ NO auto-mint, even though the
	// worker's own package.json test would "pass".
	if approval.Valid(m.P.ApprovalFile("ns1")) {
		t.Fatal("a trusted auto-mint must require a default-branch .ttorch/validate.sh; worker-defined checks must never authorize")
	}
	// Auto-merge is refused.
	if _, err := m.MergeLocal("ns1", false); err == nil {
		t.Fatal("a trusted auto-merge must be refused without a default-branch gate script")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("the broken commit must not have auto-merged")
	}
	// Defense-in-depth: even a fabricated auto token is refused at the merge before any
	// worker-defined validation runs.
	if err := approval.Grant(m.P.ApprovalFile("ns1"), time.Minute, "auto "+head); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("ns1", false)
	if err == nil || !strings.Contains(err.Error(), "no .ttorch/validate.sh on the default branch") {
		t.Fatalf("an auto token without a default-branch script must be refused at the gate, got: %v", err)
	}
	_, _ = m.Teardown("ns1", true)
}

// TestMergeLocal_TrustedAutoRunsRealDefaultBranchScript is the companion: WITH a real
// default-branch gate script that actually tests, the broken payload is caught — the
// script governs over the worker's package.json no-op, so auto-mint is refused and a
// human-approved gated merge fails the real check too.
func TestMergeLocal_TrustedAutoRunsRealDefaultBranchScript(t *testing.T) {
	m, repo := deliveryHarness(t, "realscript")
	// A real default-branch gate that fails when the broken payload is present.
	commitGateScript(t, repo, "test ! -e payload.txt")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("rs1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker tries the same package.json no-op trick alongside the broken payload.
	if err := os.WriteFile(filepath.Join(wt, "package.json"), []byte(`{"scripts":{"test":"exit 0"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "payload.txt"), []byte("BROKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "broken payload")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("rs1"), head, nil)

	if _, err := m.TrustRecord("rs1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// The real default-branch script fails on the payload ⇒ not green ⇒ no auto-mint.
	if approval.Valid(m.P.ApprovalFile("rs1")) {
		t.Fatal("the real default-branch gate (which fails on the payload) must prevent auto-mint")
	}
	// Even a human-approved gated merge runs the real script and fails — the worker's
	// package.json no-op never governs.
	if err := m.Approve("rs1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = m.MergeLocal("rs1", false)
	if err == nil {
		t.Fatal("the real default-branch gate must fail on the broken payload")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("expected the real default-branch script to fail the gate, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("the broken commit must not have merged")
	}
	_, _ = m.Teardown("rs1", true)
}

func TestTrustRecord_TrustedBlocksHighFinding(t *testing.T) {
	m, repo := deliveryHarness(t, "trustblock")
	task, err := m.Spawn("tb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("tb1"), head, map[string][]review.Finding{
		"security": {{Severity: review.SeverityHigh, Reviewer: "sec", Summary: "secret in diff"}},
	})

	v, err := m.TrustRecord("tb1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a high finding must block, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("tb1")) {
		t.Fatal("a blocking verdict must not auto-mint, even in trusted mode")
	}
	reloaded, _ := m.Store.Load("tb1")
	if reloaded.GatePassed || reloaded.ApprovedBy != "" {
		t.Fatalf("blocked verdict must not record a passing/auto provenance: %+v", reloaded)
	}
	_, _ = m.Teardown("tb1", true)
}

// TestUninitNotice pins the read-only-on-spawn decision: an uninitialized git repo
// gets a notice naming the default delivery mode and how to persist one, while an
// already-initialized repo and a non-git path get none (so initialized repos behave
// exactly as before, with no spurious output). Neither case writes any file — the
// helper only reads.
func TestUninitNotice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	// Uninitialized git repo: a notice that defaults to pr and points at `ttorch init`.
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init").Run()
	msg := uninitNotice(repo)
	if msg == "" {
		t.Fatal("an uninitialized git repo should produce a notice")
	}
	for _, want := range []string{"delivery-mode=pr", "ttorch init"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("notice %q missing %q", msg, want)
		}
	}
	// The notice must be read-only: it does not create AGENTS.md or CLAUDE.md.
	if projectinit.Initialized(repo) {
		t.Fatal("uninitNotice must not initialize the repo")
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			t.Fatalf("uninitNotice must not create %s", f)
		}
	}

	// Already-initialized repo: no notice, behaves exactly as before.
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	if msg := uninitNotice(repo); msg != "" {
		t.Fatalf("an initialized repo should produce no notice, got %q", msg)
	}

	// Non-git directory: no notice, no panic.
	if msg := uninitNotice(t.TempDir()); msg != "" {
		t.Fatalf("a non-git path should produce no notice, got %q", msg)
	}
}

// TestInitRepo covers the opt-in write path (`ttorch init` / `ttorch spawn --init`):
// it sets the repo up and reports what it wrote, and is idempotent.
func TestInitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init").Run()

	m := New(paths.Default())
	notes, err := m.InitRepo(repo, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if !projectinit.Initialized(repo) {
		t.Fatal("InitRepo should set up the repo")
	}
	if !strings.Contains(strings.Join(notes, " "), "profile") {
		t.Fatalf("InitRepo should report the project profile write, got %v", notes)
	}

	// Idempotent: a second call leaves it initialized and does not error.
	if _, err := m.InitRepo(repo, "pr"); err != nil {
		t.Fatalf("second InitRepo: %v", err)
	}
	if !projectinit.Initialized(repo) {
		t.Fatal("repo should stay initialized after a second InitRepo")
	}

	// A path outside any git repo is an error, not a silent write.
	if _, err := m.InitRepo(t.TempDir(), "pr"); err == nil {
		t.Fatal("InitRepo should reject a non-git path")
	}
}

// TestSpawnDoesNotModifyTrackedFiles is the regression guard for the spawn-ux fix:
// dispatching a worker to an uninitialized repo must leave the lead's checkout
// untouched — no managed block, no AGENTS.md/CLAUDE.md, no tracked-file changes (the
// exact condition that previously blocked a clean fast-forward merge) — and the
// delivery mode falls back to the documented default of "pr".
func TestSpawnDoesNotModifyTrackedFiles(t *testing.T) {
	m, repo := deliveryHarness(t, "noinit")

	if projectinit.Initialized(repo) {
		t.Fatal("fresh repo should not be initialized")
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("fresh repo should be clean, got %q", st)
	}

	if _, err := m.Spawn("ni1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Teardown("ni1", true) })

	if projectinit.Initialized(repo) {
		t.Fatal("spawn must not initialize the repo")
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			t.Fatalf("spawn must not create %s in the lead's checkout", f)
		}
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("spawn must not change tracked files, git status: %q", st)
	}
	if mode := projectinit.ReadMode(repo); mode != "pr" {
		t.Fatalf("uninitialized repo should default to pr, got %q", mode)
	}
}

// TestSpawnLeavesInitializedRepoUnchanged is the companion guard: an already
// `ttorch init`'d repo behaves exactly as before — spawn reads the committed managed
// block and rewrites nothing, leaving the checkout byte-identical and clean.
func TestSpawnLeavesInitializedRepoUnchanged(t *testing.T) {
	m, repo := deliveryHarness(t, "preinit")

	if _, err := m.InitRepo(repo, "validated"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "ttorch init")
	before, err := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Spawn("pi1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Teardown("pi1", true) })

	after, err := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("spawn must not rewrite an already-initialized AGENTS.md")
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("spawn must leave an initialized repo clean, git status: %q", st)
	}
	if mode := projectinit.ReadMode(repo); mode != "validated" {
		t.Fatalf("ReadMode = %q, want validated (read from the committed block)", mode)
	}
}

func TestStopSession(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	session := fmt.Sprintf("ttorch-stop-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())
	if err := tmux.EnsureSession(m.Session); err != nil {
		t.Fatal(err)
	}
	if !tmux.HasSession(session) {
		t.Fatal("session should exist before stop")
	}
	if _, err := m.StopSession(); err != nil {
		t.Fatal(err)
	}
	if tmux.HasSession(session) {
		t.Fatal("session should be gone after StopSession")
	}
	// Stopping when nothing is running is a clean no-op.
	notes, err := m.StopSession()
	if err != nil || len(notes) == 0 {
		t.Fatalf("stop with no session: notes=%v err=%v", notes, err)
	}
}

// TestRestoreAndReset spawns a worker, kills the tmux session (simulating a stop/
// reboot), then asserts restore() rebuilds the manager and worker windows from
// saved state, and that Reset() clears the saved session.
func TestRestoreAndReset(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	repo := newRepoMain(t)
	session := fmt.Sprintf("ttorch-restore-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())

	// Save a manager record so restore rebuilds the manager window too.
	if err := m.Store.SaveManager(state.Manager{Dir: repo, SessionID: "mgr-sid"}); err != nil {
		t.Fatal(err)
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		t.Fatal(err)
	}
	if err := tmux.NewWindow(m.Session, "manager", repo); err != nil {
		t.Fatal(err)
	}

	task, err := m.Spawn("r1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	if task.SessionID == "" {
		t.Fatal("spawn should assign a session id")
	}
	if task.Window != "wk-r1" {
		t.Fatalf("window = %q, want wk-r1", task.Window)
	}

	// Simulate a stop / reboot: the tmux session is gone but state persists.
	if err := tmux.KillSession(m.Session); err != nil {
		t.Fatal(err)
	}
	if tmux.HasSession(session) {
		t.Fatal("session should be gone after kill")
	}

	notes := m.restore()
	t.Logf("restore notes: %v", notes)
	if !tmux.WindowExists(m.Session, "manager") {
		t.Fatal("restore should rebuild the manager window")
	}
	if !tmux.WindowExists(m.Session, "wk-r1") {
		t.Fatal("restore should rebuild the worker window")
	}

	// Reset clears the saved session (manager record + task records).
	if _, err := m.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Store.LoadManager(); ok {
		t.Fatal("Reset should remove the manager record")
	}
	if tasks, _ := m.Store.List(); len(tasks) != 0 {
		t.Fatalf("Reset should clear task records, got %d", len(tasks))
	}
	if tmux.HasSession(session) {
		t.Fatal("Reset should kill the tmux session")
	}
}

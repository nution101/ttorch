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
	if _, err := m.MergeLocal("d1"); err == nil {
		t.Fatal("merge-local must refuse without approval")
	}

	// Approve, then merge: the default branch fast-forwards to the worker's HEAD.
	if err := m.Approve("d1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("d1"); err != nil {
		t.Fatalf("merge-local: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != gitIn(t, wt, "rev-parse", "HEAD") {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	// Approval is single-use.
	if _, err := m.MergeLocal("d1"); err == nil {
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
	if _, err := m.MergeLocal("b1"); err == nil {
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
	if _, err := m.MergeLocal("b1"); err == nil {
		t.Fatal("merge should reject work that changed since approval")
	}
	if approval.Valid(m.P.ApprovalFile("b1")) {
		t.Fatal("a stale (commit-mismatched) approval should be consumed")
	}

	// Re-approve the current commit, then merge succeeds.
	if err := m.Approve("b1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("b1"); err != nil {
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
	if _, err := m.MergeLocal("p1"); err == nil {
		t.Fatal("pr-mode merge must still require an approval token")
	}
	// Identical to today: approve, then merge succeeds.
	if err := m.Approve("p1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("p1"); err != nil {
		t.Fatalf("approved pr-mode merge: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	_, _ = m.Teardown("p1", true)
}

// TestTrustRecord_TrustedIsInert proves the foundation is fully behavior-inert: even
// in trusted mode TrustRecord records a passing verdict and provenance but mints NO
// approval token and authorizes no merge. Auto-approve is deferred to the trust-gate
// commit; until then trusted behaves exactly like pr/local/validated.
func TestTrustRecord_TrustedIsInert(t *testing.T) {
	m, repo := deliveryHarness(t, "trusted")
	task, err := m.Spawn("tr1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	if projectinit.ReadMode(repo) != "trusted" {
		t.Fatal("repo should be in trusted mode for this test")
	}
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("tr1"), head, nil) // all clean → pass

	v, err := m.TrustRecord("tr1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("clean reports should pass, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("tr1")) {
		t.Fatal("trusted mode must NOT auto-mint an approval token in the foundation")
	}
	reloaded, _ := m.Store.Load("tr1")
	if !reloaded.GatePassed || reloaded.ReviewedSHA != head || reloaded.ApprovedBy != "" {
		t.Fatalf("provenance wrong: %+v", reloaded)
	}
	// Identical to every other mode: a verdict alone does not authorize a merge.
	if _, err := m.MergeLocal("tr1"); err == nil {
		t.Fatal("trusted-mode merge must still require an approval token in the foundation")
	}
	_, _ = m.Teardown("tr1", true)
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

func TestAutoInit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init").Run()

	if projectinit.Initialized(repo) {
		t.Fatal("fresh repo should not be initialized")
	}
	autoInit(repo)
	if !projectinit.Initialized(repo) {
		t.Fatal("autoInit should have set up the repo")
	}

	// Idempotent: a second call leaves it initialized and does not error.
	autoInit(repo)
	if !projectinit.Initialized(repo) {
		t.Fatal("repo should stay initialized after a second autoInit")
	}

	// Opt-out: a fresh repo with TTORCH_NO_AUTOINIT set is left untouched.
	other := t.TempDir()
	exec.Command("git", "-C", other, "init").Run()
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	autoInit(other)
	if projectinit.Initialized(other) {
		t.Fatal("TTORCH_NO_AUTOINIT should skip auto-init")
	}

	// Non-git directory is a no-op (and never panics).
	t.Setenv("TTORCH_NO_AUTOINIT", "")
	autoInit(t.TempDir())
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

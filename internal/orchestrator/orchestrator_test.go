package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// TestMain disables native worker terminal views for the whole package. The
// integration tests below exercise Spawn, which best-effort opens a terminal tab
// or window on macOS; without this, running these tests would spawn real GUI
// windows on a developer's Mac.
func TestMain(m *testing.M) {
	os.Setenv("TTORCH_WORKER_TABS", "off")
	// Safety net: point TTORCH_HOME at a throwaway dir for the whole package so a
	// test that forgets its own t.Setenv can never resolve the DB to the real
	// ~/.ttorch — where db.Open would create state.db and ImportLegacy would rename
	// the live state/ dir away (a session-wiping data-loss bug). Per-test
	// t.Setenv("TTORCH_HOME", t.TempDir()) still overrides this for isolation; the
	// db.Open guard is the final fail-closed backstop.
	home, err := os.MkdirTemp("", "ttorch-orchestrator-test-home-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("TTORCH_HOME", home)
	// StateDB() honors TTORCH_DB ahead of TTORCH_HOME, so an inherited TTORCH_DB — as a
	// ttorch-managed worktree exports, pointing at the real ~/.ttorch/state.db — would
	// otherwise redirect the DB back into the real home despite the pin above (the db.Open
	// guard then fails the run). Clear it so TTORCH_HOME and each test's own
	// t.Setenv("TTORCH_HOME", ...) fully govern where state resolves.
	os.Unsetenv("TTORCH_DB")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
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

// TestComputeConflicts pins the pure overlap core: which live workers a proposed
// footprint collides with, independent of tmux. Disjoint footprints conflict with
// nobody; a worker that declared no footprint is exempt; an overlap names the
// worker and the colliding paths.
func TestComputeConflicts(t *testing.T) {
	tasks := []db.Task{
		{ID: "a", Window: "wk-a", Project: "/repo", Footprint: []string{"internal/cli"}},
		{ID: "b", Window: "wk-b", Project: "/repo", Footprint: []string{"internal/orchestrator"}},
		{ID: "c", Window: "wk-c", Project: "/repo"}, // declared nothing -> exempt
	}

	// Disjoint from every footprint.
	if got := computeConflicts([]string{"docs"}, tasks); len(got) != 0 {
		t.Fatalf("disjoint footprint should conflict with nobody, got %v", got)
	}
	// A footprint that declared nothing never conflicts, even against a path under it.
	if got := computeConflicts([]string{"internal/state"}, tasks); len(got) != 0 {
		t.Fatalf("footprint hitting only the undeclared worker should not conflict, got %v", got)
	}
	// Overlap: a prefix of worker a's footprint names a (and only a).
	got := computeConflicts([]string{"internal/cli/cli.go"}, tasks)
	if len(got) != 1 || got[0].TaskID != "a" {
		t.Fatalf("expected one conflict with task a, got %v", got)
	}
	if got[0].OverlapString() != "internal/cli/cli.go↔internal/cli" {
		t.Fatalf("overlap string = %q, want internal/cli/cli.go↔internal/cli", got[0].OverlapString())
	}
}

// TestFootprintCandidate pins the pure pre-liveness filter: only footprint-declaring
// ship/scout tasks in the requested repo (and not the excluded id) are eligible for
// overlap. This is the repo-scoping + cc-exclusion guarantee, tested without tmux.
func TestFootprintCandidate(t *testing.T) {
	base := db.Task{ID: "x", Kind: "ship", Project: "/repo", Footprint: []string{"internal/cli"}}
	cases := []struct {
		name          string
		task          db.Task
		repo, exclude string
		want          bool
	}{
		{"in-repo declarer", base, "/repo", "", true},
		{"unscoped matches any repo", base, "", "", true},
		{"other repo excluded by scope", base, "/other", "", false},
		{"excluded id", base, "/repo", "x", false},
		{"cc session excluded", db.Task{ID: "c", Kind: "cc", Project: "/repo", Footprint: []string{"internal/cli"}}, "/repo", "", false},
		{"no footprint excluded", db.Task{ID: "n", Kind: "ship", Project: "/repo"}, "/repo", "", false},
	}
	for _, c := range cases {
		if got := footprintCandidate(c.task, c.repo, c.exclude); got != c.want {
			t.Errorf("%s: footprintCandidate = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSpawn_RefusesFootprintOverlap proves the deterministic conflict gate: once a
// live worker holds a footprint, a second spawn onto overlapping files is refused
// (naming the conflict) with no side effects, a disjoint footprint is allowed, and
// --force-overlap overrides. It exercises the real runtime against tmux + git.
func TestSpawn_RefusesFootprintOverlap(t *testing.T) {
	m, repo := deliveryHarness(t, "overlap")

	if _, err := m.SpawnWithFootprint("a1", repo, false, "sleep 30", []string{"internal/cli"}, false); err != nil {
		t.Fatalf("first spawn: %v", err)
	}

	// An overlapping footprint is refused, names the conflicting task, and leaves
	// no task record or window behind.
	_, err := m.SpawnWithFootprint("b1", repo, false, "sleep 30", []string{"internal/cli/cli.go"}, false)
	if err == nil {
		t.Fatal("a spawn onto a live worker's files must be refused")
	}
	if !strings.Contains(err.Error(), "a1") {
		t.Fatalf("refusal should name the conflicting task a1, got %q", err)
	}
	if _, ok, _ := m.Store.GetTask(context.Background(), "b1"); ok {
		t.Fatal("a refused spawn must persist no task record")
	}
	if tmux.WindowExists(m.Session, "wk-b1") {
		t.Fatal("a refused spawn must not leave a window behind")
	}

	// A disjoint footprint dispatches fine.
	if _, err := m.SpawnWithFootprint("c1", repo, false, "sleep 30", []string{"internal/orchestrator"}, false); err != nil {
		t.Fatalf("disjoint spawn should succeed: %v", err)
	}

	// --force-overlap overrides the refusal and records the footprint anyway.
	d, err := m.SpawnWithFootprint("d1", repo, false, "sleep 30", []string{"internal/cli"}, true)
	if err != nil {
		t.Fatalf("forced overlapping spawn should succeed: %v", err)
	}
	if strings.Join(d.Footprint, ",") != "internal/cli" {
		t.Fatalf("forced spawn should record its footprint, got %v", d.Footprint)
	}

	for _, id := range []string{"a1", "c1", "d1"} {
		_, _ = m.Teardown(id, true)
	}
}

// TestSpawn_RefusedOverlapDoesNotAutoInit proves auto-init runs only for a spawn that
// actually proceeds: a spawn refused by the overlap gate must leave the lead's checkout
// untouched, the read-only-on-refusal invariant auto-init must not regress. The first
// worker is spawned with auto-init opted out so the repo stays uninitialized; the opt-out
// is then cleared, so the ONLY thing that could initialize the repo is the refused second
// spawn's auto-init — which must not run because the overlap gate returns first.
func TestSpawn_RefusedOverlapDoesNotAutoInit(t *testing.T) {
	m, repo := deliveryHarness(t, "refuseautoinit")

	// First worker holds a footprint; opt out so it does NOT auto-init the repo.
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	if _, err := m.SpawnWithFootprint("a1", repo, false, "sleep 30", []string{"internal/cli"}, false); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Teardown("a1", true) })
	if projectinit.Initialized(repo) {
		t.Fatal("opted-out first spawn must not initialize the repo")
	}

	// Clear the opt-out: now a pre-gate auto-init on the refused spawn WOULD write.
	t.Setenv("TTORCH_NO_AUTOINIT", "")
	_, err := m.SpawnWithFootprint("b1", repo, false, "sleep 30", []string{"internal/cli/cli.go"}, false)
	if err == nil {
		t.Fatal("an overlapping spawn must be refused")
	}

	// The refused spawn must have left the checkout untouched: no auto-init, no files.
	if projectinit.Initialized(repo) {
		t.Fatal("a refused spawn must not auto-init the repo (auto-init must run after the refusal gates)")
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			t.Fatalf("a refused spawn must not write %s", f)
		}
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("a refused spawn must leave the checkout clean, got %q", st)
	}
}

// TestCheckOverlap_OnlyLiveWorkers proves the conflict gate is gated on liveness:
// a torn-down worker's footprint no longer blocks, so a freed slot is reusable.
func TestCheckOverlap_OnlyLiveWorkers(t *testing.T) {
	m, repo := deliveryHarness(t, "live")
	// CheckOverlap scopes by canonicalized repo, matching the stored task.Project
	// (both go through worktree.RepoRoot); use task.Project so the test is immune to
	// the /var -> /private/var symlink on macOS.
	a, err := m.SpawnWithFootprint("a1", repo, false, "sleep 30", []string{"internal/cli"}, false)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got, err := m.CheckOverlap(a.Project, []string{"internal/cli"}); err != nil || len(got) != 1 {
		t.Fatalf("a live worker on internal/cli should conflict, got %v (err %v)", got, err)
	}
	if _, err := m.Teardown("a1", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if got, err := m.CheckOverlap(a.Project, []string{"internal/cli"}); err != nil || len(got) != 0 {
		t.Fatalf("a torn-down worker must no longer conflict, got %v (err %v)", got, err)
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
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

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

// TestSpawn_WaitsForLaunchBeforeReturning proves Spawn hands back a window that is
// running the launched command, not a bare shell that would silently drop a brief.
//
// It polls for the command to take over rather than sampling the foreground once. A
// worker's login shell sources its startup files before it runs the (type-ahead-
// buffered) command, and waitForLaunch returns on the FIRST non-shell foreground it
// sees; on a machine whose shell startup briefly forks a non-shell helper (e.g. a
// version-manager shim like fnm) that is not in the shell denylist, waitForLaunch can
// return on that helper a moment before `sleep` is foreground. A single un-retried
// read then races that startup window and reds the suite intermittently. Waiting for
// the command to actually take over still catches the real failure — a bare shell that
// never runs the command leaves the foreground a shell until the poll times out below —
// without coupling the assertion to one transient instant.
func TestSpawn_WaitsForLaunchBeforeReturning(t *testing.T) {
	m, repo := deliveryHarness(t, "ready")
	task, err := m.Spawn("r1", repo, false, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	var cmd string
	for i := 0; i < 100; i++ { // up to ~2s, well past a login shell's startup
		if cmd = tmux.PaneCurrentCommand(m.Session, task.Window); cmd == "sleep" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cmd != "sleep" {
		t.Fatalf("the worker command never took over the pane: foreground = %q, want %q", cmd, "sleep")
	}
	_, _ = m.Teardown("r1", true)
}

// TestSpawn_FailsLoudlyWhenCommandNeverStarts proves a launch that never takes over
// the pane fails the spawn loudly and unwinds — no phantom window, no recorded task —
// rather than handing back a worker that would silently drop the manager's brief.
func TestSpawn_FailsLoudlyWhenCommandNeverStarts(t *testing.T) {
	m, repo := deliveryHarness(t, "nostart")
	origTimeout, origInterval := spawnReadyTimeout, spawnReadyInterval
	spawnReadyTimeout, spawnReadyInterval = 600*time.Millisecond, 50*time.Millisecond
	defer func() { spawnReadyTimeout, spawnReadyInterval = origTimeout, origInterval }()

	// ":" is a shell no-op: it returns to the prompt at once, so the window stays a
	// bare shell and the launch is never observed to start.
	if _, err := m.Spawn("n1", repo, false, ":"); err == nil {
		t.Fatal("Spawn must fail loudly when the launched command never takes over the pane")
	}
	if tasks, _ := m.Status(); len(tasks) != 0 {
		t.Fatalf("a failed spawn must persist no task, got %d", len(tasks))
	}
	if tmux.WindowExists(m.Session, "wk-n1") {
		t.Fatal("a failed spawn must not leave a phantom window behind")
	}
}

// TestSpawn_StartsOnFreshTaskBranch proves a spawned worker starts on a fresh
// per-task branch off the default branch, never a reused worktree's prior branch.
func TestSpawn_StartsOnFreshTaskBranch(t *testing.T) {
	m, repo := deliveryHarness(t, "branch")
	task, err := m.Spawn("b1", repo, false, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if br := gitIn(t, task.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); br != "ttorch/b1" {
		t.Fatalf("worker should start on the fresh task branch ttorch/b1, got %q", br)
	}
	_, _ = m.Teardown("b1", true)
}

// TestSend_FailsLoudlyWhenWindowGone proves a message to a worker whose window has
// gone away fails loudly instead of vanishing into a dead target.
func TestSend_FailsLoudlyWhenWindowGone(t *testing.T) {
	m, repo := deliveryHarness(t, "send")
	task, err := m.Spawn("s1", repo, false, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmux.KillWindow(m.Session, task.Window); err != nil {
		t.Fatalf("kill window: %v", err)
	}
	if err := m.Send("s1", "hello"); err == nil {
		t.Fatal("Send must fail loudly when the worker has no live window")
	}
	_, _ = m.Teardown("s1", true)
}

// TestSend_FailsLoudlyWhenTaskUnknown proves a send to a task that was never
// recorded returns a non-nil error (mapped to a non-zero exit by the CLI) rather
// than silently doing nothing. Hermetic: it never reaches tmux.
func TestSend_FailsLoudlyWhenTaskUnknown(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := &Manager{Session: "ttorch-unknown", Store: store}
	if err := m.Send("ghost", "hi"); err == nil {
		t.Fatal("Send to an unknown task must return an error")
	}
}

// fakeTmux installs a stub `tmux` on PATH for the duration of the test. It answers
// list-windows with the given window name (so a task looks live) and appends every
// send-keys invocation's argv — one element per line — to the returned log file.
// This exercises the real delivery path hermetically, with no real tmux and no
// shell between ttorch and the recorded argv, which is exactly what proves a
// message is handed to `send-keys -l` verbatim.
func fakeTmux(t *testing.T, window string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "send.log")
	script := `#!/bin/sh
case "$1" in
  list-windows) printf '%s\n' "$TTORCH_FAKE_WINDOW"; exit 0 ;;
  send-keys) printf '%s\n' "$@" >> "$TTORCH_FAKE_LOG"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTORCH_FAKE_WINDOW", window)
	t.Setenv("TTORCH_FAKE_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestSend_DeliversMessageVerbatim proves a message dense with shell-significant
// characters reaches `send-keys -l` as a single, untouched argv element — the
// guarantee that closes the silent-failure hole where a caller's shell would
// otherwise re-interpret backticks or $(...).
func TestSend_DeliversMessageVerbatim(t *testing.T) {
	const msg = "build `whoami` && echo $(id) $HOME \"dq\" 'sq' <tag> a|b; c > d < e"
	if strings.Contains(msg, "\n") {
		t.Fatal("test message must be single-line for line-based capture")
	}
	log := fakeTmux(t, "wk-v1")
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := &Manager{Session: "ttorch-verbatim", Store: store}
	// Seed the task (and its project, for the FK) so Send resolves window "wk-v1".
	proj, err := store.UpsertProject(context.Background(), "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(context.Background(),
		db.Task{ID: "v1", ProjectID: proj.ID, Window: "wk-v1", Status: db.StatusActive}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("v1", msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var got string
	found := false
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == "-l" {
			got, found = lines[i+1], true
			break
		}
	}
	if !found {
		t.Fatalf("no `send-keys -l` recorded; fake tmux log:\n%s", b)
	}
	if got != msg {
		t.Fatalf("delivered message = %q, want %q (verbatim)", got, msg)
	}
}

// TestRestoreRefreshesWindowGoneAnchor proves the resume path closes the supervisor boundary:
// when restore() rebuilds a worker window in place, it appends the SAME 'spawned' sign-of-life
// event the spawn path emits, so a window_gone recorded BEFORE the resume is no longer the
// worker's latest anchor and the supervisor (db.ReclaimWindowGone) does NOT reclaim the live
// resumed worker — while a worker that dies AFTER the resume (a later window_gone) is still
// reclaimed. Hermetic via fakeTmux — the fake reports only the "manager" window as present, so
// restore skips the manager rebuild and rebuilds the (absent) worker window, exercising the
// real path.
func TestRestoreRefreshesWindowGoneAnchor(t *testing.T) {
	fakeTmux(t, "manager")
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	m := &Manager{Session: "ttorch-restore", Store: store}

	proj, err := store.UpsertProject(ctx, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	// An active worker that was spawned, then had its window confirmed gone (a crash, or a
	// `ttorch stop`). Without the resume refresh, this stale window_gone would still read as the
	// latest signal and the supervisor would reclaim the worker the instant resume revived it.
	// The worktree must exist so restore rebuilds the window rather than skipping it.
	if _, err := store.CreateTask(ctx, db.Task{
		ID: "r1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:r1", Window: "wk-r1", Worktree: t.TempDir(),
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "r1", Type: db.EventSpawned, Actor: db.ActorManager,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "r1", Type: db.EventWindowGone,
		Actor: db.ActorSystem, Actionable: true, Payload: "wk-r1",
	}); err != nil {
		t.Fatal(err)
	}

	notes := m.restore()
	restored := false
	for _, n := range notes {
		if n == "restored r1" {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("restore did not rebuild the worker window; notes=%v", notes)
	}

	// restore must append a 'spawned' sign-of-life event tagged as a resume — the precise append
	// that refreshes the anchor (the setup's pre-resume 'spawned' carries no such payload).
	evs, err := store.EventsSince(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	resumeSignOfLife := false
	for _, e := range evs {
		if e.EntityID == "r1" && e.Type == db.EventSpawned && strings.HasPrefix(e.Payload, "resume ") {
			resumeSignOfLife = true
		}
	}
	if !resumeSignOfLife {
		t.Fatalf("restore must append a 'spawned' resume sign-of-life event for r1; events=%+v", evs)
	}

	// AC1: a freshly-resumed worker is NOT reclaimed — the resume's sign-of-life outranks the
	// stale window_gone in the freshness anchor.
	out, err := store.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("a freshly-resumed worker must not be reclaimed, got %+v", out)
	}
	if tk, _, _ := store.GetTask(ctx, "r1"); tk.Status != db.StatusActive {
		t.Errorf("status = %q, want still active (the resumed worker is live)", tk.Status)
	}

	// AC2: a worker that genuinely dies AFTER the resume (a later window_gone, newer than the
	// resume sign-of-life) IS still reclaimed — the refresh does not mask a real crash.
	if _, err := store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "r1", Type: db.EventWindowGone,
		Actor: db.ActorSystem, Actionable: true, Payload: "wk-r1",
	}); err != nil {
		t.Fatal(err)
	}
	out, err = store.ReclaimWindowGone(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TaskID != "r1" {
		t.Fatalf("a worker that died after resume must be reclaimed, got %+v", out)
	}
	if tk, _, _ := store.GetTask(ctx, "r1"); tk.Status != db.StatusPending {
		t.Errorf("status = %q, want pending (reclaimed after the post-resume crash)", tk.Status)
	}
}

// TestRestoreRefreshesLeaseAnchor proves the resume path closes the supervisor's OTHER recovery
// boundary: the lease-expiry sweep (db.ReclaimExpiredLeases). That sweep is purely
// timestamp-based — it reads no sign-of-life event (unlike ReclaimWindowGone) — so the
// window-gone anchor refresh alone does not protect a resumed worker whose lease lapsed during
// a long pause. restore() must therefore extend the lease itself. A worker rebuilt in place
// (L1) has its still-held but lapsed lease pushed forward, so the sweep skips it; a worker that
// restore did NOT rebuild because its window still lingered while it stopped heartbeating (L2 —
// a worker that hung/died after an earlier resume) keeps its lapsed lease and is reclaimed. One
// ReclaimExpiredLeases call proves both: refreshed-worker-not-reclaimed and
// died-after-resume-still-reclaimed.
//
// Hermetic via fakeTmux: the fake reports "manager" and L2's window as present (so restore skips
// both) while L1's window is absent (so restore rebuilds it, exercising the real refresh path).
// Real-clock safe via past-stamped expiries (no clock seam needed, mirroring the scheduler
// tests' addExpiredLeaseWorker) — a refreshed lease lands at now+DefaultLeaseDuration, far
// enough ahead that the immediate sweep sees it unexpired.
func TestRestoreRefreshesLeaseAnchor(t *testing.T) {
	fakeTmux(t, "manager")
	// Report BOTH "manager" and L2's window as present (newline-joined): restore skips a worker
	// whose window already exists, so it rebuilds — and refreshes — only L1.
	t.Setenv("TTORCH_FAKE_WINDOW", "manager\nwk-L2")
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	m := &Manager{Session: "ttorch-restore-lease", Store: store}

	proj, err := store.UpsertProject(ctx, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	// L1: an active worker whose lease lapsed during the pause, window now absent, worktree
	// present — the resume-in-place case. Without the refresh, restore would revive it yet the
	// next lease sweep would reclaim the live worker the instant it came back.
	if _, err := store.CreateTask(ctx, db.Task{
		ID: "L1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:L1", Window: "wk-L1", Worktree: t.TempDir(),
		LeaseOwner: "worker:L1", LeaseExpiresAt: &past,
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	// L2: an active worker that was resumed earlier (its window still lingers) but then stopped
	// heartbeating — its lease lapsed. restore skips it (window present), so the lease sweep must
	// still reclaim it: the refresh must not globally disable recovery.
	if _, err := store.CreateTask(ctx, db.Task{
		ID: "L2", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip,
		Owner: "worker:L2", Window: "wk-L2", Worktree: t.TempDir(),
		LeaseOwner: "worker:L2", LeaseExpiresAt: &past,
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}

	notes := m.restore()
	restoredL1, touchedL2 := false, false
	for _, n := range notes {
		if n == "restored L1" {
			restoredL1 = true
		}
		if n == "restored L2" {
			touchedL2 = true
		}
	}
	if !restoredL1 {
		t.Fatalf("restore did not rebuild the resumed worker L1; notes=%v", notes)
	}
	if touchedL2 {
		t.Fatalf("restore must skip L2 (its window still exists), but rebuilt it; notes=%v", notes)
	}

	// restore must have pushed L1's lease forward (the worker is live again); L2's lapsed lease
	// must be untouched (restore never rebuilt it).
	l1, _, err := store.GetTask(ctx, "L1")
	if err != nil {
		t.Fatal(err)
	}
	if l1.LeaseExpiresAt == nil || !l1.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("L1 lease_expires_at = %v, want refreshed into the future", l1.LeaseExpiresAt)
	}
	l2, _, err := store.GetTask(ctx, "L2")
	if err != nil {
		t.Fatal(err)
	}
	if l2.LeaseExpiresAt == nil || l2.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("L2 lease_expires_at = %v, want still lapsed (restore must not touch a skipped worker)", l2.LeaseExpiresAt)
	}

	// The sweep: AC1 — the refreshed worker L1 is NOT reclaimed; AC2 — the died-after-resume
	// worker L2 IS reclaimed (back to pending for re-dispatch).
	out, err := store.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].TaskID != "L2" {
		t.Fatalf("lease sweep must reclaim only L2, got %+v", out)
	}
	if tk, _, _ := store.GetTask(ctx, "L1"); tk.Status != db.StatusActive {
		t.Errorf("L1 status = %q, want still active (the resumed worker is live)", tk.Status)
	}
	if tk, _, _ := store.GetTask(ctx, "L2"); tk.Status != db.StatusPending {
		t.Errorf("L2 status = %q, want pending (reclaimed after it died post-resume)", tk.Status)
	}
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
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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
	reloaded, _, _ := m.Store.GetTask(context.Background(), "s9")
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
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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
	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, repo
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

// TestTrustPrep_ScalesReviewers: prep scales the persisted reviewer set to the diff size
// — a docs-only change records the reduced {correctness, scope} (no security), a
// multi-file code change records the full three — and ReviewersFor reads back exactly what
// prep wrote, so dispatch and aggregation agree.
func TestTrustPrep_ScalesReviewers(t *testing.T) {
	cases := []struct {
		id    string
		files map[string]string
		want  string
	}{
		{
			id:    "docs",
			files: map[string]string{"NOTES.md": "# notes\nsome prose\n"},
			want:  "correctness scope",
		},
		{
			id:    "code",
			files: map[string]string{"a.go": "package a\n", "b.go": "package b\n"},
			want:  "correctness scope security",
		},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			m, repo := deliveryHarness(t, "scale-"+tc.id)
			task, err := m.Spawn(tc.id, repo, false, "sleep 60")
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(task.Worktree, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitIn(t, task.Worktree, "add", "-A")
			gitIn(t, task.Worktree, "commit", "-q", "-m", "work")

			if _, err := m.TrustPrep(tc.id); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(m.ReviewersFor(tc.id), " "); got != tc.want {
				t.Fatalf("ReviewersFor = %q, want %q", got, tc.want)
			}
			_, _ = m.Teardown(tc.id, true)
		})
	}
}

// TestTrustPrep_QuotedCodeFilenameKeepsSecurity reproduces the gate-bypass end to end: a
// worker pairs an innocuous README edit with a code file whose non-ASCII name git QUOTES
// in the patch body. Classifying off the patch text would drop the quoted path and misread
// the diff as docs-only, dropping the security reviewer. Sourcing the file list from
// `git diff --name-only -z` (unquoted) keeps the change classified as code, so prep records
// the full set including security.
func TestTrustPrep_QuotedCodeFilenameKeepsSecurity(t *testing.T) {
	m, repo := deliveryHarness(t, "quoted")
	task, err := m.Spawn("q1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// Innocuous docs edit + a code file with a non-ASCII name git quotes in the patch.
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("# readme\nharmless\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "café.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "work")

	if _, err := m.TrustPrep("q1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.ReviewersFor("q1"), " "); got != "correctness scope security" {
		t.Fatalf("a diff with a quoted code file must force the full set incl. security, got %q", got)
	}
	_, _ = m.Teardown("q1", true)
}

// TestReviewersFor_FailsSafeToFull: with no prep record (or a malformed one), ReviewersFor
// returns the full set, so a verdict can never be recorded against fewer reviewers than
// were prepared.
func TestReviewersFor_FailsSafeToFull(t *testing.T) {
	m, _ := deliveryHarness(t, "failsafe")
	full := "correctness scope security"

	// No reviewers.json at all.
	if got := strings.Join(m.ReviewersFor("nope"), " "); got != full {
		t.Fatalf("missing record: ReviewersFor = %q, want %q", got, full)
	}

	// A malformed record also falls back to full.
	dir := m.P.ReviewInputsDir("bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reviewers.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.ReviewersFor("bad"), " "); got != full {
		t.Fatalf("malformed record: ReviewersFor = %q, want %q", got, full)
	}
}

// TestTrustPrep_RefusesStaleBase: when the default branch has advanced past the worker's
// base (the worker's HEAD lacks commits the default now has), trust prep must FAIL with a
// rebase message and stage NOTHING — never run the reviewers against a stale-base diff
// whose phantom reverts of the default's lead waste a full review pass.
func TestTrustPrep_RefusesStaleBase(t *testing.T) {
	m, repo := deliveryHarness(t, "stalebase")
	task, err := m.Spawn("sb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	// The worker commits its own change on the base it was cut from.
	_ = commitFeature(t, task.Worktree, "worker.txt", "worker change\n")
	// Meanwhile the default branch advances with an INDEPENDENT commit the worker lacks.
	if err := os.WriteFile(filepath.Join(repo, "main_lead.txt"), []byte("lead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "default advances")

	_, err = m.TrustPrep("sb1")
	if err == nil {
		t.Fatal("trust prep must refuse a stale-base branch")
	}
	if msg := err.Error(); !strings.Contains(msg, "stale") || !strings.Contains(msg, "rebase") {
		t.Fatalf("stale-base refusal must name the staleness and tell the manager to rebase, got: %v", err)
	}
	// Nothing staged: the guard runs before any inputs are materialized.
	if _, statErr := os.Stat(filepath.Join(m.P.ReviewInputsDir("sb1"), "diff.patch")); statErr == nil {
		t.Fatal("a refused stale-base prep must NOT stage diff.patch")
	}
	_, _ = m.Teardown("sb1", true)
}

// TestTrustPrep_UpToDateAfterRebaseSucceeds: once the worker rebases onto the advanced
// default, its base is current again, so prep succeeds — and the staged diff carries ONLY
// the worker's own change, never the default's lead (no phantom reverts).
func TestTrustPrep_UpToDateAfterRebaseSucceeds(t *testing.T) {
	m, repo := deliveryHarness(t, "rebased")
	task, err := m.Spawn("rb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	_ = commitFeature(t, wt, "worker.txt", "worker change\n")
	// Advance the default, then rebase the worker onto it so its base is current.
	if err := os.WriteFile(filepath.Join(repo, "main_lead.txt"), []byte("lead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "default advances")
	gitIn(t, wt, "rebase", "main")

	dir, err := m.TrustPrep("rb1")
	if err != nil {
		t.Fatalf("prep on an up-to-date (rebased) branch should succeed: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "diff.patch"))
	if !strings.Contains(string(b), "worker.txt") {
		t.Fatalf("staged diff must contain the worker's own change: %s", b)
	}
	if strings.Contains(string(b), "main_lead.txt") {
		t.Fatalf("staged diff must NOT contain the default's lead (no phantom revert): %s", b)
	}
	_, _ = m.Teardown("rb1", true)
}

// TestTrustPrep_ReviewDiffIsThreeDotNoPhantomReverts is the headline diff-form guard: the
// trust gate stages the three-dot (merge-base) diff, so even against a default that has
// advanced it shows ONLY the branch's own changes — never the default's lead as phantom
// reverts. It exercises the diff computation directly because the stale-base guard refuses
// to stage in this state; the two-dot diff is asserted as the contrast it replaced.
func TestTrustPrep_ReviewDiffIsThreeDotNoPhantomReverts(t *testing.T) {
	m, repo := deliveryHarness(t, "threedot")
	task, err := m.Spawn("td1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	head := commitFeature(t, wt, "worker.txt", "worker change\n")
	// The default advances with a commit the worker's HEAD lacks (a stale base).
	if err := os.WriteFile(filepath.Join(repo, "main_lead.txt"), []byte("lead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "default advances")

	// Three-dot (what the gate stages): only the branch's own change.
	threeDot, err := mergeBaseDiff(wt, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(threeDot, "worker.txt") {
		t.Fatalf("three-dot diff must contain the branch's change: %s", threeDot)
	}
	if strings.Contains(threeDot, "main_lead.txt") {
		t.Fatalf("three-dot diff must NOT surface the default's lead as a phantom revert: %s", threeDot)
	}
	// The two-dot form it replaced WOULD surface the default's lead as a phantom revert —
	// the exact bug (cosign-strict / liveness-dwell) this change closes.
	if twoDot := gitIn(t, wt, "diff", "main", head); !strings.Contains(twoDot, "main_lead.txt") {
		t.Fatalf("sanity: the old two-dot diff should have shown the phantom revert, got: %s", twoDot)
	}
	_, _ = m.Teardown("td1", true)
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
	reloaded, _, _ := m.Store.GetTask(context.Background(), "p1")
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
	reloaded, _, _ := m.Store.GetTask(context.Background(), "ta1")
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
	if reloaded, _, _ := m.Store.GetTask(context.Background(), "ho1"); reloaded.ApprovedBy != "auto" {
		t.Fatalf("expected an auto-mint first, got ApprovedBy=%q", reloaded.ApprovedBy)
	}
	// ...but the lead then explicitly approves the same commit, which must take over the
	// token's provenance so the merge is attributed to a human, not the AI gate.
	if err := m.Approve("ho1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if reloaded, _, _ := m.Store.GetTask(context.Background(), "ho1"); reloaded.ApprovedBy != "human" {
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
	if reloaded, _, _ := m.Store.GetTask(context.Background(), "nc1"); reloaded.ApprovedBy != "" {
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
	if reloaded, _, _ := m.Store.GetTask(context.Background(), "gc1"); reloaded.ApprovedBy != "" {
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
	reloaded, _, _ := m.Store.GetTask(context.Background(), "tb1")
	if reloaded.GatePassed || reloaded.ApprovedBy != "" {
		t.Fatalf("blocked verdict must not record a passing/auto provenance: %+v", reloaded)
	}
	_, _ = m.Teardown("tb1", true)
}

// TestUninitNotice pins the nudge helper auto-init falls back to when it declines to
// write (a repo that tracks AGENTS.md/CLAUDE.md): an uninitialized git repo gets a
// notice naming the default delivery mode and how to persist one, while an already-
// initialized repo and a non-git path get none. Neither case writes any file — the
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
	t.Setenv("TTORCH_HOME", t.TempDir())
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init").Run()

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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

// TestSpawnAutoInitsUntrackedRepo proves the restored auto-init for the common case: a
// repo that does NOT commit AGENTS.md gets set up on first spawn (managed block +
// CLAUDE.md symlink + profile) so a worker always has AGENTS.md to read — and crucially,
// the writes are UNTRACKED, so the checkout has no tracked-file changes and merge-local's
// HasTrackedChanges gate stays clear. Mode reads from the freshly written block.
func TestSpawnAutoInitsUntrackedRepo(t *testing.T) {
	m, repo := deliveryHarness(t, "autoinit")

	if projectinit.Initialized(repo) {
		t.Fatal("fresh repo should not be initialized")
	}

	if _, err := m.Spawn("ai1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Teardown("ai1", true) })

	if !projectinit.Initialized(repo) {
		t.Fatal("spawn should auto-init an untracked repo")
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("spawn should create %s in the lead's checkout: %v", f, err)
		}
	}
	// The whole point: auto-init's writes are untracked, so the merge-local clean gate
	// (HasTrackedChanges) stays clear. The new files show only as untracked.
	if changed, err := worktree.HasTrackedChanges(repo); err != nil || changed {
		t.Fatalf("auto-init must not produce tracked-file changes (err=%v changed=%v); status: %q",
			err, changed, gitIn(t, repo, "status", "--porcelain"))
	}
	if mode := projectinit.ReadMode(repo); mode != "pr" {
		t.Fatalf("auto-init writes delivery-mode=pr, got %q", mode)
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

// newRepoWithTrackedAgents builds a git repo that COMMITS a hand-written AGENTS.md (so
// AGENTS.md is git-tracked) but has never been `ttorch init`'d. This is the exact shape
// that broke merge-local before a848c1c: the original auto-init injected the managed
// block into this tracked file, dirtying the working tree. It returns the repo path and
// the committed AGENTS.md bytes.
func newRepoWithTrackedAgents(t *testing.T) (string, []byte) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main", "-q")
	body := []byte("# Project guidance\n\nhand-written, committed by the developer.\n")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init with tracked AGENTS.md")
	return repo, body
}

// TestAutoInit_UntrackedRepo is the common-case proof, tmux-free: a repo that does NOT
// commit AGENTS.md gets fully set up by auto-init (managed block + CLAUDE.md + profile),
// and because those writes are untracked, the checkout has no tracked-file changes — so
// merge-local's HasTrackedChanges gate stays clear.
func TestAutoInit_UntrackedRepo(t *testing.T) {
	repo := newRepoMain(t) // commits f.txt only; no AGENTS.md

	autoInit(repo)

	if !projectinit.Initialized(repo) {
		t.Fatal("auto-init should set up a repo that does not track AGENTS.md")
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("auto-init should create %s: %v", f, err)
		}
	}
	if mode := projectinit.ReadMode(repo); mode != "pr" {
		t.Fatalf("auto-init writes delivery-mode=pr, got %q", mode)
	}
	// The merge-local gate: tracked changes must be empty (the new files are untracked).
	if changed, err := worktree.HasTrackedChanges(repo); err != nil || changed {
		t.Fatalf("auto-init must not produce tracked-file changes (err=%v changed=%v); status: %q",
			err, changed, gitIn(t, repo, "status", "--porcelain"))
	}
}

// TestAutoInit_DeclinesOnTrackedAGENTS is the a848c1c regression proof, tmux-free and the
// heart of this change: auto-init MUST NOT leave a state that blocks merge-local. On a
// repo that tracks AGENTS.md, auto-init declines to write (it would dirty the tracked
// file), leaving AGENTS.md byte-identical and the checkout with NO tracked-file changes —
// i.e. worktree.HasTrackedChanges is false, the exact predicate MergeLocal gates on.
func TestAutoInit_DeclinesOnTrackedAGENTS(t *testing.T) {
	repo, before := newRepoWithTrackedAgents(t)

	autoInit(repo)

	if projectinit.Initialized(repo) {
		t.Fatal("auto-init must decline on a tracked AGENTS.md, not inject the managed block")
	}
	after, err := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("auto-init must not modify a tracked AGENTS.md\n before: %q\n after:  %q", before, after)
	}
	// The regression itself: tracked changes must be empty, so merge-local would not be
	// blocked with "repo has uncommitted changes to tracked files".
	if changed, err := worktree.HasTrackedChanges(repo); err != nil || changed {
		t.Fatalf("auto-init left tracked-file changes — the exact a848c1c regression (err=%v changed=%v); status: %q",
			err, changed, gitIn(t, repo, "status", "--porcelain"))
	}
	// And it declined cleanly: it did not even leave an untracked CLAUDE.md behind.
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("auto-init declined but left the checkout dirty: %q", st)
	}
}

// TestAutoInit_DeclinesOnTrackedCLAUDE pins the OTHER arm of the tracksConventionFile OR:
// a repo that tracks CLAUDE.md (but not AGENTS.md) must also be declined, because
// projectinit.Init's ensureSymlink would replace the tracked CLAUDE.md and dirty the tree.
func TestAutoInit_DeclinesOnTrackedCLAUDE(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# hand-written CLAUDE.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init with tracked CLAUDE.md")

	autoInit(repo)

	if projectinit.Initialized(repo) {
		t.Fatal("auto-init must decline when CLAUDE.md is tracked, even with no AGENTS.md")
	}
	if changed, err := worktree.HasTrackedChanges(repo); err != nil || changed {
		t.Fatalf("auto-init left tracked-file changes on a tracked CLAUDE.md (err=%v changed=%v); status: %q",
			err, changed, gitIn(t, repo, "status", "--porcelain"))
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("auto-init declined but left the checkout dirty: %q", st)
	}
}

// TestAutoInit_NoOpsAndOptOut covers the remaining contract: TTORCH_NO_AUTOINIT=1 is a
// full opt-out even on a writable repo; a non-git path is a silent no-op; and a second
// call on an already-initialized repo is idempotent.
func TestAutoInit_NoOpsAndOptOut(t *testing.T) {
	// Opt-out: a repo auto-init would otherwise set up is left untouched.
	optOut := newRepoMain(t)
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	autoInit(optOut)
	if projectinit.Initialized(optOut) {
		t.Fatal("TTORCH_NO_AUTOINIT=1 must disable auto-init")
	}
	if st := gitIn(t, optOut, "status", "--porcelain"); st != "" {
		t.Fatalf("opt-out must leave the checkout clean, got %q", st)
	}

	// Non-git path: no panic, no write — the directory stays empty and uninitialized.
	nonGit := t.TempDir()
	autoInit(nonGit)
	if projectinit.Initialized(nonGit) {
		t.Fatal("auto-init must not initialize a non-git path")
	}
	if entries, err := os.ReadDir(nonGit); err != nil || len(entries) != 0 {
		t.Fatalf("auto-init must not write into a non-git path (err=%v entries=%d)", err, len(entries))
	}

	// Idempotent: with the opt-out cleared, the first call initializes and the second is a
	// no-op (Initialized short-circuits before any write).
	t.Setenv("TTORCH_NO_AUTOINIT", "")
	repo := newRepoMain(t)
	autoInit(repo)
	if !projectinit.Initialized(repo) {
		t.Fatal("auto-init should have initialized the repo once opt-out is cleared")
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "ttorch init")
	before := gitIn(t, repo, "rev-parse", "HEAD")
	autoInit(repo)
	if after := gitIn(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("second auto-init must be a no-op (HEAD %s -> %s)", before, after)
	}
	if st := gitIn(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("second auto-init must leave the checkout clean, got %q", st)
	}
}

// TestAutoInit_DoesNotTouchRealTtorchHome is the test-isolation proof: TestMain pins
// TTORCH_HOME at a throwaway dir, so a representative auto-init plus Manager construction
// resolves all ttorch state under the temp home and never creates or mutates the real
// ~/.ttorch — guarding `make test` against clobbering a developer's live session.
func TestAutoInit_DoesNotTouchRealTtorchHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	realTtorch := filepath.Join(home, ".ttorch")

	// The package-wide pin must be in effect: the resolved ttorch home is NOT the real one.
	if got := paths.Default().Home; got == realTtorch {
		t.Fatalf("test isolation broken: resolved TTORCH_HOME %q is the real ~/.ttorch", got)
	}

	// Snapshot the real ~/.ttorch (it may or may not exist on a given machine).
	snapshot := func() (bool, []string) {
		entries, rderr := os.ReadDir(realTtorch)
		if rderr != nil {
			return false, nil
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return true, names
	}
	existedBefore, before := snapshot()

	// A representative operation: auto-init a fresh repo and build a Manager (db.Open).
	t.Setenv("TTORCH_HOME", t.TempDir())
	repo := newRepoMain(t)
	autoInit(repo)
	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// The actual write target — the SQLite DB — must resolve outside the real home too.
	// StateDB() prefers TTORCH_DB over TTORCH_HOME, so this also pins that an inherited
	// TTORCH_DB cannot redirect a `make test` run into ~/.ttorch (TestMain clears it).
	if db := m.P.StateDB(); strings.HasPrefix(db, realTtorch+string(os.PathSeparator)) || db == realTtorch {
		t.Fatalf("test isolation broken: state DB %q resolves under the real ~/.ttorch", db)
	}

	existsAfter, after := snapshot()
	if !existedBefore && existsAfter {
		t.Fatalf("a ttorch operation created the real ~/.ttorch at %q", realTtorch)
	}
	if existedBefore && strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("the real ~/.ttorch changed during tests:\n before: %v\n after:  %v", before, after)
	}
}

// TestSpawnAutoInit_DoesNotBlockMergeLocal drives the regression end to end through the
// real runtime: on a repo that tracks AGENTS.md, a worker spawns (auto-init declines, so
// the checkout stays clean), commits a change, the lead approves, and merge-local MUST
// fast-forward — the precise flow the original auto-init broke. Requires tmux + git.
func TestSpawnAutoInit_DoesNotBlockMergeLocal(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	repo, before := newRepoWithTrackedAgents(t)
	session := fmt.Sprintf("ttorch-aimerge-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })
	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	task, err := m.Spawn("aim1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Teardown("aim1", true) })

	// Auto-init declined on the tracked AGENTS.md: byte-identical, clean checkout.
	if projectinit.Initialized(repo) {
		t.Fatal("auto-init must decline on a tracked AGENTS.md")
	}
	if after, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md")); string(before) != string(after) {
		t.Fatal("auto-init must not rewrite a tracked AGENTS.md")
	}
	if changed, err := worktree.HasTrackedChanges(repo); err != nil || changed {
		t.Fatalf("spawn left tracked-file changes (a848c1c regression): err=%v changed=%v status=%q",
			err, changed, gitIn(t, repo, "status", "--porcelain"))
	}

	// The worker commits; the lead approves; merge-local must fast-forward cleanly.
	wt := task.Worktree
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "add feature")
	if err := m.Approve("aim1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("aim1", false); err != nil {
		t.Fatalf("merge-local must succeed after auto-init on a tracked-AGENTS.md repo (a848c1c regression): %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != gitIn(t, wt, "rev-parse", "HEAD") {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
}

func TestStopSession(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	session := fmt.Sprintf("ttorch-stop-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", session)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// Save a manager record so restore rebuilds the manager window too.
	if err := m.Store.SetManager(context.Background(), db.Manager{Dir: repo, SessionID: "mgr-sid"}); err != nil {
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
	if _, ok, _ := m.Store.GetManager(context.Background()); ok {
		t.Fatal("Reset should remove the manager record")
	}
	if tasks, _ := m.Store.ListTasks(context.Background(), db.TaskFilter{}); len(tasks) != 0 {
		t.Fatalf("Reset should clear task records, got %d", len(tasks))
	}
	if tmux.HasSession(session) {
		t.Fatal("Reset should kill the tmux session")
	}
}

// --- ttorch land ---

// TestLand_CleanLocalMode is the happy path: a worker already current with the default
// branch, a passing gate, and a valid approval lands in one command — the default branch
// fast-forwards to the worker's commit and the deliverable is present in the checkout.
func TestLand_CleanLocalMode(t *testing.T) {
	m, repo := deliveryHarness(t, "landclean")
	commitGateScript(t, repo, "exit 0") // passing gate on the default branch
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("l1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if err := m.Approve("l1", time.Minute); err != nil {
		t.Fatal(err)
	}

	out, err := m.Land("l1", false)
	if err != nil {
		t.Fatalf("clean land should succeed: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded to the worker HEAD")
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("the deliverable did not land on the default-branch checkout: %v", err)
	}
	if !strings.Contains(out, "landed l1") || !strings.Contains(out, "verified") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	// The approval is single-use, consumed by the merge inside land.
	if approval.Valid(m.P.ApprovalFile("l1")) {
		t.Fatal("the approval must be consumed by the land merge")
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "land task=l1") {
		t.Fatalf("audit log missing the land record: %s", b)
	}
	_, _ = m.Teardown("l1", true)
}

// TestLand_RebaseConflictAborts: when the worker's commit and the default branch edit the
// same lines, land must ABORT the rebase (real overlap), restore the worktree, and never
// touch the default branch — rather than blind-merging a conflicting/far-behind diff.
func TestLand_RebaseConflictAborts(t *testing.T) {
	m, repo := deliveryHarness(t, "landconflict")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("c1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// Worker edits f.txt one way...
	head := commitFeature(t, wt, "f.txt", "worker change\n")
	// ...meanwhile the default branch makes a CONFLICTING edit to the same file.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("default change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "f.txt")
	gitIn(t, repo, "commit", "-q", "-m", "default edits f.txt")
	if err := m.Approve("c1", time.Minute); err != nil {
		t.Fatal(err)
	}

	defBefore := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := m.Land("c1", false); err == nil {
		t.Fatal("land must abort on a rebase conflict")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected a conflict error, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defBefore {
		t.Fatal("default branch must not move on a rebase-conflict abort")
	}
	if gitIn(t, wt, "rev-parse", "HEAD") != head {
		t.Fatal("the worker HEAD must be restored after the aborted rebase")
	}
	if dirty, _ := worktreeIsDirty(t, wt); dirty {
		t.Fatal("the worktree must be clean after the rebase abort (no half-applied conflict)")
	}
	_, _ = m.Teardown("c1", true)
}

// TestLand_ValidateRedAborts: land re-runs the validation gate on the rebased tree and a
// red result aborts BEFORE integration — the default branch never moves and the approval
// is left intact (the merge gate was never reached).
func TestLand_ValidateRedAborts(t *testing.T) {
	m, repo := deliveryHarness(t, "landred")
	commitGateScript(t, repo, "exit 1") // gate fails on the default branch
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("r1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if err := m.Approve("r1", time.Minute); err != nil {
		t.Fatal(err)
	}

	defBefore := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := m.Land("r1", false); err == nil {
		t.Fatal("land must abort when validation is red")
	} else if !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("expected a validate failure, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defBefore {
		t.Fatal("default branch must not move when validation is red")
	}
	if !approval.Valid(m.P.ApprovalFile("r1")) {
		t.Fatal("a pre-merge validate abort must leave the approval intact")
	}
	_, _ = m.Teardown("r1", true)
}

// TestLand_PostMergeVerifyMismatchAborts: a clean local fast-forward can never land a tree
// different from the validated commit, so we inject a faulty integrator (as a botched
// squash or a concurrent base move would behave) and assert land's post-merge verify
// catches the drift and raises a loud, file-naming alarm.
func TestLand_PostMergeVerifyMismatchAborts(t *testing.T) {
	m, repo := deliveryHarness(t, "landmismatch")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("v1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if err := m.Approve("v1", time.Minute); err != nil {
		t.Fatal(err)
	}

	// Substitute a faulty integrator that advances the default branch to a DIFFERENT
	// commit than the validated one.
	orig := landIntegrate
	t.Cleanup(func() { landIntegrate = orig })
	landIntegrate = func(_ *Manager, _ db.Task, _ string, _ bool, _ string) (string, error) {
		if err := os.WriteFile(filepath.Join(repo, "tampered.txt"), []byte("x\n"), 0o644); err != nil {
			return "", err
		}
		gitIn(t, repo, "add", "tampered.txt")
		gitIn(t, repo, "commit", "-q", "-m", "tampered landing")
		return gitIn(t, repo, "rev-parse", "HEAD"), nil
	}

	_, err = m.Land("v1", false)
	if err == nil {
		t.Fatal("land must abort when the landed tip does not match the validated commit")
	}
	if !strings.Contains(err.Error(), "POST-MERGE VERIFY FAILED") {
		t.Fatalf("expected a post-merge verify alarm, got: %v", err)
	}
	if !strings.Contains(err.Error(), "feature.txt") || !strings.Contains(err.Error(), "tampered.txt") {
		t.Fatalf("the alarm should name the drifting files, got: %v", err)
	}
	_, _ = m.Teardown("v1", true)
}

// TestVerifyLanded exercises the strict (local fast-forward) post-merge tripwire directly
// (no tmux): an identical tip verifies, a divergent one fails and names every drifting file.
func TestVerifyLanded(t *testing.T) {
	repo := newRepoMain(t)
	base := gitIn(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "feature")
	rebasedHead := gitIn(t, repo, "rev-parse", "HEAD")

	if got, err := verifyLanded(repo, "main", base, rebasedHead, true); err != nil || got != rebasedHead {
		t.Fatalf("identical tip must verify: got %q err %v", got, err)
	}

	// Advance the default to a divergent tip: drop feature.txt, add other.txt.
	gitIn(t, repo, "reset", "--hard", "-q", base)
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "other")
	_, err := verifyLanded(repo, "main", base, rebasedHead, true)
	if err == nil {
		t.Fatal("a divergent tip must fail strict post-merge verify")
	}
	if !strings.Contains(err.Error(), "POST-MERGE VERIFY FAILED") {
		t.Fatalf("got: %v", err)
	}
	if !strings.Contains(err.Error(), "feature.txt") || !strings.Contains(err.Error(), "other.txt") {
		t.Fatalf("the alarm should name every drifting file, got: %v", err)
	}
}

// TestVerifyLanded_PRModeToleratesConcurrentBaseMove covers the non-strict (PR) branch: a
// base that legitimately advanced (a concurrent file landed alongside the worker's) passes,
// because the worker's OWN files landed verbatim — but a merge that alters a worker file fails.
func TestVerifyLanded_PRModeToleratesConcurrentBaseMove(t *testing.T) {
	repo := newRepoMain(t)
	base := gitIn(t, repo, "rev-parse", "HEAD")
	// The validated commit: base + the worker's feature.txt.
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "worker feature")
	rebasedHead := gitIn(t, repo, "rev-parse", "HEAD")

	// The landed tip: the worker's feature.txt verbatim PLUS a concurrent other.txt — as a
	// PR merging onto an advanced base would produce. Worker files intact ⇒ verify passes.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("concurrent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent landing")
	landedOK := gitIn(t, repo, "rev-parse", "HEAD")
	if got, err := verifyLanded(repo, "main", base, rebasedHead, false); err != nil || got != landedOK {
		t.Fatalf("a concurrent base move that keeps worker files verbatim must verify: got %q err %v", got, err)
	}

	// Now a merge that ALTERS the worker's own file must fail even in non-strict mode.
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "tamper worker file")
	_, err := verifyLanded(repo, "main", base, rebasedHead, false)
	if err == nil {
		t.Fatal("a merge that alters a worker file must fail PR-mode verify")
	}
	if !strings.Contains(err.Error(), "POST-MERGE VERIFY FAILED") || !strings.Contains(err.Error(), "feature.txt") {
		t.Fatalf("the alarm should name the altered worker file, got: %v", err)
	}
}

// TestLand_RebaseMovedRequiresReapproval is the headline correctness case: when the default
// branch advances after approval, land rebases the worker (changing its sha), so the prior
// approval no longer covers what would merge. Land must refuse loudly WITHOUT consuming the
// approval or moving the default — and once the lead approves the rebased commit, a re-run
// lands it cleanly with both the concurrent change and the worker's change present.
func TestLand_RebaseMovedRequiresReapproval(t *testing.T) {
	m, repo := deliveryHarness(t, "landmoved")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("m1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "feature.txt", "new\n")
	if err := m.Approve("m1", time.Minute); err != nil { // pinned to the pre-rebase sha
		t.Fatal(err)
	}
	// The default branch advances non-conflictingly (a different file) after approval.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent landing")
	defBefore := gitIn(t, repo, "rev-parse", "HEAD")

	// First land: the rebase moves the worker's sha, so the stale approval no longer covers
	// it — refuse loudly, leave the default untouched, and keep the approval intact.
	_, err = m.Land("m1", false)
	if err == nil {
		t.Fatal("land must refuse when the rebase moved the worker past the approved commit")
	}
	if !strings.Contains(err.Error(), "no valid approval covers the rebased commit") {
		t.Fatalf("expected a re-approve instruction, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defBefore {
		t.Fatal("the default branch must not move when the rebased commit is unapproved")
	}
	if !approval.Valid(m.P.ApprovalFile("m1")) {
		t.Fatal("the stale approval must NOT be consumed by the refusal")
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker HEAD onto the advanced default")
	}

	// The lead reviews the rebased diff and approves the rebased commit; the re-run lands.
	if err := m.Approve("m1", time.Minute); err != nil {
		t.Fatal(err)
	}
	out, err := m.Land("m1", false)
	if err != nil {
		t.Fatalf("land should succeed after approving the rebased commit: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != rebased {
		t.Fatal("the default branch must fast-forward to the rebased commit")
	}
	// The re-run's rebase is a no-op (the first land already rebased the worktree), so the
	// summary reads "worker already current"; what matters is the verified landing.
	if !strings.Contains(out, "landed m1") || !strings.Contains(out, "verified") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	for _, f := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should be present on the landed default branch: %v", f, err)
		}
	}
	_, _ = m.Teardown("m1", true)
}

// TestLand_FastLandCarriesVerdictOnCleanRebase is the headline verdict-portable fast-land
// case: in trusted mode a worker is reviewed and auto-approved, then the default advances with
// an unrelated (disjoint) commit. Land rebases the worker onto the advanced default — moving
// its commit sha — but the worker's own three-dot diff is byte-identical to what the reviewers
// cleared, so the verdict is carried forward (re-pinned to the rebased commit) and the land
// completes in ONE command WITHOUT re-running trust prep or the reviewers.
func TestLand_FastLandCarriesVerdictOnCleanRebase(t *testing.T) {
	m, repo := deliveryHarness(t, "fastland")
	commitGateScript(t, repo, "exit 0") // passing gate on the default branch
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("fl1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "feature.txt", "new\n")
	writeReviewReports(t, m.P.ReviewInputsDir("fl1"), feat, nil) // clean → pass
	if _, err := m.TrustRecord("fl1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// The verdict pins both the reviewed commit AND its diff content identity; the trusted gate
	// auto-minted the approval.
	if v, ok := m.TrustShow("fl1"); !ok || v.ReviewedSHA != feat || v.DiffID == "" {
		t.Fatalf("recorded verdict should pin the reviewed sha and a diff identity: %+v ok=%v", v, ok)
	}
	if !approval.Valid(m.P.ApprovalFile("fl1")) {
		t.Fatal("trusted pass + green validate should auto-mint an approval")
	}

	// The default branch advances non-conflictingly (a DIFFERENT file) after review.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("concurrent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "concurrent landing")

	// One land: the rebase moves the worker's sha, but its diff is unchanged, so the verdict is
	// carried forward and the land succeeds with no manual re-gate.
	out, err := m.Land("fl1", false)
	if err != nil {
		t.Fatalf("fast-land should carry the verdict over a clean rebase: %v", err)
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker onto the advanced default")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != rebased {
		t.Fatal("the default branch must fast-forward to the rebased commit")
	}
	if !strings.Contains(out, "landed fl1") || !strings.Contains(out, "verified") {
		t.Fatalf("unexpected land summary: %q", out)
	}
	for _, f := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should be present on the landed default branch: %v", f, err)
		}
	}
	// The audit proves the verdict was CARRIED (not re-reviewed) and that the merge still went
	// through the trust gate as an auto-approved verdict merge.
	auditLog, _ := os.ReadFile(m.P.AuditLog())
	if want := fmt.Sprintf("fast-land task=fl1 carried verdict %s->%s", short(feat), short(rebased)); !strings.Contains(string(auditLog), want) {
		t.Fatalf("audit log missing the fast-land carry record (%q):\n%s", want, auditLog)
	}
	if !strings.Contains(string(auditLog), "gate=verdict approver=auto") {
		t.Fatalf("the carried-forward merge must still record a gated auto-merge: %s", auditLog)
	}
	// trust-record ran exactly once (at setup) — the fast-land did NOT re-gate the task.
	if n := strings.Count(string(auditLog), "trust-record task=fl1"); n != 1 {
		t.Fatalf("fast-land must not re-record the verdict; trust-record count = %d:\n%s", n, auditLog)
	}
	// Both tokens are consumed by the merge.
	if _, ok := m.TrustShow("fl1"); ok {
		t.Fatal("the carried verdict must be consumed by the merge")
	}
	if approval.Valid(m.P.ApprovalFile("fl1")) {
		t.Fatal("the carried approval must be consumed by the merge")
	}
	_, _ = m.Teardown("fl1", true)
}

// TestLand_FastLandRefusesOnChangedContent is the trust-boundary counterpart: when the rebase
// is NOT clean/disjoint — the default advanced by editing the SAME file the worker touched, so
// the worker's own three-dot diff changes after the rebase — the recorded verdict must NOT be
// carried onto the changed content. Land refuses with a re-gate instruction, leaves the default
// branch untouched, and leaves the verdict pinned to the original (now stale) commit.
func TestLand_FastLandRefusesOnChangedContent(t *testing.T) {
	m, repo := deliveryHarness(t, "fastlandchanged")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	// A shared, multi-line file both the worker and the default will edit — on DIFFERENT lines,
	// so the rebase auto-merges (no conflict) yet the worker's three-dot diff still changes.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "shared.txt")
	gitIn(t, repo, "commit", "-q", "-m", "seed shared.txt")

	task, err := m.Spawn("fc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	feat := commitFeature(t, wt, "shared.txt", "l1\nl2\nl3\nl4\nl5-worker\n") // worker edits the last line
	writeReviewReports(t, m.P.ReviewInputsDir("fc1"), feat, nil)
	if _, err := m.TrustRecord("fc1", "", time.Minute); err != nil {
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

	_, err = m.Land("fc1", false)
	if err == nil {
		t.Fatal("fast-land must refuse to carry a verdict onto changed content")
	}
	if !strings.Contains(err.Error(), "changed its reviewed diff") || !strings.Contains(err.Error(), "trust prep") {
		t.Fatalf("expected a re-gate instruction naming the changed diff, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defBefore {
		t.Fatal("the default branch must not move when the verdict cannot be carried")
	}
	rebased := gitIn(t, wt, "rev-parse", "HEAD")
	if rebased == feat {
		t.Fatal("the rebase should have moved the worker onto the advanced default")
	}
	// The verdict was NOT carried: it still pins the ORIGINAL reviewed commit, not the rebased one.
	if v, ok := m.TrustShow("fc1"); !ok || v.ReviewedSHA != feat {
		t.Fatalf("the verdict must remain pinned to the original commit (not carried): %+v ok=%v", v, ok)
	}
	_, _ = m.Teardown("fc1", true)
}

// TestLand_PRModeRejectsRequireVerdict: --require-verdict has no local merge to gate in pr
// mode, so it is rejected loudly rather than silently dropped.
func TestLand_PRModeRejectsRequireVerdict(t *testing.T) {
	m, repo := deliveryHarness(t, "landprrv")
	if _, err := projectinit.Init(repo, "pr"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("p1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if _, err := m.Land("p1", true); err == nil {
		t.Fatal("pr mode must reject --require-verdict")
	} else if !strings.Contains(err.Error(), "--require-verdict applies to") {
		t.Fatalf("got: %v", err)
	}
	_, _ = m.Teardown("p1", true)
}

// TestLand_PRModeRequiresOrigin: pr delivery has nowhere to push without an origin remote,
// so land refuses up front rather than failing deep in the gh flow.
func TestLand_PRModeRequiresOrigin(t *testing.T) {
	m, repo := deliveryHarness(t, "landprorigin")
	if _, err := projectinit.Init(repo, "pr"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("p2", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if _, err := m.Land("p2", false); err == nil {
		t.Fatal("pr mode must refuse a repo with no origin remote")
	} else if !strings.Contains(err.Error(), "no 'origin' remote") {
		t.Fatalf("got: %v", err)
	}
	_, _ = m.Teardown("p2", true)
}

// TestLand_RefusesUninitializedRepo: land must not guess a delivery mode — an uninitialized
// repo is refused with an actionable message rather than silently routed to pr. Auto-init
// is disabled here so spawn leaves the repo uninitialized (its whole point); the refusal
// being tested is Land's, not auto-init's.
func TestLand_RefusesUninitializedRepo(t *testing.T) {
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	m, repo := deliveryHarness(t, "landuninit")
	task, err := m.Spawn("u1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	_ = commitFeature(t, task.Worktree, "feature.txt", "new\n")
	_, err = m.Land("u1", false)
	if err == nil {
		t.Fatal("land must refuse an uninitialized repo")
	}
	if !strings.Contains(err.Error(), "no ttorch delivery mode configured") {
		t.Fatalf("expected an init-required message, got: %v", err)
	}
	_, _ = m.Teardown("u1", true)
}

// worktreeIsDirty reports whether path has any pending change (tracked or untracked).
func worktreeIsDirty(t *testing.T, path string) (bool, error) {
	t.Helper()
	out := gitIn(t, path, "status", "--porcelain")
	return strings.TrimSpace(out) != "", nil
}

// --- ttorch security-review (the security-everywhere advisory pass) ---

// writeSecurityReport drops ONLY the security reviewer's report into dir (the manager ran
// just the security reviewer, not the full three-dimension gate), as the
// security-everywhere pass expects.
func writeSecurityReport(t *testing.T, dir, sha string, findings []review.Finding) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(review.Report{Dimension: review.DimensionSecurity, ReviewedSHA: sha, Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "security.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSecurityReview_AdvisoryAndIndependentOfTrustGate is the headline guard: a security
// audit recorded in a non-trusted repo folds the security reviewer's report into a
// verdict, but is purely ADVISORY — it never mints an approval, never writes the trust
// gate's verdict file, and never touches the task's gate state. Only the security
// reviewer's report need be present (the manager ran just that one reviewer).
func TestSecurityReview_AdvisoryAndIndependentOfTrustGate(t *testing.T) {
	m, repo := deliveryHarness(t, "secrev")
	task, err := m.Spawn("sv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	writeSecurityReport(t, m.P.ReviewInputsDir("sv1"), head, nil) // clean
	v, err := m.SecurityReview("sv1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("a clean security report should pass, got %q", v.Overall)
	}
	// Advisory: no approval minted, and the trust gate's durable verdict is untouched.
	if approval.Valid(m.P.ApprovalFile("sv1")) {
		t.Fatal("security-review must NOT mint an approval token")
	}
	if _, ok := m.TrustShow("sv1"); ok {
		t.Fatal("security-review must NOT write the trust gate's verdict")
	}
	// Task gate state is untouched (the advisory pass is side-effect-free on it).
	reloaded, _, _ := m.Store.GetTask(context.Background(), "sv1")
	if reloaded.GatePassed || reloaded.ReviewedSHA != "" || reloaded.ApprovedBy != "" {
		t.Fatalf("security-review must not touch the task's gate state: %+v", reloaded)
	}
	// SecurityReviewShow returns the recorded advisory verdict.
	got, ok := m.SecurityReviewShow("sv1")
	if !ok || got.ReviewedSHA != head {
		t.Fatalf("SecurityReviewShow = %+v ok=%v, want the recorded verdict pinned to %s", got, ok, head)
	}
	_, _ = m.Teardown("sv1", true)
}

// TestSecurityReview_BlockingFindingStaysAdvisory pins that a high security finding yields
// a "block" verdict for the manager to surface, yet — outside the trusted gate — it does
// NOT block delivery: the normal approval-gated merge still proceeds. The audit's hard
// block remains exclusively the trusted gate's job.
func TestSecurityReview_BlockingFindingStaysAdvisory(t *testing.T) {
	m, repo := deliveryHarness(t, "secblock")
	task, err := m.Spawn("sb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	writeSecurityReport(t, m.P.ReviewInputsDir("sb1"), head, []review.Finding{
		{Severity: review.SeverityHigh, Reviewer: "ttorch-reviewer-security", Summary: "leaked key in fixture"},
	})
	v, err := m.SecurityReview("sb1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a high finding should yield a block verdict, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("sb1")) {
		t.Fatal("a blocking advisory must not mint an approval")
	}
	// The blocking advisory does NOT gate a non-trusted merge: approve + merge still works.
	if err := m.Approve("sb1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("sb1", false); err != nil {
		t.Fatalf("a blocking security advisory must not block a non-trusted merge: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded despite a clean approval")
	}
	_, _ = m.Teardown("sb1", true)
}

// TestSecurityReview_MissingReportFailsClosed: recording before the security reviewer ran
// (no security.json) folds to a "block" advisory verdict, not a silent pass.
func TestSecurityReview_MissingReportFailsClosed(t *testing.T) {
	m, repo := deliveryHarness(t, "secmiss")
	task, err := m.Spawn("sm1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	commitFeature(t, task.Worktree, "feature.txt", "new\n")

	v, err := m.SecurityReview("sm1", "", time.Minute)
	if err != nil {
		t.Fatalf("a missing report must fold to a block verdict, not error: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a missing security report must fail closed to block, got %q", v.Overall)
	}
	_, _ = m.Teardown("sm1", true)
}

// TestSecurityReview_RefusesStaleSha mirrors the trust-record commit pin: a report covering
// a sha that is no longer the worker HEAD is refused rather than silently accepted.
func TestSecurityReview_RefusesStaleSha(t *testing.T) {
	m, repo := deliveryHarness(t, "secstale")
	if _, err := m.Spawn("ss1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	_, err := m.SecurityReview("ss1", "deadbeefdeadbeef", time.Minute)
	if err == nil {
		t.Fatal("security-review must refuse a sha that is not the worker HEAD")
	}
	// It must fail specifically because the sha is stale (not, say, a missing report or an
	// unknown task), so the message names the commit pin.
	if !strings.Contains(err.Error(), "the worker HEAD is now") || !strings.Contains(err.Error(), "deadbeefdead") {
		t.Fatalf("want the stale-sha commit-pin error naming the requested sha, got: %v", err)
	}
	_, _ = m.Teardown("ss1", true)
}

// --- ttorch qa-review (the optional test-adequacy advisory pass) ---

// writeQAReport drops ONLY the QA reviewer's report into dir (the manager ran just the QA
// reviewer, not the full three-dimension gate), as the qa-review pass expects.
func writeQAReport(t *testing.T, dir, sha string, findings []review.Finding) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(review.Report{Dimension: review.DimensionQA, ReviewedSHA: sha, Findings: findings})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qa.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestQAReview_AdvisoryAndIndependentOfReviewPaths is the headline guard: a QA audit folds the
// QA reviewer's report into a verdict but is purely ADVISORY — it never mints an approval,
// never writes the trust gate's verdict file or the security audit's verdict file, and never
// touches the task's gate state. Only the QA reviewer's report need be present.
func TestQAReview_AdvisoryAndIndependentOfReviewPaths(t *testing.T) {
	m, repo := deliveryHarness(t, "qarev")
	task, err := m.Spawn("qv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	writeQAReport(t, m.P.ReviewInputsDir("qv1"), head, nil) // clean
	v, err := m.QAReview("qv1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Pass {
		t.Fatalf("a clean qa report should pass, got %q", v.Overall)
	}
	// Advisory: no approval minted, and neither the trust gate's durable verdict nor the
	// security audit's verdict file is written (the three paths use distinct stores).
	if approval.Valid(m.P.ApprovalFile("qv1")) {
		t.Fatal("qa-review must NOT mint an approval token")
	}
	if _, ok := m.TrustShow("qv1"); ok {
		t.Fatal("qa-review must NOT write the trust gate's verdict")
	}
	if _, ok := m.SecurityReviewShow("qv1"); ok {
		t.Fatal("qa-review must NOT write the security audit's verdict file")
	}
	// Task gate state is untouched (the advisory pass is side-effect-free on it).
	reloaded, _, _ := m.Store.GetTask(context.Background(), "qv1")
	if reloaded.GatePassed || reloaded.ReviewedSHA != "" || reloaded.ApprovedBy != "" {
		t.Fatalf("qa-review must not touch the task's gate state: %+v", reloaded)
	}
	// QAReviewShow returns the recorded advisory verdict.
	got, ok := m.QAReviewShow("qv1")
	if !ok || got.ReviewedSHA != head {
		t.Fatalf("QAReviewShow = %+v ok=%v, want the recorded verdict pinned to %s", got, ok, head)
	}
	_, _ = m.Teardown("qv1", true)
}

// TestQAReview_BlockingFindingStaysAdvisory pins that a high QA finding yields a "block"
// verdict for the manager to surface, yet it never gates delivery: the normal approval-gated
// merge still proceeds. QA is advisory in every mode and is not part of the trusted gate.
func TestQAReview_BlockingFindingStaysAdvisory(t *testing.T) {
	m, repo := deliveryHarness(t, "qablock")
	task, err := m.Spawn("qb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	writeQAReport(t, m.P.ReviewInputsDir("qb1"), head, []review.Finding{
		{Severity: review.SeverityHigh, Reviewer: "ttorch-reviewer-qa", Summary: "new failure path has no test"},
	})
	v, err := m.QAReview("qb1", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a high finding should yield a block verdict, got %q", v.Overall)
	}
	if approval.Valid(m.P.ApprovalFile("qb1")) {
		t.Fatal("a blocking advisory must not mint an approval")
	}
	// The blocking advisory does NOT gate a non-trusted merge: approve + merge still works.
	if err := m.Approve("qb1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("qb1", false); err != nil {
		t.Fatalf("a blocking qa advisory must not block a non-trusted merge: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded despite a clean approval")
	}
	_, _ = m.Teardown("qb1", true)
}

// TestQAReview_MissingReportFailsClosed: recording before the QA reviewer ran (no qa.json)
// folds to a "block" advisory verdict, not a silent pass.
func TestQAReview_MissingReportFailsClosed(t *testing.T) {
	m, repo := deliveryHarness(t, "qamiss")
	task, err := m.Spawn("qm1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	commitFeature(t, task.Worktree, "feature.txt", "new\n")

	v, err := m.QAReview("qm1", "", time.Minute)
	if err != nil {
		t.Fatalf("a missing report must fold to a block verdict, not error: %v", err)
	}
	if v.Overall != review.Block {
		t.Fatalf("a missing qa report must fail closed to block, got %q", v.Overall)
	}
	_, _ = m.Teardown("qm1", true)
}

// TestQAReview_RefusesStaleSha mirrors the trust-record commit pin: a report covering a sha
// that is no longer the worker HEAD is refused rather than silently accepted.
func TestQAReview_RefusesStaleSha(t *testing.T) {
	m, repo := deliveryHarness(t, "qastale")
	if _, err := m.Spawn("qs1", repo, false, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	_, err := m.QAReview("qs1", "deadbeefdeadbeef", time.Minute)
	if err == nil {
		t.Fatal("qa-review must refuse a sha that is not the worker HEAD")
	}
	// It must fail specifically because the sha is stale (not a missing report or unknown
	// task), so the message names the commit pin.
	if !strings.Contains(err.Error(), "the worker HEAD is now") || !strings.Contains(err.Error(), "deadbeefdead") {
		t.Fatalf("want the stale-sha commit-pin error naming the requested sha, got: %v", err)
	}
	_, _ = m.Teardown("qs1", true)
}

// TestSecurityAuditNote covers the advisory note `ttorch land` appends in non-gated modes.
// It never blocks; it just reports whether a fresh security audit covers the landed commit.
func TestSecurityAuditNote(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	const id, sha = "sn1", "deadbeefcafe0001"

	// A gated land (trusted / --require-verdict) already ran the full review gate → no note.
	if note := m.securityAuditNote(id, sha, true); note != "" {
		t.Fatalf("a gated land must not emit a security note, got %q", note)
	}
	// Non-gated, no audit recorded → a non-blocking nudge.
	if note := m.securityAuditNote(id, sha, false); !strings.Contains(note, "no security audit covers") || !strings.Contains(note, "does not block") {
		t.Fatalf("missing-audit note wrong: %q", note)
	}
	// A clean PASS advisory covering the landed sha → "passed".
	if err := review.Write(m.securityVerdictPath(id), review.Verdict{Overall: review.Pass, ReviewedSHA: sha}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if note := m.securityAuditNote(id, sha, false); !strings.Contains(note, "security audit passed") {
		t.Fatalf("pass note wrong: %q", note)
	}
	// An audit pinned to a DIFFERENT commit is stale → treated as missing (nudge again).
	if note := m.securityAuditNote(id, "OTHERSHA00000000", false); !strings.Contains(note, "no security audit covers") {
		t.Fatalf("a stale-sha audit should nudge, got %q", note)
	}
	// A blocking advisory is surfaced but explicitly did NOT block this delivery.
	if err := review.Write(m.securityVerdictPath(id), review.Verdict{Overall: review.Block, ReviewedSHA: sha}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if note := m.securityAuditNote(id, sha, false); !strings.Contains(note, "blocking findings") || !strings.Contains(note, "did not block") {
		t.Fatalf("blocking advisory note wrong: %q", note)
	}
}

// TestLand_SurfacesSecurityAdvisory checks the wiring end to end: a land in a non-gated
// mode appends the advisory security note to its summary (and does not block).
func TestLand_SurfacesSecurityAdvisory(t *testing.T) {
	m, repo := deliveryHarness(t, "landsec")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ls1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	if err := m.Approve("ls1", time.Minute); err != nil {
		t.Fatal(err)
	}

	out, err := m.Land("ls1", false)
	if err != nil {
		t.Fatalf("clean local land should succeed: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("default branch was not fast-forwarded")
	}
	// No audit recorded → the land summary carries the non-blocking advisory nudge.
	if !strings.Contains(out, "advisory: no security audit covers") {
		t.Fatalf("land summary should surface the security advisory, got: %q", out)
	}
	_, _ = m.Teardown("ls1", true)
}

// countingGateScript commits a default-branch .ttorch/validate.sh that appends one byte to
// counter (an absolute path) on every run and then exits 0, so a test can tell exactly how
// many times the gate's fresh validate actually executed. Returns the default-branch HEAD.
func countingGateScript(t *testing.T, repo, counter string) string {
	t.Helper()
	return commitGateScript(t, repo, fmt.Sprintf("printf x >> '%s'\nexit 0", counter))
}

// gateRunCount reports how many times countingGateScript ran (one byte appended per run); a
// missing counter file means it never ran.
func gateRunCount(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(b)
}

// stageValidate writes the head.txt / validate.json pair into a task's review-inputs dir
// exactly as trust prep does, pinning results to sha — letting a test set up the reuse
// (sha matches) and re-validate (sha differs) cases deterministically.
func stageValidate(t *testing.T, dir, sha string, results []validate.Result) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "validate.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestValidateForMerge_ReusesPinnedPrepValidate: when trust prep staged a green validate
// pinned to the exact sha being merged, validateForMerge returns that staged result and
// does NOT re-run the gate — the redundant full-suite run is skipped.
func TestValidateForMerge_ReusesPinnedPrepValidate(t *testing.T) {
	m, repo := deliveryHarness(t, "vfmreuse")
	counter := filepath.Join(t.TempDir(), "runs")
	countingGateScript(t, repo, counter)
	task, err := m.Spawn("rz1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	// A green, commit-pinned validate already staged for this exact sha.
	stageValidate(t, m.P.ReviewInputsDir("rz1"), head, []validate.Result{{Name: "gate", Passed: true}})

	c0 := gateRunCount(t, counter)
	green, results, reused, err := m.validateForMerge(repo, "rz1", head)
	if err != nil {
		t.Fatalf("validateForMerge: %v", err)
	}
	if !reused {
		t.Fatal("a validate pinned to the exact merged sha must be reused, not re-run")
	}
	if !green || len(results) != 1 || !results[0].Passed {
		t.Fatalf("reused result should be the staged green validate, got green=%v results=%+v", green, results)
	}
	if got := gateRunCount(t, counter); got != c0 {
		t.Fatalf("reuse must not run the gate again: gate ran %d extra time(s)", got-c0)
	}
	_, _ = m.Teardown("rz1", true)
}

// TestValidateForMerge_RevalidatesWhenHeadMoved: when the staged validate pins to an older
// sha than the one being merged, validateForMerge ignores the stale (here deliberately
// failing) result and runs a fresh validate of the CURRENT sha — preserving the safety
// property that the merged commit is validated, never trusting a result for a different
// commit.
func TestValidateForMerge_RevalidatesWhenHeadMoved(t *testing.T) {
	m, repo := deliveryHarness(t, "vfmreval")
	counter := filepath.Join(t.TempDir(), "runs")
	countingGateScript(t, repo, counter)
	task, err := m.Spawn("rv1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head1 := commitFeature(t, task.Worktree, "a.txt", "1\n")
	head2 := commitFeature(t, task.Worktree, "b.txt", "2\n")
	if head1 == head2 {
		t.Fatal("the two commits must differ for this test")
	}
	// Stale validate pinned to head1 AND failing: reusing it would both pin the wrong commit
	// and wrongly block — re-validation of head2 must override both.
	stageValidate(t, m.P.ReviewInputsDir("rv1"), head1, []validate.Result{{Name: "gate", Passed: false}})

	c0 := gateRunCount(t, counter)
	green, results, reused, err := m.validateForMerge(repo, "rv1", head2)
	if err != nil {
		t.Fatalf("validateForMerge: %v", err)
	}
	if reused {
		t.Fatal("a validate pinned to a different sha must NOT be reused")
	}
	if got := gateRunCount(t, counter); got != c0+1 {
		t.Fatalf("a moved HEAD must trigger exactly one fresh validate, got %d extra run(s)", got-c0)
	}
	if !green || len(validate.Failures(results)) != 0 {
		t.Fatalf("the fresh validate of head2 should be green, got green=%v results=%+v", green, results)
	}
	_, _ = m.Teardown("rv1", true)
}

// TestMergeLocal_ReusesPrepValidateForUnchangedHead is the headline behavior: in a trusted
// auto-merge whose committed sha is unchanged since trust prep validated it, the merge
// reuses prep's commit-pinned validate.json instead of re-running the identical suite — yet
// still fast-forwards (the merged commit remains backed by a green, pinned validate).
func TestMergeLocal_ReusesPrepValidateForUnchangedHead(t *testing.T) {
	m, repo := deliveryHarness(t, "mlreuse")
	counter := filepath.Join(t.TempDir(), "runs")
	countingGateScript(t, repo, counter)
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("mr1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")

	dir, err := m.TrustPrep("mr1") // validates head once, staging validate.json + head.txt
	if err != nil {
		t.Fatal(err)
	}
	writeReviewReports(t, dir, head, nil) // clean → pass
	if _, err := m.TrustRecord("mr1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !approval.Valid(m.P.ApprovalFile("mr1")) {
		t.Fatal("trusted + pass + green must auto-mint an approval")
	}

	before := gateRunCount(t, counter)
	if before == 0 {
		t.Fatal("prep/record should have run the counting gate at least once")
	}
	if _, err := m.MergeLocal("mr1", false); err != nil {
		t.Fatalf("trusted merge should succeed reusing prep's validate: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("the merged commit must be fast-forwarded onto the default branch")
	}
	if after := gateRunCount(t, counter); after != before {
		t.Fatalf("merge re-ran the gate %d extra time(s); it should reuse prep's commit-pinned validate", after-before)
	}
	_, _ = m.Teardown("mr1", true)
}

// TestStagedGreen locks the load-bearing no-checks guard on the reuse path: a persisted
// result set is green ONLY when at least one check ran and none failed. An empty or null
// set (no checks detected — which trust prep can legitimately stage) must read as NOT
// green, so an empty Failures() can never be mistaken for a pass when the gate reuses a
// staged result. Dropping the len>0 clause would silently merge an unvalidated commit.
func TestStagedGreen(t *testing.T) {
	cases := []struct {
		name    string
		results []validate.Result
		want    bool
	}{
		{"nil is a no-checks block", nil, false},
		{"empty is a no-checks block", []validate.Result{}, false},
		{"all passed is green", []validate.Result{{Name: "a", Passed: true}, {Name: "b", Passed: true}}, true},
		{"any failure is not green", []validate.Result{{Name: "a", Passed: true}, {Name: "b", Passed: false}}, false},
		{"single failure is not green", []validate.Result{{Name: "a", Passed: false}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stagedGreen(tc.results); got != tc.want {
				t.Fatalf("stagedGreen(%+v) = %v, want %v", tc.results, got, tc.want)
			}
		})
	}
}

// TestValidateForMerge_ReusedEmptyResultIsNotGreen exercises the no-checks edge ON the
// reuse path: trust prep can stage an empty/null validate.json (no checks detected) pinned
// to the merged sha. Reuse must return that result as-is and read it as NOT green — a hard
// block — rather than fall through to a fresh run or, worse, treat an empty result as a
// pass. This is the reuse-path counterpart of the fresh-path no-checks hard block.
func TestValidateForMerge_ReusedEmptyResultIsNotGreen(t *testing.T) {
	m, repo := deliveryHarness(t, "vfmempty")
	counter := filepath.Join(t.TempDir(), "runs")
	countingGateScript(t, repo, counter)
	task, err := m.Spawn("ze1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFeature(t, task.Worktree, "feature.txt", "new\n")
	// An empty (no-checks-detected) validate, pinned to the exact merged sha — exactly what
	// prep stages for a repo with no detectable checks.
	stageValidate(t, m.P.ReviewInputsDir("ze1"), head, nil)

	c0 := gateRunCount(t, counter)
	green, results, reused, err := m.validateForMerge(repo, "ze1", head)
	if err != nil {
		t.Fatalf("validateForMerge: %v", err)
	}
	if !reused {
		t.Fatal("an empty result pinned to the exact sha is still a reusable pinned result")
	}
	if green {
		t.Fatal("a no-checks (empty) staged result must NOT read as green — the safety guard")
	}
	if len(results) != 0 {
		t.Fatalf("the empty staged result should be returned verbatim, got %+v", results)
	}
	if got := gateRunCount(t, counter); got != c0 {
		t.Fatalf("reusing an empty result must not fall through to a fresh gate run: %d extra run(s)", got-c0)
	}
	_, _ = m.Teardown("ze1", true)
}

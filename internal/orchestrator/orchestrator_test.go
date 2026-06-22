package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/orcha/internal/paths"
	"github.com/nution101/orcha/internal/tmux"
)

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

	session := fmt.Sprintf("orcha-test-%d", os.Getpid())
	t.Setenv("ORCHA_HOME", t.TempDir())
	t.Setenv("ORCHA_TMUX_SESSION", session)
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	m := New(paths.Default())

	task, err := m.Spawn("t1", repo, false, "printf 'ORCHA_MARKER\\n'; sleep 30")
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
		if strings.Contains(out, "ORCHA_MARKER") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(out, "ORCHA_MARKER") {
		t.Fatalf("marker not found in pane output: %q", out)
	}

	// Clean worktree -> teardown succeeds without --force.
	if _, err := m.Teardown("t1", false); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if m.Live(task) {
		t.Fatal("window still alive after teardown")
	}
	if _, err := os.Stat(task.Worktree); !os.IsNotExist(err) {
		t.Fatal("worktree not removed")
	}
	if tasks, _ := m.Status(); len(tasks) != 0 {
		t.Fatalf("status returned %d tasks after teardown, want 0", len(tasks))
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

	session := fmt.Sprintf("orcha-test-dirty-%d", os.Getpid())
	t.Setenv("ORCHA_HOME", t.TempDir())
	t.Setenv("ORCHA_TMUX_SESSION", session)
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

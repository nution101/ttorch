package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkerLaunchPrefix pins the env assignments prepended to a worker's launch
// command (§3.1): both vars set, single-quoted (so a value with shell metacharacters
// survives verbatim), and empty values omitted.
func TestWorkerLaunchPrefix(t *testing.T) {
	if got := WorkerLaunchPrefix("", ""); got != "" {
		t.Fatalf("empty inputs should yield an empty prefix, got %q", got)
	}
	if got, want := WorkerLaunchPrefix("t1", "/home/.ttorch/state.db"),
		"TTORCH_TASK_ID='t1' TTORCH_DB='/home/.ttorch/state.db' "; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	// An id with an embedded single quote is closed/escaped/reopened by shq.
	if got, want := WorkerLaunchPrefix("a'b", ""), "TTORCH_TASK_ID='a'\\''b' "; got != want {
		t.Fatalf("metachar id prefix = %q, want %q", got, want)
	}
	// An empty task id is omitted but the db is still carried.
	if got, want := WorkerLaunchPrefix("", "/db"), "TTORCH_DB='/db' "; got != want {
		t.Fatalf("db-only prefix = %q, want %q", got, want)
	}
}

// TestWorkerTaskFileRoundTrip proves the file is written under <worktree>/.ttorch/task
// and reads back to the same task id and DB path.
func TestWorkerTaskFileRoundTrip(t *testing.T) {
	wt := t.TempDir()
	if err := WriteWorkerTaskFile(wt, "task-9", "/tmp/x/state.db"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".ttorch", "task")); err != nil {
		t.Fatalf(".ttorch/task not written: %v", err)
	}
	id, dbp, ok := ReadWorkerTaskFile(wt)
	if !ok || id != "task-9" || dbp != "/tmp/x/state.db" {
		t.Fatalf("round-trip = (%q, %q, %v), want (task-9, /tmp/x/state.db, true)", id, dbp, ok)
	}
	if _, _, ok := ReadWorkerTaskFile(t.TempDir()); ok {
		t.Fatal("a missing .ttorch/task should not be ok")
	}
}

// TestReadWorkerTaskFileLenient proves the parser trims whitespace, skips blank and
// non key=value lines, and ignores unknown keys (so the format can grow).
func TestReadWorkerTaskFileLenient(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".ttorch"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# a comment\n\ntask_id = spaced \ndb=/d/b\nunknown=ignored\n"
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "task"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	id, dbp, ok := ReadWorkerTaskFile(wt)
	if !ok || id != "spaced" || dbp != "/d/b" {
		t.Fatalf("lenient parse = (%q, %q, %v), want (spaced, /d/b, true)", id, dbp, ok)
	}
}

// TestWriteWorkerTaskFileExcludesFromGit proves the identity file is kept out of
// git's view (same mechanism as the Stop hook), so a worker's tree never shows it as
// untracked or commits it.
func TestWriteWorkerTaskFileExcludesFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	wt := t.TempDir()
	runGitWT(t, wt, "init", "-q")
	if err := WriteWorkerTaskFile(wt, "g1", "/db"); err != nil {
		t.Fatal(err)
	}
	if out := runGitWT(t, wt, "status", "--porcelain"); strings.Contains(out, ".ttorch/task") {
		t.Fatalf(".ttorch/task must be git-excluded; status still shows it:\n%s", out)
	}
}

func runGitWT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/clonepool"
	"github.com/nution101/ttorch/internal/tmux"
)

// TestCmdSpawn_InvalidWorkdir pins the --workdir guard: an unknown kind fails loudly,
// naming the accepted values, before any side effect. The DB and skill install are
// pinned off so a regression that lets the spawn proceed fails here rather than reaching
// either.
func TestCmdSpawn_InvalidWorkdir(t *testing.T) {
	withSeedDB(t, nil)
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	err := cmdSpawn([]string{"task1", t.TempDir(), "--workdir", "copy"})
	if err == nil {
		t.Fatal("cmdSpawn with an unknown --workdir must return an error")
	}
	if !strings.Contains(err.Error(), "invalid --workdir") || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("error = %q, want it to name the invalid --workdir and the accepted kinds", err)
	}
}

// TestCmdSpawn_WorkdirCloneReachesTheClonePool checks --workdir clone is passed through to
// the spawn, with TTORCH_WORKER_CLONES off. The repository uses LFS, which only the clone
// pool refuses, so reaching that refusal proves the flag selected the clone pool, and no
// worker is ever launched.
func TestCmdSpawn_WorkdirCloneReachesTheClonePool(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	withSeedDB(t, nil)
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-cli-workdir-test")
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	t.Setenv(clonepool.EnvVar, "")
	repo := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=w", "GIT_AUTHOR_EMAIL=w@example.com",
			"GIT_COMMITTER_NAME=w", "GIT_COMMITTER_EMAIL=w@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitRun("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.bin filter=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", "-A")
	gitRun("commit", "-q", "-m", "lfs")

	_, err := captureStdout(t, func() error {
		return cmdSpawn([]string{"wd1", repo, "--workdir", "clone", "--cmd", "sleep 30"})
	})
	if !errors.Is(err, clonepool.ErrUnsupportedRepo) {
		t.Fatalf("cmdSpawn --workdir clone: err = %v, want the clone pool's ErrUnsupportedRepo", err)
	}
}

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nution101/ttorch/internal/gittest"
)

// scratchRepoState is what a stray fixture git would change in a repository it was pointed at
// by mistake: the config file and every ref. It reads them with --git-dir and gittest.Env, so
// it sees the scratch repository whatever the test has set.
func scratchRepoState(t *testing.T, gitDir string) string {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", gitDir, "for-each-ref", "--format=%(refname) %(objectname)")
	cmd.Env = gittest.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git for-each-ref: %v: %s", err, out)
	}
	return string(cfg) + "\n--refs--\n" + string(out)
}

// TestFixtureGitIgnoresInheritedGitDir: a suite run inside `git rebase --exec` inherits a
// GIT_DIR naming the repository being rebased. A fixture helper that let it through ran its
// git init and commits there instead of in its temp dir, and set core.bare=true in the shared
// config. Each helper below runs with GIT_DIR pointing at a scratch repository, which must
// come out unchanged, while the helper still builds its own fixture.
func TestFixtureGitIgnoresInheritedGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	helpers := []struct {
		name  string
		build func(t *testing.T) string
	}{
		{"noticeRepo", func(t *testing.T) string { return noticeRepo(t, "trusted", "", true) }},
		{"branchedRepo", func(t *testing.T) string { return branchedRepo(t, "main") }},
		{"lintRepo", lintRepo},
		{"syncFixture", func(t *testing.T) string { repo, _, _ := syncFixture(t); return repo }},
	}
	for _, h := range helpers {
		t.Run(h.name, func(t *testing.T) {
			scratch := t.TempDir()
			gitDir := filepath.Join(scratch, ".git")
			for _, args := range [][]string{
				{"init", "-q", "-b", "main"},
				{"-c", "user.name=s", "-c", "user.email=s@example.invalid", "commit", "-q", "--allow-empty", "-m", "scratch"},
			} {
				if out, err := gittest.Command(scratch, args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			before := scratchRepoState(t, gitDir)

			t.Setenv("GIT_DIR", gitDir)
			repo := h.build(t)

			if after := scratchRepoState(t, gitDir); after != before {
				t.Errorf("%s wrote into the repository GIT_DIR named:\nbefore:\n%s\nafter:\n%s", h.name, before, after)
			}
			if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
				t.Errorf("%s did not build its own repository in %s: %v", h.name, repo, err)
			}
		})
	}
}

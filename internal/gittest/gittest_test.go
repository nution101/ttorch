package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// scratchState is the config file and every ref of the repository at gitDir, read with
// --git-dir and Env so nothing a test has set can redirect the read.
func scratchState(t *testing.T, gitDir string) string {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", gitDir, "for-each-ref", "--format=%(refname) %(objectname)")
	cmd.Env = Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git for-each-ref: %v: %s", err, out)
	}
	return string(cfg) + "\n--refs--\n" + string(out)
}

// scratchRepo is a repository with one commit on main, and its git dir.
func scratchRepo(t *testing.T) (dir, gitDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir = t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=s", "-c", "user.email=s@example.invalid", "commit", "-q", "--allow-empty", "-m", "scratch"},
	} {
		if out, err := Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir, filepath.Join(dir, ".git")
}

// TestCommandIgnoresInheritedRepoVars: with every repository variable pointing at a scratch
// repository, as `git rebase --exec` does with GIT_DIR, a fixture that inits and commits in a
// fresh temp dir builds its repository there and leaves the scratch config and refs alone.
func TestCommandIgnoresInheritedRepoVars(t *testing.T) {
	scratch, gitDir := scratchRepo(t)
	before := scratchState(t, gitDir)
	for name, value := range map[string]string{
		"GIT_DIR":                          gitDir,
		"GIT_WORK_TREE":                    scratch,
		"GIT_INDEX_FILE":                   filepath.Join(gitDir, "index"),
		"GIT_COMMON_DIR":                   gitDir,
		"GIT_OBJECT_DIRECTORY":             filepath.Join(gitDir, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(gitDir, "objects"),
		"GIT_CEILING_DIRECTORIES":          filepath.Dir(t.TempDir()),
		"GIT_NAMESPACE":                    "elsewhere",
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 "core.bare",
		"GIT_CONFIG_VALUE_0":               "true",
	} {
		t.Setenv(name, value)
	}

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "-q", "--allow-empty", "-m", "fixture"},
	} {
		c := Command(repo, args...)
		c.Env = append(c.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if after := scratchState(t, gitDir); after != before {
		t.Errorf("fixture git wrote into the scratch repository:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if out, err := Command(repo, "rev-parse", "--verify", "refs/heads/main").CombinedOutput(); err != nil {
		t.Errorf("fixture repository has no main: %v: %s", err, out)
	}
}

// TestEnvKeepsIdentityAndConfigVars: only the repository variables go, so a fixture's
// identity and an isolating GIT_CONFIG_GLOBAL still reach its git.
func TestEnvKeepsIdentityAndConfigVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	got := map[string]bool{}
	for _, kv := range Env("GIT_COMMITTER_NAME=c") {
		got[kv] = true
	}
	if got["GIT_DIR=/elsewhere"] {
		t.Error("Env kept GIT_DIR")
	}
	for _, kv := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_COMMITTER_NAME=c"} {
		if !got[kv] {
			t.Errorf("Env dropped %s", kv)
		}
	}
}

// TestScrubUnsetsRepoVars: after Scrub, git run with the inherited environment, as code under
// test runs it, finds no GIT_DIR, while variables it does not own stay set.
func TestScrubUnsetsRepoVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	Scrub()
	for _, name := range []string{"GIT_DIR", "GIT_CONFIG_KEY_0"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s still set after Scrub: %q", name, v)
		}
	}
	if os.Getenv("GIT_AUTHOR_NAME") != "t" {
		t.Error("Scrub unset GIT_AUTHOR_NAME")
	}
}

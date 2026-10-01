package clonepool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realGit is the git binary the test helpers run directly, so their own commands never
// pass through a recording shim and the shim log holds only what the code under test ran.
var realGit = func() string {
	p, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	return p
}()

// isolateGit keeps the lead's global and system git config out of every git process the
// test starts, and gives commits a fixed identity.
func isolateGit(t *testing.T) {
	t.Helper()
	if realGit == "" {
		t.Skip("git not installed")
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "w")
	t.Setenv("GIT_AUTHOR_EMAIL", "w@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "w")
	t.Setenv("GIT_COMMITTER_EMAIL", "w@example.com")
	t.Setenv(EnvVar, "")
}

// run runs git -C dir args with the real binary and returns trimmed output.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command(realGit, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tryRun is run without failing the test.
func tryRun(dir string, args ...string) (string, error) {
	out, err := exec.Command(realGit, append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// newMain makes a lead repository on main with one commit. The path has its symlinks
// resolved so comparisons with what git prints hold on macOS, where the temp dir is
// reached through /var.
func newMain(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "main")
	run(t, dir, "init", "-q", "-b", "main", "main")
	writeFile(t, filepath.Join(repo, "f.txt"), "hi\n")
	run(t, repo, "add", "-A")
	run(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func newPool(t *testing.T) ClonePool {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ClonePool{Root: root, Max: 4}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, file, body, msg string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), body)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// shimCall is one git invocation the shim recorded.
type shimCall struct {
	cwd  string
	args []string
}

// gitShim puts a git on PATH that records each invocation's cwd and argv, then runs the
// real git. It returns a reader for the calls made since the last reset.
func gitShim(t *testing.T) (calls func() []shimCall, reset func()) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf '%s' \"$PWD\"; for a in \"$@\"; do printf '\\037%s' \"$a\"; done; printf '\\036'; } >> '" + log + "'\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls = func() []shimCall {
		b, _ := os.ReadFile(log)
		var out []shimCall
		for _, rec := range strings.Split(string(b), "\x1e") {
			if rec == "" {
				continue
			}
			f := strings.Split(rec, "\x1f")
			out = append(out, shimCall{cwd: f[0], args: f[1:]})
		}
		return out
	}
	reset = func() { _ = os.Remove(log) }
	return calls, reset
}

// touches reports whether a call names path, or a path under it, as its cwd or as an
// argument (an -C value, a positional path, or an --opt=path value).
func (c shimCall) touches(path string) bool {
	in := func(s string) bool { return s == path || strings.HasPrefix(s, path+"/") }
	if in(c.cwd) {
		return true
	}
	for _, a := range c.args {
		if _, v, ok := strings.Cut(a, "="); ok && in(v) {
			return true
		}
		if in(a) {
			return true
		}
	}
	return false
}

// initOf reports whether the call is the `git init` that creates path.
func (c shimCall) initOf(path string) bool {
	return len(c.args) > 0 && c.args[0] == "init" && c.args[len(c.args)-1] == path
}

// sentinelRepo arms a clone the way a hostile worker could: core.fsmonitor,
// diff.external and every hook it might reach all run a script that leaves a file. It
// returns the path whose existence means one of them ran.
func sentinelRepo(t *testing.T, slot string) string {
	t.Helper()
	dir := t.TempDir()
	fired := filepath.Join(dir, "fired")
	script := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$0 $*\" >> '"+fired+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"post-checkout", "reference-transaction", "post-index-change", "pre-auto-gc", "post-rewrite"} {
		if err := os.Symlink(script, filepath.Join(hooks, h)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(script, filepath.Join(slot, ".git", "hooks", h)); err != nil && !os.IsExist(err) {
			_ = os.MkdirAll(filepath.Join(slot, ".git", "hooks"), 0o755)
			_ = os.Symlink(script, filepath.Join(slot, ".git", "hooks", h))
		}
	}
	run(t, slot, "config", "core.fsmonitor", script)
	run(t, slot, "config", "diff.external", script)
	run(t, slot, "config", "core.hooksPath", hooks)
	run(t, slot, "config", "uploadpack.packObjectsHook", script)
	return fired
}

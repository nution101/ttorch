package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realTempDir is t.TempDir with symlinks resolved, so paths compare equal to what git prints
// on a platform whose temp directory sits behind a symlink.
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// gitIsolatedEnv is the test process's environment with every GIT_* variable removed and the
// system config disabled, plus extra.
func gitIsolatedEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(append(env, "GIT_CONFIG_NOSYSTEM=1"), extra...)
}

func runGit(t *testing.T, env []string, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(t, env, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// cloneSlot makes <TTORCH_HOME>/clones/<pool>/<n> as a plain repository, the shape the clone
// pool provisions, and returns its path. TTORCH_HOME must already point at a temp dir.
func cloneSlot(t *testing.T, pool, n string) string {
	t.Helper()
	slot := filepath.Join(clonesRoot(), pool, n)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, gitIsolatedEnv("GIT_CONFIG_GLOBAL=/dev/null"), slot, "init", "-q")
	return slot
}

// readSettingsEnv returns the env block of the worker settings file under workdir, and
// whether the file has an "env" key at all.
func readSettingsEnv(t *testing.T, workdir string) (map[string]string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	raw, ok := s["env"]
	if !ok {
		return nil, false
	}
	env := map[string]string{}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env, true
}

// envList turns a settings env block into KEY=VALUE entries for exec.
func envList(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func TestGitEnvForCloneSlot(t *testing.T) {
	t.Setenv("TTORCH_HOME", realTempDir(t))
	slot := cloneSlot(t, "repo-0123abcd", "3")
	pool := filepath.Dir(slot)
	got := GitEnvFor(slot)
	want := WorkerGitEnv{CeilingDirectories: clonesRoot(), ConfigGlobal: filepath.Join(pool, ".global-3"), ConfigSystem: filepath.Join(pool, ".system-3")}
	if got != want {
		t.Fatalf("GitEnvFor(clone slot) = %+v, want %+v", got, want)
	}
	if !IsCloneWorkdir(slot + "/") {
		t.Errorf("a trailing slash must not change the kind")
	}
}

// TestGitEnvForNotAClone: only a numeric slot directly in a pool under the clones root, with
// a real .git directory, is a clone. Everything else gets no git environment.
func TestGitEnvForNotAClone(t *testing.T) {
	home := realTempDir(t)
	t.Setenv("TTORCH_HOME", home)
	env := gitIsolatedEnv("GIT_CONFIG_GLOBAL=/dev/null")

	// A linked worktree in a pool-shaped directory: its .git is a file.
	main := filepath.Join(home, "main")
	mustGit(t, env, home, "init", "-q", main)
	mustGit(t, env, main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c")
	linked := filepath.Join(clonesRoot(), "pool-a", "1")
	if err := os.MkdirAll(filepath.Dir(linked), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, main, "worktree", "add", "-q", "--detach", linked)

	// A real repository outside the clones root: a worktree-pool path and the lead's own checkout.
	elsewhere := filepath.Join(home, "worktrees", "pool-b", "1")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, elsewhere, "init", "-q")

	// A non-numeric slot name, and a slot nested one level too deep.
	named := cloneSlot(t, "pool-c", "scratch")
	deep := filepath.Join(cloneSlot(t, "pool-d", "1"), "2")
	if err := os.MkdirAll(filepath.Join(deep, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A slot whose .git is a symlink to a real gitdir.
	symSlot := filepath.Join(clonesRoot(), "pool-e", "1")
	if err := os.MkdirAll(symSlot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, ".git"), filepath.Join(symSlot, ".git")); err != nil {
		t.Fatal(err)
	}

	// A numeric slot with no .git at all.
	empty := filepath.Join(clonesRoot(), "pool-f", "1")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, dir := range map[string]string{
		"linked worktree":     linked,
		"outside clones root": elsewhere,
		"lead checkout":       main,
		"non-numeric slot":    named,
		"nested too deep":     deep,
		"symlinked .git":      symSlot,
		"no .git":             empty,
		"empty":               "",
		"relative":            filepath.Join("clones", "pool-a", "1"),
		"the clones root":     clonesRoot(),
		"a pool directory":    filepath.Dir(named),
	} {
		if got := GitEnvFor(dir); !got.IsZero() {
			t.Errorf("%s (%s): GitEnvFor = %+v, want the zero value", name, dir, got)
		}
	}
}

// TestCloneWorkerGitEnvOnEveryRoute: a clone worker's git environment is in the settings env
// block, the launch prefix and both halves of the resume command, because each route covers a
// case the others miss (a resume has no launch prefix; an assignment before `a || b` reaches
// only a).
func TestCloneWorkerGitEnvOnEveryRoute(t *testing.T) {
	t.Setenv("TTORCH_HOME", realTempDir(t))
	slot := cloneSlot(t, "repo-0123abcd", "7")
	pool := filepath.Dir(slot)
	global := filepath.Join(pool, ".global-7")
	system := filepath.Join(pool, ".system-7")

	if err := WriteWorkerSettings("claude", slot); err != nil {
		t.Fatal(err)
	}
	env, ok := readSettingsEnv(t, slot)
	if !ok {
		t.Fatalf("clone worker settings have no env block")
	}
	if env[envGitCeiling] != clonesRoot() || env[envGitGlobal] != global || env[envGitSystem] != system || len(env) != 3 {
		t.Errorf("settings env = %v, want %s=%s, %s=%s and %s=%s only", env, envGitCeiling, clonesRoot(), envGitGlobal, global, envGitSystem, system)
	}
	for _, f := range []string{global, system} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("private config not written: %v", err)
		}
	}

	gitEnv := envGitCeiling + "=" + shq(clonesRoot()) + " " + envGitGlobal + "=" + shq(global) + " " + envGitSystem + "=" + shq(system) + " "
	if got, want := WorkerLaunchPrefix("t1", "/db", slot), "TTORCH_TASK_ID='t1' TTORCH_DB='/db' "+gitEnv; got != want {
		t.Errorf("launch prefix:\n got %q\nwant %q", got, want)
	}
	if got := WorkerLaunchPrefix("", "", slot); got != gitEnv {
		t.Errorf("launch prefix with no task: got %q, want %q", got, gitEnv)
	}

	rf := WorkerResumeOrFresh("claude", "sid", "/b.md", "", "", slot)
	resume, fresh, found := strings.Cut(rf, " || ")
	if !found {
		t.Fatalf("resume command has no fallback: %q", rf)
	}
	if !strings.HasPrefix(resume, gitEnv+"claude ") || !strings.HasPrefix(fresh, gitEnv+"claude ") {
		t.Errorf("both halves of the resume command must carry the git env:\n%s", rf)
	}
	if got := WorkerResumeOrFresh("codex", "sid", "/b.md", "", "", slot); !strings.HasPrefix(got, gitEnv) {
		t.Errorf("non-claude resume must carry the git env: %q", got)
	}
}

// TestWorktreeWorkerUnchanged is the flag-off proof at this layer: a worker in a linked
// worktree gets exactly the launch prefix, resume command and settings file it got before the
// clone environment existed, and no private config file appears.
func TestWorktreeWorkerUnchanged(t *testing.T) {
	home := realTempDir(t)
	t.Setenv("TTORCH_HOME", home)
	env := gitIsolatedEnv("GIT_CONFIG_GLOBAL=/dev/null")
	main := filepath.Join(home, "main")
	mustGit(t, env, home, "init", "-q", main)
	mustGit(t, env, main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c")
	wt := filepath.Join(home, "worktrees", "main-0123abcd", "1")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, main, "worktree", "add", "-q", "--detach", wt)

	if got, want := WorkerLaunchPrefix("t1", "/db", wt), "TTORCH_TASK_ID='t1' TTORCH_DB='/db' "; got != want {
		t.Errorf("worktree launch prefix = %q, want %q", got, want)
	}
	if got, want := WorkerResumeOrFresh("claude", "sid", "/b.md", "high", "opus", wt),
		ResumeCommand("claude", "sid", "high", "opus")+" || "+BriefCommand("claude", "/b.md", "sid", "high", "opus"); got != want {
		t.Errorf("worktree resume command changed:\n got %q\nwant %q", got, want)
	}

	if err := WriteWorkerSettings("claude", wt); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSettingsEnv(t, wt); ok {
		t.Errorf("worktree worker settings must have no env block")
	}
	plain := t.TempDir()
	if err := WriteWorkerSettings("claude", plain); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json"))
	b, _ := os.ReadFile(filepath.Join(plain, ".claude", "settings.local.json"))
	if string(a) != string(b) {
		t.Errorf("worktree settings differ from a plain directory's:\n%s\n---\n%s", a, b)
	}
	if _, err := os.Stat(clonesRoot()); !os.IsNotExist(err) {
		t.Errorf("a worktree spawn must create nothing under the clones root (stat err = %v)", err)
	}
}

// TestPrivateGlobalConfigAbsorbsGlobalWrites: under the env block a clone worker is given,
// `git config --global` writes the slot's private file and leaves the lead's untouched, while
// the lead's identity and includeIf rules still resolve through the include.
func TestPrivateGlobalConfigAbsorbsGlobalWrites(t *testing.T) {
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("TTORCH_HOME", filepath.Join(home, ".ttorch"))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(home, "clones.inc")
	if err := os.WriteFile(extra, []byte("[ttorchtest]\n\tmatched = yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	leadCfg := filepath.Join(home, ".gitconfig")
	lead := "[user]\n\tname = Lead Person\n\temail = lead@example.invalid\n" +
		"[includeIf \"gitdir:" + clonesRoot() + "/\"]\n\tpath = " + extra + "\n"
	if err := os.WriteFile(leadCfg, []byte(lead), 0o644); err != nil {
		t.Fatal(err)
	}
	slot := cloneSlot(t, "repo-0123abcd", "1")
	if err := WriteWorkerSettings("claude", slot); err != nil {
		t.Fatal(err)
	}
	settingsEnv, _ := readSettingsEnv(t, slot)
	workerEnv := gitIsolatedEnv(append(envList(settingsEnv), "HOME="+home)...)

	if got := mustGit(t, workerEnv, slot, "config", "user.name"); got != "Lead Person" {
		t.Errorf("lead identity through the include: got %q", got)
	}
	if got := mustGit(t, workerEnv, slot, "config", "ttorchtest.matched"); got != "yes" {
		t.Errorf("lead includeIf rule through the include: got %q", got)
	}
	mustGit(t, workerEnv, slot, "config", "--global", "core.fsmonitor", "echo planted")

	if b, _ := os.ReadFile(leadCfg); string(b) != lead {
		t.Errorf("git config --global changed the lead's file:\n%s", b)
	}
	priv, err := os.ReadFile(settingsEnv[envGitGlobal])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(priv), "planted") {
		t.Errorf("git config --global did not land in the private file:\n%s", priv)
	}
}

// TestPrivateGlobalConfigRewrittenPerTask: the private file sits in the pool directory, which
// outlives a slot's recycle, so a spawn rewrites it rather than inheriting the previous
// worker's keys, and a symlink planted at its path is replaced, not written through.
func TestPrivateGlobalConfigRewrittenPerTask(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("TTORCH_HOME", filepath.Join(root, ".ttorch"))
	slot := cloneSlot(t, "repo-0123abcd", "2")
	global := GitEnvFor(slot).ConfigGlobal

	if err := os.WriteFile(global, []byte("[core]\n\tfsmonitor = echo previous-task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkerSettings("claude", slot); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(global); strings.Contains(string(b), "previous-task") {
		t.Errorf("the previous task's global key survived the spawn:\n%s", b)
	}

	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(global); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, global); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkerSettings("claude", slot); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "untouched\n" {
		t.Errorf("the write followed a planted symlink: target now %q", b)
	}
	if fi, err := os.Lstat(global); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("private config should be a regular file after the rewrite (err=%v)", err)
	}
}

// TestPrivateGlobalConfigIncludes: the include list follows git's own choice of global files,
// and a path is quoted so git reads it back byte for byte.
func TestPrivateGlobalConfigIncludes(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	if got, want := leadGlobalConfigs(), []string{filepath.Join(root, "xdg", "git", "config"), filepath.Join(root, ".gitconfig")}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("default includes = %q, want %q (XDG first, as git reads it)", got, want)
	}
	odd := filepath.Join(root, `a "quoted" \ path #not-a-comment;x`)
	t.Setenv("GIT_CONFIG_GLOBAL", odd)
	if got := leadGlobalConfigs(); len(got) != 1 || got[0] != odd {
		t.Errorf("with GIT_CONFIG_GLOBAL set git reads only that file; includes = %q", got)
	}
	body, err := privateConfig("global", []string{odd})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "priv")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, gitIsolatedEnv(), root, "config", "--file", f, "--get", "include.path"); got != odd {
		t.Errorf("include.path read back as %q, want %q", got, odd)
	}
	if _, err := privateConfig("global", []string{"/a\nb"}); err == nil {
		t.Errorf("a path with a newline must be refused, not written into the config")
	}
}

// TestCeilingStopsDiscoveryAbovePool: from the pool directory, and from a non-repository
// directory inside the pool, a repository in an ancestor is found without the ceiling and not
// with it, and inside the clone the ceiling changes nothing. git applies a ceiling only below
// it, so this fails if the ceiling is the pool directory itself.
func TestCeilingStopsDiscoveryAbovePool(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("TTORCH_HOME", filepath.Join(root, ".ttorch"))
	base := gitIsolatedEnv("GIT_CONFIG_GLOBAL=/dev/null")
	mustGit(t, base, root, "init", "-q") // a repository in an ancestor, like a dotfiles repo in $HOME
	slot := cloneSlot(t, "repo-0123abcd", "1")
	sub := filepath.Join(slot, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	pool := filepath.Dir(slot)
	withCeiling := append(base[:len(base):len(base)], envGitCeiling+"="+GitEnvFor(slot).CeilingDirectories)

	if got := mustGit(t, base, pool, "rev-parse", "--show-toplevel"); got != root {
		t.Fatalf("setup: without the ceiling the pool directory should resolve to the ancestor repo, got %q", got)
	}
	if out, err := runGit(t, withCeiling, pool, "rev-parse", "--show-toplevel"); err == nil {
		t.Errorf("with the ceiling, the pool directory still found a repository: %q", out)
	}
	stray := filepath.Join(pool, "stray")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(t, withCeiling, stray, "rev-parse", "--show-toplevel"); err == nil {
		t.Errorf("with the ceiling, a directory in the pool still found a repository: %q", out)
	}
	if got := mustGit(t, withCeiling, sub, "rev-parse", "--show-toplevel"); got != slot {
		t.Errorf("inside the clone the ceiling must change nothing: got %q, want %q", got, slot)
	}
	// The documented limit: git ignores a ceiling equal to the directory it runs from, so from
	// the clones root itself discovery still reaches the ancestor repository.
	if got := mustGit(t, withCeiling, clonesRoot(), "rev-parse", "--show-toplevel"); got != root {
		t.Errorf("from the clones root git should still find the ancestor (git's ceiling rule); got %q", got)
	}
}

// stubSystemConfig points the system-config discovery at path for the test.
func stubSystemConfig(t *testing.T, path string) {
	t.Helper()
	old := systemConfigPath
	t.Cleanup(func() { systemConfigPath = old })
	systemConfigPath = func() string { return path }
}

// TestPrivateSystemConfigAbsorbsSystemWrites: under the env block a clone worker is given,
// `git config --system` writes the slot's private system file, not the system file every git
// process on the machine reads, while the system file's settings still apply through the
// include. The test's own GIT_CONFIG_SYSTEM is a decoy that the worker env must override, so a
// regression writes the decoy and never the machine's real system config.
func TestPrivateSystemConfigAbsorbsSystemWrites(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("GIT_CONFIG_SYSTEM", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	t.Setenv("TTORCH_HOME", filepath.Join(root, ".ttorch"))
	realSystem := filepath.Join(root, "etc-gitconfig")
	const systemBody = "[ttorchtest]\n\tfromsystem = yes\n"
	if err := os.WriteFile(realSystem, []byte(systemBody), 0o644); err != nil {
		t.Fatal(err)
	}
	stubSystemConfig(t, realSystem)
	decoy := filepath.Join(root, "decoy-system")
	slot := cloneSlot(t, "repo-0123abcd", "1")
	if err := WriteWorkerSettings("claude", slot); err != nil {
		t.Fatal(err)
	}
	settingsEnv, _ := readSettingsEnv(t, slot)
	workerEnv := append(envWithoutGit(), "GIT_CONFIG_SYSTEM="+decoy)
	workerEnv = append(workerEnv, envList(settingsEnv)...)

	if got, _ := runGit(t, workerEnv, slot, "config", "ttorchtest.fromsystem"); got != "yes" {
		t.Errorf("the system config's settings should apply through the include; got %q", got)
	}
	mustGit(t, workerEnv, slot, "config", "--system", "core.fsmonitor", "echo planted")
	if b, _ := os.ReadFile(realSystem); string(b) != systemBody {
		t.Errorf("git config --system changed the real system file:\n%s", b)
	}
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Errorf("git config --system wrote outside the private file (decoy stat err = %v)", err)
	}
	priv, err := os.ReadFile(settingsEnv[envGitSystem])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(priv), "planted") {
		t.Errorf("git config --system did not land in the private file:\n%s", priv)
	}
}

// TestLeadSystemConfigs: the include follows what the lead's git reads at the system level.
func TestLeadSystemConfigs(t *testing.T) {
	stubSystemConfig(t, "/compiled/in/gitconfig")
	t.Setenv("GIT_CONFIG_SYSTEM", "")
	for _, v := range []string{"1", "true", "yes", "on", "2"} {
		t.Setenv("GIT_CONFIG_NOSYSTEM", v)
		if got := leadSystemConfigs(); got != nil {
			t.Errorf("GIT_CONFIG_NOSYSTEM=%s: git reads no system config, includes = %q", v, got)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off"} {
		t.Setenv("GIT_CONFIG_NOSYSTEM", v)
		if got := leadSystemConfigs(); len(got) != 1 || got[0] != "/compiled/in/gitconfig" {
			t.Errorf("GIT_CONFIG_NOSYSTEM=%q: includes = %q, want git's compiled-in path", v, got)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	t.Setenv("GIT_CONFIG_SYSTEM", "/lead/system")
	if got := leadSystemConfigs(); len(got) != 1 || got[0] != "/lead/system" {
		t.Errorf("with GIT_CONFIG_SYSTEM set git reads only that file; includes = %q", got)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", "")
	stubSystemConfig(t, "")
	if got := leadSystemConfigs(); got != nil {
		t.Errorf("when git cannot say where its system config is, include nothing; got %q", got)
	}
}

// TestSystemConfigPathAsksGit: the real discovery returns an absolute path and leaves whatever
// is at that path as it was (git seeds a missing file only for --global).
func TestSystemConfigPathAsksGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	p := systemConfigPath()
	if !filepath.IsAbs(p) {
		t.Fatalf("systemConfigPath() = %q, want an absolute path", p)
	}
	before, beforeErr := os.Lstat(p)
	if again := systemConfigPath(); again != p {
		t.Errorf("systemConfigPath is not stable: %q then %q", p, again)
	}
	after, afterErr := os.Lstat(p)
	if os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) || (beforeErr == nil && !after.ModTime().Equal(before.ModTime())) {
		t.Errorf("asking git for the system config path changed %s", p)
	}
}

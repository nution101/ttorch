package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nution101/ttorch/internal/paths"
)

// A worker whose working directory is a per-worker clone (TTORCH_WORKER_CLONES) runs with three
// git variables that a worker in a linked worktree does not get:
//
//   - GIT_CEILING_DIRECTORIES is the clones root, so git run from a pool directory, or from any
//     directory in a pool that is not inside a clone, never discovers a repository in an
//     ancestor (a dotfiles repo in $HOME, say). From inside the clone and its subdirectories
//     discovery is unchanged. The root and not the pool directory, because git applies a
//     ceiling only to directories strictly below it: with the pool directory as the ceiling,
//     git run from the pool directory itself still walks up into the ancestor repository. For
//     the same reason it does nothing for git run from the clones root itself or above it.
//   - GIT_CONFIG_GLOBAL is a private file per slot that includes the lead's global config, so
//     identity and includeIf rules still resolve while `git config --global` writes the
//     private file instead of the lead's ~/.gitconfig.
//   - GIT_CONFIG_SYSTEM is a second private file per slot that includes the system config the
//     lead's git reads, so `git config --system` writes the private file instead of a system
//     file that every git process on the machine reads, the scheduler's gate and land
//     included. Where git is installed under the lead's own user, that system file is
//     writable by a worker.
//
// These close the routes git itself offers a worker. They do not stop a worker naming the
// lead's repository explicitly (`git -C <main>`, `cd <main>`, GIT_DIR=<main>/.git) or writing
// a config file by path: a worker runs as the lead's OS user, and that residual stays open.
//
// GIT_DIR and GIT_WORK_TREE are deliberately not set: exported, they break every test fixture
// that runs `git init` in a temp directory.
//
// The variables reach the worker three ways, because each covers a gap in the others: the
// launch prefix (a fresh spawn), the resume command (a restore, which has no launch prefix),
// and the env block of the worker's settings.local.json (which the harness applies to the
// processes its tools start, and which survives a resume). A worker in a linked worktree gets
// none of them, so with the flag off every launch, resume and settings file is what it was.

const (
	envGitCeiling = "GIT_CEILING_DIRECTORIES"
	envGitGlobal  = "GIT_CONFIG_GLOBAL"
	envGitSystem  = "GIT_CONFIG_SYSTEM"
)

// WorkerGitEnv is the git environment of a clone worker. The zero value means "not a clone":
// no variables.
type WorkerGitEnv struct {
	CeilingDirectories string // GIT_CEILING_DIRECTORIES: the clones root, the pool's parent
	ConfigGlobal       string // GIT_CONFIG_GLOBAL: the slot's private global config file
	ConfigSystem       string // GIT_CONFIG_SYSTEM: the slot's private system config file
}

// IsZero reports whether e sets no variables.
func (e WorkerGitEnv) IsZero() bool { return e == WorkerGitEnv{} }

// pairs returns the variables in a fixed order, or nil for the zero value.
func (e WorkerGitEnv) pairs() [][2]string {
	if e.IsZero() {
		return nil
	}
	return [][2]string{{envGitCeiling, e.CeilingDirectories}, {envGitGlobal, e.ConfigGlobal}, {envGitSystem, e.ConfigSystem}}
}

// clonesRoot is the directory clone pools live under: ~/.ttorch/clones/<repo>-<hash8>/<N>.
func clonesRoot() string { return filepath.Join(paths.Default().Home, "clones") }

// IsCloneWorkdir reports whether workdir is a per-worker clone slot: a numeric directory in a
// pool directly under the clones root, whose .git is a real directory. A linked worktree's .git
// is a file, and a symlinked .git is refused, so neither reads as a clone. The kind is read
// from the workdir itself, which is how a task keeps the kind it was spawned with whatever the
// flag says now.
func IsCloneWorkdir(workdir string) bool {
	if workdir == "" || !filepath.IsAbs(workdir) {
		return false
	}
	workdir = filepath.Clean(workdir)
	pool := filepath.Dir(workdir)
	if filepath.Dir(pool) != filepath.Clean(clonesRoot()) || !isSlotName(filepath.Base(workdir)) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(workdir, ".git"))
	return err == nil && fi.IsDir()
}

func isSlotName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// GitEnvFor returns the git environment for a worker running in workdir: for a clone slot, the
// clones root as the ceiling and <pool>/.global-<N> and <pool>/.system-<N> as the private
// global and system config; the zero value for anything else.
func GitEnvFor(workdir string) WorkerGitEnv {
	if !IsCloneWorkdir(workdir) {
		return WorkerGitEnv{}
	}
	workdir = filepath.Clean(workdir)
	pool := filepath.Dir(workdir)
	return WorkerGitEnv{
		CeilingDirectories: filepath.Dir(pool),
		ConfigGlobal:       filepath.Join(pool, ".global-"+filepath.Base(workdir)),
		ConfigSystem:       filepath.Join(pool, ".system-"+filepath.Base(workdir)),
	}
}

// gitEnvPrefix renders the git environment for workdir as shell assignments ending in a
// space, ready to prepend to a command, or "" when workdir is not a clone.
func gitEnvPrefix(workdir string) string {
	var parts []string
	for _, kv := range GitEnvFor(workdir).pairs() {
		parts = append(parts, kv[0]+"="+shq(kv[1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// leadGlobalConfigs returns the files git reads as the lead's global config, in git's read
// order, so including them in that order keeps the lead's precedence. When the lead has set
// GIT_CONFIG_GLOBAL, git reads only that file. Otherwise it reads the XDG file and then
// ~/.gitconfig. Both are included whether or not they exist yet: git skips a missing include.
func leadGlobalConfigs() []string {
	if v := os.Getenv(envGitGlobal); v != "" {
		return []string{v}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return []string{filepath.Join(xdg, "git", "config"), filepath.Join(home, ".gitconfig")}
}

// leadSystemConfigs returns the system config file the lead's git reads: none when the lead
// has GIT_CONFIG_NOSYSTEM set, the lead's GIT_CONFIG_SYSTEM when set, and otherwise the path
// compiled into git, which only git can say. If git cannot say, the worker gets no system
// config, which fails toward the worker losing a setting rather than writing a shared file.
func leadSystemConfigs() []string {
	if noSystemConfig(os.Getenv("GIT_CONFIG_NOSYSTEM")) {
		return nil
	}
	if v := os.Getenv(envGitSystem); v != "" {
		return []string{v}
	}
	if p := systemConfigPath(); p != "" {
		return []string{p}
	}
	return nil
}

// noSystemConfig reads GIT_CONFIG_NOSYSTEM the way git reads a boolean environment value.
func noSystemConfig(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return false
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	}
	n, err := strconv.Atoi(v)
	return err != nil || n != 0
}

// systemConfigPath asks git where its system config is. `git config --system --edit` hands
// that path to the editor, so with `echo` as the editor it prints the path. For the system
// level git creates nothing (only --global seeds a missing file). It runs from / with every
// GIT_ variable dropped and no global config, so neither the caller's directory nor a
// worker-written global file can steer it. A package var so tests can stub it.
var systemConfigPath = func() string {
	cmd := exec.Command("git", "config", "--system", "--edit")
	cmd.Dir = "/"
	cmd.Env = append(envWithoutGit(), "GIT_EDITOR=echo", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\n\r") {
		return ""
	}
	return p
}

// envWithoutGit is the process environment with every GIT_ variable removed.
func envWithoutGit() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// privateConfig renders the body of a slot's private config file: one include per file the
// lead's git reads at that level. A path git cannot hold in a quoted config value is an error
// rather than a mangled include.
func privateConfig(level string, includes []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by ttorch at spawn. It includes the lead's %s git config so its settings\n", level)
	fmt.Fprintf(&b, "# still apply, and it absorbs `git config --%s` writes so they never reach the\n", level)
	b.WriteString("# lead's file. It is rewritten for every task in this slot.\n")
	if len(includes) == 0 {
		return b.String(), nil
	}
	b.WriteString("[include]\n")
	for _, p := range includes {
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", fmt.Errorf("%s git config path %q has a control character", level, p)
		}
		p = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p)
		fmt.Fprintf(&b, "\tpath = \"%s\"\n", p)
	}
	return b.String(), nil
}

// writePrivateConfig writes a slot's private config file fresh for a new task, so nothing the
// previous worker in the slot wrote with `git config --global` or `--system` carries over. It
// replaces the file by rename, which replaces a symlink planted at the path rather than
// writing through it. If the write fails it removes the old file, so the worker never starts
// on the previous task's keys.
func writePrivateConfig(path, level string, includes []string) (err error) {
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	body, err := privateConfig(level, includes)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// prepareWorkerGitEnv sets up a clone worker's git environment for a new task: it rewrites the
// slot's private global and system config files and returns the variables. For anything that
// is not a clone slot it returns nil and touches nothing.
func prepareWorkerGitEnv(workdir string) (map[string]string, error) {
	e := GitEnvFor(workdir)
	if e.IsZero() {
		return nil, nil
	}
	if err := writePrivateConfig(e.ConfigGlobal, "global", leadGlobalConfigs()); err != nil {
		return nil, fmt.Errorf("writing the worker's private global git config: %w", err)
	}
	if err := writePrivateConfig(e.ConfigSystem, "system", leadSystemConfigs()); err != nil {
		return nil, fmt.Errorf("writing the worker's private system git config: %w", err)
	}
	env := map[string]string{}
	for _, kv := range e.pairs() {
		env[kv[0]] = kv[1]
	}
	return env, nil
}

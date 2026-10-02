// Package gittest keeps the git a test runs inside the repository the test names. It is
// imported only from tests.
//
// git reads the repository it acts on from the environment ahead of the working directory and
// -C. A suite run inside `git rebase --exec` inherits GIT_DIR naming the repository being
// rebased, so a fixture's `git -C <tempdir> init` or commit lands in that repository instead,
// and an init there can set core.bare=true in its shared config. Env and Command drop those
// variables for a fixture's own git, and Scrub drops them from the test process so the code
// under test, which inherits the environment, does not see them either.
package gittest

import (
	"os"
	"os/exec"
	"strings"
)

// repoVars are the variables that point git at a repository other than the one it would find
// from its working directory: what `git rev-parse --local-env-vars` lists, plus
// GIT_CEILING_DIRECTORIES and GIT_NAMESPACE. Identity and global-config variables such as
// GIT_AUTHOR_NAME and GIT_CONFIG_GLOBAL are not among them, so a fixture that sets those
// keeps them.
var repoVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_GRAFT_FILE",
	"GIT_SHALLOW_FILE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE",
	"GIT_PREFIX",
	"GIT_CONFIG",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	"GIT_CEILING_DIRECTORIES",
	"GIT_NAMESPACE",
}

func isRepoVar(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	for _, v := range repoVars {
		if name == v {
			return true
		}
	}
	// GIT_CONFIG_COUNT's numbered pairs.
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// Env is the process environment without the repository variables, plus extra.
func Env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !isRepoVar(kv) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// Command is `git -C dir args...` running with Env(). A caller that needs more, such as a
// fixed identity, appends to its Env.
func Command(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = Env()
	return cmd
}

// Scrub unsets the repository variables in the test process. A package's TestMain calls it
// before m.Run, so code under test that runs git with the inherited environment is covered
// as well as the fixtures. A test that sets one of them on purpose, with t.Setenv, still can.
func Scrub() {
	for _, kv := range os.Environ() {
		if isRepoVar(kv) {
			name, _, _ := strings.Cut(kv, "=")
			os.Unsetenv(name)
		}
	}
}

package doctor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// WorkerClonesEnvVar selects per-worker clones instead of linked worktrees for new spawns.
// Doctor reads it only to decide whether to check git's version against the clone floor.
const WorkerClonesEnvVar = "TTORCH_WORKER_CLONES"

// The oldest git a clone worker can run on. The binding feature is GIT_CONFIG_GLOBAL, which
// git 2.32 introduced: an older git ignores the variable, so a clone worker's private global
// config is never read and `git config --global` writes the lead's own file. The other
// features clones use are older (`git init --initial-branch` is 2.28, `git fetch
// --no-write-fetch-head` and `--no-auto-maintenance` are 2.29). These two constants are the
// only spelling of the floor.
const (
	gitCloneFloorMajor = 2
	gitCloneFloorMinor = 32
)

func gitCloneFloor() string {
	return strconv.Itoa(gitCloneFloorMajor) + "." + strconv.Itoa(gitCloneFloorMinor)
}

// gitVersion returns git's version banner ("git version 2.50.1"), or "" when git cannot be
// run. A var so tests can supply a banner.
var gitVersion = func() string {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseGitVersion pulls major and minor out of a banner such as "git version 2.50.1 (Apple
// Git-155)" or "git version 2.45.1.windows.1".
func parseGitVersion(banner string) ([2]int, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(banner), "git version ")
	if !ok {
		return [2]int{}, false
	}
	parts := strings.SplitN(strings.Fields(v + " ")[0], ".", 3)
	if len(parts) < 2 {
		return [2]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return [2]int{}, false
	}
	return [2]int{major, minor}, true
}

// reportGitCloneFloor prints the git version line when per-worker clones are switched on. With
// the flag off it prints nothing, so the report is what it was before clones existed.
func reportGitCloneFloor(out io.Writer, enabled bool, banner string) {
	if !enabled {
		return
	}
	got, ok := parseGitVersion(banner)
	shown := strings.TrimPrefix(strings.TrimSpace(banner), "git version ")
	switch {
	case !ok:
		fmt.Fprintf(out, "  git version: could not be read from %q; %s=1 needs git %s or newer\n", banner, WorkerClonesEnvVar, gitCloneFloor())
	case got[0] > gitCloneFloorMajor || (got[0] == gitCloneFloorMajor && got[1] >= gitCloneFloorMinor):
		fmt.Fprintf(out, "  git version: %s (at or above the %s that %s needs)\n", shown, gitCloneFloor(), WorkerClonesEnvVar)
	default:
		fmt.Fprintf(out, "  git version: %s — below %s, which %s needs: an older git ignores GIT_CONFIG_GLOBAL, so a clone worker's `git config --global` would write your own global config. Upgrade git, or unset %s\n",
			shown, gitCloneFloor(), WorkerClonesEnvVar, WorkerClonesEnvVar)
	}
}

// workerClonesEnabled reports whether WorkerClonesEnvVar reads as on.
func workerClonesEnabled() bool { return truthyEnv(os.Getenv(WorkerClonesEnvVar)) }

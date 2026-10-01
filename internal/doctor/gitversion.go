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

// GitCloneFloor is the oldest git per-worker clones run on, as major, minor, patch. 2.45.1
// carries the fixes for CVE-2024-32002, -32004, -32020, -32021 and -32465; before it, a
// fetch from a clone could lazy-fetch through a promisor remote the clone's own config names
// and run a program the worker chose. The clone import and ttorch sync also set
// GIT_NO_LAZY_FETCH, which 2.45 documents. Older floors are inside it: GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM, which a clone worker's private config files rely on, arrived in 2.32,
// `git init --initial-branch` in 2.28 and `git fetch --no-write-fetch-head` in 2.29. The fix
// was also released as 2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4 and 2.44.1; those are refused
// anyway, since one floor is simpler to state and to check than seven.
var GitCloneFloor = [3]int{2, 45, 1}

func gitCloneFloor() string {
	return fmt.Sprintf("%d.%d.%d", GitCloneFloor[0], GitCloneFloor[1], GitCloneFloor[2])
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

// parseGitVersion pulls major, minor and patch out of a banner such as "git version 2.50.1
// (Apple Git-155)", "git version 2.45.1.windows.1" or "git version 2.45.0.rc1". A missing or
// non-numeric patch reads as 0, so a release candidate never passes for the release.
func parseGitVersion(banner string) ([3]int, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(banner), "git version ")
	if !ok {
		return [3]int{}, false
	}
	parts := strings.SplitN(strings.Fields(v + " ")[0], ".", 4)
	if len(parts) < 2 {
		return [3]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return [3]int{}, false
	}
	patch := 0
	if len(parts) > 2 {
		if n, err := strconv.Atoi(parts[2]); err == nil && n >= 0 {
			patch = n
		}
	}
	return [3]int{major, minor, patch}, true
}

// atLeast reports whether version v is at or above floor.
func atLeast(v, floor [3]int) bool {
	for i := range v {
		if v[i] != floor[i] {
			return v[i] > floor[i]
		}
	}
	return true
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
	case atLeast(got, GitCloneFloor):
		fmt.Fprintf(out, "  git version: %s (at or above the %s that %s needs)\n", shown, gitCloneFloor(), WorkerClonesEnvVar)
	default:
		fmt.Fprintf(out, "  git version: %s — below %s, which %s needs: an older git can run a program a worker's clone names while ttorch fetches from it (CVE-2024-32004), and before 2.32 it ignores the private GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM a clone worker gets. Upgrade git, or unset %s\n",
			shown, gitCloneFloor(), WorkerClonesEnvVar, WorkerClonesEnvVar)
	}
}

// workerClonesEnabled reports whether WorkerClonesEnvVar reads as on.
func workerClonesEnabled() bool { return truthyEnv(os.Getenv(WorkerClonesEnvVar)) }

package doctor

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/nution101/ttorch/internal/clonepool"
	"github.com/nution101/ttorch/internal/worktree"
)

// The git floor for per-worker clones is the clone import's own check,
// worktree.ImportGitOK, so doctor warns about exactly the versions the import refuses. That
// floor is the CVE-2024-32004 fix: 2.45.1, or the backport for its series. Before it, a fetch
// from a clone could lazy-fetch through a promisor remote the clone's config names and run a
// program the worker chose. It also clears the older requirements clones have:
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM (2.32), which a clone worker's private config files
// rely on, and `git fetch --no-write-fetch-head` (2.29).

// gitVersion returns git's version banner ("git version 2.50.1"), or "" when git cannot be
// run. A var so tests can supply a banner.
var gitVersion = func() string {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// reportGitCloneFloor prints the git version line when per-worker clones are switched on. With
// the flag off it prints nothing, so the report is what it was before clones existed.
func reportGitCloneFloor(out io.Writer, enabled bool, banner string) {
	if !enabled {
		return
	}
	ok, readable := worktree.ImportGitOK(banner)
	shown := strings.TrimPrefix(strings.TrimSpace(banner), "git version ")
	switch {
	case !readable:
		fmt.Fprintf(out, "  git version: could not be read from %q; %s=1 needs git %s. Upgrade git, or unset %s\n",
			banner, clonepool.EnvVar, worktree.ImportGitFloor(), clonepool.EnvVar)
	case ok:
		fmt.Fprintf(out, "  git version: %s (carries the CVE-2024-32004 fix that %s needs)\n", shown, clonepool.EnvVar)
	default:
		fmt.Fprintf(out, "  git version: %s — does not carry the CVE-2024-32004 fix, which %s needs: an older git can run a program a worker's clone names while ttorch fetches from it, and before 2.32 it ignores the private GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM a clone worker gets. Needs %s; the version string decides, so a distro git with the fix backported under an older string is refused too. Upgrade git, or unset %s\n",
			shown, clonepool.EnvVar, worktree.ImportGitFloor(), clonepool.EnvVar)
	}
}

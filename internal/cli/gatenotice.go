package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/worktree"
)

// printGateNotices prints the trust-gate lines `ttorch update` (through the `install` it
// re-executes) and `ttorch doctor` show. For each project it says, once, which default branch a
// seed recorded (see orchestrator.SeedDefaultBranches), so a wrong guess is seen and can be
// changed; it names every git project with no default branch recorded, since the gate refuses
// those; and it names every trusted project whose default branch has no gate-change-approval
// line, since the human gate-change approval became off by default and installing a binary
// with that default silently drops the approval for exactly those. With status (doctor), it
// also prints, every time, what the gate reads for each trusted project (printGateStatus). It is
// best-effort and silent on any read failure, and it never creates the state database.
//
// From a worker's context (harness.WorkerContextSignal) it prints the same lines and writes
// nothing: the seed does not run and a notice is not cleared, so a worker that runs doctor in
// its own pane neither records a branch from refs it can write nor uses up the notice meant for
// the lead.
func printGateNotices(w io.Writer, p paths.Paths, status bool) {
	if _, err := os.Stat(p.StateDB()); err != nil {
		return
	}
	store, err := db.Open(p.StateDB())
	if err != nil {
		return
	}
	defer store.Close()
	ctx := context.Background()
	lead := harness.WorkerContextSignal() == ""
	if lead {
		_ = orchestrator.SeedDefaultBranches(ctx, store)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return
	}
	for _, proj := range projects {
		shown := status && projectIsTrusted(proj)
		if shown {
			printGateStatus(w, proj)
		}
		switch {
		case proj.DefaultBranchSeed == db.DefaultBranchSeedNotice && proj.DefaultBranch != "":
			fmt.Fprintf(w, "recorded %s as the default branch the trust gate reads for %s; if that is wrong, run '%s %d <branch>'\n",
				proj.DefaultBranch, proj.RepoPath, orchestrator.SetBranchCommand, proj.ID)
			if lead {
				_ = store.SetProjectDefaultBranchSeed(ctx, proj.ID, "")
			}
		case proj.DefaultBranch == "" && !shown && isRepoRoot(proj.RepoPath):
			fmt.Fprintf(w, "no default branch is recorded for %s, so the trust gate refuses it; run '%s %d <branch>'\n",
				proj.RepoPath, orchestrator.SetBranchCommand, proj.ID)
		}
		if gateChangeApprovalDefaulted(proj.RepoPath, proj.DefaultBranch) {
			fmt.Fprintf(w, "gate-change approval is now off by default for %s; add '- gate-change-approval: required' to its AGENTS.md to keep it\n", proj.RepoPath)
		}
	}
}

// projectIsTrusted reports whether doctor shows proj's gate status: its cached delivery mode or
// the mode its checkout's AGENTS.md records is trusted. Either is enough, so a worker that edits
// one of them does not drop the project from the status.
func projectIsTrusted(proj db.Project) bool {
	if proj.DeliveryMode == "trusted" {
		return true
	}
	mode, ok := projectinit.LiveMode(proj.RepoPath)
	return ok && mode == "trusted"
}

// printGateStatus prints what the trust gate reads for proj: the recorded default branch and
// the commit it is at now, the commit the last land left it at, and the URL origin resolves to.
// A worker can move the branch and redirect origin from its own worktree, since refs and git
// config are shared by every linked worktree, and nothing in ttorch prevents that (see "Known
// limit: shared git state" in docs/ARCHITECTURE.md). It warns when the branch no longer contains
// the last landed commit, and when origin's URL differs from the one recorded with the branch.
// That only shows a URL rewrite still in place when it runs, not a transport setting that
// redirects origin without changing the URL, or a rewrite already undone. It reads only.
func printGateStatus(w io.Writer, proj db.Project) {
	fmt.Fprintf(w, "trust gate for %s (project %d):\n", proj.RepoPath, proj.ID)
	var warnings []string
	switch base, err := worktree.ResolveGateBase(proj.RepoPath, proj.DefaultBranch); {
	case proj.DefaultBranch == "":
		fmt.Fprintf(w, "  default branch: none recorded, so the trust gate refuses it; run '%s %d <branch>'\n",
			orchestrator.SetBranchCommand, proj.ID)
	case err != nil:
		fmt.Fprintf(w, "  default branch: %s, which has no commit at refs/heads/%s, so the trust gate refuses it\n", proj.DefaultBranch, proj.DefaultBranch)
	default:
		fmt.Fprintf(w, "  default branch: %s at %s\n", proj.DefaultBranch, base.SHA)
		if warn := orchestrator.LastLandedWarning(proj.RepoPath, proj.LastLandedSHA, base); warn != "" {
			warnings = append(warnings, warn)
		}
	}
	if proj.LastLandedSHA != "" {
		fmt.Fprintf(w, "  last landed: %s\n", proj.LastLandedSHA)
	} else {
		fmt.Fprintln(w, "  last landed: none recorded")
	}
	origin := worktree.OriginURL(proj.RepoPath)
	fmt.Fprintf(w, "  origin: %s\n", originForDisplay(origin))
	if proj.DefaultBranch != "" && origin != proj.OriginURL {
		warnings = append(warnings, fmt.Sprintf(
			"warning: origin is %s, but it was %s when the default branch was recorded; the gate fetches and checks the land base against origin, and a worker can rewrite it. If you changed it, record it again with '%s %d %s'",
			originForDisplay(origin), originForDisplay(proj.OriginURL), orchestrator.SetBranchCommand, proj.ID, proj.DefaultBranch))
	}
	for _, warn := range warnings {
		fmt.Fprintf(w, "  %s\n", warn)
	}
}

// originForDisplay renders an origin URL for the terminal: "none" for a repository without
// one, and otherwise the URL with control characters escaped, since it comes from git config a
// worker can write.
func originForDisplay(url string) string {
	if url == "" {
		return "none"
	}
	return worktree.EscapeForTerminal(url)
}

// isRepoRoot reports whether dir is the top level of a git repository. Project rows are keyed
// by repo root, but a cc session registers its own directory, which need not be one. git
// reports the root with symlinks resolved, so dir is compared the same way.
func isRepoRoot(dir string) bool {
	root, err := worktree.RepoRoot(dir)
	if err != nil {
		return false
	}
	if root == dir {
		return true
	}
	real, err := filepath.EvalSymlinks(dir)
	return err == nil && real == root
}

// gateChangeApprovalDefaulted reports whether repo is trusted and its gate-change approval is
// off only because the default says so: the AGENTS.md committed on branch, the default branch
// recorded for the project (what the gate reads, see worktree.ResolveGateBase), records trusted
// mode and has no gate-change-approval line. A default branch the gate cannot read, or one
// without a trusted block, keeps the approval, so it is not reported.
func gateChangeApprovalDefaulted(repo, branch string) bool {
	if mode, ok := projectinit.LiveMode(repo); !ok || mode != "trusted" {
		return false
	}
	base, err := worktree.ResolveGateBase(repo, branch)
	if err != nil {
		return false
	}
	text, ok := worktree.ShowFile(repo, base.SHA, "AGENTS.md")
	if !ok || projectinit.ParseMode(text) != "trusted" {
		return false
	}
	return !projectinit.HasGateChangeApprovalLine(text)
}

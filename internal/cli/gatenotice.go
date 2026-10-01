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
// with that default silently drops the approval for exactly those. It is best-effort and silent
// on any read failure, and it never creates the state database.
//
// From a worker's context (harness.WorkerContextSignal) it prints the same lines and writes
// nothing: the seed does not run and a notice is not cleared, so a worker that runs doctor in
// its own pane neither records a branch from refs it can write nor uses up the notice meant for
// the lead.
func printGateNotices(w io.Writer, p paths.Paths) {
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
		switch {
		case proj.DefaultBranchSeed == db.DefaultBranchSeedNotice && proj.DefaultBranch != "":
			fmt.Fprintf(w, "recorded %s as the default branch the trust gate reads for %s; if that is wrong, run '%s %d <branch>'\n",
				proj.DefaultBranch, proj.RepoPath, orchestrator.SetBranchCommand, proj.ID)
			if lead {
				_ = store.SetProjectDefaultBranchSeed(ctx, proj.ID, "")
			}
		case proj.DefaultBranch == "" && isRepoRoot(proj.RepoPath):
			fmt.Fprintf(w, "no default branch is recorded for %s, so the trust gate refuses it; run '%s %d <branch>'\n",
				proj.RepoPath, orchestrator.SetBranchCommand, proj.ID)
		}
		if gateChangeApprovalDefaulted(proj.RepoPath, proj.DefaultBranch) {
			fmt.Fprintf(w, "gate-change approval is now off by default for %s; add '- gate-change-approval: required' to its AGENTS.md to keep it\n", proj.RepoPath)
		}
	}
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

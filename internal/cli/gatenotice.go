package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/worktree"
)

// printGateChangeApprovalNotices prints one line for every trusted project whose default
// branch has no gate-change-approval line. The human gate-change approval became off by
// default, so installing a binary with that default silently drops the approval for exactly
// these projects; `ttorch update` (through the `install` it re-executes) and `ttorch doctor`
// say so until the project sets the line either way. It is best-effort and silent on any
// read failure, and it never creates the state database.
func printGateChangeApprovalNotices(w io.Writer, p paths.Paths) {
	if _, err := os.Stat(p.StateDB()); err != nil {
		return
	}
	store, err := db.Open(p.StateDB())
	if err != nil {
		return
	}
	defer store.Close()
	projects, err := store.ListProjects(context.Background())
	if err != nil {
		return
	}
	for _, proj := range projects {
		if gateChangeApprovalDefaulted(proj.RepoPath) {
			fmt.Fprintf(w, "gate-change approval is now off by default for %s; add '- gate-change-approval: required' to its AGENTS.md to keep it\n", proj.RepoPath)
		}
	}
}

// gateChangeApprovalDefaulted reports whether repo is trusted and its gate-change approval is
// off only because the default says so: the AGENTS.md committed on its default branch (what
// the gate reads, see worktree.ResolveGateBase) records trusted mode and has no
// gate-change-approval line. A default branch the gate cannot read, or one without a trusted
// block, keeps the approval, so it is not reported.
func gateChangeApprovalDefaulted(repo string) bool {
	if mode, ok := projectinit.LiveMode(repo); !ok || mode != "trusted" {
		return false
	}
	base, err := worktree.ResolveGateBase(repo)
	if err != nil {
		return false
	}
	text, ok := worktree.ShowFile(repo, base.SHA, "AGENTS.md")
	if !ok || projectinit.ParseMode(text) != "trusted" {
		return false
	}
	return !projectinit.HasGateChangeApprovalLine(text)
}

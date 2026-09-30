package orchestrator

import (
	"context"
	"fmt"
	"os"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// SetBranchCommand is the command that records or changes a project's default branch, for
// the messages that tell the lead to run it.
const SetBranchCommand = "ttorch project set-branch"

// SeedDefaultBranches records a default branch for every project that migration 0010 marked
// as awaiting one, and marks it for the one-time notice `ttorch update` and `ttorch doctor`
// print so a wrong guess is seen. The branch comes from worktree.DetectDefaultBranch. A
// project whose branch cannot be detected (not a git repository, or a detached checkout with
// no usable origin/HEAD) is left without one, and the gate refuses it until the lead records
// one with `ttorch project set-branch`. Each row is tried once. It runs on every Manager open,
// and after the first run it only reads the project list.
func SeedDefaultBranches(ctx context.Context, store *db.Store) error {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.DefaultBranchSeed != db.DefaultBranchSeedPending {
			continue
		}
		if p.DefaultBranch == "" {
			if b, err := worktree.DetectDefaultBranch(p.RepoPath); err == nil {
				if wrote, err := store.FillProjectDefaultBranch(ctx, p.ID, b, true); err != nil {
					return err
				} else if wrote {
					continue
				}
			}
		}
		if err := store.SetProjectDefaultBranchSeed(ctx, p.ID, ""); err != nil {
			return err
		}
	}
	return nil
}

// registerDefaultBranch records repo's default branch on its project row at spawn, when none
// is recorded yet and no worker has had a checkout of the repository. Once a worker has one, the
// refs worktree.DetectDefaultBranch reads are ones it can have changed, so the branch is left
// for the lead to record and the gate refuses until then. The branch is marked for the
// one-time notice, since spawn has nowhere to show it. It is best-effort: a spawn does not
// depend on the branch, and every gate read refuses on its own without one.
func (m *Manager) registerDefaultBranch(ctx context.Context, repo string) {
	p, ok, err := m.Store.GetProjectByRepo(ctx, repo)
	if err != nil || (ok && p.DefaultBranch != "") {
		return
	}
	if ok {
		tasks, err := m.Store.ListTasks(ctx, db.TaskFilter{ProjectID: p.ID})
		if err != nil {
			return
		}
		for _, t := range tasks {
			if t.Kind != db.KindCC && t.Worktree != "" {
				return
			}
		}
	}
	b, err := worktree.DetectDefaultBranch(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: %v; record it with '%s %s <branch>' before the trust gate can run here\n", err, SetBranchCommand, repo)
		return
	}
	if !ok {
		if p, err = m.Store.UpsertProject(ctx, repo, ""); err != nil {
			return
		}
	}
	_, _ = m.Store.FillProjectDefaultBranch(ctx, p.ID, b, true)
}

// recordedDefaultBranch returns the default branch recorded for repo's project, or "" when the
// repo is not registered or has none recorded.
func (m *Manager) recordedDefaultBranch(repo string) string {
	p, ok, err := m.Store.GetProjectByRepo(context.Background(), repo)
	if err != nil || !ok {
		return ""
	}
	return p.DefaultBranch
}

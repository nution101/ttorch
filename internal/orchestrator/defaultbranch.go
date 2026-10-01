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
// one with `ttorch project set-branch`. Each row is tried once. It runs, outside a worker's
// context (harness.WorkerContextSignal), on every Manager open and in `ttorch update` and
// `ttorch doctor`; after the first run it only reads the project list.
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
			if b, err := seedDetect(p.RepoPath); err == nil {
				if wrote, err := store.FillProjectDefaultBranch(ctx, p.ID, b, true); err != nil {
					return err
				} else if wrote {
					continue
				}
			}
		}
		// Only while still pending: a registration that recorded the branch since the list was
		// read set its own notice, and clearing it would hide the branch from the lead.
		if err := store.ClearPendingDefaultBranchSeed(ctx, p.ID); err != nil {
			return err
		}
	}
	return nil
}

// seedDetect is the detection SeedDefaultBranches runs, a variable so a test can make a
// registration land between the seed's read of the project and its write.
var seedDetect = worktree.DetectDefaultBranch

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

// gateBase resolves the default branch the trust gate reads for repo: the branch recorded on
// its project row, resolved fully qualified to the commit it points at (worktree.ResolveGateBase).
// It never derives the branch from refs, so no ref a worker can write decides it. A repo with no
// recorded branch, or one whose recorded branch has no local ref, is an error that names the
// command that fixes it, and every gate read refuses on it.
func (m *Manager) gateBase(repo string) (worktree.GateBase, error) {
	p, ok, err := m.Store.GetProjectByRepo(context.Background(), repo)
	if err != nil {
		return worktree.GateBase{}, fmt.Errorf("could not read the project for %s: %w", repo, err)
	}
	branch := ""
	if ok {
		branch = p.DefaultBranch
	}
	b, err := worktree.ResolveGateBase(repo, branch)
	if err != nil {
		return worktree.GateBase{}, fmt.Errorf("%w; set it with '%s %s <branch>'", err, SetBranchCommand, repo)
	}
	return b, nil
}

// lastLandedWarning returns a warning when base, the default branch as the gate resolved it, no
// longer contains the commit the last successful land left it at (projects.last_landed_sha), and
// "" otherwise. A worker can move refs/heads/<default> itself from its worktree, and nothing in
// ttorch prevents that; this is how the move is noticed when it drops a commit a land put there.
// A move forward onto commits no land brought in still contains the last landed commit, so it
// is not noticed.
func (m *Manager) lastLandedWarning(repo string, base worktree.GateBase) string {
	p, ok, err := m.Store.GetProjectByRepo(context.Background(), repo)
	if err != nil || !ok || p.LastLandedSHA == "" {
		return ""
	}
	if p.LastLandedSHA == base.SHA || worktree.IsAncestor(repo, p.LastLandedSHA, base.SHA) {
		return ""
	}
	return fmt.Sprintf("warning: %s is at %s, which does not contain %s, the commit the last land left it at; something other than a ttorch land moved the branch",
		base.Name, short(base.SHA), short(p.LastLandedSHA))
}

// recordLastLanded records sha as the commit a successful merge or land left repo's default
// branch at. Best-effort, like recordDelivered: the merge has happened, and a failure only
// costs the next gate run its lastLandedWarning.
func (m *Manager) recordLastLanded(repo, sha string) {
	if _, err := m.Store.SetProjectLastLanded(context.Background(), repo, sha); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the last landed commit for %s: %v\n", repo, err)
	}
}

// defaultBranch is the default branch for the paths that are not gate reads: the recorded one
// when there is one, else worktree.DefaultBranch's guess.
func (m *Manager) defaultBranch(repo string) string {
	if b := m.recordedDefaultBranch(repo); b != "" {
		return b
	}
	return worktree.DefaultBranch(repo)
}

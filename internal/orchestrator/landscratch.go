package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/worktree"
)

// A land of a clone task rebases in the project repository, never in the clone. A worktree's
// land rebases the worktree itself, and the worker's branch ends up at the rebased commit. A
// clone's branch is the worker's own, and running git rebase there would run the clone's hooks,
// filters and merge drivers as the lead and leave a commit in a repository the gate does not
// read. So the land checks out the imported commit in a scratch worktree of the project,
// rebases it there, records the result under the task's clone refs and removes the scratch.
// The clone stays exactly as the worker left it, whether the rebase succeeds or conflicts, and
// the rebased commit is an object in the project like any other the gate validates and merges.

// cloneRebasedRef is the ref in the project repository that holds the commit the last land of
// task produced by rebasing its clone's head. It sits beside the task's imports, so dropping a
// task's clone refs drops it too.
func cloneRebasedRef(task string) string { return worktree.CloneRefs + task + "/rebased" }

// rebaseInScratch rebases the commit sha onto onto in a scratch worktree of repo, holds the
// result under cloneRebasedRef(task) and returns it. Both commits must already be objects in
// repo: sha is a clone's head the gate imported, and onto is the base the land resolved.
//
// The scratch is a detached worktree of repo in its own temporary directory, outside every
// repository and review root, with a unique name so concurrent lands do not share one, as
// validate's checkouts do. It is removed on every path. The rebase runs with an empty hooks
// directory, so none of repo's rebase hooks runs over the worker's commits, and with
// --no-update-refs, so the only ref of repo's it moves is the one written here. A rebase that
// fails, a conflict above all, wraps ErrLandRebaseConflict and leaves no ref behind; a failure
// to set the scratch up or to record its result does not, because nothing about the overlap is
// known then.
func rebaseInScratch(repo, task, sha, onto string) (string, error) {
	if !worktree.ValidTaskID(task) {
		return "", fmt.Errorf("scratch rebase: invalid task id %q", task)
	}
	if !worktree.ValidObjectID(sha) || !worktree.ValidObjectID(onto) {
		return "", fmt.Errorf("scratch rebase: %q onto %q is not a pair of object ids", sha, onto)
	}
	parent, err := os.MkdirTemp("", "ttorch-land-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(parent)
	hooks := filepath.Join(parent, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return "", err
	}
	scratch := filepath.Join(parent, filepath.Base(parent))
	// Registered before the add: AddDetached reports a path collision after git has already
	// created the worktree, and that one must be removed too.
	defer worktree.RemoveWorktree(repo, scratch)
	if err := worktree.AddDetached(repo, scratch, sha); err != nil {
		return "", fmt.Errorf("scratch rebase: checking out %s in %s: %w", short(sha), repo, err)
	}

	args := append([]string{"-c", "core.hooksPath=" + hooks}, scratchIdentity(scratch)...)
	if _, err := gitOut(scratch, append(args, "rebase", "--no-update-refs", onto)...); err != nil {
		return "", fmt.Errorf("rebasing %s onto %s in a scratch worktree of %s: %s: %w",
			short(sha), short(onto), repo, worktree.EscapeForTerminal(err.Error()), ErrLandRebaseConflict)
	}
	out, err := gitOut(scratch, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("scratch rebase: reading the rebased head: %w", err)
	}
	rebased := strings.TrimSpace(out)
	if !worktree.ValidObjectID(rebased) {
		return "", fmt.Errorf("scratch rebase: the rebased head %q is not an object id", rebased)
	}
	if _, err := gitOut(repo, "-c", "core.hooksPath="+hooks, "update-ref", cloneRebasedRef(task), rebased); err != nil {
		return "", fmt.Errorf("scratch rebase: recording %s as %s: %w", short(rebased), cloneRebasedRef(task), err)
	}
	return rebased, nil
}

// scratchIdentity returns -c user.name and user.email flags only when the scratch has no
// committer identity configured, as worktree.Rebase does for a worker's tree. The rebase writes
// new commits and fails with "empty ident name" on a machine with no identity, so a placeholder
// is the last resort and a configured identity always wins.
func scratchIdentity(scratch string) []string {
	name, _ := gitOut(scratch, "config", "user.name")
	email, _ := gitOut(scratch, "config", "user.email")
	if strings.TrimSpace(name) != "" && strings.TrimSpace(email) != "" {
		return nil
	}
	return []string{"-c", "user.name=ttorch", "-c", "user.email=ttorch@localhost"}
}

// landRebaseClone is landPrep's rebase step for a clone task whose head preRebase is behind
// the land's base. It returns the rebased commit, which rebaseInScratch leaves in the project.
//
// An ungated land outside pr mode is refused before anything is rebased. Its merge needs an
// approval of the exact commit that lands, and `ttorch approve` pins the clone's head, which the
// scratch rebase never moves, so no approval could ever cover the rebased commit. A worktree's
// land gets past this by rebasing the worktree, so the next approve reads the rebased head. For
// a clone the worker rebases its own branch instead; then the land has nothing to replay.
func landRebaseClone(spec landSpec, base, baseSha, preRebase string) (string, error) {
	if !spec.gated && spec.mode != "pr" {
		return "", fmt.Errorf("land: %q is behind %s, and an ungated land of a clone task cannot carry its approval onto the commit a rebase would make; have the worker rebase its branch onto %s, approve that commit, then re-run land", spec.taskID, base, baseSha)
	}
	rebased, err := rebaseInScratch(spec.repo, spec.taskID, preRebase, baseSha)
	if errors.Is(err, ErrLandRebaseConflict) {
		return "", fmt.Errorf("land: rebasing %q onto %s hit conflicts (real overlap with changes already on %s); the rebase ran in a scratch worktree of the project and the clone was not touched. Have the worker rebase its branch onto %s and resolve the overlap there, then re-run land: %w", spec.taskID, base, spec.def, baseSha, err)
	}
	if err != nil {
		return "", fmt.Errorf("land: %q: %w", spec.taskID, err)
	}
	return rebased, nil
}

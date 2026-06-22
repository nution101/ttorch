// Package worktree provides isolated git worktrees for workers, so parallel agents
// on one repository never collide and never touch the lead's checkout.
//
// This is a lean implementation built on the git CLI: one detached worktree per
// task. A future milestone can upgrade it to a reusable, dependency-warm pool with
// process-aware reclamation (the treehouse model).
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// RepoRoot returns the top-level directory of the git repo containing dir.
func RepoRoot(dir string) (string, error) {
	return git("-C", dir, "rev-parse", "--show-toplevel")
}

func poolName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Base(repo) + "-" + hex.EncodeToString(sum[:])[:8]
}

// Acquire returns an isolated worktree for taskID under root, creating it (detached
// at the repo's current HEAD) if it does not exist. Reusing an existing path lets a
// re-spawn land in the same worktree.
func Acquire(root, repo, taskID string) (string, error) {
	pool := filepath.Join(root, poolName(repo))
	wt := filepath.Join(pool, taskID)
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		return wt, nil
	}
	if err := os.MkdirAll(pool, 0o755); err != nil {
		return "", err
	}
	if _, err := git("-C", repo, "worktree", "add", "--detach", wt); err != nil {
		return "", err
	}
	return wt, nil
}

// IsDirty reports whether the worktree has uncommitted tracked or untracked changes.
func IsDirty(wt string) (bool, error) {
	out, err := git("-C", wt, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Remove returns a worktree to the repo: removes it and prunes the registration.
func Remove(repo, wt string) error {
	_, rmErr := git("-C", repo, "worktree", "remove", "--force", wt)
	_ = os.RemoveAll(wt)
	_, _ = git("-C", repo, "worktree", "prune")
	return rmErr
}

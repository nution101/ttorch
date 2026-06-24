// Package worktree manages a pool of reusable, isolated git worktrees so parallel
// workers never collide and never touch the lead's checkout. Reusing a clean idle
// worktree (resetting tracked files but keeping untracked build/dependency caches)
// is far faster than cloning per task.
//
// ttorch's own task records are the source of truth for which slots are in use, so
// the pool needs no separate reservation or liveness bookkeeping.
package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"
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

func headCommit(repo string) (string, error) {
	return git("-C", repo, "rev-parse", "HEAD")
}

func poolName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Base(repo) + "-" + hex.EncodeToString(sum[:])[:8]
}

// DefaultMax bounds the number of worktrees per repository.
const DefaultMax = 16

// MaxFromEnv returns the configured pool size (TTORCH_MAX_WORKTREES) or the default.
func MaxFromEnv() int {
	if v := os.Getenv("TTORCH_MAX_WORKTREES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMax
}

// Pool is a per-repository worktree pool rooted at Root.
type Pool struct {
	Root string
	Max  int
}

func (p Pool) dir(repo string) string { return filepath.Join(p.Root, poolName(repo)) }

// Acquire returns a worktree for a new task: it reuses a clean idle slot (one not
// in inUse and free of tracked changes), resetting it to the repo's HEAD while
// keeping untracked caches; otherwise it creates a new slot up to Max.
func (p Pool) Acquire(repo string, inUse []string) (string, error) {
	poolDir := p.dir(repo)
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		return "", err
	}
	unlock, err := lock(poolDir)
	if err != nil {
		return "", err
	}
	defer unlock()

	busy := map[string]bool{}
	for _, w := range inUse {
		if abs, err := filepath.Abs(w); err == nil {
			busy[abs] = true
		}
	}

	slots := listSlots(poolDir)
	for _, s := range slots {
		abs, _ := filepath.Abs(s)
		if busy[abs] {
			continue
		}
		tracked, err := HasTrackedChanges(s)
		if err != nil || tracked {
			continue // skip slots with orphaned uncommitted work or unreadable state
		}
		if err := reset(s, repo); err != nil {
			continue
		}
		return s, nil
	}

	if len(slots) >= p.Max {
		return "", fmt.Errorf("worktree pool full (max %d); tear down a worker first", p.Max)
	}
	s := filepath.Join(poolDir, strconv.Itoa(nextIndex(slots)))
	if _, err := git("-C", repo, "worktree", "add", "--detach", s); err != nil {
		return "", err
	}
	return s, nil
}

// Release resets a finished slot to a clean tracked state and keeps it for reuse.
func (p Pool) Release(repo, slot string) error {
	return reset(slot, repo)
}

// Destroy removes a slot from the repo entirely (for prune/uninstall).
func (p Pool) Destroy(repo, slot string) error {
	_, rmErr := git("-C", repo, "worktree", "remove", "--force", slot)
	_ = os.RemoveAll(slot)
	_, _ = git("-C", repo, "worktree", "prune")
	return rmErr
}

// IsDirty reports any uncommitted change (tracked or untracked) — used as the
// teardown safety check.
func IsDirty(slot string) (bool, error) {
	out, err := git("-C", slot, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// HasTrackedChanges reports uncommitted changes to tracked files (ignoring
// untracked files). A fast-forward merge is safe with untracked files present, so
// this is the right cleanliness gate for merge-local.
func HasTrackedChanges(path string) (bool, error) {
	out, err := git("-C", path, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// IsClean reports whether path has NO pending changes at all — nothing staged,
// unstaged, or untracked (`git status --porcelain` empty). Unlike HasTrackedChanges
// (the merge-target gate, which tolerates untracked files), this is the gate for a
// worker's own worktree before trust review: it guarantees the state being reviewed
// and validated is exactly the committed HEAD that will be merged.
func IsClean(path string) (bool, error) {
	out, err := git("-C", path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

func reset(slot, repo string) error {
	head, err := headCommit(repo)
	if err != nil {
		return err
	}
	_, err = git("-C", slot, "reset", "--hard", "-q", head)
	return err
}

func listSlots(poolDir string) []string {
	entries, err := os.ReadDir(poolDir)
	if err != nil {
		return nil
	}
	type slot struct {
		n    int
		path string
	}
	var slots []slot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(e.Name()); err == nil {
			slots = append(slots, slot{n, filepath.Join(poolDir, e.Name())})
		}
	}
	// ascending by slot number for stable reuse
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0 && slots[j-1].n > slots[j].n; j-- {
			slots[j-1], slots[j] = slots[j], slots[j-1]
		}
	}
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.path
	}
	return out
}

func nextIndex(slots []string) int {
	max := 0
	for _, s := range slots {
		if n, err := strconv.Atoi(filepath.Base(s)); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// --- repo/branch helpers for the delivery lifecycle ---

// DefaultBranch returns the repo's default branch name (origin/HEAD, else
// main/master, else the current branch).
func DefaultBranch(repo string) string {
	if out, err := git("-C", repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(out), "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git("-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	if out, err := git("-C", repo, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	return "main"
}

// Head returns the HEAD commit of a repo or worktree.
func Head(path string) (string, error) { return git("-C", path, "rev-parse", "HEAD") }

// CurrentBranch returns the checked-out branch name (or "HEAD" if detached).
func CurrentBranch(repo string) (string, error) {
	return git("-C", repo, "rev-parse", "--abbrev-ref", "HEAD")
}

// IsAncestor reports whether commit a is an ancestor of commit b (so b can
// fast-forward from a).
func IsAncestor(repo, a, b string) bool {
	_, err := git("-C", repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// ShowFile returns the contents of repoPath as it exists at ref in repo
// (`git show <ref>:<repoPath>`), and whether that file exists there. It is read-only
// and never touches any working tree — the trust gate uses it to read the gate
// definition from the default branch rather than the worker-controlled worktree copy.
func ShowFile(repo, ref, repoPath string) (string, bool) {
	out, err := exec.Command("git", "-C", repo, "show", ref+":"+repoPath).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// ChangedFiles returns the repo-relative paths changed between base and the COMMITTED
// rev (`git diff --name-only base rev`) — committed objects, never the working tree.
// Used to detect when a worker's committed diff touches the trust gate's definition.
func ChangedFiles(path, base, rev string) ([]string, error) {
	out, err := git("-C", path, "diff", "--name-only", base, rev)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// DiffCommitted returns the diff between base and a COMMITTED rev (`git diff base rev`),
// i.e. the changes in the committed object — NOT the working tree. The trust gate shows
// reviewers this so a worker cannot present a benign working tree while a different
// commit is what actually fast-forwards.
func DiffCommitted(path, base, rev string) (string, error) {
	return git("-C", path, "diff", base, rev)
}

// AddDetached creates a temporary linked worktree at dir checked out (detached) to rev,
// materializing exactly that committed tree in isolation from any live worktree, so it
// can be validated free of mutation by a running worker. The caller must RemoveWorktree
// it when done. dir must not already exist.
func AddDetached(repo, dir, rev string) error {
	_, err := git("-C", repo, "worktree", "add", "--detach", dir, rev)
	return err
}

// RemoveWorktree removes a linked worktree created by AddDetached.
func RemoveWorktree(repo, dir string) error {
	_, err := git("-C", repo, "worktree", "remove", "--force", dir)
	return err
}

// Diff returns the diff of a worktree against base (working tree vs base ref).
func Diff(path, base string, stat bool) (string, error) {
	args := []string{"-C", path, "diff"}
	if stat {
		args = append(args, "--stat")
	}
	if base != "" {
		args = append(args, base)
	}
	return git(args...)
}

// MergeFastForward fast-forwards the repo's current branch to commit (refusing a
// non-fast-forward merge).
func MergeFastForward(repo, commit string) error {
	_, err := git("-C", repo, "merge", "--ff-only", commit)
	return err
}

// Fetch updates remotes and prunes deleted remote branches.
func Fetch(repo string) error {
	_, err := git("-C", repo, "fetch", "--prune", "--quiet")
	return err
}

// GoneBranches lists local branches whose upstream is gone.
func GoneBranches(repo string) ([]string, error) {
	out, err := git("-C", repo, "for-each-ref", "--format=%(refname:short) %(upstream:track)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[gone]") {
			gone = append(gone, strings.Fields(line)[0])
		}
	}
	return gone, nil
}

// DeleteBranch force-deletes a local branch.
func DeleteBranch(repo, branch string) error {
	_, err := git("-C", repo, "branch", "-D", branch)
	return err
}

// RemoteExists reports whether the repo has a remote with the given name.
func RemoteExists(repo, name string) bool {
	_, err := git("-C", repo, "remote", "get-url", name)
	return err == nil
}

// RefExists reports whether ref resolves to a commit in repo (e.g. "origin/main").
func RefExists(repo, ref string) bool {
	_, err := git("-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// ResolveRef returns the commit sha that ref points to in repo.
func ResolveRef(repo, ref string) (string, error) {
	return git("-C", repo, "rev-parse", ref)
}

// Rebase replays the commits in dir's current HEAD onto onto (`git rebase onto`),
// leaving HEAD at the rebased tip. On conflict it returns an error WITHOUT cleaning
// up — the caller must RebaseAbort to restore the original HEAD.
func Rebase(dir, onto string) error {
	_, err := git("-C", dir, "rebase", onto)
	return err
}

// RebaseAbort aborts an in-progress rebase in dir, restoring the pre-rebase HEAD.
func RebaseAbort(dir string) error {
	_, err := git("-C", dir, "rebase", "--abort")
	return err
}

// Push pushes refspec to remote from repo (`git push remote refspec`), e.g.
// "<sha>:refs/heads/<branch>" to publish a detached worktree's commit as a branch.
func Push(repo, remote, refspec string) error {
	_, err := git("-C", repo, "push", remote, refspec)
	return err
}

func lock(poolDir string) (func(), error) {
	lp := filepath.Join(poolDir, ".lock")
	for i := 0; i < 50; i++ {
		if err := os.Mkdir(lp, 0o755); err == nil {
			return func() { _ = os.Remove(lp) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("could not acquire worktree pool lock")
}

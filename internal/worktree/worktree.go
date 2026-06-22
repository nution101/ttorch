// Package worktree manages a pool of reusable, isolated git worktrees so parallel
// workers never collide and never touch the lead's checkout. Reusing a clean idle
// worktree (resetting tracked files but keeping untracked build/dependency caches)
// is far faster than cloning per task.
//
// The pool design follows treehouse (MIT) — see THIRD_PARTY.md. orcha's own state
// (active task records) is the source of truth for which slots are in use, so the
// pool needs no separate reservation/liveness bookkeeping.
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

// MaxFromEnv returns the configured pool size (ORCHA_MAX_WORKTREES) or the default.
func MaxFromEnv() int {
	if v := os.Getenv("ORCHA_MAX_WORKTREES"); v != "" {
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
		tracked, err := hasTrackedChanges(s)
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

func hasTrackedChanges(slot string) (bool, error) {
	out, err := git("-C", slot, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
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

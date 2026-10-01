// Package clonepool gives each worker a private git repository instead of a linked
// worktree, so that nothing a worker does with git reaches the lead's repository. A
// linked worktree shares main's refs, tags, config, hooks and info/exclude; a clone has
// its own, and borrows only main's object store, read-only, through an alternates file.
//
// The pool mirrors the worktree pool's layout and contract: one directory per repository
// under paths.Clones(), numbered slot directories, a mkdir lock, and ttorch's task records
// as the source of truth for which slots are in use. It differs in one rule: the pool
// never runs git inside an existing slot, because that repository's config and hooks are
// the last worker's. It reads a slot's tip from files, asks main whether it has landed,
// and recycles a slot by deleting it and provisioning a fresh repository in its place.
//
// The pool is selected by TTORCH_WORKER_CLONES (off by default) or `ttorch spawn
// --workdir`. It is one part of a larger change, and the gate, land and teardown paths
// that read a clone's work back into main are separate tasks; until all of them land,
// turning the flag on is not safe.
package clonepool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// EnvVar selects clones for new spawns when truthy (1, true, yes, on).
const EnvVar = "TTORCH_WORKER_CLONES"

// The two kinds of worker directory `ttorch spawn --workdir` accepts.
const (
	KindClone    = "clone"
	KindWorktree = "worktree"
)

// Enabled reports whether TTORCH_WORKER_CLONES selects clones. Default off: unset, empty
// or any other value is off.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ValidKind reports whether k is a value --workdir accepts; "" means "use the flag".
func ValidKind(k string) bool { return k == "" || k == KindClone || k == KindWorktree }

// ResolveKind returns the kind of worker directory a spawn uses: the explicit request when
// one is given, else clone when TTORCH_WORKER_CLONES is on, else worktree.
func ResolveKind(requested string) (string, error) {
	switch requested {
	case KindClone, KindWorktree:
		return requested, nil
	case "":
		if Enabled() {
			return KindClone, nil
		}
		return KindWorktree, nil
	}
	return "", fmt.Errorf("invalid workdir kind %q (want %s or %s)", requested, KindClone, KindWorktree)
}

// taskIDPattern is the allowlist for a task id that is spliced into a ref name
// (refs/ttorch/clones/<id>/base, ttorch/<id>). It is checked before any git command runs;
// git's own ref-format check is the second line.
var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validTaskID reports whether id is safe to put in a ref name.
func validTaskID(id string) bool {
	return taskIDPattern.MatchString(id) && !strings.Contains(id, "..") && !strings.HasSuffix(id, ".lock")
}

// ClonePool is a per-repository pool of private clones rooted at Root.
type ClonePool struct {
	Root string
	Max  int
}

func poolName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Base(repo) + "-" + hex.EncodeToString(sum[:])[:8]
}

// Dir returns the pool directory for repo.
func (p ClonePool) Dir(repo string) string { return filepath.Join(p.Root, poolName(repo)) }

// Acquire returns a clone slot for taskID, provisioned at the freshly fetched default and
// checked out on ttorch/<taskID>, and pins the base in main under
// refs/ttorch/clones/<taskID>/base.
//
// It reuses an idle slot (one not in inUse) only when its tip is provably landed: read
// from files, present in main, and reachable from main's default branch, origin's, or
// main's HEAD. Any slot whose tip cannot be read, or holds a commit main does not have,
// is left alone, because it may be a prior task's unlanded work whose task record was
// lost. A reused slot is deleted and provisioned fresh, so nothing of the previous task
// survives, tracked or untracked. Otherwise a new slot is provisioned, up to Max.
//
// A repository that uses LFS, is a partial clone, or has submodules is refused with
// ErrUnsupportedRepo before anything is written.
func (p ClonePool) Acquire(repo, taskID string, inUse []string) (string, error) {
	if !validTaskID(taskID) {
		return "", fmt.Errorf("cannot provision a clone for task %q: a task id spliced into a ref name must match %s, without \"..\" or a .lock suffix", taskID, taskIDPattern)
	}
	poolDir := p.Dir(repo)
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

	src, err := resolveSource(repo)
	if err != nil {
		return "", err
	}
	if err := refuseUnsupported(src); err != nil {
		return "", err
	}

	slots := listSlots(poolDir)
	slot := ""
	for _, s := range slots {
		abs, _ := filepath.Abs(s)
		if busy[abs] || !recyclable(src, s) {
			continue
		}
		if err := os.RemoveAll(s); err != nil {
			continue
		}
		slot = s
		break
	}
	if slot == "" {
		if len(slots) >= p.Max || len(busy) >= p.Max {
			return "", fmt.Errorf("clone pool full (max %d); tear down a worker first", p.Max)
		}
		slot = filepath.Join(poolDir, strconv.Itoa(nextIndex(slots)))
	}

	if _, err := git("-C", repo, "update-ref", pinRef(taskID), src.base); err != nil {
		return "", err
	}
	if err := provision(src, slot, taskID); err != nil {
		_ = os.RemoveAll(slot)
		_, _ = git("-C", repo, "update-ref", "-d", pinRef(taskID))
		return "", fmt.Errorf("provisioning a clone at %s: %w", slot, err)
	}
	return slot, nil
}

// recyclable reports whether an idle slot may be deleted and reused. A slot with no .git
// at all holds no commit and is reusable (a provision that died partway leaves one).
// Otherwise its tip, read from files, must be a commit main has and reaches from one of
// its safe bases. Every git command here runs in main; the slot is only read as files.
func recyclable(src source, slot string) bool {
	if _, err := os.Lstat(filepath.Join(slot, ".git")); errors.Is(err, os.ErrNotExist) {
		return true
	}
	tip, ok := observeTip(slot)
	if !ok {
		return false
	}
	if _, err := git("-C", src.repo, "cat-file", "-e", tip+"^{commit}"); err != nil {
		return false
	}
	for _, base := range []string{"refs/heads/" + src.def, "refs/remotes/origin/" + src.def, "HEAD"} {
		if _, err := git("-C", src.repo, "merge-base", "--is-ancestor", tip, base); err == nil {
			return true
		}
	}
	return false
}

// FreeSlots reports how many more workers the pool can host for a repo: Max minus the
// distinct paths in inUse, clamped at zero. inUse is every worker directory held for the
// repo, clones and worktrees together, so the cap counts both kinds.
func (p ClonePool) FreeSlots(inUse []string) int {
	busy := map[string]bool{}
	for _, w := range inUse {
		if abs, err := filepath.Abs(w); err == nil {
			busy[abs] = true
		}
	}
	if free := p.Max - len(busy); free > 0 {
		return free
	}
	return 0
}

// Owns reports whether path names a directory under the pool root, by name. A caller that
// holds a task's worker directory uses it to route a release here rather than to the
// worktree pool, which would run git inside the clone.
func (p ClonePool) Owns(path string) bool {
	root, err := filepath.Abs(p.Root)
	if err != nil || p.Root == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Release recycles a finished slot by deleting it, without running git in it. The next
// Acquire provisions a fresh repository in its place. Deciding whether the slot's work
// may be discarded is the caller's job, as it is for the worktree pool, whose Release
// resets the slot. Release refuses any path that is not a numbered slot directly under
// this repository's pool directory, or that is a symlink.
func (p ClonePool) Release(repo, slot string) error {
	if err := p.checkSlot(repo, slot); err != nil {
		return err
	}
	return os.RemoveAll(slot)
}

// Destroy removes a slot from the pool entirely. With fresh recycling it is the same
// operation as Release.
func (p ClonePool) Destroy(repo, slot string) error { return p.Release(repo, slot) }

// checkSlot accepts slot only as a numbered directory directly under repo's pool dir.
func (p ClonePool) checkSlot(repo, slot string) error {
	poolDir, err := filepath.Abs(p.Dir(repo))
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(slot)
	if err != nil {
		return err
	}
	if filepath.Dir(abs) != poolDir {
		return fmt.Errorf("refusing to release %s: it is not a slot of the clone pool %s", slot, poolDir)
	}
	if _, err := strconv.Atoi(filepath.Base(abs)); err != nil {
		return fmt.Errorf("refusing to release %s: it is not a numbered clone slot", slot)
	}
	fi, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to release %s: it is not a directory", slot)
	}
	return nil
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
	return nil, errors.New("could not acquire clone pool lock")
}

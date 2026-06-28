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

// warnf prints a non-fatal operational warning to stderr, where the manager that ran
// the spawn sees it. It is a package var so tests can capture warnings instead of
// inspecting stderr; production writes a "ttorch: " line to match the rest of the CLI.
var warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ttorch: "+format+"\n", args...)
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

// Acquire returns a worktree for a new task, based on the up-to-date default tip. It
// first refreshes origin and resolves the fresh base once (see fetchAndBase), then
// reuses a clean idle slot (one not in inUse and free of tracked changes), resetting it
// to origin/<default> while keeping untracked caches; otherwise it creates a new slot at
// that base, up to Max. Resetting a recycled slot to the lead's possibly-stale local
// HEAD was the stale-base bug — a worker could start several commits behind origin — so
// reuse and creation both anchor on the freshly fetched default instead.
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

	// Refresh origin and resolve the fresh base once, so every slot this call hands out
	// (reused or newly created) starts from the up-to-date default rather than a stale
	// local HEAD. Offline-safe: a failed fetch warns and falls back to the last-known
	// default (see fetchAndBase).
	base := fetchAndBase(repo)

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
		if err := resetTo(s, base); err != nil {
			continue
		}
		return s, nil
	}

	if len(slots) >= p.Max {
		return "", fmt.Errorf("worktree pool full (max %d); tear down a worker first", p.Max)
	}
	s := filepath.Join(poolDir, strconv.Itoa(nextIndex(slots)))
	if _, err := git("-C", repo, "worktree", "add", "--detach", s, base); err != nil {
		return "", err
	}
	return s, nil
}

// FreeSlots reports how many MORE workers the pool can host for a repo right now: the
// pool cap (Max) minus the slots already in use, clamped at zero. inUse is the set of
// worktree paths held by live workers for that repo — ttorch's task records are the
// occupancy source of truth (see the package doc), so no separate reservation state is
// consulted. Paths are de-duplicated by absolute path to mirror Acquire's busy-set, so
// the result is exactly the number of additional disjoint workers that can be dispatched
// before Acquire reports the pool full. It is a read-only projection of Max and current
// occupancy: it touches no worktree and changes no pool state.
func (p Pool) FreeSlots(inUse []string) int {
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

// Release resets a finished slot to a clean tracked state and keeps it for reuse. It
// first parks the slot on a detached HEAD, dropping any per-task branch it was on, so
// a pooled slot never pins a branch name (e.g. ttorch/<id>) that a later spawn may
// need to (re)create in a different slot, and idle task branches do not accumulate.
func (p Pool) Release(repo, slot string) error {
	detachTaskBranch(slot)
	return reset(slot, repo)
}

// detachTaskBranch parks slot on a detached HEAD and deletes the branch it was on.
// Best-effort: a slot already detached (a freshly created slot, or one on "HEAD") is
// left untouched, and any git hiccup is swallowed so the caller's reset still runs.
// Releasing a slot already discards its commits (reset to the repo HEAD), so dropping
// the now-idle branch ref alongside is consistent — landed work lives on the default
// branch, and abandoned work was going to be discarded anyway.
func detachTaskBranch(slot string) {
	br, err := CurrentBranch(slot)
	if err != nil || br == "" || br == "HEAD" {
		return
	}
	if _, err := git("-C", slot, "checkout", "-q", "--detach"); err != nil {
		return
	}
	_, _ = git("-C", slot, "branch", "-D", br)
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

// reset hard-resets a slot to the repo's local HEAD. Release uses it to park a finished
// slot clean; the next Acquire re-anchors a reused slot on the freshly fetched default
// (resetTo + fetchAndBase), so an idle slot's local-HEAD parking is always superseded
// before a worker starts from it.
func reset(slot, repo string) error {
	head, err := headCommit(repo)
	if err != nil {
		return err
	}
	return resetTo(slot, head)
}

// resetTo hard-resets a slot's tracked tree to ref, discarding tracked changes while
// leaving untracked build caches in place (no `git clean`).
func resetTo(slot, ref string) error {
	_, err := git("-C", slot, "reset", "--hard", "-q", ref)
	return err
}

// StartBranch prepares a (possibly reused) pooled worktree slot for a NEW task on a
// fresh branch. It fetches the default branch from origin when the repo has one, so
// the branch is cut from the up-to-date tip, then force-creates branch at that base
// and hard-resets the slot's tracked tree to it. The slot leaves whatever branch it
// was on and any tracked changes are discarded; untracked build caches are kept (no
// `git clean`). After it returns, the slot is checked out on branch at the current
// default-branch tip with a clean tracked tree, so a worker never inherits a previous
// task's branch or state. `--no-track` keeps the task branch from adopting
// origin/<default> as its upstream when cut from a remote ref.
//
// The fetch is best-effort (offline, or a repo with no remote, falls back to the
// local default branch WITH a warning, see fetchAndBase); an unresolvable base or a
// failed checkout is returned as an error so a stale-branch start fails loudly rather
// than silently reusing prior state.
func StartBranch(repo, slot, branch string) error {
	base := fetchAndBase(repo)
	if _, err := git("-C", slot, "checkout", "-q", "--no-track", "-B", branch, base); err != nil {
		return fmt.Errorf("checkout %s off %s: %w", branch, base, err)
	}
	// Belt-and-suspenders: guarantee the tracked tree matches base exactly even if the
	// checkout carried something across (untracked caches are left untouched).
	_, err := git("-C", slot, "reset", "--hard", "-q", base)
	return err
}

// fetchAndBase refreshes origin (when the repo has one) so origin/<default> is current,
// then returns the ref a fresh worktree or branch should be based on (see defaultBase).
// The fetch is best-effort and never fatal: on failure it warns and falls back to the
// last-known base, so an offline or transient hiccup degrades to the last-known default
// VISIBLY rather than silently starting a worker several commits behind origin. It is
// the single fetch+base point shared by Acquire and StartBranch, so each is independently
// safe against a stale base (the spawn path runs both; a redundant up-to-date fetch is
// cheap).
func fetchAndBase(repo string) string {
	if RemoteExists(repo, "origin") {
		if err := Fetch(repo); err != nil {
			warnf("could not fetch origin in %s: %v; basing on the last-known default, which may be behind origin", repo, err)
		}
	}
	return defaultBase(repo)
}

// defaultBase returns the ref a fresh task branch should be cut from: the remote
// default branch origin/<default> when it resolves (the authoritative, just-fetched
// tip), else the local <default> branch, else HEAD.
func defaultBase(repo string) string {
	def := DefaultBranch(repo)
	if RefExists(repo, "origin/"+def) {
		return "origin/" + def
	}
	if RefExists(repo, def) {
		return def
	}
	return "HEAD"
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

// UnmergedCommits returns the one-line summaries (most-recent first) of commits
// reachable from branch but from NONE of bases (`git log branch --not base...`). An
// empty result means every commit on branch already lives on a base — the branch was
// just cut from one, or its work has landed — so discarding branch loses nothing.
//
// Teardown's data-loss guard uses it to detect a worker's own COMMITTED-but-unmerged
// work (a clean worktree can still hold commits that were never merged). The caller
// passes BOTH the local default branch and origin/<default> as bases: a fresh worker's
// branch is cut from origin/<default> (see StartBranch/defaultBase), so comparing only
// against the local default would wrongly flag an untouched worker whenever origin is
// ahead of local — and a worker whose work landed via the remote must read as merged.
// Bases that do not resolve are skipped; with no resolvable base it errors rather than
// list a branch's entire history.
func UnmergedCommits(repo, branch string, bases ...string) ([]string, error) {
	var exclude []string
	for _, b := range bases {
		if b != "" && RefExists(repo, b) {
			exclude = append(exclude, b)
		}
	}
	if len(exclude) == 0 {
		return nil, fmt.Errorf("no default-branch ref to compare %s against", branch)
	}
	args := append([]string{"-C", repo, "log", "--oneline", "--no-decorate", branch, "--not"}, exclude...)
	out, err := git(args...)
	if err != nil {
		return nil, err
	}
	if out = strings.TrimSpace(out); out != "" {
		return strings.Split(out, "\n"), nil
	}
	return nil, nil
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

// SetRef points refName at commit (`git update-ref`), creating or moving it. refName
// must be a fully-qualified ref (e.g. refs/ttorch/discarded/<id>-<sha>). Teardown's
// --force path uses it to stash a discarded task branch under refs/ttorch/discarded/
// before the branch is deleted, so committed-but-unmerged work stays recoverable
// (`git log <refName>`) instead of being lost outright.
func SetRef(repo, refName, commit string) error {
	_, err := git("-C", repo, "update-ref", refName, commit)
	return err
}

// Rebase replays the commits in dir's current HEAD onto onto (`git rebase onto`),
// leaving HEAD at the rebased tip. On conflict it returns an error WITHOUT cleaning
// up — the caller must RebaseAbort to restore the original HEAD.
func Rebase(dir, onto string) error {
	args := append([]string{"-C", dir}, fallbackIdentityArgs(dir)...)
	_, err := git(append(args, "rebase", onto)...)
	return err
}

// fallbackIdentityArgs returns `-c user.name=… -c user.email=…` flags only when dir
// has no committer identity configured. Rebase writes new commits and aborts with
// "empty ident name" if neither git config nor the environment supplies one — this
// happens on bare CI runners and on developer machines without a global identity.
// We inject a placeholder only as a last resort, so a real configured identity (the
// common case) is always preserved.
func fallbackIdentityArgs(dir string) []string {
	if name, _ := git("-C", dir, "config", "user.name"); name != "" {
		if email, _ := git("-C", dir, "config", "user.email"); email != "" {
			return nil
		}
	}
	return []string{"-c", "user.name=ttorch", "-c", "user.email=ttorch@localhost"}
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

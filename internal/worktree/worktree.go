// Package worktree manages a pool of reusable, isolated git worktrees so parallel
// workers never collide and never touch the lead's checkout. Reusing a clean idle
// worktree (resetting tracked files but keeping untracked build/dependency caches)
// is far faster than cloning per task.
//
// ttorch's own task records are the source of truth for which slots are in use, so
// the pool needs no separate reservation or liveness bookkeeping.
package worktree

import (
	"bytes"
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

// git runs a git command and returns its combined output, trimmed. The ERROR text is escaped
// because it ends up in a CLI message on the lead's terminal and carries git's own stderr,
// which echoes back committed filenames and .gitattributes lines — attacker-controlled bytes,
// ANSI escape sequences included, which a terminal executes. checkedGitRaw and warnf were
// fixed for this first; leaving the most-used wrapper in the file unescaped would rest on
// every caller remembering which one they used, and the reason for fixing at a sink is that
// they will not. No worker-controlled route into a git() argument is known today; this closes
// the asymmetry rather than a demonstrated exploit.
//
// The returned VALUE is left verbatim: callers parse it, and escaping would corrupt the data.
func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		// The ARGS are escaped as well as the output. git sanitizes its own stderr, but the
		// command line is echoed back verbatim, so a ref or path with an escape sequence in
		// it reaches the terminal through the args half instead.
		return s, fmt.Errorf("git %s: %v: %s", escapeForTerminal(strings.Join(args, " ")), err, escapeForTerminal(s))
	}
	return s, nil
}

// warnf prints a non-fatal operational warning to stderr, where the manager that ran
// the spawn sees it. It is a package var so tests can capture warnings instead of
// inspecting stderr; production writes a "ttorch: " line to match the rest of the CLI.
//
// The formatted message is escaped before it reaches the terminal. Some of what this prints
// is git's own stderr, which carries whatever a worker put in a committed filename or a
// .gitattributes line — including ANSI escape sequences, which a terminal EXECUTES. A CSI
// sequence can recolour the lead's screen, erase the line above it, or move the cursor, so a
// warning is a place to fabricate what the lead appears to be reading. Escaping at this sink
// rather than at each call site is deliberate: every caller shares the exposure and no caller
// can be relied on to remember, which is the same reasoning as the audit log's sanitizer.
var warnf = func(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "ttorch: "+escapeForTerminal(fmt.Sprintf(format, args...)))
}

// escapeForTerminal renders untrusted text safe to print: every C0 control character, DEL and
// C1 control becomes a visible escape, so ESC (the lead byte of every ANSI sequence), CR
// (which rewrites the current line) and LF (which fabricates a second line) are shown rather
// than obeyed.
//
// C1 (U+0080–U+009F) is included because U+009B is CSI — the single-character form of "ESC [",
// which several terminals accept, so escaping ESC alone leaves a second door into the same
// sequences. U+0085 NEL is a line break in its own right.
func escapeForTerminal(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == 0x1b:
			b.WriteString(`\e`)
		case isControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isControl reports whether r is a C0 control, DEL, or a C1 control — the code points that
// a terminal acts on rather than displays, and that a line-delimited log cannot carry.
func isControl(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
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
// reuses a clean idle slot — one not in inUse, free of tracked changes, AND free of
// committed-but-unmerged work (hasUnlandedWork) — resetting it to origin/<default> while
// keeping untracked caches; otherwise it creates a new slot at that base, up to Max, and
// when none is free it refuses (pool full) rather than clobber a held slot. Resetting a
// recycled slot to the lead's possibly-stale local HEAD was the stale-base bug — a worker
// could start several commits behind origin — so reuse and creation both anchor on the
// freshly fetched default instead. The unlanded-work skip is the defense in depth behind
// "never reuse a slot that still holds another task's work": inUse is the primary guard,
// but a lost task→worktree mapping must never let a recycle clobber a prior task's commit.
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
		// Defense in depth beyond the inUse set: never reset a slot that still holds
		// committed-but-unmerged work out from under whatever task left it there. inUse is
		// the primary occupancy guard, but if a task→worktree mapping is ever lost (a
		// half-finished teardown, a crashed dispatch), a slot can hold a prior task's commit
		// yet be absent from inUse — and committed work leaves no tracked changes, so the
		// check above waves it through. Recycling it would clobber that branch and let the
		// next StartBranch stack a new task on top of the leftover commit: the exact
		// cross-task contamination this skip prevents. A clean slot (freshly created, or
		// properly released to the default tip) has no unlanded commits and is still reused.
		if hasUnlandedWork(repo, s) {
			continue
		}
		// The slot is provably free of unlanded work; park it detached and drop any leftover
		// task branch BEFORE resetting, so a recycle never moves a stale branch ref and no
		// orphaned ttorch/<id> ref accumulates across reuse (mappings stay consistent).
		detachTaskBranch(s)
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

// hasUnlandedWork reports whether slot's checked-out HEAD carries commit(s) that are not
// already reachable from a SAFE base — committed work a recycle would clobber. Acquire uses
// it as the committed-work counterpart to HasTrackedChanges: a slot still holding a prior
// task's unlanded commit (its task→worktree mapping lost, or never torn down) must NOT be
// reused/reset, even when it is clean of uncommitted changes and absent from the inUse set.
//
// The safe bases are the default branch, origin/<default>, AND the lead repo's current
// checkout HEAD. The repo-HEAD base is essential: Release parks a freed slot at the lead's
// checkout HEAD (reset → headCommit(repo)), which is routinely a feature branch AHEAD of the
// default branch (the normal state of a ttorch-managed repo mid-task). That is not unlanded
// task work — just the base the lead sits on — so without repo HEAD as a base every cleanly
// released slot would be wrongly skipped, proliferating slots and, at Max, starving dispatch.
// A genuine prior-task commit lives on a worker's ttorch/<id> line and is never the lead's
// HEAD unless it has landed, so contamination protection is preserved.
//
// It fails safe — if merge status cannot be determined (no base resolves, or git errors) it
// reports true, so an indeterminate slot is left alone rather than reset. The slot is a
// linked worktree sharing the repo's refs/objects, so each base resolves from it directly
// (repo HEAD is passed as a resolved SHA, reachable via the shared object store).
func hasUnlandedWork(repo, slot string) bool {
	def := DefaultBranch(slot)
	bases := []string{def, "origin/" + def}
	if head, err := headCommit(repo); err == nil {
		bases = append(bases, head)
	}
	unmerged, err := UnmergedCommits(slot, "HEAD", bases...)
	return err != nil || len(unmerged) > 0
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
	// Loud guard (never silently stack): a fresh start force-creates branch at base, which
	// would discard any commits an already-existing branch of the same name carries. If
	// branch already exists AHEAD of base, that is an inconsistent lifecycle — a prior
	// incarnation of this task, or a contaminated slot — not a clean start, so refuse and
	// let the caller surface it rather than silently clobber the leftover commits. A resume
	// reuses its worktree and never calls StartBranch, so this only fires on a fresh
	// dispatch; a branch sitting exactly at base (e.g. a just-re-created task branch with no
	// commits yet) is fully merged and allowed through.
	if RefExists(repo, branch) {
		ahead, err := UnmergedCommits(repo, branch, base)
		if err != nil {
			return fmt.Errorf("start %s: cannot verify it sits at the base %s: %w", branch, base, err)
		}
		if len(ahead) > 0 {
			return fmt.Errorf("refusing to start %s: it already exists with %d commit(s) ahead of %s; land or tear down the prior work first (a fresh start would silently discard it)", branch, len(ahead), base)
		}
	}
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

// ErrPathCollision marks a git invocation whose own stderr reported that two index entries
// resolve to ONE file on the target filesystem ("the following paths have collided"). It is
// distinct from an ordinary git failure because the command SUCCEEDS: git warns, picks a
// winner, and exits 0. Every caller that materializes or lists a tree treats it as fatal,
// because a tree with a collision is not the tree that ends up on disk — which is exactly the
// substitution the trust gate's gate-config guard exists to refuse.
var ErrPathCollision = errors.New("two paths in this tree collide into one file on the target filesystem")

// collisionWarning reports whether git's stderr carries its path-collision warning. git's
// wording has been stable ("warning: the following paths have collided (e.g. case-insensitive
// paths on a case-insensitive filesystem)"), but the match is deliberately loose: it keys on
// "collid", so a reworded or localized variant that still says the word is caught. A false
// positive costs a refused merge the lead can re-drive; a false negative loses the only signal
// git gives for this attack.
func collisionWarning(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "collid")
}

// gitRaw runs git and returns stdout VERBATIM plus stderr SEPARATELY. It exists alongside
// git() for MACHINE-READABLE output: git() folds stderr into the result and trims it, so a
// warning line would be glued onto the first record of a listing and a trailing NUL separator
// would be eaten. A reader that parses git's output by separator must not be handed either.
//
// stderr is returned rather than dropped on success. git reports the path-collision warning
// on a SUCCESSFUL command, so discarding stderr whenever the exit code is zero throws away
// the one signal git gives that two index entries are about to become one file. Callers must
// inspect it; checkedGitRaw is the wrapper that does so for them.
func gitRaw(args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("git", args...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	stderr = errBuf.String()
	if err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" {
			// Escaped, args included: this text ends up in a CLI error on a terminal.
			return "", stderr, fmt.Errorf("git %s: %w: %s", escapeForTerminal(strings.Join(args, " ")), err, escapeForTerminal(msg))
		}
		return "", stderr, fmt.Errorf("git %s: %w", escapeForTerminal(strings.Join(args, " ")), err)
	}
	return string(out), stderr, nil
}

// checkedGitRaw runs gitRaw and acts on the stderr of a SUCCESSFUL command: a collision
// warning becomes ErrPathCollision (fail closed), and anything else is surfaced through
// warnf so an operational git message is visible rather than swallowed.
func checkedGitRaw(args ...string) (string, error) {
	out, stderr, err := gitRaw(args...)
	if err != nil {
		return "", err
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		if collisionWarning(msg) {
			return "", fmt.Errorf("git %s: %w: %s", escapeForTerminal(strings.Join(args, " ")), ErrPathCollision, escapeForTerminal(msg))
		}
		warnf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// splitNUL turns git's -z output into paths, dropping the empty record left by the trailing
// separator. It never splits on newlines, which a path may legitimately contain.
func splitNUL(out string) []string {
	var files []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			files = append(files, n)
		}
	}
	return files
}

// THE INPUT SET IS THE PART THAT KEEPS BEING WRONG.
//
// Five separate defects in the gate-config guard have now been defects in the set of paths
// handed to the comparison, not in the comparison itself: no case folding at all, then simple
// lowercasing instead of folding, then single-rune folding instead of full folding, then blobs
// without the directories they imply, and then this one — renames reporting only their
// destination. Each time the matcher was doing exactly what it said and the SET was short.
//
// So: anyone auditing this guard should check what reaches it before checking what it does
// with what reaches it. The questions that have each caught a real bypass are — is every path
// present (renames, directories), is it spelled the way the matcher matches (quoting,
// normalization, folding), and is it the committed tree rather than the working one.
//
// ChangedFiles returns the repo-relative paths changed between base and the COMMITTED
// rev — committed objects, never the working tree. It is an input to the trust gate's
// gate-config guard (orchestrator.diffTouchesGateConfig), so the list must be complete and
// every path must be spelled the way the guard matches it.
//
// It therefore uses `--name-only -z`: NUL-separated and UNQUOTED regardless of
// core.quotePath. Plain `--name-only` C-quotes any path containing non-ASCII bytes,
// control characters, quotes or backslashes — "content/skills/caf\303\251.md" — and a
// quoted path matches neither an exact gate-config name nor a covered prefix, so the guard
// would silently let it through. Splitting on newlines has the mirror-image failure once a
// user sets core.quotePath=false: a path containing a literal newline splits into two
// fragments, neither of which is a real path. internal/review/size.go states this same
// requirement for the reviewer-set classifier and orchestrator.diffFiles already meets it;
// this is the gate-config guard's half of it.
//
// Removing the quoting removes an incidental defence: a quoted path could not carry a raw
// newline or control byte into a caller. orchestrator.hostilePath now refuses those
// explicitly, and the audit sink escapes them, rather than relying on git's display quoting.
//
// --no-renames because diff.renames has defaulted TRUE since git 2.9, and a detected rename is
// reported as its DESTINATION only. The source path simply does not appear, so
// `git mv content/agents/ttorch-reviewer-security.md content/agents/security-review-guidance.md`
// produced a diff that touched no covered path and merged unflagged — while the reviewer
// definition left the install set, because installer.desiredFiles walks the embedded tree
// rather than working from a list, so no code change was needed either. The same move applied
// to internal/installer/ relocates the whole package out of coverage with one import to fix in
// internal/cli/, which is deliberately uncovered. The collision check cannot backstop this: a
// rename produces no colliding pair. With --no-renames git reports both sides and the source
// trips the guard.
func ChangedFiles(path, base, rev string) ([]string, error) {
	out, err := checkedGitRaw("-C", path, "diff", "--name-only", "-z", "--no-renames", base, rev)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// ChangedLink is a path whose entry in rev is a SYMLINK (mode 120000) or a GITLINK /
// submodule pointer (mode 160000), and which was not already that kind of entry in base.
type ChangedLink struct {
	Path string
	Mode string // "120000" or "160000"
}

// ChangedLinks returns the links base..rev introduces.
//
// A path-matching guard reads the diff's path names and assumes the bytes at a path are the
// bytes committed there. A symlink breaks that assumption without producing a second entry
// for a collision check to notice: committing `.claude -> docs/payload` alongside
// `docs/payload/agents/ttorch-reviewer-security.md` changes two paths that matched nothing,
// and a fresh clone then resolves .claude/agents/ttorch-reviewer-security.md to the payload.
// A gitlink does the same with one entry and no second path at all.
//
// Only NEWLY INTRODUCED links are reported: dst is a link mode and src is not the same mode.
// The repository's own CLAUDE.md is a committed symlink, so reporting every changed link
// would refuse every commit that touches it, with no flag to clear it. Repointing an
// existing symlink keeps src == dst == 120000 and stays with the name match, which is
// flaggable; introducing one where none stood is the shape that hides a path from the diff.
//
// `git diff --raw -z` emits ":srcmode dstmode srcsha dstsha status\0path\0". --no-renames
// for the same reason ChangedFiles needs it.
func ChangedLinks(path, base, rev string) ([]ChangedLink, error) {
	out, err := checkedGitRaw("-C", path, "diff", "--raw", "-z", "--no-renames", base, rev)
	if err != nil {
		return nil, err
	}
	fields := splitNUL(out)
	var links []ChangedLink
	// Records alternate: metadata, path, metadata, path...
	for i := 0; i+1 < len(fields); i += 2 {
		meta, p := fields[i], fields[i+1]
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("unparseable `git diff --raw` record %q", meta)
		}
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(parts) < 2 {
			return nil, fmt.Errorf("unparseable `git diff --raw` record %q", meta)
		}
		src, dst := parts[0], parts[1]
		if dst != "120000" && dst != "160000" {
			continue
		}
		if src == dst {
			continue
		}
		links = append(links, ChangedLink{Path: p, Mode: dst})
	}
	return links, nil
}

// CatBlobs returns the RAW bytes of each path in rev's committed tree, keyed by path.
//
// Raw is the whole point. `git grep <rev>` and `git diff` read gitattributes from the
// WORKING TREE rather than from the rev they are given, so a committed `.gitattributes`
// saying `content.go -diff` made `git grep` report "Binary file ... matches" for a commit
// whose bytes never changed. Anything that decides policy from a text-search tool inherits
// that. `git cat-file` emits the stored object bytes; twelve hostile attribute spellings
// were set against the same blob and every one returned the committed bytes.
//
// The framing is the part that has to be right, because three ways of getting it wrong all
// fail OPEN, handing a caller a short map with a nil error:
//
//   - Nothing derived from a PATH is written into the batch input. Paths came from the
//     tree, and a committed path may contain a newline; written into newline-delimited
//     batch input it split into two object specs and desynced every later record. So the
//     paths are resolved to object ids by `ls-tree -r -z` first, and the ids are what the
//     batch reads. An id is forty hex characters and cannot carry a delimiter, which is a
//     stronger guarantee than escaping or than `--batch -z`: it does not depend on a git
//     version, and there is no input a tree can hold that changes the framing.
//   - Every record's content is consumed EXACTLY, whatever its type, and each record's
//     echoed id is checked against the id that was asked for. Skipping a record without
//     consuming its bytes misaligns the rest of the batch: one gitlink at a path sorting
//     before content.go was enough to make the next blob unreadable and flip the gate's
//     scope. The id check is what makes a misalignment loud instead of silent — a
//     desynced stream cannot echo the id that was requested.
//   - A short batch, a missing blob or anything on stderr is an ERROR. Absence and
//     failure are not the same answer, and the caller documents itself as failing closed,
//     which it can only do if it is told.
//
// Missing paths are reported as an error rather than omitted. Callers ask for paths they
// read out of the same tree, so a miss means the read is wrong, not that the file is
// absent. Entries that are not blobs — a gitlink, a submodule whose commit this repository
// does not have — are read to keep the stream in frame and then dropped, because they are
// not content a caller can use.
func CatBlobs(path, rev string, paths []string) (map[string][]byte, error) {
	if len(paths) == 0 {
		return map[string][]byte{}, nil
	}
	entries, err := treeEntries(path, rev)
	if err != nil {
		return nil, err
	}
	wanted := make([]blobRequest, 0, len(paths))
	for _, p := range paths {
		e, ok := entries[p]
		if !ok {
			return nil, fmt.Errorf("git cat-file: %s:%s is not in the tree; the caller reads "+
				"its paths out of this same tree, so a miss is a failed read, not an absent file",
				rev, p)
		}
		wanted = append(wanted, blobRequest{path: p, entry: e})
	}

	var in bytes.Buffer
	for _, w := range wanted {
		in.WriteString(w.entry.OID)
		in.WriteByte('\n')
	}
	cmd := exec.Command("git", "-C", path, "cat-file", "--batch")
	cmd.Stdin = &in
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch in %s: %w: %s", rev, err, strings.TrimSpace(errOut.String()))
	}
	if msg := strings.TrimSpace(errOut.String()); msg != "" {
		return nil, fmt.Errorf("git cat-file --batch in %s wrote to stderr: %s", rev, msg)
	}

	return parseBatchRecords(rev, out.Bytes(), wanted)
}

// blobRequest pairs a path the caller asked for with the tree entry it resolved to.
type blobRequest struct {
	path  string
	entry treeEntry
}

// parseBatchRecords walks `git cat-file --batch` output against the ids that were fed to
// it, in order, and returns the blob contents keyed by path.
//
// It is separate from the process call so the framing can be tested on a stream that no
// repository could be made to produce. The checks it carries are the ones that make a
// desync loud: a record that does not echo the id it answers, a record that claims more
// bytes than remain, and a batch that ends early are all errors, because every one of them
// otherwise yields a short map with a nil error and reads to the caller as absence.
func parseBatchRecords(rev string, out []byte, wanted []blobRequest) (map[string][]byte, error) {
	res := make(map[string][]byte, len(wanted))
	buf := out
	for _, w := range wanted {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("git cat-file --batch: output ended before %s:%s; "+
				"a short batch is a failed read, not an absent file", rev, w.path)
		}
		header := string(buf[:nl])
		buf = buf[nl+1:]
		fields := strings.Fields(header)
		if len(fields) == 0 || fields[0] != w.entry.OID {
			return nil, fmt.Errorf("git cat-file --batch: record %q does not answer %s (%s:%s); "+
				"the stream is out of frame", header, w.entry.OID, rev, w.path)
		}
		// "<id> missing" carries no content, so there is nothing to consume. A blob the
		// tree just named cannot be missing; a gitlink pointing into a submodule this
		// repository does not have routinely is.
		if len(fields) == 2 && fields[1] == "missing" {
			if w.entry.Type == "blob" {
				return nil, fmt.Errorf("git cat-file --batch: %s:%s is a blob the tree names "+
					"but the object store does not have", rev, w.path)
			}
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("git cat-file --batch: unparseable record %q", header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch: unparseable size in %q", header)
		}
		// A negative size slips past the bounds check below, because a negative is not
		// greater than a length, and then buf[:size] panics. Nothing in this process
		// recovers, so one malformed header would take down ttorch rather than fail one
		// gate closed. Real git never emits one; this function's contract is to survive a
		// stream no repository could produce, so it has to hold anyway.
		if size < 0 {
			return nil, fmt.Errorf("git cat-file --batch: negative size in %q", header)
		}
		if size > len(buf) {
			return nil, fmt.Errorf("git cat-file --batch: record for %s:%s claims %d bytes "+
				"but only %d remain; the stream is truncated", rev, w.path, size, len(buf))
		}
		content := buf[:size]
		buf = buf[size:]
		if len(buf) > 0 && buf[0] == '\n' {
			buf = buf[1:]
		}
		// Consumed for every type, then kept only for blobs. A tree or a commit at this
		// path is not content the caller can use, but its bytes still have to leave the
		// stream or every later record is read at the wrong offset.
		if fields[1] == "blob" {
			res[w.path] = content
		}
	}
	return res, nil
}

// treeEntry is one record of a recursive tree listing: what the object is, and its id.
type treeEntry struct {
	Type string
	OID  string
}

// treeEntries lists rev's tree recursively as path -> entry.
//
// `-z` here is on the OUTPUT, where it has been supported for as long as ls-tree has, and
// it is what keeps a path containing a newline from splitting a record. It also hands back
// each object's id, which is what lets CatBlobs read by id rather than by path.
func treeEntries(path, rev string) (map[string]treeEntry, error) {
	out, err := checkedGitRaw("-C", path, "ls-tree", "-r", "-z", rev)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -r -z %s in %s: %w", rev, path, err)
	}
	entries := make(map[string]treeEntry)
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		// "<mode> SP <type> SP <oid> TAB <path>", and with -z the path is never quoted.
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("unparseable `git ls-tree` record %q", rec)
		}
		meta := strings.Fields(rec[:tab])
		if len(meta) != 3 {
			return nil, fmt.Errorf("unparseable `git ls-tree` record %q", rec)
		}
		entries[rec[tab+1:]] = treeEntry{Type: meta[1], OID: meta[2]}
	}
	return entries, nil
}

// TreeFiles returns every path in rev's committed tree, NUL-separated and unquoted.
//
// The gate-config guard needs the WHOLE tree, not just the diff. The collision attack adds
// one new index entry ("agent\u017f.md") beside an UNCHANGED covered file ("AGENTS.md"): only
// the new entry appears in base..rev, so a guard that compares changed paths against each
// other sees one path and nothing to collide with. Comparing each changed path against the
// full resulting tree is what makes the pair visible.
func TreeFiles(path, rev string) ([]string, error) {
	out, err := checkedGitRaw("-C", path, "ls-tree", "-r", "--name-only", "-z", rev)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// AddDetached creates a temporary linked worktree at dir checked out (detached) to rev,
// materializing exactly that committed tree in isolation from any live worktree, so it
// can be validated free of mutation by a running worker. The caller must RemoveWorktree
// it when done. dir must not already exist.
func AddDetached(repo, dir, rev string) error {
	// checkedGitRaw, not git(): this is the command that actually MATERIALIZES the tree, so
	// it is where git emits "the following paths have collided" — on a successful checkout,
	// having silently picked one entry as the winner. The trust gate validates this checkout
	// and then fast-forwards the sha it came from, so a collision here means the bytes
	// validated are not the bytes the tree defines. Fail closed rather than validate a
	// coin-flip.
	//
	// -q because `git worktree add` writes its "Preparing worktree" progress to stderr, and
	// checkedGitRaw surfaces anything on stderr; without it every gate run would print a
	// warning about nothing and the real signal would be lost in it.
	_, err := checkedGitRaw("-C", repo, "worktree", "add", "-q", "--detach", dir, rev)
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

// TreeHash returns the git TREE object id of the commit sha resolves to
// (`git rev-parse <sha>^{tree}`). Unlike a commit sha — which git rewrites on every rebase
// even for a byte-identical tree (new committer metadata / fork-point replay) — the tree id
// is a cryptographic content identity of the checked-out files, so two commits with identical
// trees share one tree hash. The trust gate's validate cache keys on it (via ResolveRef's
// sibling in the orchestrator), so re-validations of an identical tree collapse into one run.
func TreeHash(repo, sha string) (string, error) {
	return git("-C", repo, "rev-parse", sha+"^{tree}")
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

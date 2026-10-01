package watch

// headIdentity tells the stall ladder whether a worker's HEAD moved, without starting a
// git process. The worktree and its git config belong to the worker, and any git command
// run there can be steered into running a program the worker configured: log.showSignature
// hands a signed commit to gpg.program, and a partial clone lazily fetches a missing object
// through the promisor remote's uploadpack, core.sshCommand or a credential helper. Closing
// those one flag at a time leaves the next route open, so HEAD is read from files instead.
//
// Only the identity (the commit id HEAD resolves to) is read, never a timestamp: the ladder
// asks whether HEAD changed since the clock last restarted, which a committer date forged
// into the future cannot fake. Anything unexpected reads as unknown, and an unknown HEAD
// neither restarts the clock nor counts against the worker. The read runs under a short
// deadline (boundedHead), so a path that never answers cannot hold up the sweep. Only the
// files ref backend is read: a reftable repository's HEAD is always unknown.

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

// headReadTimeout bounds one HEAD read. The files are the worker's, and a gitdir: line or
// commondir can name an automount or FUSE path whose open never returns; past the deadline
// the read is unknown and the sweep moves on to the next task.
const headReadTimeout = 2 * time.Second

// boundedHead runs a HEAD read under headReadTimeout, at most one per task at a time. A
// read that outlives its deadline keeps its slot until it returns, so a hung path costs
// one goroutine per task rather than one per sweep; until then the task reads as unknown.
type boundedHead struct {
	timeout time.Duration
	read    func(worktree, project string) (string, bool)

	mu       sync.Mutex
	inflight map[string]bool // task id → a read is still running
}

// headReader is shared by every Watcher in the process, so a read left hanging by one
// arm still holds its task's slot in the next.
var headReader = newBoundedHead(headReadTimeout, headIdentity)

func newBoundedHead(timeout time.Duration, read func(worktree, project string) (string, bool)) *boundedHead {
	return &boundedHead{timeout: timeout, read: read, inflight: map[string]bool{}}
}

// identity reads the HEAD id of t's worktree. ok is false when the read fails, times out,
// or an earlier read for t has not returned yet.
func (b *boundedHead) identity(t db.Task) (string, bool) {
	b.mu.Lock()
	if b.inflight[t.ID] {
		b.mu.Unlock()
		return "", false
	}
	b.inflight[t.ID] = true
	b.mu.Unlock()

	type result struct {
		id string
		ok bool
	}
	done := make(chan result, 1) // buffered: a read that outlives its deadline never blocks on send
	go func() {
		id, ok := b.read(t.Worktree, t.Project)
		b.mu.Lock()
		delete(b.inflight, t.ID)
		b.mu.Unlock()
		done <- result{id, ok}
	}()
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.id, r.ok
	case <-timer.C:
		return "", false
	}
}

// Read bounds. HEAD, a .git file, commondir and a loose ref are one short line each; a
// packed-refs file can hold many refs, so it gets a larger cap. Over the cap reads as unknown.
const (
	maxSmallGitFile  = 4 << 10 // 4 KiB
	maxPackedRefFile = 8 << 20 // 8 MiB
	maxGitConfig     = 1 << 20 // 1 MiB
)

// headIdentity resolves HEAD in the worktree at dir to a commit id, using file reads only.
// project is the repository ttorch cut the worktree from ("" when none is recorded). It
// follows the worktree's .git (a directory, or a file carrying "gitdir: <path>"), the
// gitdir's commondir for a linked worktree, at most one symref level from HEAD to a ref,
// and packed-refs when the ref is not loose. The git dir and commondir must stay where
// ttorch put them (see confineGitDir). ok is false for anything else.
func headIdentity(dir, project string) (string, bool) {
	if dir == "" {
		return "", false
	}
	// The recorded path must be the worktree itself, not a symlink the worker swapped in.
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	gitdir, ok := resolveGitDir(root)
	if !ok {
		return "", false
	}
	gitdir, commondir, ok := confineGitDir(root, project, gitdir)
	if !ok || !refsInFiles(commondir) {
		return "", false
	}

	b, found := readGitFile(filepath.Join(gitdir, "HEAD"), maxSmallGitFile)
	if !found {
		return "", false
	}
	head := strings.TrimSpace(string(b))
	if isObjectID(head) {
		return head, true // detached
	}
	ref, isSym := strings.CutPrefix(head, "ref: ")
	if !isSym || !validRefName(ref) {
		return "", false
	}
	// A per-worktree ref lives under the gitdir, a branch under the commondir.
	for _, base := range []string{gitdir, commondir} {
		p, inside := joinWithin(base, ref)
		if !inside {
			return "", false
		}
		if b, found := readGitFile(p, maxSmallGitFile); found {
			id := strings.TrimSpace(string(b))
			if isObjectID(id) {
				return id, true
			}
			return "", false // a second symref level, or garbage
		}
	}
	return packedRef(filepath.Join(commondir, "packed-refs"), ref)
}

// resolveGitDir returns the git directory for the worktree at dir: dir/.git when it is a
// directory, or the path a regular .git file names. A symlinked .git reads as unknown.
func resolveGitDir(dir string) (string, bool) {
	dotgit := filepath.Join(dir, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return "", false
	}
	if fi.IsDir() {
		return dotgit, true
	}
	b, found := readGitFile(dotgit, maxSmallGitFile)
	if !found {
		return "", false
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok || p == "" || strings.ContainsRune(p, 0) {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p), true
}

// confineGitDir accepts the git dir a worktree's .git names only where ttorch put it:
// inside the worktree (root, already free of symlinks), or the project's admin entry for
// this worktree, <common>/worktrees/<name>, whose gitdir file points back at root/.git.
// The commondir it names must be inside the worktree or be the project's common git dir.
// Both are returned with symlinks resolved. Each candidate is checked by name before any
// symlink in it is followed, so an outside path is refused without touching it.
func confineGitDir(root, project, gitdir string) (string, string, bool) {
	pc := commonGitDir(project)
	adminDir := filepath.Join(pc, "worktrees")
	admin := !within(root, gitdir)
	if admin && (pc == "" || filepath.Dir(gitdir) != adminDir) {
		return "", "", false
	}
	real, err := filepath.EvalSymlinks(gitdir)
	if err != nil {
		return "", "", false
	}
	if admin {
		if filepath.Dir(real) != adminDir || !linksBack(real, filepath.Join(root, ".git")) {
			return "", "", false
		}
	} else if !within(root, real) {
		return "", "", false
	}

	b, found := readGitFile(filepath.Join(real, "commondir"), maxSmallGitFile)
	if !found {
		if admin {
			return "", "", false // git always writes commondir for a linked worktree
		}
		return real, real, true
	}
	c := strings.TrimSpace(string(b))
	if c == "" || strings.ContainsRune(c, 0) {
		return "", "", false
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(real, c)
	}
	allowed := func(p string) bool { return within(root, p) || (pc != "" && p == pc) }
	if c = filepath.Clean(c); !allowed(c) {
		return "", "", false
	}
	realCommon, err := filepath.EvalSymlinks(c)
	if err != nil || !allowed(realCommon) {
		return "", "", false
	}
	return real, realCommon, true
}

// commonGitDir returns the project's common git dir with symlinks resolved, the directory
// `git rev-parse --git-common-dir` names there, read from files; "" when it cannot be read.
func commonGitDir(project string) string {
	if project == "" {
		return ""
	}
	pr, err := filepath.EvalSymlinks(project)
	if err != nil {
		return ""
	}
	gd, ok := resolveGitDir(pr)
	if !ok {
		return ""
	}
	if b, found := readGitFile(filepath.Join(gd, "commondir"), maxSmallGitFile); found {
		c := strings.TrimSpace(string(b))
		if c == "" || strings.ContainsRune(c, 0) {
			return ""
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gd, c)
		}
		gd = c
	}
	real, err := filepath.EvalSymlinks(gd)
	if err != nil {
		return ""
	}
	return real
}

// linksBack reports whether the admin entry's gitdir file names dotgit, the worktree's .git
// file. git writes it absolute, or relative to the entry with worktree.useRelativePaths.
func linksBack(entry, dotgit string) bool {
	b, found := readGitFile(filepath.Join(entry, "gitdir"), maxSmallGitFile)
	if !found {
		return false
	}
	p := strings.TrimSpace(string(b))
	if p == "" || strings.ContainsRune(p, 0) {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(entry, p)
	}
	if p = filepath.Clean(p); p == dotgit {
		return true
	}
	real, err := filepath.EvalSymlinks(p)
	return err == nil && real == dotgit
}

// refsInFiles reports whether the repository at commondir keeps its refs as files. A
// reftable repository (extensions.refStorage = reftable) keeps HEAD in the reftable stack
// and leaves stub HEAD and refs files that git ignores, so it reads as unknown rather
// than trusting them; there is no reftable parser here. A config that exists but cannot
// be read is unknown too. A missing config is the files backend, git's default.
func refsInFiles(commondir string) bool {
	path := filepath.Join(commondir, "config")
	b, found := readGitFile(path, maxGitConfig)
	if !found {
		_, err := os.Lstat(path)
		return os.IsNotExist(err)
	}
	return !configSetsReftable(b)
}

// configSetsReftable reports whether a git config sets extensions.refStorage to reftable.
// Section and key names are matched case-insensitively, a key may follow its section
// header on the same line, and a value may be quoted or carry a trailing comment. git
// reads repository extensions from this file alone, so includes are not followed.
func configSetsReftable(b []byte) bool {
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				section = ""
				continue
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			line = strings.TrimSpace(line[end+1:])
		}
		if section != "extensions" || line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		if !strings.EqualFold(strings.TrimSpace(key), "refstorage") {
			continue
		}
		if i := strings.IndexAny(value, "#;"); i >= 0 {
			value = value[:i]
		}
		if strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), "reftable") {
			return true
		}
	}
	return false
}

// packedRef looks ref up in a packed-refs file.
func packedRef(path, ref string) (string, bool) {
	b, found := readGitFile(path, maxPackedRefFile)
	if !found {
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxPackedRefFile)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue // header, or the peeled id of the tag above
		}
		id, name, ok := strings.Cut(line, " ")
		if ok && name == ref && isObjectID(id) {
			return id, true
		}
	}
	return "", false
}

// readGitFile reads at most max bytes of a regular file. found is false when the path is
// missing, is not a regular file (a symlink, FIFO or device is refused without blocking),
// or is larger than max.
func readGitFile(path string, max int64) ([]byte, bool) {
	// O_NOFOLLOW refuses a symlink in the last component; O_NONBLOCK keeps a FIFO planted
	// in place of the file from blocking the open, and the regular-file check then refuses it.
	f, err := openGitFile(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, false
	}
	return b, true
}

// openGitFile opens one git file for readGitFile. Tests swap it for a slow opener.
var openGitFile = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// joinWithin joins a ref name onto base and reports whether the result, once cleaned,
// is still inside base.
func joinWithin(base, ref string) (string, bool) {
	p := filepath.Join(base, ref)
	if !within(base, p) {
		return "", false
	}
	return p, true
}

// within reports whether the clean path p is base or lies under it, by name alone.
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// validRefName accepts a ref name HEAD may point at: under refs/, already clean, with no
// "." or ".." component, backslash or NUL.
func validRefName(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\\\x00") {
		return false
	}
	if filepath.Clean(ref) != ref {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object id in lowercase hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

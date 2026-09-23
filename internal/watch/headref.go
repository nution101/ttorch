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
// neither restarts the clock nor counts against the worker.

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Read bounds. HEAD, a .git file, commondir and a loose ref are one short line each; a
// packed-refs file can hold many refs, so it gets a larger cap. Over the cap reads as unknown.
const (
	maxSmallGitFile  = 4 << 10 // 4 KiB
	maxPackedRefFile = 8 << 20 // 8 MiB
)

// headIdentity resolves HEAD in the worktree at dir to a commit id, using file reads only.
// It follows the worktree's .git (a directory, or a file carrying "gitdir: <path>"), the
// gitdir's commondir for a linked worktree, at most one symref level from HEAD to a ref,
// and packed-refs when the ref is not loose. ok is false for anything else.
func headIdentity(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	gitdir, ok := resolveGitDir(dir)
	if !ok {
		return "", false
	}
	commondir := gitdir
	if b, found := readGitFile(filepath.Join(gitdir, "commondir"), maxSmallGitFile); found {
		c := strings.TrimSpace(string(b))
		if c == "" || strings.ContainsRune(c, 0) {
			return "", false
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		commondir = filepath.Clean(c)
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
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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

// joinWithin joins a ref name onto base and reports whether the result, once cleaned,
// is still inside base.
func joinWithin(base, ref string) (string, bool) {
	p := filepath.Join(base, ref)
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return p, true
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

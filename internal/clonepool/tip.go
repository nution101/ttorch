package clonepool

// observeTip reads the commit a clone slot's HEAD names, from files, without starting a git
// process. A slot's repository belongs to the worker that last held it, and any git command
// run there reads that worker's config: core.fsmonitor runs on status and checkout,
// diff.external on diff, hooks on checkout. So the pool never runs git in an existing slot.
// It reads the tip from files and asks main about it.
//
// It covers the clone layout only: <slot>/.git is a directory ttorch created, with no
// gitfile and no commondir to follow. Anything else reads as unknown, and an unknown tip
// keeps the slot out of reuse. The reader in internal/watch handles linked worktrees as
// well; this is the subset a clone needs.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Read bounds. HEAD and a loose ref are one short line; packed-refs can hold many refs.
const (
	maxSmallFile  = 4 << 10 // 4 KiB
	maxPackedRefs = 8 << 20 // 8 MiB
	maxConfig     = 1 << 20 // 1 MiB
)

// errMissing reports that a file, or a directory on the way to it, does not exist.
var errMissing = errors.New("missing")

// observeTip returns the commit id HEAD resolves to in the clone at slot. ok is false when
// .git is not a directory, a file on the way is a symlink or not a regular file, the
// repository keeps refs in a reftable, or HEAD does not resolve to a full object id
// within one symref level.
func observeTip(slot string) (string, bool) {
	if !refsInFiles(slot) {
		return "", false
	}
	b, err := readUnder(slot, ".git/HEAD", maxSmallFile)
	if err != nil {
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
	b, err = readUnder(slot, ".git/"+ref, maxSmallFile)
	switch {
	case err == nil:
		id := strings.TrimSpace(string(b))
		if isObjectID(id) {
			return id, true
		}
		return "", false // a second symref level, or garbage
	case errors.Is(err, errMissing):
		return packedRef(slot, ref)
	default:
		return "", false
	}
}

// refsInFiles reports whether the clone keeps its refs as files. A worker can convert its
// clone to the reftable backend, which leaves stub HEAD and ref files that git ignores, so
// any mention of refstorage in the config reads as not files. That is deliberately
// coarse: a false positive keeps a slot out of reuse, which is the safe direction.
func refsInFiles(slot string) bool {
	b, err := readUnder(slot, ".git/config", maxConfig)
	switch {
	case errors.Is(err, errMissing):
		return true
	case err != nil:
		return false
	}
	return !bytes.Contains(bytes.ToLower(b), []byte("refstorage"))
}

// packedRef looks ref up in the clone's packed-refs file.
func packedRef(slot, ref string) (string, bool) {
	b, err := readUnder(slot, ".git/packed-refs", maxPackedRefs)
	if err != nil {
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxPackedRefs)
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

// readUnder reads at most max bytes of the regular file rel names under the directory
// anchor. Every component, anchor's own last one included, is opened with O_NOFOLLOW, so
// a symlink anywhere on the way fails the read instead of being followed, and O_NONBLOCK
// keeps a FIFO from blocking the open. A missing component reports errMissing.
func readUnder(anchor, rel string, max int64) ([]byte, error) {
	f, err := openUnder(anchor, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errMissing
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("over the read bound")
	}
	return b, nil
}

// openUnder opens rel under anchor one component at a time with O_NOFOLLOW (see readUnder).
func openUnder(anchor, rel string) (*os.File, error) {
	if filepath.IsAbs(rel) {
		return nil, errors.New("path is not relative")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("path is not clean")
		}
	}
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := retryEINTR(func() (int, error) { return unix.Open(anchor, dirFlags, 0) })
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: anchor, Err: err}
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := retryEINTR(func() (int, error) { return unix.Openat(fd, part, dirFlags, 0) })
		_ = unix.Close(fd)
		if err != nil {
			return nil, &fs.PathError{Op: "openat", Path: filepath.Join(anchor, rel), Err: err}
		}
		fd = next
	}
	last := parts[len(parts)-1]
	ffd, err := retryEINTR(func() (int, error) {
		return unix.Openat(fd, last, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	})
	_ = unix.Close(fd)
	if err != nil {
		return nil, &fs.PathError{Op: "openat", Path: filepath.Join(anchor, rel), Err: err}
	}
	return os.NewFile(uintptr(ffd), filepath.Join(anchor, rel)), nil
}

func retryEINTR(open func() (int, error)) (int, error) {
	for {
		fd, err := open()
		if err != unix.EINTR {
			return fd, err
		}
	}
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

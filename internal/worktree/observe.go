package worktree

// ObserveHead tells which commit a worker's working directory has checked out without
// starting a git process. The directory and its git config belong to the worker, and any
// git command run there can be steered into running a program the worker configured:
// log.showSignature hands a signed commit to gpg.program, and a partial clone lazily fetches
// a missing object through the promisor remote's uploadpack, core.sshCommand or a credential
// helper. Closing those one flag at a time leaves the next route open, so HEAD is read from
// files instead.
//
// Anything unexpected reads as unknown. Every file is opened through openUnder, which
// follows no symlink on the way. Only the files ref backend is read: a reftable
// repository's HEAD is always unknown. The read takes no deadline of its own; a caller that
// must not wait on a path that never answers (an automount or FUSE path behind gitdir:)
// runs it under one, as the stall watcher does.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Read bounds. HEAD, a .git file, commondir and a loose ref are one short line each; a
// packed-refs file can hold many refs, so it gets a larger cap. Over the cap reads as unknown.
const (
	maxSmallGitFile  = 4 << 10 // 4 KiB
	maxPackedRefFile = 8 << 20 // 8 MiB
	maxGitConfig     = 1 << 20 // 1 MiB
)

// ObserveHead resolves HEAD in the worktree at dir to a commit id, using file reads only.
// project is the repository ttorch cut the worktree from ("" when none is recorded). It
// follows the worktree's .git (a directory, or a file carrying "gitdir: <path>"), the
// gitdir's commondir for a linked worktree, at most one symref level from HEAD to a ref,
// and packed-refs when the ref is not loose. The git dir and commondir must stay where
// ttorch put them (see confineGitDir), and every file is opened through openUnder, which
// follows no symlink. ok is false for anything else.
//
// A clone reads the same way: its .git is a directory inside dir and it has no commondir,
// so project is not consulted. The id is what the worker's files claim. Callers pin every
// later step to it, so a worker that lies about HEAD only chooses which of its own commits
// is looked at.
func ObserveHead(dir, project string) (string, bool) {
	root, ok := resolveWorktree(dir)
	if !ok {
		return "", false
	}
	dotgit, _, ok := resolveGitDir(root)
	if !ok {
		return "", false
	}
	gitdir, commondir, ok := confineGitDir(root, project, dotgit)
	if !ok || !refsInFiles(commondir) {
		return "", false
	}

	b, st := readGitFile(gitdir.join("HEAD"), maxSmallGitFile)
	if st != fileRead {
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
	for _, base := range []gitPath{gitdir, commondir} {
		b, st := readGitFile(base.join(ref), maxSmallGitFile)
		switch st {
		case fileMissing:
			continue
		case fileRefused:
			return "", false
		}
		id := strings.TrimSpace(string(b))
		if isObjectID(id) {
			return id, true
		}
		return "", false // a second symref level, or garbage
	}
	return packedRef(commondir.join("packed-refs"), ref)
}

// gitPath names a file or directory the walk may open: rel, relative and clean, under
// anchor, a directory ttorch recorded (the worktree root or the project's common git dir).
type gitPath struct {
	anchor, rel string
}

func (g gitPath) join(name string) gitPath { return gitPath{g.anchor, filepath.Join(g.rel, name)} }
func (g gitPath) path() string             { return filepath.Join(g.anchor, g.rel) }

// under returns p as a gitPath below anchor when, by name, p is anchor or lies inside it.
func under(anchor, p string) (gitPath, bool) {
	if !within(anchor, p) {
		return gitPath{}, false
	}
	rel, _ := filepath.Rel(anchor, p)
	return gitPath{anchor, rel}, true
}

// resolveWorktree returns the worktree path ttorch recorded with the symlinks in its
// parent resolved, once, so it can be compared with the resolved paths git writes. The
// last component is left as recorded: openUnder opens it with O_NOFOLLOW, so a symlink
// swapped in at the worktree's own path fails every read rather than being followed.
func resolveWorktree(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return "", false
	}
	return filepath.Join(parent, base), true
}

// resolveGitDir returns the git directory for the worktree at root, by name: root/.git
// when it is a directory (isDir), or the path a regular .git file names. A symlinked .git
// reads as unknown.
func resolveGitDir(root string) (gitdir string, isDir, ok bool) {
	f, err := openUnder(root, ".git")
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.IsDir() {
		return filepath.Join(root, ".git"), true, true
	}
	b, st := readOpened(f, maxSmallGitFile)
	if st != fileRead {
		return "", false, false
	}
	p, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !found || p == "" || strings.ContainsRune(p, 0) {
		return "", false, false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), false, true
}

// confineGitDir accepts the git dir a worktree's .git names only where ttorch put it:
// inside the worktree, or the project's admin entry for this worktree,
// <common>/worktrees/<name>, whose gitdir file points back at root/.git. The commondir
// it names must be inside the worktree or be the project's common git dir. Both checks
// are on the path as written, and nothing is opened for a path that fails them. What
// passes is returned anchored at the worktree root or the project's common git dir, and
// every later read walks down from that anchor through openUnder, so a symlink anywhere
// below it is refused rather than followed. The only files opened here are the admin
// entry's gitdir and the git dir's commondir, both through openUnder, plus the project's
// own .git and commondir in commonGitDir.
func confineGitDir(root, project, dotgit string) (gitPath, gitPath, bool) {
	pc := commonGitDir(project)
	gitdir, admin := under(root, dotgit)
	admin = !admin
	if admin {
		if pc == "" || filepath.Dir(dotgit) != filepath.Join(pc, "worktrees") {
			return gitPath{}, gitPath{}, false
		}
		gitdir, _ = under(pc, dotgit)
		if !linksBack(gitdir, filepath.Join(root, ".git")) {
			return gitPath{}, gitPath{}, false
		}
	}

	b, st := readGitFile(gitdir.join("commondir"), maxSmallGitFile)
	switch st {
	case fileMissing:
		if admin {
			return gitPath{}, gitPath{}, false // git always writes commondir for a linked worktree
		}
		return gitdir, gitdir, true
	case fileRefused:
		return gitPath{}, gitPath{}, false
	}
	c := strings.TrimSpace(string(b))
	if c == "" || strings.ContainsRune(c, 0) {
		return gitPath{}, gitPath{}, false
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(gitdir.path(), c)
	}
	c = filepath.Clean(c)
	if common, ok := under(root, c); ok {
		return gitdir, common, true
	}
	if pc != "" && c == pc {
		return gitdir, gitPath{pc, "."}, true
	}
	return gitPath{}, gitPath{}, false
}

// commonGitDir returns the project's common git dir with symlinks resolved, the directory
// `git rev-parse --git-common-dir` names there, read from files; "" when it cannot be read.
// The project is the lead's checkout as ttorch recorded it, so its path is resolved
// freely. A .git directory is taken as the common dir as it stands: it is shared with
// every worker, so a commondir file planted in it is not followed. A .git file (the
// project is itself a linked worktree) is followed to its git dir and that dir's commondir.
func commonGitDir(project string) string {
	if project == "" {
		return ""
	}
	pr, err := filepath.EvalSymlinks(project)
	if err != nil {
		return ""
	}
	dotgit, isDir, ok := resolveGitDir(pr)
	if !ok {
		return ""
	}
	gd, err := filepath.EvalSymlinks(dotgit)
	if err != nil {
		return ""
	}
	if isDir {
		return gd
	}
	b, st := readGitFile(gitPath{gd, "commondir"}, maxSmallGitFile)
	switch st {
	case fileMissing:
		return gd
	case fileRefused:
		return ""
	}
	c := strings.TrimSpace(string(b))
	if c == "" || strings.ContainsRune(c, 0) {
		return ""
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(gd, c)
	}
	real, err := filepath.EvalSymlinks(c)
	if err != nil {
		return ""
	}
	return real
}

// linksBack reports whether the admin entry's gitdir file names dotgit, the worktree's .git
// file, by name. git writes it resolved and absolute, or relative to the entry with
// worktree.useRelativePaths.
func linksBack(entry gitPath, dotgit string) bool {
	b, st := readGitFile(entry.join("gitdir"), maxSmallGitFile)
	if st != fileRead {
		return false
	}
	p := strings.TrimSpace(string(b))
	if p == "" || strings.ContainsRune(p, 0) {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(entry.path(), p)
	}
	return filepath.Clean(p) == dotgit
}

// refsInFiles reports whether the repository at commondir keeps its refs as files. A
// reftable repository (extensions.refStorage = reftable) keeps HEAD in the reftable stack
// and leaves stub HEAD and refs files that git ignores, so it reads as unknown rather
// than trusting them; there is no reftable parser here. A config that exists but cannot
// be read is unknown too. A missing config is the files backend, git's default.
func refsInFiles(commondir gitPath) bool {
	b, st := readGitFile(commondir.join("config"), maxGitConfig)
	switch st {
	case fileMissing:
		return true
	case fileRefused:
		return false
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
func packedRef(path gitPath, ref string) (string, bool) {
	b, st := readGitFile(path, maxPackedRefFile)
	if st != fileRead {
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

// fileState is the outcome of reading one git file.
type fileState int

const (
	fileRead    fileState = iota
	fileMissing           // the file, or a directory on the way to it, does not exist
	fileRefused           // a symlink on the way, not a regular file, over the cap, or unreadable
)

// readGitFile reads at most max bytes of the regular file p names, opened through
// openUnder.
func readGitFile(p gitPath, max int64) ([]byte, fileState) {
	f, err := openUnder(p.anchor, p.rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fileMissing
		}
		return nil, fileRefused
	}
	defer f.Close()
	return readOpened(f, max)
}

// readOpened reads at most max bytes of f, refusing anything that is not a regular file
// (a FIFO, device or directory) or is larger than max.
func readOpened(f *os.File, max int64) ([]byte, fileState) {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fileRefused
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, fileRefused
	}
	return b, fileRead
}

// errBadGitPath is returned for a rel that is absolute or has an empty, "." or ".."
// component; gitPath values built by under and join never do.
var errBadGitPath = errors.New("git path is not a clean relative path")

// openUnder opens rel under the directory anchor one component at a time, each with
// O_NOFOLLOW, so a symlink anywhere on the way (anchor's own last component, a directory
// partway down, or the file itself) fails the open instead of being followed, and the
// check cannot be raced by swapping a directory for a symlink between check and open.
// O_NONBLOCK keeps a FIFO planted as the file from blocking the open; readOpened then
// refuses it. A missing component reports fs.ErrNotExist.
func openUnder(anchor, rel string) (*os.File, error) {
	if filepath.IsAbs(rel) {
		return nil, errBadGitPath
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errBadGitPath
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

// Kind is the shape of a task's working directory: a linked worktree of the lead's
// repository, or a repository of its own.
type Kind int

const (
	// KindUnknown is anything KindOf cannot place.
	KindUnknown Kind = iota
	// KindWorktree is a linked worktree: .git is a regular file naming its git dir.
	KindWorktree
	// KindClone is a repository of its own: .git is a directory.
	KindClone
)

func (k Kind) String() string {
	switch k {
	case KindWorktree:
		return "worktree"
	case KindClone:
		return "clone"
	}
	return "unknown"
}

// KindOf reports which kind of working directory dir is, from its .git entry alone: a
// directory for a clone, a regular file for a linked worktree. It starts no process and
// opens no file, so it is safe on a directory the worker controls, and a task's kind needs
// no record of its own. Anything else is KindUnknown with an error saying why, and that
// includes dir or its .git being a symlink: dir is the path ttorch recorded, and as in
// ObserveHead a link swapped in there is refused rather than followed.
//
// The answer comes from the worker's files, so like ObserveHead's it is the worker's claim:
// a worker can turn its clone's .git into a file and its task then reads as a worktree. The
// lead's own checkout has a .git directory too, so a caller that can be handed one (a cc
// session opened without isolation records its cwd) must not take KindClone to mean ttorch
// provisioned the directory.
func KindOf(dir string) (Kind, error) {
	if dir == "" {
		return KindUnknown, errors.New("no working directory recorded")
	}
	dir = filepath.Clean(dir)
	fi, err := os.Lstat(dir)
	if err != nil {
		return KindUnknown, err
	}
	if !fi.IsDir() {
		return KindUnknown, fmt.Errorf("%s is not a directory (mode %s)", dir, fi.Mode().Type())
	}
	dotgit := filepath.Join(dir, ".git")
	gi, err := os.Lstat(dotgit)
	if err != nil {
		return KindUnknown, err
	}
	switch {
	case gi.IsDir():
		return KindClone, nil
	case gi.Mode().IsRegular():
		return KindWorktree, nil
	}
	return KindUnknown, fmt.Errorf("%s is neither a directory nor a regular file (mode %s)", dotgit, gi.Mode().Type())
}

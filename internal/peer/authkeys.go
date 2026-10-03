package peer

// The authorized_keys line that pins a parent's control key to the control channel, and putting
// it in place on the peer. `ttorch peer init` runs this on the peer, inside the lead's own
// interactive ssh session; nothing on the control channel can.
//
// The line is the whole bound on what the control key can do:
//
//	command="<absolute path to ttorch> peer serve --parent <parent>",restrict ssh-ed25519 <key> ttorch-peer-control:<parent>
//
// sshd runs the forced command for that key whatever the client asked for, and puts the client's
// request in SSH_ORIGINAL_COMMAND, which Serve reads as one verb. restrict turns off forwarding of
// every kind, pty allocation and ~/.ssh/rc. The path is absolute so the PATH sshd gives a forced
// command, which may not reach ~/.ttorch/bin, does not decide which program runs. --parent is the
// coordinator id of the parent the key was installed for: serve takes the parent a request comes
// from from its own argv, which only this line sets, so holding one parent's key does not let a
// client speak as another. The comment carries the same id, for a person reading the file and for
// adopt --force, which finds every control key's line by it. This file is in
// the gate's covered set (orchestrator.ttorchSourceFiles) beside serve.go: a change to the line,
// or to which binary it names, changes what a parent's key reaches.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/nution101/ttorch/internal/db"
)

// ControlKeyType is the only key type a control key may be.
const ControlKeyType = "ssh-ed25519"

// controlKeyCommentPrefix starts the comment on every control key's line, followed by the
// parent's coordinator id.
const controlKeyCommentPrefix = "ttorch-peer-control:"

// maxAuthorizedKeys bounds how much of authorized_keys is read to look for an existing entry.
const maxAuthorizedKeys = 8 << 20

// ControlKeyComment is the comment a parent's control key carries.
func ControlKeyComment(parentID string) string { return controlKeyCommentPrefix + parentID }

// AuthorizedKeyLine is the line that admits a parent's control key and runs nothing but
// `<binary> peer serve --parent <parentID>` for it. binary must have passed resolveServeBinary,
// blob ParseControlKey and parentID db.ValidCoordID.
func AuthorizedKeyLine(binary, blob, parentID string) string {
	return fmt.Sprintf(`command="%s peer serve --parent %s",restrict %s %s %s`, binary, parentID, ControlKeyType, blob, ControlKeyComment(parentID))
}

// ParseControlKey reads a public key as ssh-keygen writes it, "ssh-ed25519 <base64> [comment]",
// and returns the base64 blob. Anything else is refused: another key type, options in front, a
// second line, a non-printing character, or a blob that does not hold exactly one ed25519 key.
func ParseControlKey(pub string) (string, error) {
	s := strings.TrimSpace(pub)
	if strings.IndexFunc(s, func(r rune) bool { return r != ' ' && !unicode.IsGraphic(r) }) >= 0 {
		return "", errors.New("a control key is one line of printable text")
	}
	fields := strings.Fields(s)
	if len(fields) < 2 || fields[0] != ControlKeyType {
		return "", fmt.Errorf("a control key is %s <base64> [comment]", ControlKeyType)
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", fmt.Errorf("the control key is not base64: %w", err)
	}
	typ, rest, ok := readWireString(raw)
	if !ok || string(typ) != ControlKeyType {
		return "", fmt.Errorf("the control key's blob is not an %s key", ControlKeyType)
	}
	key, rest, ok := readWireString(rest)
	if !ok || len(key) != 32 || len(rest) != 0 {
		return "", fmt.Errorf("the control key's blob is not one 32-byte %s key", ControlKeyType)
	}
	return fields[1], nil
}

// readWireString reads one SSH wire-format string (a big-endian uint32 length, then the bytes).
func readWireString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// shellSafe reports whether every rune of path is one a shell reads as part of a plain word.
// sshd runs a forced command with the account's shell (`$SHELL -c`), so the path must need no
// quoting.
func shellSafe(path string) bool {
	for _, r := range path {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._+@-", r)) {
			return false
		}
	}
	return path != ""
}

// ProvisionControlKey admits a parent's control key on this machine: it reads pubKey
// (ParseControlKey), resolves the running ttorch to the path the forced command will name
// (ServeBinary), and installs the line in <sshDir>/authorized_keys (installControlKey). Everything
// that decides what the line says is in this file. It returns the binary path with the result.
// With replace (adopt --force), every other control key's line is removed in the same step.
func ProvisionControlKey(sshDir, pubKey, parentID string, uid int, replace bool) (string, InstallResult, error) {
	// The id goes into the forced command, which sshd hands to a shell.
	if err := db.ValidCoordID(parentID); err != nil {
		return "", InstallResult{}, fmt.Errorf("the parent's coordinator id: %w", err)
	}
	blob, err := ParseControlKey(pubKey)
	if err != nil {
		return "", InstallResult{}, err
	}
	bin, err := ServeBinary(uid)
	if err != nil {
		return "", InstallResult{}, err
	}
	res, err := installControlKey(sshDir, AuthorizedKeyLine(bin, blob, parentID), blob, uid, replace)
	return bin, res, err
}

// ServeBinary is the path of the running ttorch as the forced command will name it
// (resolveServeBinary on os.Executable).
func ServeBinary(uid int) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding the running ttorch binary: %w", err)
	}
	return resolveServeBinary(exe, uid)
}

// resolveServeBinary resolves exe, the ttorch that is running `peer init`, to the path the forced
// command will name: absolute, with every symlink resolved, so self-update replacing the file at
// that path keeps the line right. It refuses a path a shell would not read as one plain word, and
// a binary that someone other than its owner could replace: it must be an executable regular
// file owned by uid or by root, with no group or other write bit, in a directory with the same
// owners and no group or other write bit.
func resolveServeBinary(exe string, uid int) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("the ttorch binary's path %q is not absolute", exe)
	}
	path, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolving the ttorch binary: %w", err)
	}
	if !shellSafe(path) {
		return "", fmt.Errorf("the ttorch binary's path %q holds a character the shell running a forced command would read; install ttorch at a path of letters, digits and / . _ + @ -", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", path)
	}
	if err := ownedBy(path, fi, uid, true); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	di, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if err := ownedBy(dir, di, uid, true); err != nil {
		return "", err
	}
	return path, nil
}

// ownedBy reports why fi, the entry at path, is owned by neither uid nor (when rootToo) root, or
// carries a group or other write bit.
func ownedBy(path string, fi fs.FileInfo, uid int, rootToo bool) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: its owner cannot be read", path)
	}
	if int(st.Uid) != uid && !(rootToo && st.Uid == 0) {
		return fmt.Errorf("%s is owned by uid %d, not by this account (uid %d)", path, st.Uid, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by its group or by others (mode %s)", path, fi.Mode().Perm())
	}
	return nil
}

// CheckPrivate reports why fi, the entry at path from Lstat or fstat, is not a directory (dir
// set) or a regular file (dir unset) owned by uid with no group or other write bit. A symlink is
// refused as one.
func CheckPrivate(path string, fi fs.FileInfo, uid int, dir bool) error {
	kind := "a regular file"
	if dir {
		kind = "a directory"
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link; it must be %s this account owns that no one else can write", path, kind)
	case dir && !fi.IsDir(), !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not %s", path, kind)
	}
	return ownedBy(path, fi, uid, false)
}

// OpenPrivateDir opens dir without following a symlink in its last component, and checks the
// directory it got, through the descriptor (fstat), with CheckPrivate: a directory uid owns with no
// group or other write bit. With create set, a missing dir is first made 0700; without it, a
// missing dir is (nil, nil). Files in it are then opened with OpenAt, relative to the descriptor,
// so a rename of dir or of its parents after the check cannot point a read or a write somewhere
// the check never saw.
func OpenPrivateDir(dir string, uid int, create bool) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the directory %q is not absolute", dir)
	}
	if create {
		if err := os.Mkdir(dir, 0o700); err == nil {
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, nil
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		// Linux answers a symlink with ELOOP and darwin with ENOTDIR. The open has refused
		// either way; the Lstat only names what was there.
		if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link; it must be a directory this account owns that no one else can write", dir)
		}
		return nil, fmt.Errorf("%s is not a directory", dir)
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir)
	fi, err := f.Stat()
	if err == nil {
		err = CheckPrivate(dir, fi, uid, true)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// OpenAt opens name, one path component, in the directory dir was opened as, never following a
// symlink: a symlink there fails with ELOOP. flags are os.OpenFile's.
func OpenAt(dir *os.File, name string, flags int, perm os.FileMode) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("%q is not one path component", name)
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// InstallResult is what installControlKey did. Added is false when the line was already there.
// StaleControlKeys counts other control keys' lines left in the file, a previous parent's for
// instance. Removed is the parent each removed control key's line named (its comment), in file
// order, when replace removed any.
type InstallResult struct {
	Path             string
	Added            bool
	StaleControlKeys int
	Removed          []string
}

// afterAuthorizedKeysRead, when set, runs once authorized_keys has been read, before anything is
// written: a test's way to change the file at that point. It is nil outside tests.
var afterAuthorizedKeysRead func()

// installControlKey puts line, which admits the key blob, in <sshDir>/authorized_keys. It is
// unexported so that outside this package the only way to admit a key is ProvisionControlKey,
// whose line always carries the forced command and restrict. sshDir is created 0700 and the file
// 0600 when missing. Both must then be owned by uid and writable by no one else, and neither may
// be a symlink: the directory is opened once (OpenPrivateDir) and the file opened relative to it
// with O_NOFOLLOW (OpenAt), each checked through its descriptor, so what is written is what was
// checked. sshd's StrictModes refuses a looser file anyway, and a file someone else can write
// could admit any key.
//
// Without replace the file is appended to, never rewritten, and a last line without a newline
// gets one first, so no existing entry is touched. If the file already holds line, nothing is
// written. If it lists the same key any other way (without the forced command, say), that is
// refused and nothing is written: the key would then have two meanings. Other control keys'
// lines are counted, not removed.
//
// With replace, every line that ends in a control key's comment and is not line is removed,
// this key's own older lines included, and every other line is kept byte for byte. A line that
// lists this key without that comment is still refused: it is someone's own entry, and could
// give the key more than the forced command. The new contents go to a new 0600 file in the same
// directory, which is synced and then renamed over authorized_keys relative to the directory's
// descriptor, so sshd sees the old file or the new one, never a partial one, and no symlink is
// followed. Just before the rename the file is checked against what was read (same file, size
// and modification time); if anything wrote it meanwhile, the new file is discarded and the
// install refused, so that write is not lost.
func installControlKey(sshDir, line, blob string, uid int, replace bool) (InstallResult, error) {
	if !filepath.IsAbs(sshDir) {
		return InstallResult{}, fmt.Errorf("the ssh directory %q is not absolute", sshDir)
	}
	if strings.IndexFunc(line, func(r rune) bool { return r != ' ' && !unicode.IsGraphic(r) }) >= 0 || blob == "" ||
		!containsField(line, blob) {
		return InstallResult{}, errors.New("the authorized_keys line must be one printable line holding the key")
	}
	dir, err := OpenPrivateDir(sshDir, uid, true)
	if err != nil {
		return InstallResult{}, err
	}
	defer dir.Close()
	path := filepath.Join(sshDir, "authorized_keys")
	out := InstallResult{Path: path}
	f, err := OpenAt(dir, "authorized_keys", os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NONBLOCK, 0o600)
	if errors.Is(err, syscall.ELOOP) {
		return out, fmt.Errorf("%s is a symbolic link; it must be a regular file this account owns that no one else can write", path)
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	if err := CheckPrivate(path, fi, uid, false); err != nil {
		return out, err
	}
	have, err := io.ReadAll(io.LimitReader(f, maxAuthorizedKeys+1))
	if err != nil {
		return out, err
	}
	if len(have) > maxAuthorizedKeys {
		return out, fmt.Errorf("%s is over %d bytes", path, maxAuthorizedKeys)
	}
	read, err := f.Stat()
	if err != nil {
		return out, err
	}
	if read.Size() != int64(len(have)) {
		return out, fmt.Errorf("%s changed while it was being read; nothing was written, run it again", path)
	}
	if afterAuthorizedKeysRead != nil {
		afterAuthorizedKeysRead()
	}
	present := false
	var kept bytes.Buffer
	raw := strings.SplitAfter(string(have), "\n")
	for i, r := range raw {
		l := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(r, "\n"), "\r"))
		switch {
		case l == line:
			present = true
		case replace && isControlKeyLine(l):
			fields := strings.Fields(l)
			out.Removed = append(out.Removed, removedParent(fields[len(fields)-1]))
			continue
		case containsField(l, blob):
			return InstallResult{Path: path}, fmt.Errorf("%s line %d already lists this control key another way; remove that line and provision again", path, i+1)
		case isControlKeyLine(l):
			out.StaleControlKeys++
		}
		kept.WriteString(r)
	}
	if len(out.Removed) > 0 {
		if kept.Len() > 0 && !bytes.HasSuffix(kept.Bytes(), []byte("\n")) {
			kept.WriteByte('\n')
		}
		if !present {
			kept.WriteString(line + "\n")
			out.Added = true
		}
		if err := replaceAt(dir, "authorized_keys", read, kept.Bytes()); err != nil {
			return InstallResult{Path: path}, err
		}
		return out, nil
	}
	if present {
		return out, nil
	}
	var b bytes.Buffer
	if len(have) > 0 && have[len(have)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(line)
	b.WriteByte('\n')
	if _, err := f.Write(b.Bytes()); err != nil {
		return out, err
	}
	if err := f.Sync(); err != nil {
		return out, err
	}
	out.Added = true
	return out, nil
}

// removedParent is what a removed line's comment says about the parent it was for: the
// coordinator id, or a placeholder when the comment holds something else.
func removedParent(comment string) string {
	id := strings.TrimPrefix(comment, controlKeyCommentPrefix)
	if db.ValidCoordID(id) != nil {
		return "(not a coordinator id)"
	}
	return id
}

// replaceAt replaces the file name in dir with content, atomically, if it is still the file read
// describes, unchanged: same file, size and modification time. The content goes to a new 0600
// file beside it (O_EXCL, never through a symlink), which is synced, checked private, and renamed
// over name relative to dir's descriptor; dir is then synced so the rename survives a crash. On a
// failure the new file is removed and name is left as it was.
func replaceAt(dir *os.File, name string, read os.FileInfo, content []byte) (err error) {
	path := filepath.Join(dir.Name(), name)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmpName := "." + name + ".ttorch-" + hex.EncodeToString(suffix[:])
	tmp, err := OpenAt(dir, tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		tmp.Close()
		if !renamed {
			_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	tfi, err := tmp.Stat()
	if err != nil {
		return err
	}
	st, ok := read.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no file identity to check", path)
	}
	if err := CheckPrivate(filepath.Join(dir.Name(), tmpName), tfi, int(st.Uid), false); err != nil {
		return err
	}
	var now unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	if uint64(now.Dev) != uint64(st.Dev) || uint64(now.Ino) != uint64(st.Ino) || now.Size != read.Size() ||
		time.Unix(now.Mtim.Unix()).UnixNano() != read.ModTime().UnixNano() {
		return fmt.Errorf("%s changed while it was being rewritten; nothing was written, run it again", path)
	}
	if err := unix.Renameat(int(dir.Fd()), tmpName, int(dir.Fd()), name); err != nil {
		return &fs.PathError{Op: "rename", Path: path, Err: err}
	}
	renamed = true
	return dir.Sync()
}

func containsField(line, field string) bool {
	for _, f := range strings.Fields(line) {
		if f == field {
			return true
		}
	}
	return false
}

// isControlKeyLine reports whether an authorized_keys line ends with a control key's comment.
func isControlKeyLine(line string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && strings.HasPrefix(f[len(f)-1], controlKeyCommentPrefix)
}

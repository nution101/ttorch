package peer

// The authorized_keys line that pins a parent's control key to the control channel, and putting
// it in place on the peer. `ttorch peer init` runs this on the peer, inside the lead's own
// interactive ssh session; nothing on the control channel can.
//
// The line is the whole bound on what the control key can do:
//
//	command="<absolute path to ttorch> peer serve",restrict ssh-ed25519 <key> ttorch-peer-control:<parent>
//
// sshd runs the forced command for that key whatever the client asked for, and puts the client's
// request in SSH_ORIGINAL_COMMAND, which Serve reads as one verb. restrict turns off forwarding of
// every kind, pty allocation and ~/.ssh/rc. The path is absolute so the PATH sshd gives a forced
// command, which may not reach ~/.ttorch/bin, does not decide which program runs. This file is in
// the gate's covered set (orchestrator.ttorchSourceFiles) beside serve.go: a change to the line,
// or to which binary it names, changes what a parent's key reaches.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
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
// `<binary> peer serve` for it. binary must have passed resolveServeBinary and blob
// ParseControlKey.
func AuthorizedKeyLine(binary, blob, parentID string) string {
	return fmt.Sprintf(`command="%s peer serve",restrict %s %s %s`, binary, ControlKeyType, blob, ControlKeyComment(parentID))
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
func ProvisionControlKey(sshDir, pubKey, parentID string, uid int) (string, InstallResult, error) {
	blob, err := ParseControlKey(pubKey)
	if err != nil {
		return "", InstallResult{}, err
	}
	bin, err := ServeBinary(uid)
	if err != nil {
		return "", InstallResult{}, err
	}
	res, err := installControlKey(sshDir, AuthorizedKeyLine(bin, blob, parentID), blob, uid)
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

// InstallResult is what installControlKey did. Added is false when the line was already there.
// StaleControlKeys counts other control keys' lines in the file, a previous parent's for
// instance, which were left as they are.
type InstallResult struct {
	Path             string
	Added            bool
	StaleControlKeys int
}

// installControlKey appends line, which admits the key blob, to <sshDir>/authorized_keys. It is
// unexported so that outside this package the only way to admit a key is ProvisionControlKey,
// whose line always carries the forced command and restrict. sshDir
// is created 0700 and the file 0600 when missing. Both must then be owned by uid and writable by
// no one else, and neither may be a symlink: the directory is checked with Lstat, and the file is
// opened with O_NOFOLLOW and checked with fstat, so what is written is what was checked. sshd's
// StrictModes refuses a looser file anyway, and a file someone else can write could admit any key.
//
// The file is appended to, never rewritten, and a last line without a newline gets one first, so
// no existing entry is touched. If the file already holds line, nothing is written. If it lists
// the same key any other way (without the forced command, say), that is refused and nothing is
// written: the key would then have two meanings. Other control keys' lines are counted, not
// removed.
func installControlKey(sshDir, line, blob string, uid int) (InstallResult, error) {
	if !filepath.IsAbs(sshDir) {
		return InstallResult{}, fmt.Errorf("the ssh directory %q is not absolute", sshDir)
	}
	if strings.IndexFunc(line, func(r rune) bool { return r != ' ' && !unicode.IsGraphic(r) }) >= 0 || blob == "" ||
		!containsField(line, blob) {
		return InstallResult{}, errors.New("the authorized_keys line must be one printable line holding the key")
	}
	fi, err := os.Lstat(sshDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(sshDir, 0o700); err != nil {
			return InstallResult{}, err
		}
		if err := os.Chmod(sshDir, 0o700); err != nil {
			return InstallResult{}, err
		}
		fi, err = os.Lstat(sshDir)
	}
	if err != nil {
		return InstallResult{}, err
	}
	if err := CheckPrivate(sshDir, fi, uid, true); err != nil {
		return InstallResult{}, err
	}
	path := filepath.Join(sshDir, "authorized_keys")
	out := InstallResult{Path: path}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if errors.Is(err, syscall.ELOOP) {
		return out, fmt.Errorf("%s is a symbolic link; it must be a regular file this account owns that no one else can write", path)
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	if fi, err = f.Stat(); err != nil {
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
	present := false
	for i, l := range strings.Split(string(have), "\n") {
		l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		switch {
		case l == line:
			present = true
		case containsField(l, blob):
			return out, fmt.Errorf("%s line %d already lists this control key another way; remove that line and provision again", path, i+1)
		case isControlKeyLine(l):
			out.StaleControlKeys++
		}
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

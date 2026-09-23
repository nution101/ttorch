package herdr

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

var (
	// ErrNoConfigDir means no per-user config directory could be found to
	// hold the default socket: neither XDG_CONFIG_HOME nor HOME is set.
	ErrNoConfigDir = errors.New("herdr: no per-user config directory (XDG_CONFIG_HOME and HOME are unset)")
	// ErrUnsafeSocket means the socket path could have been planted by
	// another user. The error is an *UnsafeSocketError naming the path and
	// the reason.
	ErrUnsafeSocket = errors.New("herdr: refusing unsafe socket")
	// ErrInvalidSessionName means a session name is not one herdr accepts.
	ErrInvalidSessionName = errors.New("herdr: invalid session name")
)

// UnsafeSocketError is returned, without dialling, when the socket or its
// directory is not safe to trust. It matches ErrUnsafeSocket.
type UnsafeSocketError struct {
	Path   string
	Reason string
}

func (e *UnsafeSocketError) Error() string {
	return fmt.Sprintf("herdr: refusing socket %s: %s", e.Path, e.Reason)
}

func (e *UnsafeSocketError) Is(target error) bool { return target == ErrUnsafeSocket }

// DefaultSocketPath resolves the socket the way the herdr CLI does when no
// --session flag is given: HERDR_SOCKET_PATH, then HERDR_SESSION, then the
// default session.
func DefaultSocketPath() (string, error) {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p, nil
	}
	if name := os.Getenv("HERDR_SESSION"); name != "" {
		return SessionSocketPath(name)
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "herdr.sock"), nil
}

// maxSessionName is herdr's own limit on a session name, in bytes.
const maxSessionName = 64

// SessionSocketPath is the socket of the named Herdr session. Herdr keeps
// sessions under its config directory: $XDG_CONFIG_HOME/herdr when set,
// otherwise ~/.config/herdr. The name becomes a path component, so it must
// be one herdr itself accepts: 1 to 64 ASCII letters, digits, '.', '_' or
// '-', and not "." or "..". Anything else is ErrInvalidSessionName.
func SessionSocketPath(name string) (string, error) {
	if err := validSessionName(name); err != nil {
		return "", err
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions", name, "herdr.sock"), nil
}

func validSessionName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrInvalidSessionName)
	case len(name) > maxSessionName:
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidSessionName, maxSessionName)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q", ErrInvalidSessionName, name)
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		ok := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
			b == '.' || b == '_' || b == '-'
		if !ok {
			return fmt.Errorf("%w: %q may only contain ASCII letters, digits, '.', '_' and '-'", ErrInvalidSessionName, name)
		}
	}
	return nil
}

func configDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "herdr"), nil
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "herdr"), nil
	}
	// There is deliberately no shared fallback such as the temp directory:
	// a predictable name there can be created first by another user.
	return "", ErrNoConfigDir
}

// checkSocket is the pre-dial half of the socket check. It runs on every
// path, whether it came from HERDR_SOCKET_PATH, XDG_CONFIG_HOME, HOME or the
// caller, and refuses unless:
//
//   - the path itself (Lstat) is a socket, not a symlink or other file, and
//     is owned by this process's uid;
//   - its parent directory (Lstat) is a directory, not a symlink, owned by
//     this uid, with neither the group nor the other write bit set.
//
// A missing socket is ErrNoSocket. Only the mode bits are read: an ACL that
// grants another user write access to the directory is not detected. Nothing
// above the parent directory is checked either, so a user who can write to
// a directory higher up could still swap the path between this check and
// the dial. These checks only narrow that window; checkPeer, run on the
// connected socket, is what closes it.
func (c *Client) checkSocket() error {
	path := c.socketPath
	owner := c.owner
	if owner == nil {
		owner = statOwner
	}
	uid := uint32(os.Getuid())
	refuse := func(format string, args ...any) error {
		return &UnsafeSocketError{Path: path, Reason: fmt.Sprintf(format, args...)}
	}

	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNoSocket, path)
	}
	if err != nil {
		return fmt.Errorf("herdr: stat socket %s: %w", path, err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return refuse("not a socket (mode %s)", fi.Mode())
	}
	if got, ok := owner(fi); !ok {
		return refuse("cannot read the socket's owner")
	} else if got != uid {
		return refuse("socket is owned by uid %d, not %d", got, uid)
	}

	dir := filepath.Dir(path)
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("herdr: stat socket directory %s: %w", dir, err)
	}
	if !di.IsDir() {
		return refuse("directory %s is a symlink or not a directory (mode %s)", dir, di.Mode())
	}
	if got, ok := owner(di); !ok {
		return refuse("cannot read the owner of directory %s", dir)
	} else if got != uid {
		return refuse("directory %s is owned by uid %d, not %d", dir, got, uid)
	}
	if perm := di.Mode().Perm(); perm&0o022 != 0 {
		return refuse("directory %s is group- or world-writable (mode %04o)", dir, perm)
	}
	return nil
}

// checkPeer is the post-connect half of the socket check, and the one that
// does not depend on the path: it asks the kernel for the uid of the process
// listening on the connected socket (LOCAL_PEERCRED on macOS, SO_PEERCRED on
// Linux) and refuses unless it is this process's uid. A socket swapped in by
// another user, by any route, is served by that user's process and fails
// here, before the client writes a byte. A peer whose uid cannot be read is
// refused too, including on any OS without an implementation. A server run
// by this same uid is trusted; that is the same boundary as the user's own
// files.
func (c *Client) checkPeer(conn net.Conn) error {
	lookup := c.peer
	if lookup == nil {
		lookup = peerUID
	}
	uid := uint32(os.Getuid())
	got, err := lookup(conn)
	if err != nil {
		return &UnsafeSocketError{Path: c.socketPath, Reason: fmt.Sprintf("cannot read the server's uid: %v", err)}
	}
	if got != uid {
		return &UnsafeSocketError{Path: c.socketPath, Reason: fmt.Sprintf("server is running as uid %d, not %d", got, uid)}
	}
	return nil
}

// controlUnix runs fn on the file descriptor of a Unix socket connection.
func controlUnix(conn net.Conn, fn func(fd int) error) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a Unix socket connection: %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var fnErr error
	if err := raw.Control(func(fd uintptr) { fnErr = fn(int(fd)) }); err != nil {
		return err
	}
	return fnErr
}

// statOwner reads the owning uid from a Unix stat result.
func statOwner(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

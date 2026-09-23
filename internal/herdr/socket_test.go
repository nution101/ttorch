package herdr

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownedBy returns an owner lookup that reports uid for the file named name
// and the real owner for everything else.
func ownedBy(name string, uid uint32) func(fs.FileInfo) (uint32, bool) {
	return func(fi fs.FileInfo) (uint32, bool) {
		if fi.Name() == name {
			return uid, true
		}
		return statOwner(fi)
	}
}

// wantRefused fails unless err is an *UnsafeSocketError whose reason
// contains every one of reasons, and the server never saw a request.
func wantRefused(t *testing.T, s *fakeServer, err error, reasons ...string) {
	t.Helper()
	var unsafe *UnsafeSocketError
	if !errors.As(err, &unsafe) || !errors.Is(err, ErrUnsafeSocket) {
		t.Fatalf("err = %v, want *UnsafeSocketError", err)
	}
	if unsafe.Path != s.path {
		t.Errorf("error names %q, want %q", unsafe.Path, s.path)
	}
	for _, r := range reasons {
		if !strings.Contains(unsafe.Reason, r) {
			t.Errorf("reason = %q, want it to mention %q", unsafe.Reason, r)
		}
	}
	if n := len(s.received()); n != 0 {
		t.Fatalf("server received %d requests; an unsafe socket must not be written to", n)
	}
}

func pingServer(s *fakeServer) {
	s.handle("ping", reply(map[string]any{"type": "pong", "version": "x", "protocol": 1}))
}

func TestDial_OwnedSocketInPrivateDirConnects(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	if err := os.Chmod(filepath.Dir(s.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client().Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// A directory other users can read but not write is still safe: they cannot
// replace the socket in it.
func TestDial_ReadableButNotWritableDirConnects(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	if err := os.Chmod(filepath.Dir(s.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client().Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestDial_RefusesSocketOwnedByAnotherUser(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	c := s.client()
	c.owner = ownedBy(filepath.Base(s.path), uint32(os.Getuid())+1)
	_, err := c.Ping(context.Background())
	wantRefused(t, s, err, "socket is owned by uid")
}

func TestDial_RefusesDirectoryOwnedByAnotherUser(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	c := s.client()
	c.owner = ownedBy(filepath.Base(filepath.Dir(s.path)), uint32(os.Getuid())+1)
	_, err := c.Ping(context.Background())
	wantRefused(t, s, err, "directory", "is owned by uid")
}

// An owner that cannot be read fails closed, for the socket and for its
// directory independently.
func TestDial_RefusesOwnerThatCannotBeRead(t *testing.T) {
	for _, target := range []string{"socket", "directory"} {
		t.Run(target, func(t *testing.T) {
			s := newFakeServer(t)
			pingServer(s)
			name := filepath.Base(s.path)
			if target == "directory" {
				name = filepath.Base(filepath.Dir(s.path))
			}
			c := s.client()
			c.owner = func(fi fs.FileInfo) (uint32, bool) {
				if fi.Name() == name {
					return 0, false
				}
				return statOwner(fi)
			}
			_, err := c.Ping(context.Background())
			wantRefused(t, s, err, "cannot read", target)
		})
	}
}

func TestDial_RefusesWritableDirectory(t *testing.T) {
	for _, mode := range []fs.FileMode{0o777, 0o770, 0o702, 0o777 | fs.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			s := newFakeServer(t)
			pingServer(s)
			dir := filepath.Dir(s.path)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			_, err := s.client().Ping(context.Background())
			wantRefused(t, s, err, "writable")
		})
	}
}

// A symlink at the socket path could point anywhere; only a real socket is
// trusted.
func TestDial_RefusesSymlinkToSocket(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	link := filepath.Join(filepath.Dir(s.path), "link.sock")
	if err := os.Symlink(s.path, link); err != nil {
		t.Fatal(err)
	}
	_, err := New(link).Ping(context.Background())
	var unsafe *UnsafeSocketError
	if !errors.As(err, &unsafe) || !strings.Contains(unsafe.Reason, "not a socket") {
		t.Fatalf("err = %v, want refusal: not a socket", err)
	}
	if n := len(s.received()); n != 0 {
		t.Fatalf("server received %d requests through the symlink", n)
	}
}

func TestDial_RefusesRegularFile(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "herdr.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(path).Ping(context.Background())
	var unsafe *UnsafeSocketError
	if !errors.As(err, &unsafe) || !strings.Contains(unsafe.Reason, "not a socket") {
		t.Fatalf("err = %v, want refusal: not a socket", err)
	}
}

// The checks run on every path, including an explicit HERDR_SOCKET_PATH.
func TestDial_ChecksEnvOverridePath(t *testing.T) {
	s := newFakeServer(t)
	pingServer(s)
	dir := filepath.Dir(s.path)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	t.Setenv("HERDR_SOCKET_PATH", s.path)
	path, err := DefaultSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(path).Ping(context.Background())
	wantRefused(t, s, err, "writable")
}

// The subscription dials the same way and is refused the same way.
func TestSubscribe_RefusesUnsafeSocket(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", streamHandler(true))
	c := s.client()
	c.owner = ownedBy(filepath.Base(s.path), uint32(os.Getuid())+1)
	_, err := c.Subscribe(context.Background(), AgentStatusChanges("w1:p1", ""))
	wantRefused(t, s, err, "socket is owned by uid")
}

// A missing socket is still ErrNoSocket, not an ownership refusal.
func TestDial_MissingSocketInSafeDirIsErrNoSocket(t *testing.T) {
	_, err := New(filepath.Join(shortSocketDir(t), "absent.sock")).Ping(context.Background())
	if !errors.Is(err, ErrNoSocket) || errors.Is(err, ErrUnsafeSocket) {
		t.Fatalf("err = %v, want ErrNoSocket only", err)
	}
}

func TestSocketPath_NoConfigDirIsTypedError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "")
	if p, err := DefaultSocketPath(); !errors.Is(err, ErrNoConfigDir) {
		t.Fatalf("DefaultSocketPath = %q, %v; want ErrNoConfigDir", p, err)
	}
	t.Setenv("HERDR_SESSION", "work")
	if p, err := DefaultSocketPath(); !errors.Is(err, ErrNoConfigDir) {
		t.Fatalf("DefaultSocketPath with a session = %q, %v; want ErrNoConfigDir", p, err)
	}
	// An explicit socket path needs no config directory.
	t.Setenv("HERDR_SOCKET_PATH", "/run/h.sock")
	if p, err := DefaultSocketPath(); err != nil || p != "/run/h.sock" {
		t.Fatalf("DefaultSocketPath = %q, %v; want /run/h.sock", p, err)
	}
}

func TestSocketPathResolution(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "")

	check := func(label, want string) {
		t.Helper()
		got, err := DefaultSocketPath()
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", label, got, err, want)
		}
	}
	check("default", "/home/u/.config/herdr/herdr.sock")
	t.Setenv("HERDR_SESSION", "work")
	check("session", "/home/u/.config/herdr/sessions/work/herdr.sock")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	check("xdg session", "/xdg/herdr/sessions/work/herdr.sock")
	t.Setenv("HERDR_SOCKET_PATH", "/run/h.sock")
	check("override", "/run/h.sock")
}

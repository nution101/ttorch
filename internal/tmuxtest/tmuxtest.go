// Package tmuxtest runs a test binary against a tmux server of its own. It is imported only
// from tests.
//
// tmux finds its server through $TMUX and then $TMUX_TMPDIR, so a package that spawns windows
// would otherwise open them on whatever server the caller is using. Isolate points the process
// at a private TMUX_TMPDIR, which the code under test inherits. Everything a test runs itself,
// and above all the kill-server at the end, goes through the pinned socket with -S instead:
// when TMUX_TMPDIR names a directory that no longer exists, tmux skips it and falls back to
// /tmp, which is the caller's default server, so a bare `tmux kill-server` issued after the
// directory is gone (or by a process that outlives it) kills the caller's server and every
// window on it. For the same reason the directory stays until the test binary has exited:
// Close kills the server and leaves it, and the reaper removes it once the process is gone.
//
// Every package whose tests can reach tmux, directly or through a package that runs it, runs
// its tests through Run or Isolate in its TestMain. TestEveryTmuxReachingPackageIsIsolated
// holds that.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// Server is a test binary's private tmux server: the TMUX_TMPDIR the process runs under and
// the socket tmux puts the server on inside it.
type Server struct {
	Dir    string
	Socket string
}

// Isolate makes a private TMUX_TMPDIR, points this process at it and clears $TMUX, so the
// code under test talks to a server of the binary's own. The directory name is short because
// a unix socket path is limited to about 100 bytes.
func Isolate() (*Server, error) {
	dir, err := os.MkdirTemp("", "ttx-")
	if err != nil {
		return nil, err
	}
	s := newServer(dir)
	os.Unsetenv("TMUX")
	os.Setenv("TMUX_TMPDIR", s.Dir)
	return s, nil
}

// newServer names the socket tmux uses under dir: <dir>/tmux-<uid>/default, with dir resolved
// the way tmux resolves it.
func newServer(dir string) *Server {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return &Server{
		Dir:    dir,
		Socket: filepath.Join(dir, "tmux-"+strconv.Itoa(os.Getuid()), "default"),
	}
}

// Command is tmux with args, against this server's socket and no other.
func (s *Server) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-S", s.Socket}, args...)...)
}

// Close kills the private server. It leaves the directory: while this process lives, a tmux
// that runs after Close (a test's leftover goroutine, a child it started) must still find
// TMUX_TMPDIR, because with the directory gone tmux falls back to the default socket. The
// reaper removes the directory once the process has exited.
func (s *Server) Close() {
	if _, err := exec.LookPath("tmux"); err == nil {
		_ = s.Command("kill-server").Run()
	}
}

// Run is a TestMain body for a package whose tests can reach tmux: it isolates the process
// on a private server, starts the reaper, runs the tests, kills the server, and returns the
// exit code for os.Exit.
func Run(m *testing.M) int {
	s, err := Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tmuxtest: cannot make a private tmux server: %v\n", err)
		return 1
	}
	s.StartReaper()
	code := m.Run()
	s.Close()
	return code
}

// reaperScript waits for pid $1 to exit, then kills the server on socket $3 only if that
// socket exists, and removes $2. tmux leaves its socket file behind when the server exits, so
// after a normal exit, with the server already killed by Close, this kill-server finds no
// server and does nothing. It can only ever reach the private socket.
const reaperScript = `while kill -0 "$1" 2>/dev/null; do sleep 1; done
if [ -S "$3" ]; then tmux -S "$3" kill-server 2>/dev/null; fi
rm -rf "$2"`

// reaper is the reaper process for a test binary with the given pid.
func (s *Server) reaper(pid int) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", reaperScript, "tmux-reaper", strconv.Itoa(pid), s.Dir, s.Socket)
}

// StartReaper starts a process that outlives this one to kill the private server once this
// test binary has exited, however it exits. Close runs after m.Run, but a test that panics or
// hits -timeout ends the process from another goroutine, where neither that code nor a defer
// in TestMain runs, and the server would be left running with the windows of the test that
// died. It is also what removes the directory, so it starts whether or not tmux is installed.
// The reaper polls for this pid once a second, so it costs nothing while the tests run; it
// has no stdout or stderr, so go test does not wait on it, and its own process group, so a
// signal to this one's does not reach it. Best-effort: if it cannot start, Close still kills
// the server on a normal exit, and the directory is left in the temp dir.
func (s *Server) StartReaper() {
	reaper := s.reaper(os.Getpid())
	reaper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := reaper.Start(); err == nil {
		_ = reaper.Process.Release()
	}
}

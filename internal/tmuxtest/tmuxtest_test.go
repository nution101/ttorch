package tmuxtest

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeTmux puts a tmux first on PATH that records each invocation's argv, one call per line
// with the arguments tab-separated, and returns the log path and the PATH it set. No test in
// this file runs the real tmux: every kill path is exercised against this stand-in, so a
// regression shows up as a recorded argv instead of a killed server.
func fakeTmux(t *testing.T) (log, path string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\n(IFS=\"$(printf '\\t')\"; printf '%s\\n' \"$*\") >> \"" + log + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path = bin + string(os.PathListSeparator) + "/usr/bin:/bin"
	t.Setenv("PATH", path)
	return log, path
}

// calls reads the fake's log; a log it never wrote means tmux was never run.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	f, err := os.Open(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, strings.Split(sc.Text(), "\t"))
	}
	return out
}

// testServer is a Server on a directory of the test's own, with a listening unix socket where
// tmux would put the server, so the reaper's [ -S ] check sees a live socket. The directory
// comes from os.MkdirTemp rather than t.TempDir because t.TempDir's longer path can push the
// socket past the unix path limit.
func testServer(t *testing.T) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := newServer(dir)
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return s
}

// requirePinned fails unless every recorded call starts with -S <socket> and names no other
// socket or socket name, and at least one of them is a kill-server.
func requirePinned(t *testing.T, got [][]string, socket string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatal("tmux was never run; want a kill-server on the private socket")
	}
	killed := false
	for _, argv := range got {
		if len(argv) < 2 || argv[0] != "-S" || argv[1] != socket {
			t.Errorf("tmux %q does not start with -S %s: it reaches whatever server $TMUX or $TMUX_TMPDIR names, or the default socket", argv, socket)
			continue
		}
		for i, a := range argv[2:] {
			if a == "-S" || a == "-L" {
				t.Errorf("tmux %q names a second socket at argument %d", argv, i+2)
			}
		}
		if argv[len(argv)-1] == "kill-server" {
			killed = true
		}
	}
	if !killed {
		t.Errorf("no kill-server among %q", got)
	}
}

func TestClose_KillsOnlyThePrivateSocket(t *testing.T) {
	log, _ := fakeTmux(t)
	s := testServer(t)
	s.Close()
	requirePinned(t, calls(t, log), s.Socket)
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("Close left %s behind (stat err %v)", s.Dir, err)
	}
}

func TestReaper_KillsOnlyThePrivateSocket(t *testing.T) {
	log, path := fakeTmux(t)
	s := testServer(t)
	cmd := s.reaper(exitedPID(t))
	cmd.Env = append(os.Environ(), "PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper: %v: %s", err, out)
	}
	requirePinned(t, calls(t, log), s.Socket)
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("reaper left %s behind (stat err %v)", s.Dir, err)
	}
}

// After a normal exit Close has removed the directory, and with it the socket. That is the
// state in which a bare kill-server falls back to the default socket, so the reaper must not
// run tmux at all.
func TestReaper_RunsNoTmuxOnceTheSocketIsGone(t *testing.T) {
	log, path := fakeTmux(t)
	s := testServer(t)
	if err := os.RemoveAll(s.Dir); err != nil {
		t.Fatal(err)
	}
	cmd := s.reaper(exitedPID(t))
	cmd.Env = append(os.Environ(), "PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper: %v: %s", err, out)
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("reaper ran tmux with no private socket left: %q", got)
	}
}

func TestCommand_PinsTheSocket(t *testing.T) {
	log, _ := fakeTmux(t)
	s := testServer(t)
	if err := s.Command("kill-session", "-t", "x").Run(); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	want := []string{"-S", s.Socket, "kill-session", "-t", "x"}
	if len(got) != 1 || strings.Join(got[0], "\t") != strings.Join(want, "\t") {
		t.Errorf("Command ran tmux %q, want %q", got, want)
	}
}

// bareTmux matches a test that runs tmux itself instead of through Server.Command, which is
// the only way test code can reach a server without -S.
var bareTmux = regexp.MustCompile(`exec\.Command(Context)?\([^)]*"tmux"`)

// TestNoBareTmuxInTests keeps every tmux a test runs on a pinned socket: a test that builds its
// own exec.Command("tmux", ...) reaches whatever server the environment names when it runs.
func TestNoBareTmuxInTests(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if bareTmux.MatchString(line) {
				t.Errorf("%s:%d runs tmux without the private socket; use tmuxtest.Server.Command: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// exitedPID is the pid of a process that has exited and been waited for, so the reaper's
// kill -0 loop ends at once.
func exitedPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

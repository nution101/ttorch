package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// startSleeper starts a real child process of the test binary and returns it unreaped, so
// its pid cannot be reused until the test waits on it. Killing a child we have not waited
// for signals exactly that process.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	return c
}

// fakeProcesses swaps readProcess for a table, restoring it after the test.
func fakeProcesses(t *testing.T, procs map[int]Process, err error) {
	t.Helper()
	prev := readProcess
	readProcess = func(pid int) (Process, bool, error) {
		if err != nil {
			return Process{}, false, err
		}
		p, ok := procs[pid]
		return p, ok, nil
	}
	t.Cleanup(func() { readProcess = prev })
}

// A live child of this test process verifies as alive against its own fingerprint, and as
// exited once it has been killed and reaped. This exercises the host's real reader: ps on
// macOS, /proc on Linux.
func TestVerify_RealProcessAliveThenExited(t *testing.T) {
	c := startSleeper(t)
	p, ok, err := ReadProcess(c.Process.Pid)
	if err != nil || !ok {
		t.Fatalf("ReadProcess(%d) = ok %v, err %v; want the running sleep", c.Process.Pid, ok, err)
	}
	if p.PPID != os.Getpid() {
		t.Fatalf("sleep's parent = %d, want this test process %d", p.PPID, os.Getpid())
	}
	if !strings.Contains(p.Cmdline, "sleep") {
		t.Fatalf("command line %q does not name sleep", p.Cmdline)
	}
	fp := p.Fingerprint()
	if st, why := Verify(fp, os.Getpid()); st != AgentAlive {
		t.Fatalf("Verify(live sleep) = %v (%s), want alive", st, why)
	}
	// The same process read again gives the same fingerprint.
	again, _, err := ReadProcess(c.Process.Pid)
	if err != nil || again.Fingerprint() != fp {
		t.Fatalf("second read = %+v (err %v), want %+v", again.Fingerprint(), err, fp)
	}

	if err := c.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	if st, why := Verify(fp, os.Getpid()); st != AgentExited {
		t.Fatalf("Verify(killed sleep) = %v (%s), want exited", st, why)
	}
}

// PID reuse: the recorded pid is running, with the same command, under the same parent, but
// it started at a different time. That is a different process and is not alive.
func TestVerify_SamePIDDifferentStartIsNotAlive(t *testing.T) {
	c := startSleeper(t)
	p, ok, err := ReadProcess(c.Process.Pid)
	if err != nil || !ok {
		t.Fatalf("ReadProcess: ok %v err %v", ok, err)
	}
	fp := p.Fingerprint()
	fp.Start = "Thu-Jan-1-00:00:00-1970"
	if st, why := Verify(fp, os.Getpid()); st != AgentExited {
		t.Fatalf("Verify(same pid, other start) = %v (%s), want exited", st, why)
	}
}

func TestVerify_Table(t *testing.T) {
	const pane = 100
	agent := Process{PID: 200, PPID: pane, PGID: 200, Start: "t0", Cmdline: "claude --session-id x"}
	fp := agent.Fingerprint()
	tests := []struct {
		name  string
		now   map[int]Process
		want  AgentState
		match string
	}{
		{"unchanged", map[int]Process{200: agent}, AgentAlive, ""},
		{"gone", map[int]Process{}, AgentExited, "has exited"},
		{"pid reused", map[int]Process{200: {PID: 200, PPID: pane, Start: "t1", Cmdline: agent.Cmdline}}, AgentExited, "started later"},
		{"exec'd into another command", map[int]Process{200: {PID: 200, PPID: pane, Start: "t0", Cmdline: "-zsh"}}, AgentExited, "different command"},
		{"no longer under the pane", map[int]Process{200: {PID: 200, PPID: 1, Start: "t0", Cmdline: agent.Cmdline}}, AgentExited, "no longer in the window's pane"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeProcesses(t, tc.now, nil)
			st, why := Verify(fp, pane)
			if st != tc.want || !strings.Contains(why, tc.match) {
				t.Fatalf("Verify = %v (%q), want %v containing %q", st, why, tc.want, tc.match)
			}
		})
	}
}

// A failed read (ps erroring, /proc unreadable) is unknown: not exited, and not alive.
func TestVerify_ReadFailureIsUnknown(t *testing.T) {
	fakeProcesses(t, nil, errors.New("ps: exit status 2"))
	fp := Fingerprint{PID: 200, Start: "t0", CmdHash: hashCmdline("claude")}
	st, why := Verify(fp, 100)
	if st != AgentUnknown {
		t.Fatalf("Verify with a failing read = %v (%s), want unknown", st, why)
	}
	if !strings.Contains(why, "ps: exit status 2") {
		t.Fatalf("reason %q does not carry the read error", why)
	}
	if st, _ := Verify(fp, 0); st != AgentUnknown {
		t.Fatalf("Verify with no pane pid = %v, want unknown", st)
	}
}

func TestAgentOf(t *testing.T) {
	shell := Process{PID: 100, PPID: 1, PGID: 100, TPGID: 200, Start: "s", Cmdline: "-zsh"}
	agent := Process{PID: 200, PPID: 100, PGID: 200, TPGID: 200, Start: "a", Cmdline: "claude --session-id x"}

	t.Run("agent is the shell's foreground child", func(t *testing.T) {
		fakeProcesses(t, map[int]Process{100: shell, 200: agent}, nil)
		fp, err := AgentOf(100)
		if err != nil || fp != agent.Fingerprint() {
			t.Fatalf("AgentOf = %+v, %v; want %+v", fp, err, agent.Fingerprint())
		}
	})
	t.Run("shell in the foreground", func(t *testing.T) {
		bare := shell
		bare.TPGID = 100
		fakeProcesses(t, map[int]Process{100: bare}, nil)
		if _, err := AgentOf(100); !errors.Is(err, ErrNoAgent) {
			t.Fatalf("AgentOf(bare shell) err = %v, want ErrNoAgent", err)
		}
	})
	t.Run("foreground leader is not the shell's child", func(t *testing.T) {
		stray := agent
		stray.PPID = 1
		fakeProcesses(t, map[int]Process{100: shell, 200: stray}, nil)
		if _, err := AgentOf(100); err == nil {
			t.Fatal("AgentOf accepted a foreground process that is not a child of the pane")
		}
	})
	t.Run("read failure", func(t *testing.T) {
		fakeProcesses(t, nil, errors.New("ps failed"))
		if _, err := AgentOf(100); err == nil || errors.Is(err, ErrNoAgent) {
			t.Fatalf("AgentOf with a failing read err = %v, want the read error", err)
		}
	})
}

func TestStoredAgentState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task", "agent.fingerprint")
	pane := func() (int, error) { return 100, nil }
	agent := Process{PID: 200, PPID: 100, Start: "a", Cmdline: "claude"}

	// No fingerprint stored: the caller keeps window-presence behaviour, and the pane is
	// not even read.
	st, _ := StoredAgentState(path, func() (int, error) {
		t.Fatal("pane pid read for an untracked task")
		return 0, nil
	})
	if st != AgentUntracked {
		t.Fatalf("no fingerprint: %v, want untracked", st)
	}

	if err := SaveFingerprint(path, agent.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadFingerprint(path)
	if err != nil || !ok || got != agent.Fingerprint() {
		t.Fatalf("LoadFingerprint = %+v %v %v, want %+v", got, ok, err, agent.Fingerprint())
	}

	fakeProcesses(t, map[int]Process{200: agent}, nil)
	if st, why := StoredAgentState(path, pane); st != AgentAlive {
		t.Fatalf("stored, unchanged: %v (%s), want alive", st, why)
	}
	if st, _ := StoredAgentState(path, func() (int, error) { return 0, errors.New("tmux hiccup") }); st != AgentUnknown {
		t.Fatalf("pane unreadable: %v, want unknown", st)
	}

	if err := os.WriteFile(path, []byte("pid=200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := StoredAgentState(path, pane); st != AgentUnknown {
		t.Fatalf("incomplete fingerprint: %v, want unknown", st)
	}

	if err := RemoveFingerprint(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFingerprint(path); err != nil {
		t.Fatalf("removing a missing fingerprint: %v", err)
	}
	if st, _ := StoredAgentState(path, pane); st != AgentUntracked {
		t.Fatalf("after remove: %v, want untracked", st)
	}
}

func TestParsePSLine(t *testing.T) {
	line := "16921 16628 16921 16921 Wed Sep  3 17:38:54 2026     claude --model opus  \"two  spaces\""
	p, err := parsePSLine(line)
	if err != nil {
		t.Fatal(err)
	}
	want := Process{PID: 16921, PPID: 16628, PGID: 16921, TPGID: 16921,
		Start: "Wed-Sep-3-17:38:54-2026", Cmdline: "claude --model opus  \"two  spaces\""}
	if p != want {
		t.Fatalf("parsePSLine = %+v, want %+v", p, want)
	}
	for _, bad := range []string{"", "12 34", "x 1 1 1 Wed Sep 3 17:38:54 2026 cmd"} {
		if _, err := parsePSLine(bad); err == nil {
			t.Errorf("parsePSLine(%q) accepted a malformed line", bad)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	// The comm field holds spaces and a ')' of its own; fields are read after the last one.
	stat := "4242 (my (odd) cmd) S 4200 4242 4200 34816 4242 4194560 1 0 0 0 0 0 0 0 20 0 1 0 987654 1000 100 18446744073709551615"
	p, err := parseProcStat(stat)
	if err != nil {
		t.Fatal(err)
	}
	want := Process{PID: 4242, PPID: 4200, PGID: 4242, TPGID: 4242, Start: "987654"}
	if p != want {
		t.Fatalf("parseProcStat = %+v, want %+v", p, want)
	}
	for _, bad := range []string{"", "4242 (x) S 1 2", "nope (x) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20"} {
		if _, err := parseProcStat(bad); err == nil {
			t.Errorf("parseProcStat(%q) accepted a malformed stat", bad)
		}
	}
}

// The /proc reader, against a fake procfs: a present pid is read with its NUL-separated
// command line, and a missing one is "no such process", not an error.
func TestReadProcFS(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "4242")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "4242 (claude) S 4200 4242 4200 34816 4242 4194560 1 0 0 0 0 0 0 0 20 0 1 0 987654 1000 100 18446744073709551615"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("claude\x00--model\x00opus\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, ok, err := readProcFS(root, 4242)
	if err != nil || !ok {
		t.Fatalf("readProcFS = ok %v err %v", ok, err)
	}
	if p.PPID != 4200 || p.Start != "987654" || p.Cmdline != "claude\x00--model\x00opus\x00" {
		t.Fatalf("readProcFS = %+v", p)
	}
	if _, ok, err := readProcFS(root, 4243); ok || err != nil {
		t.Fatalf("readProcFS(missing) = ok %v err %v, want not found", ok, err)
	}
}

// The live reader reports a pid that no process holds as not found, with no error.
func TestReadProcess_MissingPID(t *testing.T) {
	c := startSleeper(t)
	pid := c.Process.Pid
	_ = c.Process.Kill()
	_ = c.Wait()
	// The pid is free now; the OS could hand it out again, but not within this read on any
	// host this runs on, and a reuse would read as found, failing loudly rather than falsely.
	if _, ok, err := ReadProcess(pid); ok || err != nil {
		t.Fatalf("ReadProcess(reaped pid %d) = ok %v err %v, want not found (%s)", pid, ok, err, runtime.GOOS)
	}
}

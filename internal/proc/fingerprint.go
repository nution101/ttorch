package proc

// Process fingerprints: telling whether the process in a worker's window is still the
// agent ttorch launched there.
//
// A tmux window outlives its agent. When the agent exits, the pane's shell prints a prompt
// and the window stays; when a window is replaced by something else under the same name, it
// is still "present". A fingerprint pins the identity of the agent process at spawn so a
// later read can tell those cases from a live agent.
//
// Identity is the pid and the process start time read together. The pid alone is not an
// identity, because the OS reuses pids; a reused pid belongs to a process that started
// later, so the start time differs. A hash of the command line is recorded too, but it only
// decides when a start time is unavailable: a process that exec's in place keeps its pid and
// start time under a new command line and is still the same agent. A read also checks that
// the process is still a child of the window's pane process, which is how panes are started:
// tmux runs a shell in the pane and the launch command is typed into it, so the agent is the
// shell's child and leads the terminal's foreground process group.
//
// Reading is done with ps on macOS and from /proc on Linux, with no cgo. A read that fails
// (ps missing, erroring, or printing something unparseable) is reported as AgentUnknown,
// never as exited and never as alive: a transient read failure must not wake the manager
// with a false death, and it must not vouch for a process it did not see.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Fingerprint identifies one process instance. Start is the platform's encoding of the
// process start time (ps lstart on macOS, clock ticks since boot on Linux); it is only ever
// compared with a value read the same way on the same host.
type Fingerprint struct {
	PID     int
	Start   string
	CmdHash string
}

// Process is one read of a process's identity and its place in the terminal's job control.
type Process struct {
	PID     int
	PPID    int
	PGID    int
	TPGID   int // foreground process group of the process's controlling terminal
	Start   string
	Cmdline string
}

// Fingerprint returns the identity fields of p.
func (p Process) Fingerprint() Fingerprint {
	return Fingerprint{PID: p.PID, Start: p.Start, CmdHash: hashCmdline(p.Cmdline)}
}

func hashCmdline(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// AgentState is the answer to "is the recorded agent still the process in its window".
type AgentState int

const (
	// AgentUnknown means the check could not be completed: the process or pane could not be
	// read, or the stored fingerprint could not be parsed. It is neither alive nor exited.
	AgentUnknown AgentState = iota
	// AgentAlive means the recorded process is running, unchanged, under the window's pane.
	AgentAlive
	// AgentExited means the recorded process is gone, or its pid now belongs to a different
	// process, or it is no longer in the window's pane.
	AgentExited
	// AgentUntracked means no fingerprint is stored (a task spawned before fingerprints, or
	// one resumed without waiting for its agent). The caller keeps window-presence behaviour.
	AgentUntracked
)

func (s AgentState) String() string {
	switch s {
	case AgentAlive:
		return "alive"
	case AgentExited:
		return "exited"
	case AgentUntracked:
		return "untracked"
	default:
		return "unknown"
	}
}

// ErrNoAgent reports a pane whose foreground is not a child process of the pane: the shell
// itself is in the foreground (nothing launched, or the launched command has exited).
var ErrNoAgent = errors.New("proc: no agent process in the pane's foreground")

// readProcess reads one process: (p, true, nil) when it exists, (_, false, nil) when there
// is no such process, and an error when the read itself failed. A var so tests can make the
// read fail without breaking ps on the host.
var readProcess = platformReadProcess

// ReadProcess reads pid's identity and job-control fields. ok is false, with a nil error,
// only when the process verifiably does not exist.
func ReadProcess(pid int) (Process, bool, error) {
	if pid <= 0 {
		return Process{}, false, fmt.Errorf("proc: invalid pid %d", pid)
	}
	return readProcess(pid)
}

// AgentOf returns the fingerprint of the agent running in a pane whose process is panePID.
// The agent is the leader of the terminal's foreground process group, and it must be a
// child of the pane process. When the pane process itself holds the foreground, there is
// no agent and ErrNoAgent is returned.
func AgentOf(panePID int) (Fingerprint, error) {
	pane, ok, err := ReadProcess(panePID)
	if err != nil {
		return Fingerprint{}, err
	}
	if !ok {
		return Fingerprint{}, fmt.Errorf("proc: pane process %d is gone", panePID)
	}
	fg := pane.TPGID
	if fg <= 0 || fg == pane.PID || fg == pane.PGID {
		return Fingerprint{}, ErrNoAgent
	}
	agent, ok, err := ReadProcess(fg)
	if err != nil {
		return Fingerprint{}, err
	}
	if !ok {
		return Fingerprint{}, ErrNoAgent
	}
	if agent.PPID != panePID {
		return Fingerprint{}, fmt.Errorf("proc: foreground process %d is not a child of pane process %d (parent %d)", agent.PID, panePID, agent.PPID)
	}
	return agent.Fingerprint(), nil
}

// Verify compares a recorded agent fingerprint with what is running now under the pane
// process panePID. The reason says why the answer is not AgentAlive.
func Verify(want Fingerprint, panePID int) (AgentState, string) {
	if panePID <= 0 {
		return AgentUnknown, "the window's pane process could not be read"
	}
	got, ok, err := ReadProcess(want.PID)
	if err != nil {
		return AgentUnknown, err.Error()
	}
	if !ok {
		return AgentExited, fmt.Sprintf("agent process %d has exited", want.PID)
	}
	// The start time decides identity. A process that exec's in place keeps its pid and start
	// time and changes only its command line, and it is still the agent ttorch launched. The
	// command hash only decides when a start time is missing from either side.
	switch {
	case want.Start != "" && got.Start != "":
		if got.Start != want.Start {
			return AgentExited, fmt.Sprintf("pid %d now belongs to a process started later", want.PID)
		}
	case want.CmdHash == "":
		return AgentUnknown, fmt.Sprintf("no start time or command recorded to compare for pid %d", want.PID)
	case got.Fingerprint().CmdHash != want.CmdHash:
		return AgentExited, fmt.Sprintf("pid %d now runs a different command", want.PID)
	}
	if got.PPID != panePID {
		return AgentExited, fmt.Sprintf("agent process %d is no longer in the window's pane", want.PID)
	}
	return AgentAlive, ""
}

// StoredAgentState loads the fingerprint recorded at path and verifies it against the pane
// process panePID reports. A missing file is AgentUntracked; an unreadable file, or a pane
// that cannot be read, is AgentUnknown.
func StoredAgentState(path string, panePID func() (int, error)) (AgentState, string) {
	fp, ok, err := LoadFingerprint(path)
	if err != nil {
		return AgentUnknown, err.Error()
	}
	if !ok {
		return AgentUntracked, ""
	}
	pid, err := panePID()
	if err != nil {
		return AgentUnknown, "the window's pane process could not be read: " + err.Error()
	}
	return Verify(fp, pid)
}

// SaveFingerprint writes fp to path as key=value lines, creating the parent directory. The
// write goes through a temp file and a rename so a concurrent reader sees the old record or
// the new one, never half of one.
func SaveFingerprint(path string, fp Fingerprint) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("pid=%d\nstart=%s\ncmd_sha256=%s\n", fp.PID, fp.Start, fp.CmdHash)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fingerprint-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// LoadFingerprint reads a fingerprint written by SaveFingerprint. ok is false, with a nil
// error, when no file exists at path.
func LoadFingerprint(path string) (Fingerprint, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Fingerprint{}, false, nil
	}
	if err != nil {
		return Fingerprint{}, false, err
	}
	var fp Fingerprint
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			fp.PID, _ = strconv.Atoi(v)
		case "start":
			fp.Start = v
		case "cmd_sha256":
			fp.CmdHash = v
		}
	}
	if fp.PID <= 0 || fp.Start == "" || fp.CmdHash == "" {
		return Fingerprint{}, false, fmt.Errorf("proc: fingerprint %s is incomplete", path)
	}
	return fp, true, nil
}

// RemoveFingerprint deletes the fingerprint at path. A missing file is not an error.
func RemoveFingerprint(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// psPath is where ps lives on macOS and the BSDs. It is run by absolute path rather than
// looked up on PATH, so a ps placed earlier on PATH cannot answer for the agent.
const psPath = "/bin/ps"

// psTimeout bounds one ps call. ps answers in milliseconds; the bound only matters for a
// wedged one.
const psTimeout = 5 * time.Second

func platformReadProcess(pid int) (Process, bool, error) {
	if runtime.GOOS == "linux" {
		return readProcFS("/proc", pid)
	}
	return readPS(pid)
}

// readPS reads a process with ps. Existence is decided by kill(pid, 0) rather than by ps's
// exit status, because ps exits non-zero both for "no such process" and for its own
// failures: ESRCH is the kernel saying the pid is unused. The locale and zone are pinned so
// lstart renders the same way on every read.
func readPS(pid int) (Process, bool, error) {
	if !pidExists(pid) {
		return Process{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	c := Command(ctx, psPath, "-ww", "-o", "pid=,ppid=,pgid=,tpgid=,lstart=,command=", "-p", strconv.Itoa(pid))
	c.Env = append(environWithout("TZ", "LC_ALL", "LANG"), "TZ=UTC", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	runErr := Run(c)
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		// The process can exit between the kill probe and ps; ask the kernel again before
		// calling it a failure.
		if !pidExists(pid) {
			return Process{}, false, nil
		}
		if runErr == nil {
			runErr = errors.New("no output")
		}
		return Process{}, false, fmt.Errorf("proc: ps -p %d: %v: %s", pid, runErr, strings.TrimSpace(stderr.String()))
	}
	p, err := parsePSLine(out)
	if err != nil {
		return Process{}, false, err
	}
	if p.PID != pid {
		return Process{}, false, fmt.Errorf("proc: ps -p %d answered for pid %d", pid, p.PID)
	}
	return p, true, nil
}

// pidExists reports whether pid names a process. EPERM means it exists under another user.
func pidExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// parsePSLine parses one `ps -o pid=,ppid=,pgid=,tpgid=,lstart=,command=` line. lstart is
// five fields under LC_ALL=C ("Wed Sep 23 17:38:54 2026"); the command is the rest of the
// line with its spacing kept.
func parsePSLine(line string) (Process, error) {
	fields, rest := cutFields(line, 9)
	if len(fields) < 9 {
		return Process{}, fmt.Errorf("proc: unparseable ps line %q", line)
	}
	var ints [4]int
	for i := range ints {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			return Process{}, fmt.Errorf("proc: unparseable ps line %q: %v", line, err)
		}
		ints[i] = n
	}
	return Process{
		PID: ints[0], PPID: ints[1], PGID: ints[2], TPGID: ints[3],
		Start:   strings.Join(fields[4:9], "-"),
		Cmdline: rest,
	}, nil
}

// cutFields splits the first n whitespace-separated fields off s and returns them with the
// remainder of s, leading whitespace trimmed and inner spacing intact.
func cutFields(s string, n int) ([]string, string) {
	var fields []string
	for len(fields) < n {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			fields = append(fields, s)
			s = ""
			break
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
	return fields, strings.TrimLeft(s, " \t")
}

// readProcFS reads a process from a procfs mounted at root. A missing /proc/<pid> is the
// kernel saying the pid is unused.
func readProcFS(root string, pid int) (Process, bool, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if errors.Is(err, os.ErrNotExist) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	p, err := parseProcStat(string(stat))
	if err != nil {
		return Process{}, false, err
	}
	p.Cmdline = string(cmdline)
	return p, true, nil
}

// parseProcStat parses /proc/<pid>/stat. The command name in parentheses may itself contain
// spaces and parentheses, so the fixed fields are read after the LAST ')'. Field numbers
// below are proc(5)'s: 4 ppid, 5 pgrp, 8 tpgid, 22 starttime.
func parseProcStat(stat string) (Process, error) {
	open := strings.IndexByte(stat, '(')
	rparen := strings.LastIndexByte(stat, ')')
	if open <= 0 || rparen < open {
		return Process{}, fmt.Errorf("proc: unparseable stat %q", stat)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(stat[:open]))
	if err != nil {
		return Process{}, fmt.Errorf("proc: unparseable stat %q: %v", stat, err)
	}
	// rest[0] is field 3 (state), so field N is rest[N-3].
	rest := strings.Fields(stat[rparen+1:])
	if len(rest) < 22-3+1 {
		return Process{}, fmt.Errorf("proc: short stat %q", stat)
	}
	num := func(field int) (int, error) { return strconv.Atoi(rest[field-3]) }
	p := Process{PID: pid, Start: rest[22-3]}
	for _, f := range []struct {
		field int
		dst   *int
	}{{4, &p.PPID}, {5, &p.PGID}, {8, &p.TPGID}} {
		n, err := num(f.field)
		if err != nil {
			return Process{}, fmt.Errorf("proc: unparseable stat %q: %v", stat, err)
		}
		*f.dst = n
	}
	return p, nil
}

// environWithout returns the environment with every assignment of the named keys removed,
// so the caller's own values are the only ones present.
func environWithout(keys ...string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, e := range src {
		k, _, _ := strings.Cut(e, "=")
		drop := false
		for _, key := range keys {
			if k == key {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

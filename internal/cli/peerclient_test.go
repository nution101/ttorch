package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// The parent's peer commands, end to end. The parent runs as its own process (Main, through
// runMainEnv) against its own temp TTORCH_HOME. ssh is a stand-in selected through the peerSSH
// seam: this test binary again, in shim mode (runSSHShim), which logs each invocation and plays
// sshd and the peer's shell. For the control key it looks the key up in the peer account's
// authorized_keys, refuses it if absent, and runs that line's forced command with the verb in
// SSH_ORIGINAL_COMMAND; for the lead's session it runs the remote command under /bin/sh. Either
// way the peer side runs against a second temp TTORCH_HOME and TTORCH_DB, both set.

const (
	// testSSHShimEnv names the shim's config file; set, this binary is ssh.
	testSSHShimEnv = "TTORCH_CLI_TEST_SSH_SHIM"
	// testPeerSSHEnv is the ssh program the parent's commands run; testPeerTimeoutEnv bounds a
	// control call and an init session (a time.Duration).
	testPeerSSHEnv     = "TTORCH_CLI_TEST_PEER_SSH"
	testPeerTimeoutEnv = "TTORCH_CLI_TEST_PEER_TIMEOUT"
)

// applyPeerClientSeams points the parent's commands at the stand-in ssh, in a process TestMain
// runs Main in.
func applyPeerClientSeams() {
	if ssh := os.Getenv(testPeerSSHEnv); ssh != "" {
		peerSSH = ssh
	}
	if d, err := time.ParseDuration(os.Getenv(testPeerTimeoutEnv)); err == nil {
		peerCallTimeout, peerInitTimeout = d, d
	}
}

// shimConfig is the stand-in ssh's world: where it logs, the peer account's home, the peer's
// ttorch home, and, with Hang set, that it never answers.
type shimConfig struct {
	Log     string `json:"log"`
	Account string `json:"account"`
	Home    string `json:"home"`
	Hang    bool   `json:"hang"`
}

type shimCall struct {
	Args []string `json:"args"`
	PID  int      `json:"pid"`
}

// runSSHShim is ssh, as far as the parent can tell.
func runSSHShim(cfgPath string, args []string) int {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		return 255
	}
	var cfg shimConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		return 255
	}
	if cfg.Hang {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			return 255
		}
		writeShimLog(cfg, shimCall{Args: args, PID: child.Process.Pid})
		select {}
	}
	writeShimLog(cfg, shimCall{Args: args, PID: os.Getpid()})
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(args) < sep+3 {
		fmt.Fprintln(os.Stderr, "shim: no destination and command")
		return 255
	}
	remote := strings.Join(args[sep+2:], " ")
	env := []string{
		runMainEnv + "=1", testPeerHomeEnv + "=" + cfg.Account, testPeerTtorchEnv + "=" + cfg.Home,
		"HOME=" + cfg.Account, "PATH=" + os.Getenv("PATH"),
		"TTORCH_HOME=" + cfg.Home, "TTORCH_DB=" + filepath.Join(cfg.Home, "state.db"),
	}
	command := remote
	if key := valueAfter(args[:sep], "-i"); key != "" {
		forced, ok := forcedCommandFor(cfg.Account, key)
		if !ok {
			fmt.Fprintln(os.Stderr, "ttorch@peer: Permission denied (publickey).")
			return 255
		}
		command = forced
		env = append(env, "SSH_ORIGINAL_COMMAND="+remote)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir, cmd.Env = cfg.Account, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return 255
	}
	return 0
}

func writeShimLog(cfg shimConfig, c shimCall) {
	f, err := os.OpenFile(cfg.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(c)
	_, _ = f.Write(append(b, '\n'))
}

func valueAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// forcedCommandFor finds the authorized_keys line for the key whose public half is key.pub and
// returns its forced command, as sshd would. A key with no line, or a line with no forced command
// and restrict, is not admitted.
func forcedCommandFor(account, key string) (string, bool) {
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		return "", false
	}
	f, err := os.Open(filepath.Join(account, ".ssh", "authorized_keys"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	re := regexp.MustCompile(`^command="([^"]*)",restrict ssh-ed25519 (\S+) `)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil && m[2] == fields[1] {
			return m[1], true
		}
	}
	return "", false
}

// peerFixture is a parent coordinator and one machine it can provision, joined by the shim.
type peerFixture struct {
	parentHome, parentAccount string
	peer                      initHome
	shim, dir                 string
	cfg                       shimConfig
	extraEnv                  []string
}

func newPeerFixture(t *testing.T) *peerFixture {
	t.Helper()
	f := &peerFixture{parentHome: t.TempDir(), parentAccount: t.TempDir(), peer: newInitHome(t), dir: t.TempDir()}
	// The standard install on the peer, which the lead's session runs as .ttorch/bin/ttorch.
	bin := filepath.Join(f.peer.account, ".ttorch", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "ttorch")); err != nil {
		t.Fatal(err)
	}
	f.cfg = shimConfig{Log: filepath.Join(f.dir, "calls.jsonl"), Account: f.peer.account, Home: f.peer.home}
	f.writeConfig(t)
	f.shim = filepath.Join(f.dir, "ssh")
	script := fmt.Sprintf("#!/bin/sh\nexec env %s='%s' '%s' \"$@\"\n", testSSHShimEnv, filepath.Join(f.dir, "shim.json"), os.Args[0])
	if err := os.WriteFile(f.shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// A tmux session name of its own, handed to the peer's peer.env as one of the lead's
	// settings, so nothing the served process does reads a real fleet's session.
	f.extraEnv = []string{"TTORCH_TMUX_SESSION=ttorch-peer-test-" + filepath.Base(f.dir), "TTORCH_SCHEDULER_AUTOSTART=0", "TTORCH_MODEL=opus"}
	return f
}

func (f *peerFixture) writeConfig(t *testing.T) {
	t.Helper()
	b, _ := json.Marshal(f.cfg)
	if err := os.WriteFile(filepath.Join(f.dir, "shim.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type ranCmd struct {
	code           int
	stdout, stderr string
}

// run runs a parent command as its own process. stdin is /dev/zero, a character device that is
// not the null device, which is what the lead's terminal looks like to checkLeadCaller; pass a
// reader to give it a pipe instead.
func (f *peerFixture) run(t *testing.T, stdin *strings.Reader, args ...string) ranCmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = f.dir
	cmd.Env = append([]string{
		runMainEnv + "=1", testPeerSSHEnv + "=" + f.shim,
		"TTORCH_HOME=" + f.parentHome, "HOME=" + f.parentAccount, "PATH=" + os.Getenv("PATH"),
	}, f.extraEnv...)
	if stdin != nil {
		cmd.Stdin = stdin
	} else {
		zero, err := os.Open("/dev/zero")
		if err != nil {
			t.Skipf("no /dev/zero: %v", err)
		}
		defer zero.Close()
		cmd.Stdin = zero
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	r := ranCmd{stdout: out.String(), stderr: errOut.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return r
}

func (f *peerFixture) calls(t *testing.T) []shimCall {
	t.Helper()
	b, err := os.ReadFile(f.cfg.Log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []shimCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c shimCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f *peerFixture) parentStore(t *testing.T) *db.Store {
	t.Helper()
	return reopen(t, filepath.Join(f.parentHome, "state.db"))
}

func (f *peerFixture) peerStore(t *testing.T) *db.Store {
	t.Helper()
	return reopen(t, f.peer.db())
}

// add provisions the fixture's peer as name and fails the test unless it went live.
func (f *peerFixture) add(t *testing.T, name string) {
	t.Helper()
	if r := f.run(t, nil, "peer", "add", name, "ttorch@build-host"); r.code != 0 {
		t.Fatalf("peer add %s: exit %d\nstdout: %s\nstderr: %s", name, r.code, r.stdout, r.stderr)
	}
}

// controlArgs is the exact command line every control call must run ssh with.
func controlArgs(key, dest, verb string) []string {
	return []string{
		"-F", "none", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-i", key, "--", dest, verb,
	}
}

// TestPeerAddRefusesWithoutTheLead: add and adopt from a pipe, or from a worker context even with
// a terminal, are refused before anything happens: no ssh call, no key, no store.
func TestPeerAddRefusesWithoutTheLead(t *testing.T) {
	f := newPeerFixture(t)
	for _, args := range [][]string{
		{"peer", "add", "build", "ttorch@build-host"},
		{"peer", "adopt", "build", "ttorch@build-host", "--force"},
	} {
		r := f.run(t, strings.NewReader(""), args...)
		if r.code == 0 || !strings.Contains(r.stderr, "interactive terminal") {
			t.Errorf("%v from a pipe: exit %d, stderr %q; want refused for want of a terminal", args, r.code, r.stderr)
		}
	}
	f.extraEnv = append(f.extraEnv, "TTORCH_TASK_ID=t1")
	if r := f.run(t, nil, "peer", "add", "build", "ttorch@build-host"); r.code == 0 || !strings.Contains(r.stderr, "worker context") {
		t.Errorf("peer add from a worker context: exit %d, stderr %q", r.code, r.stderr)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("a refused add ran ssh %d time(s): %+v", len(calls), calls)
	}
	for _, p := range []string{filepath.Join(f.parentHome, "peers"), filepath.Join(f.parentHome, "state.db")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("a refused add created %s", p)
		}
	}
}

// TestPeerAddProvisions: add generates a private control key, runs init over the lead's session,
// and proves the key with version over the control channel alone, whose ssh command line is
// exactly the pinned one. The peer then records this coordinator as its parent and the parent
// records the peer as live.
func TestPeerAddProvisions(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	r := f.run(t, nil, "peer", "add", "build", "ttorch@build-host", "--approve-dest", "lead@build-host")
	if r.code != 0 {
		t.Fatalf("peer add: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "peer build is live") {
		t.Errorf("stdout = %q", r.stdout)
	}

	ps := f.parentStore(t)
	self, err := ps.GetCoordinator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok, err := ps.GetPeer(ctx, "build")
	if err != nil || !ok {
		t.Fatalf("GetPeer = %v, %v", ok, err)
	}
	key := filepath.Join(f.parentHome, "peers", "build", "control")
	if p.Status != db.PeerLive || p.ControlDest != "ttorch@build-host" || p.ApproveDest != "lead@build-host" ||
		p.ControlKey != key || p.Protocol != peer.ProtocolVersion || p.LastOKAt.IsZero() {
		t.Errorf("peer row = %+v", p)
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(f.parentHome, "peers"): 0o700, filepath.Dir(key): 0o700, key: 0o600} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v (%v), want %v", path, fi.Mode(), err, mode)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := peer.ParseControlKey(string(pub))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(pub)), "ttorch-peer-control:"+self.CoordID) {
		t.Errorf("control key %q: %v", pub, err)
	}

	c, err := f.peerStore(t).GetCoordinator(ctx)
	if err != nil || c.Role != db.CoordinatorPeer || c.Name != "build" || c.ParentID != self.CoordID {
		t.Errorf("the peer's coordinator row = %+v, %v; want peer build of %s", c, err, self.CoordID)
	}
	keys, err := os.ReadFile(f.peer.authorizedKeys())
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := filepath.EvalSymlinks(os.Args[0])
	if want := peer.AuthorizedKeyLine(exe, blob, self.CoordID) + "\n"; string(keys) != want {
		t.Errorf("the peer's authorized_keys = %q\nwant %q", keys, want)
	}
	conf, err := readPeerEnv(f.peer.home, os.Getuid())
	if err != nil || conf["TTORCH_MODEL"] != "opus" || !strings.HasPrefix(conf["TTORCH_TMUX_SESSION"], "ttorch-peer-test-") {
		t.Errorf("the peer's peer.env = %v, %v; want the lead's settings", conf, err)
	}

	calls := f.calls(t)
	want := [][]string{
		{"-T", "--", "lead@build-host", ".ttorch/bin/ttorch peer init"},
		controlArgs(key, "ttorch@build-host", "version"),
	}
	if len(calls) != len(want) {
		t.Fatalf("ssh ran %d time(s): %+v; want the init session, then version", len(calls), calls)
	}
	for i := range want {
		if !reflect.DeepEqual(calls[i].Args, want[i]) {
			t.Errorf("ssh call %d = %q\nwant        %q", i, calls[i].Args, want[i])
		}
	}

	// A live peer is not added again; nothing is run.
	if r := f.run(t, nil, "peer", "add", "build", "ttorch@build-host"); r.code == 0 || !strings.Contains(r.stderr, "already registered") {
		t.Errorf("adding a live peer again: exit %d, stderr %q", r.code, r.stderr)
	}
	if n := len(f.calls(t)); n != 2 {
		t.Errorf("a refused second add ran ssh (%d calls)", n-2)
	}
}

// TestPeerAddFailsClosed: a peer whose control key does not reach the channel stays
// provisioning with the reason recorded; a later add resumes with the same key.
func TestPeerAddFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	// The init session runs, but the account's authorized_keys is a directory: init cannot admit
	// the key, so the version proof never runs.
	if err := os.MkdirAll(f.peer.authorizedKeys(), 0o700); err != nil {
		t.Fatal(err)
	}
	r := f.run(t, nil, "peer", "add", "build", "ttorch@build-host")
	if r.code == 0 || !strings.Contains(r.stderr, "stays provisioning") {
		t.Fatalf("peer add with no way to admit the key: exit %d, stderr %q", r.code, r.stderr)
	}
	p, _, _ := f.parentStore(t).GetPeer(ctx, "build")
	if p.Status != db.PeerProvisioning || p.LastError == "" {
		t.Errorf("after a failed add: %+v", p)
	}
	if n := len(f.calls(t)); n != 1 {
		t.Errorf("ssh ran %d times; want only the init session", n)
	}
	firstKey, _ := os.ReadFile(p.ControlKey + ".pub")

	if err := os.Remove(f.peer.authorizedKeys()); err != nil {
		t.Fatal(err)
	}
	f.add(t, "build")
	p, _, _ = f.parentStore(t).GetPeer(ctx, "build")
	if again, _ := os.ReadFile(p.ControlKey + ".pub"); p.Status != db.PeerLive || !bytes.Equal(again, firstKey) {
		t.Errorf("the resumed add = %+v; key reused: %v", p, bytes.Equal(again, firstKey))
	}
}

// TestPeerAdopt: a second parent cannot add a peer another one provisioned, and adopt needs
// --force; with it, the peer moves to the second parent, which reports the parent it replaced and
// the first parent's control key left in authorized_keys.
func TestPeerAdopt(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")

	second := *f
	second.parentHome, second.parentAccount = t.TempDir(), t.TempDir()
	if r := second.run(t, nil, "peer", "add", "build", "ttorch@build-host"); r.code == 0 || !strings.Contains(r.stderr, "adopt --force") {
		t.Errorf("a second parent's add: exit %d, stderr %q; want refused, naming adopt --force", r.code, r.stderr)
	}
	if r := second.run(t, nil, "peer", "adopt", "build", "ttorch@build-host"); r.code == 0 || !strings.Contains(r.stderr, "--force") {
		t.Errorf("adopt without --force: exit %d, stderr %q", r.code, r.stderr)
	}
	r := second.run(t, nil, "peer", "adopt", "build", "ttorch@build-host", "--force")
	if r.code != 0 || !strings.Contains(r.stdout, "moved from parent") || !strings.Contains(r.stdout, "1 other control key") {
		t.Fatalf("adopt --force: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	secondSelf, _ := second.parentStore(t).GetCoordinator(ctx)
	if c, _ := f.peerStore(t).GetCoordinator(ctx); c.ParentID != secondSelf.CoordID {
		t.Errorf("after adopt the peer's parent is %s, want %s", c.ParentID, secondSelf.CoordID)
	}
}

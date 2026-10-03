package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
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
// ttorch home, and, with Hang set, that it never answers. With Unbound set, a control key's
// forced command runs without its --parent, as a line an older init wrote would.
type shimConfig struct {
	Log     string `json:"log"`
	Account string `json:"account"`
	Home    string `json:"home"`
	Hang    bool   `json:"hang"`
	Unbound bool   `json:"unbound"`
}

type shimCall struct {
	Args  []string `json:"args"`
	PID   int      `json:"pid"`
	Child int      `json:"child,omitempty"`
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
		writeShimLog(cfg, shimCall{Args: args, PID: os.Getpid(), Child: child.Process.Pid})
		for {
			time.Sleep(time.Hour)
		}
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
		if cfg.Unbound {
			command = regexp.MustCompile(` --parent [0-9a-f]+$`).ReplaceAllString(command, "")
		}
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
	// The key is admitted, but its forced command binds no parent, so the peer would refuse
	// every task, goal and answer the parent sends: the version proof says so, and add stops.
	f.cfg.Unbound = true
	f.writeConfig(t)
	r = f.run(t, nil, "peer", "add", "build", "ttorch@build-host")
	if r.code == 0 || !strings.Contains(r.stderr, "stays provisioning") || !strings.Contains(r.stderr, "bound to") {
		t.Fatalf("peer add through a key bound to no parent: exit %d, stderr %q", r.code, r.stderr)
	}
	if p, _, _ = f.parentStore(t).GetPeer(ctx, "build"); p.Status != db.PeerProvisioning {
		t.Errorf("after an unbound key: %+v", p)
	}
	f.cfg.Unbound = false
	f.writeConfig(t)

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

// lastCall is the most recent ssh invocation, which must be a control call for verb with exactly
// the pinned options.
func (f *peerFixture) lastCall(t *testing.T, ctx context.Context, verb string) shimCall {
	t.Helper()
	calls := f.calls(t)
	if len(calls) == 0 {
		t.Fatalf("no ssh call for %s", verb)
	}
	p, _, _ := f.parentStore(t).GetPeer(ctx, "build")
	last := calls[len(calls)-1]
	if want := controlArgs(p.ControlKey, "ttorch@build-host", verb); !reflect.DeepEqual(last.Args, want) {
		t.Errorf("ssh ran %q\nwant    %q", last.Args, want)
	}
	return last
}

// TestPeerTaskAdd: task-add puts a briefed backlog row in the peer's store, created by the
// parent, after the peer's own brief lint passed, and records the delegation in the parent's
// store, which holds no row for the task. A repeat of the request id changes nothing; a brief the
// lint refuses creates nothing on either side.
func TestPeerTaskAdd(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	repo := lintRepo(t)
	if _, err := f.peerStore(t).UpsertProject(ctx, repo, "fixture"); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(briefPath, []byte(cleanBrief), 0o600); err != nil {
		t.Fatal(err)
	}
	add := []string{"peer", "task-add", "build", "p-1", "--repo", repo, "--brief-file", briefPath,
		"--title", "from the parent", "--touches", "pkg/thing.go", "--effort", "high", "--request-id", "pt-1"}
	r := f.run(t, nil, add...)
	if r.code != 0 || !strings.Contains(r.stdout, "task p-1 added") {
		t.Fatalf("peer task-add: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	f.lastCall(t, ctx, "task-add")

	task := mustTask(t, f.peerStore(t), "p-1")
	if task.Status != db.StatusPending || !task.HasBrief || task.CreatedBy != db.ActorParent || task.Title != "from the parent" ||
		task.Effort != "high" || strings.Join(task.Footprint, ",") != "pkg/thing.go" {
		t.Errorf("the peer's task = %+v", task)
	}
	if b, err := os.ReadFile(paths.Paths{Home: f.peer.home}.BriefPath("p-1")); err != nil || string(b) != cleanBrief {
		t.Errorf("the peer's stored brief = %q, %v", b, err)
	}
	ps := f.parentStore(t)
	if _, ok, _ := ps.GetTask(ctx, "p-1"); ok {
		t.Error("the parent holds a row for the peer's task")
	}
	ds, err := ps.ListDelegations(ctx, "build")
	sum := sha256Hex(cleanBrief)
	if err != nil || len(ds) != 1 || ds[0].RequestID != "pt-1" || ds[0].Kind != db.DelegationTask || ds[0].RemoteTaskID != "p-1" || ds[0].BriefSHA256 != sum {
		t.Errorf("the parent's delegations = %+v, %v", ds, err)
	}

	r = f.run(t, nil, add...)
	if r.code != 0 || !strings.Contains(r.stdout, "already added") {
		t.Errorf("a repeat: exit %d, stdout %q", r.code, r.stdout)
	}
	if n := countIn(t, f.peerStore(t), "tasks"); n != 1 {
		t.Errorf("a repeat left %d tasks on the peer", n)
	}

	bad := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(bad, []byte(defectiveBrief), 0o600); err != nil {
		t.Fatal(err)
	}
	r = f.run(t, nil, "peer", "task-add", "build", "p-bad", "--repo", repo, "--brief-file", bad)
	if r.code == 0 || !strings.Contains(r.stderr, "brief_lint") || !strings.Contains(r.stderr, "violation") {
		t.Errorf("a defective brief: exit %d, stderr %q; want the peer's lint refusal and report", r.code, r.stderr)
	}
	if _, ok, _ := f.peerStore(t).GetTask(ctx, "p-bad"); ok {
		t.Error("a refused brief created a task on the peer")
	}
	if ds, _ := ps.ListDelegations(ctx, "build"); len(ds) != 1 {
		t.Errorf("a refused add left a delegation record: %+v", ds)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestPeerDecisionsAndAnswer: decisions reads an escalation raised on the peer, its text escaped
// on arrival; answer records it on the peer as relayed by the parent, wakes the peer's manager,
// and says so without claiming the lead.
func TestPeerDecisionsAndAnswer(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	ps := f.peerStore(t)
	proj, err := ps.UpsertProject(ctx, "/srv/q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.CreateTask(ctx, db.Task{ID: "t-q", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	esc, err := ps.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "which one?\x1b]0;pwned\x07\nsecond line")
	if err != nil {
		t.Fatal(err)
	}

	r := f.run(t, nil, "peer", "decisions", "build")
	if r.code != 0 {
		t.Fatalf("peer decisions: exit %d, %s", r.code, r.stderr)
	}
	f.lastCall(t, ctx, "decisions")
	if !strings.Contains(r.stdout, fmt.Sprintf("#%d  question  task t-q", esc.ID)) || !strings.Contains(r.stdout, `which one?\x1b]0;pwned\x07\nsecond line`) {
		t.Errorf("peer decisions = %q", r.stdout)
	}
	if strings.ContainsAny(r.stdout, "\x1b\x07") {
		t.Errorf("peer decisions printed a raw control character: %q", r.stdout)
	}
	if r := f.run(t, nil, "peer", "decisions", "build", "--since", fmt.Sprint(esc.ID)); r.code != 0 || strings.Contains(r.stdout, "which one") {
		t.Errorf("decisions above the only escalation = %q", r.stdout)
	}

	r = f.run(t, nil, "peer", "answer", "build", fmt.Sprint(esc.ID), "-m", "the first one", "--request-id", "pa-1")
	if r.code != 0 {
		t.Fatalf("peer answer: exit %d, %s", r.code, r.stderr)
	}
	f.lastCall(t, ctx, "answer")
	if !strings.Contains(r.stdout, "relayed by this coordinator") || strings.Contains(strings.ToLower(r.stdout), "lead") {
		t.Errorf("peer answer printed %q; it must say the parent relayed it and must not claim the lead", r.stdout)
	}
	e, _, _ := ps.GetEscalation(ctx, esc.ID)
	if e.Status != db.EscalationAnswered || e.AnsweredBy != db.ActorParent || e.Answer != "the first one" {
		t.Errorf("the peer's escalation = %+v", e)
	}
	evs := managerEventsIn(t, ps)
	if len(evs) != 1 || evs[0].Actor != db.ActorParent || !evs[0].Actionable {
		t.Errorf("the peer's manager events = %+v, want one actionable event recorded as the parent's", evs)
	}
	if r := f.run(t, nil, "peer", "answer", "build", fmt.Sprint(esc.ID), "-m", "again", "--request-id", "pa-1"); r.code != 0 || !strings.Contains(r.stdout, "nothing changed") {
		t.Errorf("a repeated answer: exit %d, %q", r.code, r.stdout)
	}
	f.extraEnv = append(f.extraEnv, "TTORCH_TASK_ID=t1")
	before := len(f.calls(t))
	if r := f.run(t, nil, "peer", "answer", "build", fmt.Sprint(esc.ID), "-m", "x"); r.code == 0 || !strings.Contains(r.stderr, "worker context") {
		t.Errorf("an answer from a worker context: exit %d, %q", r.code, r.stderr)
	}
	if n := len(f.calls(t)); n != before {
		t.Error("an answer from a worker context ran ssh")
	}
}

// TestPeerGoal: a goal reaches the peer's manager as one actionable event recorded as the
// parent's, and the parent records it as a delegation. After the peer is adopted by another
// parent, this one's goal is refused and appends nothing, and its record is dropped.
func TestPeerGoal(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	r := f.run(t, nil, "peer", "goal", "build", "-m", "split the importer", "--request-id", "pg-1")
	if r.code != 0 || !strings.Contains(r.stdout, "goal handed to its manager") {
		t.Fatalf("peer goal: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	f.lastCall(t, ctx, "goal")
	evs := managerEventsIn(t, f.peerStore(t))
	if len(evs) != 1 || evs[0].Type != db.EventGoal || evs[0].Actor != db.ActorParent || evs[0].Payload != "split the importer" {
		t.Errorf("the peer's manager events = %+v", evs)
	}
	ds, _ := f.parentStore(t).ListDelegations(ctx, "build")
	if len(ds) != 1 || ds[0].Kind != db.DelegationGoal || ds[0].BriefSHA256 != sha256Hex("split the importer") {
		t.Errorf("the parent's delegations = %+v", ds)
	}

	second := *f
	second.parentHome, second.parentAccount = t.TempDir(), t.TempDir()
	if r := second.run(t, nil, "peer", "adopt", "build", "ttorch@build-host", "--force"); r.code != 0 {
		t.Fatalf("adopt: %s", r.stderr)
	}
	r = f.run(t, nil, "peer", "goal", "build", "-m", "something else", "--request-id", "pg-2")
	if r.code == 0 || !strings.Contains(r.stderr, peer.CodeWrongParent) {
		t.Errorf("the old parent's goal: exit %d, stderr %q; want wrong_parent", r.code, r.stderr)
	}
	if evs := managerEventsIn(t, f.peerStore(t)); len(evs) != 1 {
		t.Errorf("the old parent's goal appended an event: %+v", evs)
	}
	if ds, _ := f.parentStore(t).ListDelegations(ctx, "build"); len(ds) != 1 {
		t.Errorf("a refused goal left its record: %+v", ds)
	}
}

// TestPeerStatusAndLs: status prints the peer's summary and caches it; ls shows the registry,
// with what was delegated, from this coordinator's store alone.
func TestPeerStatusAndLs(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	if r := f.run(t, nil, "peer", "goal", "build", "-m", "tidy"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	r := f.run(t, nil, "peer", "status", "build")
	if r.code != 0 || !strings.Contains(r.stdout, "peer build (live") || !strings.Contains(r.stdout, "tasks:") {
		t.Fatalf("peer status: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	f.lastCall(t, ctx, "summary")
	p, _, _ := f.parentStore(t).GetPeer(ctx, "build")
	var cached peer.Summary
	if err := json.Unmarshal([]byte(p.Summary), &cached); err != nil || cached.SchemaVersion != peer.SchemaVersion {
		t.Errorf("cached summary %q: %v", p.Summary, err)
	}
	if r := f.run(t, nil, "peer", "status", "build", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"schema_version"`) {
		t.Errorf("peer status --json: %q", r.stdout)
	}

	before := len(f.calls(t))
	r = f.run(t, nil, "peer", "ls")
	if r.code != 0 || !strings.Contains(r.stdout, "build  live  control ttorch@build-host") || !strings.Contains(r.stdout, "0 task(s), 1 goal(s)") {
		t.Errorf("peer ls = %q", r.stdout)
	}
	if n := len(f.calls(t)); n != before {
		t.Error("peer ls ran ssh")
	}
}

// TestPeerRepoAdd: a repo is recorded only when the peer has a project at that path, and not
// when this coordinator already has a project with the same origin.
func TestPeerRepoAdd(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	if _, err := f.peerStore(t).UpsertProject(ctx, "/srv/app", "app"); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t, nil, "peer", "repo", "add", "build", "/srv/elsewhere", "--origin", "git@example.com:org/x.git"); r.code == 0 || !strings.Contains(r.stderr, "no project at") {
		t.Errorf("a path the peer has no project at: exit %d, %q", r.code, r.stderr)
	}
	ps := f.parentStore(t)
	proj, err := ps.UpsertProject(ctx, "/local/app", "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetProjectDefaultBranch(ctx, proj.ID, "main", "https://example.com/org/app"); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t, nil, "peer", "repo", "add", "build", "/srv/app", "--origin", "git@example.com:org/app.git"); r.code == 0 || !strings.Contains(r.stderr, "already belongs") {
		t.Errorf("an origin this coordinator has: exit %d, %q", r.code, r.stderr)
	}
	if r := f.run(t, nil, "peer", "repo", "add", "build", "/srv/app", "--origin", "git@example.com:org/other.git"); r.code != 0 {
		t.Fatalf("peer repo add: exit %d, %s", r.code, r.stderr)
	}
	if repos, _ := ps.ListPeerRepos(ctx, "build"); len(repos) != 1 || repos[0].RemotePath != "/srv/app" {
		t.Errorf("the peer's repos = %+v", repos)
	}
}

// TestPeerRetire: retire is the lead's (refused from a pipe), keeps the row, deletes the control
// key, and a retired peer is never called again.
func TestPeerRetire(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	p, _, _ := f.parentStore(t).GetPeer(ctx, "build")
	if r := f.run(t, strings.NewReader(""), "peer", "retire", "build"); r.code == 0 || !strings.Contains(r.stderr, "interactive terminal") {
		t.Errorf("retire from a pipe: exit %d, %q", r.code, r.stderr)
	}
	if _, err := os.Stat(p.ControlKey); err != nil {
		t.Fatalf("a refused retire touched the key: %v", err)
	}
	if r := f.run(t, nil, "peer", "retire", "build"); r.code != 0 {
		t.Fatalf("peer retire: exit %d, %s", r.code, r.stderr)
	}
	for _, k := range []string{p.ControlKey, p.ControlKey + ".pub"} {
		if _, err := os.Lstat(k); !os.IsNotExist(err) {
			t.Errorf("retire left %s", k)
		}
	}
	if p, _, _ = f.parentStore(t).GetPeer(ctx, "build"); p.Status != db.PeerRetired {
		t.Errorf("after retire: %+v", p)
	}
	before := len(f.calls(t))
	for _, args := range [][]string{{"peer", "status", "build"}, {"peer", "goal", "build", "-m", "x"}, {"peer", "decisions", "build"}} {
		if r := f.run(t, nil, args...); r.code == 0 || !strings.Contains(r.stderr, "retired") {
			t.Errorf("%v on a retired peer: exit %d, %q", args, r.code, r.stderr)
		}
	}
	if n := len(f.calls(t)); n != before {
		t.Error("a retired peer was called")
	}
}

// TestPeerCallDeadline: a control call to an ssh that never answers is killed at the client's
// deadline, together with the child it started, and the failure is recorded on the peer's row.
func TestPeerCallDeadline(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	f.add(t, "build")
	f.cfg.Hang = true
	f.writeConfig(t)
	f.extraEnv = append(f.extraEnv, testPeerTimeoutEnv+"=1s")
	start := time.Now()
	r := f.run(t, nil, "peer", "status", "build")
	if r.code == 0 || !strings.Contains(r.stderr, "did not answer in time") {
		t.Fatalf("peer status against a hung ssh: exit %d, stderr %q", r.code, r.stderr)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("peer status took %s past a 1s deadline", took)
	}
	calls := f.calls(t)
	hung := calls[len(calls)-1]
	if hung.Child == 0 {
		t.Fatalf("the hung shim logged no child: %+v", hung)
	}
	deadline := time.Now().Add(10 * time.Second)
	for (alive(hung.PID) || alive(hung.Child)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(hung.PID) {
		t.Errorf("the ssh process %d outlived the deadline", hung.PID)
	}
	if alive(hung.Child) {
		_ = syscall.Kill(hung.Child, syscall.SIGKILL)
		t.Errorf("the child %d of the ssh process outlived the deadline", hung.Child)
	}
	if p, _, _ := f.parentStore(t).GetPeer(ctx, "build"); !strings.Contains(p.LastError, "did not answer in time") || p.Status != db.PeerLive {
		t.Errorf("after a timeout: status %s, last error %q; want the error recorded and the status left to the poll", p.Status, p.LastError)
	}
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// TestPeerCommandsNeedAUsablePeer: an unknown or still-provisioning peer is refused before ssh
// runs.
func TestPeerCommandsNeedAUsablePeer(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	ps := f.parentStore(t)
	if _, err := ps.RegisterPeer(ctx, db.Peer{Name: "half", ControlDest: "ttorch@build-host", ApproveDest: "ttorch@build-host", ControlKey: "/nowhere/control"}, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"peer", "status", "nobody"}, "no peer is registered"},
		{[]string{"peer", "status", "half"}, "still provisioning"},
		{[]string{"peer", "goal", "half", "-m", "x"}, "still provisioning"},
	} {
		if r := f.run(t, nil, c.args...); r.code == 0 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%v: exit %d, %q; want %q", c.args, r.code, r.stderr, c.want)
		}
	}
	if n := len(f.calls(t)); n != 0 {
		t.Errorf("ssh ran %d time(s) for peers that cannot be called", n)
	}
}

// TestPeerCommandsRefuseAWorkerContext: every command that sends a peer anything refuses a
// worker context before it runs ssh, the accident guard `ttorch answer` and `ttorch escalate`
// have. A worker told by text it read to hand another machine work, or to read it, is stopped
// here; one set on getting past it can, as everywhere on one machine.
func TestPeerCommandsRefuseAWorkerContext(t *testing.T) {
	f := newPeerFixture(t)
	f.add(t, "build")
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte(cleanBrief), 0o600); err != nil {
		t.Fatal(err)
	}
	f.extraEnv = append(f.extraEnv, "TTORCH_TASK_ID=t1")
	for _, args := range [][]string{
		{"peer", "status", "build"},
		{"peer", "decisions", "build"},
		{"peer", "answer", "build", "1", "-m", "yes"},
		{"peer", "task-add", "build", "p-1", "--repo", "/srv/app", "--brief-file", brief},
		{"peer", "goal", "build", "-m", "tidy the importer"},
		{"peer", "repo", "add", "build", "/srv/app", "--origin", "git@example.com:org/app.git"},
	} {
		t.Run(args[1], func(t *testing.T) {
			before := len(f.calls(t))
			r := f.run(t, nil, args...)
			if r.code == 0 || !strings.Contains(r.stderr, "worker context") {
				t.Errorf("%v from a worker context: exit %d, stderr %q", args, r.code, r.stderr)
			}
			if n := len(f.calls(t)); n != before {
				t.Errorf("%v from a worker context ran ssh", args)
			}
		})
	}
	if ds, _ := f.parentStore(t).ListDelegations(context.Background(), "build"); len(ds) != 0 {
		t.Errorf("a refused command recorded a delegation: %+v", ds)
	}
}

package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// initHome is one peer account before provisioning: a home directory with no .ssh, and a ttorch
// home with no store and no peer.env. As with serveHome, the ttorch home is its own temp dir
// because the store's test guard refuses anything under $HOME/.ttorch.
type initHome struct {
	account, home, dir string
	extraEnv           []string
}

func newInitHome(t *testing.T) initHome {
	t.Helper()
	return initHome{account: t.TempDir(), home: t.TempDir(), dir: t.TempDir()}
}

func (h initHome) db() string             { return filepath.Join(h.home, "state.db") }
func (h initHome) authorizedKeys() string { return filepath.Join(h.account, ".ssh", "authorized_keys") }

// sessionEnv is roughly what sshd gives an interactive session: HOME, PATH, and nothing of the
// client's. TTORCH_HOME and TTORCH_DB point at a decoy, as a lead's profile on the peer might,
// and must not be where init writes.
func (h initHome) sessionEnv(decoy string) []string {
	return append([]string{
		runMainEnv + "=1",
		testPeerHomeEnv + "=" + h.account,
		testPeerTtorchEnv + "=" + h.home,
		"HOME=" + h.account,
		"PATH=" + os.Getenv("PATH"),
		"TTORCH_HOME=" + decoy,
		"TTORCH_DB=" + filepath.Join(decoy, "state.db"),
	}, h.extraEnv...)
}

// newControlKey returns a fresh ed25519 public key line as ssh-keygen writes it, and its blob.
func newControlKey(t *testing.T) (line, blob string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wire := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	blob = base64.StdEncoding.EncodeToString(append(wire([]byte("ssh-ed25519")), wire(pub)...))
	return "ssh-ed25519 " + blob + " ttorch-peer-control:" + servedParent + "\n", blob
}

type initRan struct {
	code   int
	resp   peer.Response
	result peer.InitResult
	raw    string
	stderr string
}

// initRun runs `ttorch peer init` as its own process with body on stdin.
func initRun(t *testing.T, h initHome, body string, args ...string) initRan {
	t.Helper()
	decoy := t.TempDir()
	cmd := exec.Command(os.Args[0], append([]string{"peer", "init"}, args...)...)
	cmd.Dir = h.dir
	cmd.Env = h.sessionEnv(decoy)
	cmd.Stdin = strings.NewReader(body)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	r := initRan{raw: out.String(), stderr: errOut.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running peer init: %v", err)
	}
	if entries, _ := os.ReadDir(decoy); len(entries) != 0 {
		t.Errorf("peer init wrote into the session's TTORCH_HOME: %v", entries)
	}
	if len(args) > 0 {
		return r
	}
	dec := json.NewDecoder(strings.NewReader(r.raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r.resp); err != nil {
		t.Fatalf("stdout is not one response: %v\nstdout: %s\nstderr: %s", err, r.raw, r.stderr)
	}
	if r.resp.OK {
		b, _ := json.Marshal(r.resp.Result)
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&r.result); err != nil {
			t.Fatalf("result is not an InitResult: %v\n%s", err, b)
		}
	}
	return r
}

func initBody(t *testing.T, req peer.InitRequest) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r initRan) refused(t *testing.T, label, code string) {
	t.Helper()
	if r.code != 1 || r.resp.OK || r.resp.Error == nil || r.resp.Error.Code != code {
		t.Errorf("%s: exit %d, response %s; want exit 1 refused with %s", label, r.code, r.raw, code)
	}
}

// forcedCommand is the command="..." of an authorized_keys line.
func forcedCommand(t *testing.T, line string) string {
	t.Helper()
	m := regexp.MustCompile(`^command="([^"]*)",restrict ssh-ed25519 `).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("authorized_keys line %q has no forced command with restrict", line)
	}
	return m[1]
}

// TestPeerInitProvisions: init records the parent, writes a private peer.env with a PATH that
// reaches the session's directories and the parent's settings, and admits the control key with a
// forced command naming this binary by absolute path, with restrict. Running that forced command
// as sshd would then answers version from the store init wrote.
func TestPeerInitProvisions(t *testing.T) {
	ctx := context.Background()
	h := newInitHome(t)
	pub, blob := newControlKey(t)
	req := peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub,
		Settings: map[string]string{"TTORCH_MODEL": "opus", "TTORCH_EFFORT": "high"}}
	r := initRun(t, h, initBody(t, req))
	if r.code != 0 || !r.resp.OK {
		t.Fatalf("peer init: exit %d, %s\nstderr: %s", r.code, r.raw, r.stderr)
	}
	exe, _ := filepath.EvalSymlinks(os.Args[0])
	got := r.result
	if len(got.CoordID) != 32 || got.Name != "build" || got.Role != db.CoordinatorPeer || got.ParentID != servedParent ||
		got.PreviousParent != "" || got.Binary != exe || got.AuthorizedKeys != "added" || got.StaleControlKeys != 0 ||
		got.PeerEnv != "written" || got.PeerEnvPath != filepath.Join(h.home, "peer.env") {
		t.Errorf("init result = %+v", got)
	}
	for _, m := range got.Missing {
		if m != "tmux" && m != "claude" && m != "git" {
			t.Errorf("missing names %q, which is not a fleet program", m)
		}
	}

	s := reopen(t, h.db())
	c, err := s.GetCoordinator(ctx)
	if err != nil || c.CoordID != got.CoordID || c.Role != db.CoordinatorPeer || c.Name != "build" || c.ParentID != servedParent {
		t.Errorf("coordinator row = %+v, %v", c, err)
	}

	b, err := os.ReadFile(h.authorizedKeys())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(string(b), "\n")
	if want := peer.AuthorizedKeyLine(exe, blob, servedParent); line != want {
		t.Errorf("authorized_keys = %q\nwant              %q", b, want)
	}
	for path, mode := range map[string]os.FileMode{filepath.Dir(h.authorizedKeys()): 0o700, h.authorizedKeys(): 0o600, got.PeerEnvPath: 0o600} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %v (%v), want %v", path, fi.Mode(), err, mode)
		}
	}
	conf, err := readPeerEnv(h.home, os.Getuid())
	if err != nil {
		t.Fatalf("the peer.env init wrote is one the channel refuses: %v", err)
	}
	if conf["TTORCH_MODEL"] != "opus" || conf["TTORCH_EFFORT"] != "high" || len(conf) != 3 {
		t.Errorf("peer.env = %v, want PATH and the two settings", conf)
	}
	path := filepath.SplitList(conf["PATH"])
	if len(path) == 0 || path[0] != filepath.Join(h.home, "bin") {
		t.Errorf("peer.env PATH = %q, want the default first", conf["PATH"])
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		fi, err := os.Stat(d)
		if filepath.IsAbs(d) && err == nil && fi.IsDir() && fi.Mode().Perm()&0o022 == 0 && !strings.Contains(conf["PATH"], d) {
			t.Errorf("peer.env PATH %q lacks the session's %s", conf["PATH"], d)
		}
	}

	// The line works: sshd would run its command with the client's request in
	// SSH_ORIGINAL_COMMAND, under the account's shell.
	cmd := exec.Command("/bin/sh", "-c", forcedCommand(t, line))
	cmd.Dir = h.account
	cmd.Env = []string{runMainEnv + "=1", testPeerHomeEnv + "=" + h.account, testPeerTtorchEnv + "=" + h.home,
		"HOME=" + h.account, "PATH=/usr/bin:/bin", "SSH_ORIGINAL_COMMAND=version"}
	cmd.Stdin = strings.NewReader("{}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the forced command failed: %v\n%s", err, out)
	}
	var resp struct {
		OK     bool               `json:"ok"`
		Result peer.VersionResult `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || !resp.OK ||
		resp.Result.Coordinator != (peer.CoordinatorInfo{Name: "build", Role: db.CoordinatorPeer, ParentID: servedParent}) {
		t.Errorf("version through the forced command = %s (%v)", out, err)
	}

	// Again: nothing new is written.
	before, _ := os.ReadFile(h.authorizedKeys())
	envBefore, _ := os.ReadFile(got.PeerEnvPath)
	r = initRun(t, h, initBody(t, req))
	if r.code != 0 || r.result.AuthorizedKeys != "present" || r.result.PeerEnv != "kept" || r.result.PreviousParent != servedParent {
		t.Errorf("a second init = exit %d, %+v", r.code, r.result)
	}
	if after, _ := os.ReadFile(h.authorizedKeys()); !bytes.Equal(after, before) {
		t.Errorf("a second init changed authorized_keys:\n%s", after)
	}
	if after, _ := os.ReadFile(got.PeerEnvPath); !bytes.Equal(after, envBefore) {
		t.Errorf("a second init changed peer.env")
	}
}

// TestPeerInitKeepsItsParent: another parent is refused and changes nothing unless it forces the
// move, which reports the parent it replaced and removes that parent's control key line.
func TestPeerInitKeepsItsParent(t *testing.T) {
	ctx := context.Background()
	h := newInitHome(t)
	pub, _ := newControlKey(t)
	if r := initRun(t, h, initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub})); r.code != 0 {
		t.Fatalf("first init: %s", r.raw)
	}
	before, _ := os.ReadFile(h.authorizedKeys())
	other := strings.Repeat("e", 32)
	otherKey, otherBlob := newControlKey(t)
	r := initRun(t, h, initBody(t, peer.InitRequest{Name: "build", ParentID: other, ControlKey: otherKey}))
	r.refused(t, "another parent", peer.CodeWrongParent)
	if r.resp.Error != nil && !strings.Contains(r.resp.Error.Message, "adopt --force") {
		t.Errorf("the refusal %q does not say how to move the peer", r.resp.Error.Message)
	}
	if after, _ := os.ReadFile(h.authorizedKeys()); !bytes.Equal(after, before) {
		t.Errorf("a refused init changed authorized_keys:\n%s", after)
	}
	if c, _ := reopen(t, h.db()).GetCoordinator(ctx); c.ParentID != servedParent {
		t.Errorf("a refused init moved the peer to %s", c.ParentID)
	}

	r = initRun(t, h, initBody(t, peer.InitRequest{Name: "build", ParentID: other, ControlKey: otherKey, Force: true}))
	if r.code != 0 || r.result.ParentID != other || r.result.PreviousParent != servedParent || r.result.StaleControlKeys != 0 ||
		strings.Join(r.result.RemovedControlKeys, ",") != servedParent || r.result.AuthorizedKeys != "added" {
		t.Fatalf("a forced init = exit %d, %+v (%s)", r.code, r.result, r.raw)
	}
	// The first parent's line is gone; only the new parent's key is admitted.
	b, _ := os.ReadFile(h.authorizedKeys())
	if lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"); len(lines) != 1 || !strings.Contains(lines[0], otherBlob) || !strings.Contains(lines[0], "--parent "+other) {
		t.Errorf("after a forced init, authorized_keys = %q", b)
	}
}

// TestPeerInitRefusesBeforeWriting: a worker context, a malformed request, a setting peer.env may
// not hold from the parent, and an existing peer.env the channel would refuse all stop init with
// nothing written: no store, no peer.env, no authorized_keys.
func TestPeerInitRefusesBeforeWriting(t *testing.T) {
	pub, _ := newControlKey(t)
	good := peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub}
	cases := []struct {
		label, body, code string
		env               []string
	}{
		{"worker context", initBody(t, good), peer.CodeWorkerContext, []string{"TTORCH_TASK_ID=t1"}},
		{"bad name", initBody(t, peer.InitRequest{Name: "Build!", ParentID: servedParent, ControlKey: pub}), peer.CodeBadRequest, nil},
		{"bad parent", initBody(t, peer.InitRequest{Name: "build", ParentID: "nope", ControlKey: pub}), peer.CodeBadRequest, nil},
		{"rsa key", initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: "ssh-rsa AAAAB3Nza"}), peer.CodeBadRequest, nil},
		{"key with options", initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: `command="sh" ` + pub}), peer.CodeBadRequest, nil},
		{"a path setting", initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub, Settings: map[string]string{"TTORCH_HOME": "/x"}}), peer.CodeBadRequest, nil},
		{"PATH from the parent", initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub, Settings: map[string]string{"PATH": "/x"}}), peer.CodeBadRequest, nil},
		{"a value with ESC", initBody(t, peer.InitRequest{Name: "build", ParentID: servedParent, ControlKey: pub, Settings: map[string]string{"TTORCH_MODEL": "a\x1bb"}}), peer.CodeBadRequest, nil},
		{"an unknown field", `{"name":"build","parent_id":"` + servedParent + `","control_key":"x","command":"sh"}`, peer.CodeBadBody, nil},
		{"two objects", initBody(t, good) + initBody(t, good), peer.CodeBadBody, nil},
	}
	for _, c := range cases {
		h := newInitHome(t)
		h.extraEnv = c.env
		initRun(t, h, c.body).refused(t, c.label, c.code)
		for _, p := range []string{h.db(), filepath.Join(h.home, "peer.env"), filepath.Join(h.account, ".ssh")} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("%s: %s was created", c.label, p)
			}
		}
	}

	h := newInitHome(t)
	if err := os.WriteFile(filepath.Join(h.home, "peer.env"), []byte("TTORCH_MODEL=opus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(h.home, "peer.env"), 0o660); err != nil {
		t.Fatal(err)
	}
	r := initRun(t, h, initBody(t, good))
	r.refused(t, "a group-writable peer.env", peer.CodeUnavailable)
	if _, err := os.Lstat(filepath.Join(h.account, ".ssh")); !os.IsNotExist(err) {
		t.Error("init admitted the key for a peer whose channel refuses every verb")
	}

	if r := initRun(t, newInitHome(t), "", "extra"); r.code != 2 || !strings.Contains(r.stderr, "takes no arguments") {
		t.Errorf("peer init with an argument: exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestInitPath: a new peer.env's PATH is the channel's default, then the session's directories,
// minus any that are relative, missing, already listed, or writable by someone else.
func TestInitPath(t *testing.T) {
	u := peerUser{home: t.TempDir(), name: "p", ttorchHome: t.TempDir()}
	mk := func(mode os.FileMode) string {
		d := t.TempDir()
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		return d
	}
	tools, shared, group := mk(0o755), mk(0o777), mk(0o775)
	session := strings.Join([]string{
		filepath.Join(t.TempDir(), "absent"), shared, "relative/bin", tools, group, tools, "/usr/bin",
	}, string(os.PathListSeparator))
	want := defaultPeerPath(u) + string(os.PathListSeparator) + tools
	if got := initPath(u, session); got != want {
		t.Errorf("initPath =\n%s\nwant\n%s", got, want)
	}
}

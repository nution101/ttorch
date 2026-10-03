package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/peer"
)

// peerEnvSettings are the TTORCH_* variables peer.env may set: settings that tune how the fleet
// runs, none of which names a file or directory or a worker's identity. Every variable ttorch
// reads is either here or in peerEnvRefused (TestPeerEnvClassifiesEveryVariable).
var peerEnvSettings = []string{
	"TTORCH_BACKEND", "TTORCH_CODEGRAPH", "TTORCH_EFFORT", "TTORCH_GATE_TICK_BUDGET",
	"TTORCH_IDLE_NUDGE_GRACE", "TTORCH_LOAD_CEILING", "TTORCH_MANAGER_EFFORT",
	"TTORCH_MANAGER_MODEL", "TTORCH_MAX_ACTIVE_WORKERS", "TTORCH_MAX_CLAIMS_PER_TICK",
	"TTORCH_MAX_IDLE_NUDGES", "TTORCH_MAX_LAND_CONCURRENCY", "TTORCH_MAX_STALL_NUDGES",
	"TTORCH_MAX_WORKTREES", "TTORCH_MODEL", "TTORCH_NO_AUTOINIT", "TTORCH_NO_AUTOTRUST",
	"TTORCH_NO_GLOBAL_HOOKS", "TTORCH_NO_PROMPT_REMINDERS", "TTORCH_NO_STOP_REPORT",
	"TTORCH_PEER_POLL", "TTORCH_SCHEDULER_AUTOSTART", "TTORCH_SERIALIZE_OVERLAP", "TTORCH_SKIP_SKILL_INSTALL",
	"TTORCH_STALL_AFTER", "TTORCH_STALL_NUDGE_GRACE", "TTORCH_STALL_REPEAT",
	"TTORCH_STALL_RERAISES", "TTORCH_TERMINAL", "TTORCH_TMUX_SESSION",
	"TTORCH_VALIDATE_INFRA_RETRIES", "TTORCH_VALIDATE_RETRY_BACKOFF", "TTORCH_VALIDATE_TIMEOUT",
	"TTORCH_WORKER_CLONES",
}

// TestLeadSettings: peer add hands the peer only the lead's policy settings (parentSettings),
// whatever else the lead's shell sets, and nothing empty or unprintable.
func TestLeadSettings(t *testing.T) {
	for _, k := range parentSettings {
		t.Setenv(k, "")
	}
	t.Setenv("TTORCH_MODEL", "opus")
	t.Setenv("TTORCH_MANAGER_EFFORT", "medium")
	t.Setenv("TTORCH_EFFORT", "high\x1b[2J")
	for _, k := range []string{"TTORCH_TMUX_SESSION", "TTORCH_BACKEND", "TTORCH_NO_GLOBAL_HOOKS", "TTORCH_NO_AUTOTRUST", "TTORCH_SCHEDULER_AUTOSTART", "TTORCH_HOME", "TTORCH_TASK_ID"} {
		t.Setenv(k, "x")
	}
	got := leadSettings()
	if want := map[string]string{"TTORCH_MODEL": "opus", "TTORCH_MANAGER_EFFORT": "medium"}; !reflect.DeepEqual(got, want) {
		t.Errorf("leadSettings = %v, want %v", got, want)
	}
	for _, k := range parentSettings {
		if !peerEnvKey(k) || k == "PATH" {
			t.Errorf("parentSettings lists %s, which peer.env may not hold", k)
		}
	}
}

// TestPeerEnvRefusesEveryPathOverride: peer.env may not move anything ttorch reads or writes.
// Each path override is refused, the validate cache and claude's config file included.
func TestPeerEnvRefusesEveryPathOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer.env")
	for _, k := range []string{
		"TTORCH_HOME", "TTORCH_DB", "TTORCH_CLAUDE_DIR", "TTORCH_AGENTS_DIR", "TTORCH_BIN_DIR",
		"TTORCH_VALIDATE_CACHE_DIR", "TTORCH_CLAUDE_JSON",
	} {
		if err := os.WriteFile(path, []byte(k+"=/elsewhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := readPeerEnv(dir, os.Getuid()); err == nil {
			t.Errorf("peer.env setting %s was accepted: %v", k, got)
		}
	}
}

// TestPeerEnvClassifiesEveryVariable reads every TTORCH_* name in ttorch's own code and the
// content it installs, and requires each to be refused in peer.env or listed in peerEnvSettings.
// A new variable that names a path then fails here until someone decides which it is, instead of
// becoming settable from peer.env by default.
func TestPeerEnvClassifiesEveryVariable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	literal := regexp.MustCompile(`"(TTORCH_[A-Z0-9_]+)"`)
	bare := regexp.MustCompile(`TTORCH_[A-Z0-9_]+`)
	found := map[string]string{}
	walk := func(dir string, match func(path string) bool, re *regexp.Regexp, group int) {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !match(path) {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				if _, ok := found[m[group]]; !ok {
					found[m[group]], _ = filepath.Rel(root, path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	goSource := func(path string) bool {
		return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
	}
	for _, dir := range []string{"cmd", "internal"} {
		walk(dir, goSource, literal, 1)
	}
	if b, err := os.ReadFile(filepath.Join(root, "content.go")); err == nil {
		for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = "content.go"
		}
	}
	// The hooks and skills ttorch installs run inside the sessions ensure-up starts.
	walk("content", func(string) bool { return true }, bare, 0)
	if len(found) < 20 {
		t.Fatalf("found only %d TTORCH_ names; the scan is not reading the source", len(found))
	}

	settings := map[string]bool{}
	for _, k := range peerEnvSettings {
		settings[k] = true
	}
	names := make([]string, 0, len(found))
	for k := range found {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch {
		case peerEnvRefused[k] && settings[k]:
			t.Errorf("%s is both refused and listed as a setting", k)
		case peerEnvRefused[k]:
			if peerEnvKey(k) {
				t.Errorf("%s is in peerEnvRefused but peerEnvKey accepts it", k)
			}
		case settings[k]:
			if !peerEnvKey(k) {
				t.Errorf("%s is listed as a setting but peerEnvKey refuses it", k)
			}
		default:
			t.Errorf("%s (read in %s) is neither refused in peer.env nor listed in peerEnvSettings: decide whether it names a path or an identity", k, found[k])
		}
	}
	for _, k := range peerEnvSettings {
		if _, ok := found[k]; !ok {
			t.Errorf("peerEnvSettings lists %s, which nothing reads any more", k)
		}
	}
	for k := range peerEnvRefused {
		if _, ok := found[k]; !ok {
			t.Errorf("peerEnvRefused lists %s, which nothing reads any more", k)
		}
	}
}

// TestPeerEnvRefusesWhatAnotherAccountCouldWrite: peer.env decides the PATH and the settings of
// every process the control channel starts, so it is read only from a regular file the account
// owns that no one else can write, in a ttorch home with the same properties. A symlink is refused
// at either level (peer.env is opened with O_NOFOLLOW), and so is anything that is not a regular
// file. A missing ttorch home or peer.env sets nothing.
func TestPeerEnvRefusesWhatAnotherAccountCouldWrite(t *testing.T) {
	uid := os.Getuid()
	other := uid + 1
	good := func(t *testing.T) (dir, path string) {
		t.Helper()
		dir = t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, peerEnvFile)
		if err := os.WriteFile(path, []byte("TTORCH_MODEL=opus\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := readPeerEnv(dir, uid); err != nil || got["TTORCH_MODEL"] != "opus" {
			t.Fatalf("a private peer.env = %v, %v; want it read", got, err)
		}
		return dir, path
	}
	refused := func(t *testing.T, label, dir string, uid int, want string) {
		t.Helper()
		got, err := readPeerEnv(dir, uid)
		if err == nil {
			t.Errorf("%s: read %v, want it refused", label, got)
			return
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: refused with %q, want it to say %q", label, err, want)
		}
	}

	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		dir, path := good(t)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		refused(t, "peer.env mode "+mode.String(), dir, uid, "writable by")
	}

	dir, path := good(t)
	target := filepath.Join(t.TempDir(), "elsewhere.env")
	if err := os.WriteFile(target, []byte("TTORCH_MODEL=opus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	refused(t, "peer.env a symlink to a private file", dir, uid, "symbolic link")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	refused(t, "peer.env a dangling symlink", dir, uid, "symbolic link")

	dir, path = good(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		refused(t, "peer.env a fifo", dir, uid, "not a regular file")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading a fifo peer.env blocked")
	}

	dir, path = good(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	refused(t, "peer.env a directory", dir, uid, "not a regular file")

	// Another account's file. Making one needs root, so the owner the check expects is moved
	// instead: openPeerEnv is what readPeerEnv opens peer.env with, after the home has passed.
	dir, _ = good(t)
	home, err := peer.OpenPrivateDir(dir, uid, false)
	if err != nil {
		t.Fatal(err)
	}
	defer home.Close()
	if f, err := openPeerEnv(home, other); err == nil {
		f.Close()
		t.Error("a peer.env another account owns was opened")
	} else if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("a peer.env another account owns: %v, want it to say who owns it", err)
	}

	// The ttorch home: a symlink, writable by others, or another account's.
	dir, _ = good(t)
	link := filepath.Join(t.TempDir(), "ttorch-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	refused(t, "ttorch home a symlink", link, uid, "symbolic link")
	for _, mode := range []os.FileMode{0o770, 0o707, 0o777} {
		dir, _ := good(t)
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		refused(t, "ttorch home mode "+mode.String(), dir, uid, "writable by")
	}
	dir, _ = good(t)
	refused(t, "ttorch home another account's", dir, other, "owned by")

	// Nothing there sets nothing.
	if got, err := readPeerEnv(filepath.Join(t.TempDir(), "absent"), uid); err != nil || len(got) != 0 {
		t.Errorf("a missing ttorch home = %v, %v; want nothing set", got, err)
	}
	dir, path = good(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := readPeerEnv(dir, uid); err != nil || len(got) != 0 {
		t.Errorf("a missing peer.env = %v, %v; want nothing set", got, err)
	}
}

// TestPeerServeRefusesAnUnsafePeerEnv: through the binary, a peer.env that is a symlink or that
// another account could write refuses every verb as unavailable, before the store is opened.
func TestPeerServeRefusesAnUnsafePeerEnv(t *testing.T) {
	h := newServeHome(t)
	path := filepath.Join(h.home, peerEnvFile)
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	s := serveRun(t, h, "version", "")
	s.refused(t, "a group-writable peer.env", peer.CodeUnavailable)
	if s.resp.Error != nil && !strings.Contains(s.resp.Error.Message, "writable by") {
		t.Errorf("the refusal %q does not say why", s.resp.Error.Message)
	}

	h = newServeHome(t)
	path = filepath.Join(h.home, peerEnvFile)
	target := filepath.Join(t.TempDir(), "peer.env")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	serveRun(t, h, "summary", "").refused(t, "a symlinked peer.env", peer.CodeUnavailable)
	serveRun(t, h, "goal", `{"request_id":"g1","text":"x"}`).refused(t, "a symlinked peer.env", peer.CodeUnavailable)
	if evs := managerEventsIn(t, h.store(t)); len(evs) != 0 {
		t.Errorf("a refused goal appended %d events", len(evs))
	}
}

// TestPeerAccountUnderTestIsNeverTheReal: in a test binary the peer account comes only from the
// seam. With the seam unset, looking it up fails rather than falling back to the user database,
// whose answer is the real home with the real ~/.ssh and ~/.ttorch.
func TestPeerAccountUnderTestIsNeverTheReal(t *testing.T) {
	// This process started without the seam (only the processes a test starts get one).
	if u, err := peerAccount(); err == nil {
		t.Fatalf("peerAccount() with no seam = %+v; want an error, never the real account", u)
	}
	for _, half := range [][2]string{{"", t.TempDir()}, {t.TempDir(), ""}} {
		if u, err := testPeerAccount(half[0], half[1])(); err == nil {
			t.Errorf("half a seam %q gave %+v; want an error", half, u)
		}
	}
	home, ttorch := t.TempDir(), t.TempDir()
	if u, err := testPeerAccount(home, ttorch)(); err != nil || u.home != home || u.ttorchHome != ttorch {
		t.Errorf("the seam = %+v, %v", u, err)
	}
}

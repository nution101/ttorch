package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// fakeTmuxScript is tmux as far as ensure-up can tell. It keeps the session and its windows in
// files under the directory it is written to, so a second restore sees the windows the first one
// created, and logs every invocation, one line each, to calls.
const fakeTmuxScript = `#!/bin/sh
D=$(dirname "$0")/state
mkdir -p "$D"
echo "$*" >> "$D/calls"
case "$1" in
-V) echo 'tmux 3.5a' ;;
has-session) [ -f "$D/session" ] || exit 1 ;;
new-session) : > "$D/session" ;;
list-windows)
	[ -f "$D/session" ] || { echo "can't find session" >&2; exit 1; }
	[ -f "$D/windows" ] || exit 0
	case "$5" in
	*window_id*) n=0; while read -r w; do n=$((n+1)); echo "@$n $w"; done < "$D/windows" ;;
	*) cat "$D/windows" ;;
	esac ;;
new-window)
	while [ $# -gt 0 ]; do
		[ "$1" = -n ] && echo "$2" >> "$D/windows"
		shift
	done ;;
display-message) echo 0 ;;
esac
exit 0
`

// fakeFleet is what ensure-up starts on a peer, faked: tmux (fakeTmuxScript) first on the peer's
// PATH, and the installed binary the scheduler start forks (paths.Binary under the ttorch home),
// which runs daemon.
type fakeFleet struct{ bin string }

// installFakeFleet writes the fakes and points the served home's peer.env at them, with a tmux
// session name of the test's own and the scheduler auto-start left on.
func installFakeFleet(t *testing.T, h serveHome, daemon string) fakeFleet {
	t.Helper()
	f := fakeFleet{bin: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.bin, "tmux"), []byte(fakeTmuxScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, "bin", "ttorch"), []byte(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	h.writePeerEnv(t, "PATH="+f.bin+":/usr/bin:/bin", "TTORCH_TMUX_SESSION=ttorch-peer-mgr-"+filepath.Base(h.account))
	return f
}

// lines reads one of the fake tmux's state files, one entry per line; a file it never wrote
// reads as none.
func (f fakeFleet) lines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.bin, "state", name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// waitForFile waits for path to exist, for a process started in the background to write it.
func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened (%s was not written)", what, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPeerServeEnsureUpChecksTheParent: ensure-up restarts the peer's manager, workers and
// scheduler, so it takes the same parent check as task-add, goal and answer. Through a key whose
// forced command has no --parent, or one bound to a parent that no longer has this peer (it was
// adopted away), it is refused with wrong_parent before anything starts: neither tmux nor the
// installed binary the scheduler start forks is ever run. Through the parent's own key the same
// request restores the manager and starts the scheduler, which shows the fakes record a start.
func TestPeerServeEnsureUpChecksTheParent(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	launched := filepath.Join(t.TempDir(), "launched")
	fleet := installFakeFleet(t, h, "#!/bin/sh\necho \"$*\" >> '"+launched+"'\n")
	if err := h.store(t).SetManager(ctx, db.Manager{Dir: h.dir, SessionID: "sid-1"}); err != nil {
		t.Fatal(err)
	}

	unbound := h
	unbound.keyParent = ""
	serveRun(t, unbound, "ensure-up", "").refused(t, "ensure-up through a key without --parent", peer.CodeWrongParent)
	displaced := h
	displaced.keyParent = strings.Repeat("e", 32)
	s := serveRun(t, displaced, "ensure-up", "")
	s.refused(t, "ensure-up through a displaced parent's key", peer.CodeWrongParent)
	if s.resp.Error != nil && !strings.Contains(s.resp.Error.Message, servedParent) {
		t.Errorf("the refusal %q does not name this peer's parent", s.resp.Error.Message)
	}
	if calls := fleet.lines(t, "calls"); len(calls) != 0 {
		t.Errorf("a refused ensure-up ran tmux %d time(s): %q", len(calls), calls)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Errorf("a refused ensure-up launched the scheduler (%v)", err)
	}

	var res peer.EnsureUpResult
	serveRun(t, h, "ensure-up", "").result(t, &res)
	if res.Scheduler != "started" || len(res.Restored) == 0 || res.Restored[0] != "restored manager" {
		t.Fatalf("ensure-up through the parent's key = %+v, want the manager restored and the scheduler started", res)
	}
	waitForFile(t, launched, "the scheduler launch")
	if w := fleet.lines(t, "windows"); len(w) != 1 || w[0] != "manager" {
		t.Errorf("windows after ensure-up = %q, want the manager's", w)
	}
}

// TestPeerAddRefusesOnAPeer: a peer starts no peers of its own (design 3.3), so peer add and peer
// adopt on a coordinator whose row says peer are refused before anything happens: no control key
// is generated, no peer is registered, and ssh never runs. It is the same command, from the same
// terminal, that provisions a peer from a root.
func TestPeerAddRefusesOnAPeer(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t)
	s, err := db.Open(filepath.Join(f.parentHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProvisionAsPeer(ctx, "mid", strings.Repeat("a", 32), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"peer", "add", "build", "ttorch@build-host"},
		{"peer", "adopt", "build", "ttorch@build-host", "--force"},
	} {
		r := f.run(t, nil, args...)
		if r.code == 0 || !strings.Contains(r.stderr, "this coordinator is a peer") {
			t.Errorf("%v on a peer: exit %d, stderr %q; want it refused because this coordinator is a peer", args, r.code, r.stderr)
		}
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("a refused add ran ssh %d time(s): %+v", len(calls), calls)
	}
	if _, err := os.Lstat(filepath.Join(f.parentHome, "peers")); !os.IsNotExist(err) {
		t.Errorf("a refused add made a control key directory (%v)", err)
	}
	if peers, err := f.parentStore(t).ListPeers(ctx); err != nil || len(peers) != 0 {
		t.Errorf("a refused add registered %d peer(s) (%v)", len(peers), err)
	}
}

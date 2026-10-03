package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/backend/backendtest"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/paths"
)

// charterText is what a harness charter writer puts in a file.
func charterText(t *testing.T, write func(string) error) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "charter.md")
	if err := write(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// restoreManager records a manager on a fresh store, provisioned as a peer when peer is set, and
// runs restore with the manager window absent, against home. It returns the notes and the
// launch lines typed into the manager window.
func restoreManager(t *testing.T, home string, peer bool) ([]string, []string) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if peer {
		if _, err := store.ProvisionAsPeer(ctx, "build", strings.Repeat("a", 32), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetManager(ctx, db.Manager{Dir: t.TempDir(), SessionID: "sid-1"}); err != nil {
		t.Fatal(err)
	}
	const sess = "charter-sess"
	fake := backendtest.New(sess)
	m := &Manager{P: paths.Paths{Home: home}, Session: sess, Store: store, Backend: fake}
	notes := m.restore()
	var launches []string
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "SendLine("+sess+", manager, ") {
			launches = append(launches, c)
		}
	}
	return notes, launches
}

// TestRestoreLaunchesAPeersManagerUnderThePeerCharter: the charter a manager launches with
// follows the coordinator row. A peer's manager, restored as ensure-up restores it, is launched
// with the charter file holding the peer charter; a root's with the manager charter, as before.
func TestRestoreLaunchesAPeersManagerUnderThePeerCharter(t *testing.T) {
	for _, c := range []struct {
		label string
		peer  bool
		want  string
	}{
		{"peer", true, charterText(t, harness.WritePeerManagerCharter)},
		{"root", false, charterText(t, harness.WriteManagerCharter)},
	} {
		home := t.TempDir()
		notes, launches := restoreManager(t, home, c.peer)
		file := filepath.Join(home, "manager-charter.md")
		if len(launches) != 1 || !strings.Contains(launches[0], "--append-system-prompt-file '"+file+"'") {
			t.Errorf("%s: launch lines %q (notes %q), want one launch passing %s", c.label, launches, notes, file)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		if string(b) != c.want {
			t.Errorf("%s: the manager launched with charter %.80q..., want %.80q...", c.label, b, c.want)
		}
	}
}

// TestRestoreRefusesAPeersManagerWithoutThePeerCharter: the peer charter is passed only as a
// file. When it cannot be written, a peer's manager is not launched at all, and the restore says
// why, rather than starting it under the inline manager charter, which would have it wait in a
// tab nobody reads. A root in the same spot still launches with the inline manager charter.
func TestRestoreRefusesAPeersManagerWithoutThePeerCharter(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(blocker, "home") // a file's child: nothing can be written under it

	notes, launches := restoreManager(t, home, true)
	if len(launches) != 0 {
		t.Errorf("a peer's manager was launched without the peer charter: %q", launches)
	}
	if len(notes) == 0 || !strings.HasPrefix(notes[0], "skipped manager (") || !strings.Contains(notes[0], "peer manager charter") {
		t.Errorf("notes = %q, want the manager skipped for want of the peer charter", notes)
	}

	_, launches = restoreManager(t, home, false)
	if len(launches) != 1 || !strings.Contains(launches[0], "--append-system-prompt 'You are the ttorch MANAGER for this tmux session") {
		t.Errorf("a root's launch with no charter file = %q, want the inline manager charter", launches)
	}
}

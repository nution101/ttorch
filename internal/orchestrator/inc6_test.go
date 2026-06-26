package orchestrator

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot walks up from this test file to the directory holding go.mod — the
// repository root — so the source-scanning invariants below cover the whole tree
// regardless of the working directory the test runs in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", file)
		}
		dir = parent
	}
}

// goSourceFiles returns every non-test .go file under root.
func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// sendsToManagerWindow reports whether a source line is a tmux.SendLine/SendKey call
// that addresses the manager window — either by the "manager" string literal (how the
// orchestrator's launch sites name it) or by an identifier like managerWindow (how the
// retired supervisor poke named it), so a re-introduced poke under either spelling is
// caught. The manager→worker `ttorch send` path addresses a *worker* window (t.Window),
// never the "manager" literal, so it is not matched here.
func sendsToManagerWindow(line string) bool {
	if !strings.Contains(line, "tmux.SendLine(") && !strings.Contains(line, "tmux.SendKey(") {
		return false
	}
	return strings.Contains(line, `"manager"`) || strings.Contains(strings.ToLower(line), "managerwindow")
}

// isManagerLaunch reports whether a manager-window send carries a manager
// launch/resume command (harness.Manager…) — the bootstrap that types the `claude …`
// startup command into the freshly created manager window's shell to *create* the
// session. That is categorically distinct from injecting a directive into a running
// manager (the retired poke).
func isManagerLaunch(line string) bool {
	return strings.Contains(line, "harness.Manager")
}

// TestNoInjectionIntoManagerSession is the increment-6 net invariant: after retiring
// the supervisor, NO code path types into the manager session. The supervisor's poke
// (tmux.SendLine into the "manager" window carrying a directive) was the only such
// path; with it gone, the ONLY remaining tmux.SendLine/SendKey calls that target the
// manager window are the manager launch/resume bootstrap, which start the session
// rather than inject into a running one. This scans all non-test Go source and fails
// if any send to the manager window carries anything other than a harness.Manager…
// launch command.
//
// The manager→worker `ttorch send` path (SendLine into a WORKER window) is unaffected:
// its window argument is never the "manager" literal, so it is never matched.
func TestNoInjectionIntoManagerSession(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	for _, path := range goSourceFiles(t, root) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !sendsToManagerWindow(line) || isManagerLaunch(line) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("forbidden injection into the manager session — tmux.SendLine/SendKey to the manager "+
			"window carrying something other than a harness.Manager… launch command:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestManagerInjectionDetector pins the matcher above so the invariant test cannot pass
// vacuously: it must FLAG a poke (under either window spelling) and PASS both a manager
// launch and a worker send.
func TestManagerInjectionDetector(t *testing.T) {
	cases := []struct {
		line      string
		injection bool // want: flagged as injection into the manager session
	}{
		{`tmux.SendLine(s.Session, managerWindow, pokeDirective)`, true},
		{`_ = tmux.SendLine(m.Session, "manager", "ttorch wake: drain and advance")`, true},
		{`tmux.SendKey(m.Session, "manager", "Enter")`, true},
		{`_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(harness.Resolve(), sid, m.charterFile()))`, false},
		{`_ = tmux.SendLine(m.Session, "manager", harness.ManagerResumeOrFresh(h, mgr.SessionID, m.charterFile()))`, false},
		{`return tmux.SendLine(m.Session, t.Window, text)`, false},               // manager→worker send
		{`if err := tmux.SendLine(m.Session, window, cmd); err != nil {`, false}, // worker launch
	}
	for _, c := range cases {
		flagged := sendsToManagerWindow(c.line) && !isManagerLaunch(c.line)
		if flagged != c.injection {
			t.Errorf("detector(%q) = %v, want %v", c.line, flagged, c.injection)
		}
	}
}

// TestSupervisorAndWakeRetired proves the increment-6 deletions: the supervisor daemon
// and the wake-queue packages are gone, nothing imports them, spawn starts no daemon,
// and no source revives the daemon-start path or the retired `ttorch daemon` verb.
func TestSupervisorAndWakeRetired(t *testing.T) {
	root := moduleRoot(t)

	for _, dir := range []string{"internal/supervisor", "internal/wake"} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Errorf("%s must be deleted entirely (stat err = %v)", dir, err)
		}
	}

	// Tokens that, if present in production source, would mean the retired machinery
	// (or a path that starts the daemon) is still wired up. The detached daemon was
	// launched via `<binary> daemon run`, so that exact verb must be gone too.
	forbidden := []string{
		"internal/supervisor",
		"internal/wake",
		"ensureSupervisor",
		"supervisor.Start",
		"daemon run",
	}
	for _, path := range goSourceFiles(t, root) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		for _, tok := range forbidden {
			if strings.Contains(src, tok) {
				t.Errorf("%s still references retired machinery %q", rel, tok)
			}
		}
	}
}

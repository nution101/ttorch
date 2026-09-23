package orchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/backend"
	"github.com/nution101/ttorch/internal/backend/backendtest"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/worktree"
)

// forbidTmux puts a tmux on PATH that records each invocation and fails it, and returns
// a func reporting what was recorded. A test that expects its caller to reach the
// session only through a Backend asserts the record is empty: any direct tmux call left
// behind would show up here, rather than quietly succeeding against a real server.
func forbidTmux(t *testing.T) func() string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
}

// TestWorkerLifecycle_GoesThroughBackend proves spawn, send, peek, state and teardown
// reach the worker's session through Manager.Backend and nothing else. The Manager is
// given a recording fake and a tmux that fails every command, so the spawn succeeds only
// if every session call went to the fake. The expected call lists pin the order as well:
// the pre-flight window check, then the session, the window, the launch, and the
// readiness poll, exactly as the tmux calls ran before the seam.
func TestWorkerLifecycle_GoesThroughBackend(t *testing.T) {
	tmuxCalls := forbidTmux(t)
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	t.Setenv("TTORCH_NO_AUTOTRUST", "1")
	repo := newRepoMain(t)

	p := paths.Default()
	store, err := db.Open(p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const sess = "fake-sess"
	fake := backendtest.New(sess)
	m := &Manager{P: p, Session: sess, Store: store, Pool: worktree.Pool{Root: p.Worktrees(), Max: 2}, Backend: fake}

	task, err := m.Spawn("f1", repo, false, "sleep 30")
	if err != nil {
		t.Fatalf("Spawn: %v\ntmux was run directly:\n%s", err, tmuxCalls())
	}
	seen := 0
	step := func(what string, want ...string) {
		t.Helper()
		calls := fake.Calls()
		if got := calls[seen:]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: backend calls =\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		seen = len(calls)
	}
	step("Spawn",
		"Available()",
		"WindowExists(fake-sess, wk-f1)",
		"EnsureSession(fake-sess)",
		"NewWindow(fake-sess, wk-f1, "+task.Worktree+", f1)",
		"SendLine(fake-sess, wk-f1, sleep 30)",
		"WindowExistsErr(fake-sess, wk-f1)",
		"PaneCurrentCommand(fake-sess, wk-f1)",
	)

	if err := m.Send("f1", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	step("Send", "WindowExists(fake-sess, wk-f1)", "SendLine(fake-sess, wk-f1, hello)")

	out, err := m.Peek("f1", 20)
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("Peek = (%q, %v), want the pane the fake holds, which includes the sent line", out, err)
	}
	step("Peek", "CapturePane(fake-sess, wk-f1, 20)")

	if !m.Live(task) {
		t.Fatal("Live = false for a window the backend reports present")
	}
	step("Live", "WindowExists(fake-sess, wk-f1)")

	if st := m.TaskState(task); st != "idle" {
		t.Fatalf("TaskState = %q, want idle", st)
	}
	step("TaskState", "WindowExists(fake-sess, wk-f1)", "CapturePane(fake-sess, wk-f1, 6)")

	if _, err := m.Teardown("f1", false); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	step("Teardown", "PanePID(fake-sess, wk-f1)", "KillWindow(fake-sess, wk-f1)")
	if fake.HasWindow(sess, "wk-f1") {
		t.Fatal("Teardown left the worker's window in the backend")
	}

	if got := tmuxCalls(); got != "" {
		t.Fatalf("the worker lifecycle ran tmux directly instead of through the backend:\n%s", got)
	}
}

// TestNew_SelectsBackendFromEnv covers the startup hook: unset and "tmux" give the tmux
// backend, and an unknown TTORCH_BACKEND is refused before the state store is opened, so
// a typo leaves no state behind.
func TestNew_SelectsBackendFromEnv(t *testing.T) {
	for _, v := range []string{"", "tmux"} {
		t.Setenv("TTORCH_HOME", t.TempDir())
		t.Setenv(backend.Env, v)
		m, err := New(paths.Default())
		if err != nil {
			t.Fatalf("%s=%q: New: %v", backend.Env, v, err)
		}
		if _, ok := m.Backend.(backend.Tmux); !ok {
			t.Errorf("%s=%q: Backend = %T, want backend.Tmux", backend.Env, v, m.Backend)
		}
		_ = m.Close()
	}

	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	t.Setenv(backend.Env, "bogus")
	m, err := New(paths.Default())
	if err == nil {
		_ = m.Close()
		t.Fatalf("%s=bogus: New succeeded with %T, want it refused", backend.Env, m.Backend)
	}
	if !strings.Contains(err.Error(), `TTORCH_BACKEND="bogus"`) {
		t.Fatalf("%s=bogus: error = %q, want it to name the variable and the value", backend.Env, err)
	}
	if _, serr := os.Stat(paths.Default().StateDB()); !os.IsNotExist(serr) {
		t.Fatalf("a refused backend still opened the state store at %s (stat err = %v)", paths.Default().StateDB(), serr)
	}
}

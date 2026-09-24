package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/backend/backendtest"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
)

// forbidTmux puts a tmux on PATH that records each invocation and fails it, and returns
// a func reporting what was recorded, so a test can require that nothing reached tmux
// except through the Backend it supplied.
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

// TestLiveness_GoesThroughBackend proves the production seams New wires read and steer
// worker windows through the Backend it is given. Unlike newWatcher, it keeps New's own
// capture, nudge, manager-presence and session-token seams, hands New a recording fake,
// and puts a failing tmux on PATH: a window the fake reports absent must come out as
// window_gone, which only happens if the liveness sweep asked the fake rather than tmux.
func TestLiveness_GoesThroughBackend(t *testing.T) {
	tmuxCalls := forbidTmux(t)
	home := t.TempDir()
	t.Setenv("TTORCH_HOME", home)
	t.Setenv("TTORCH_DB", filepath.Join(home, "state.db"))
	s, err := db.Open(paths.Default().StateDB())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const sess = "watch-sess"
	fake := backendtest.New(sess)
	fake.AddWindow(sess, "wk-live", "$ idle at the prompt")
	fake.AddWindow(sess, managerWindow, "")
	fake.SetPanePID(sess, managerWindow, os.Getpid())
	seedActiveTask(t, s, "gone", "wk-gone")
	seedActiveTask(t, s, "live", "wk-live")

	w := New(s, paths.Default(), fake, sess)
	w.Stale = -1 // inspect every active task on the first sweep

	ctx := context.Background()
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	if gone, err := s.HasEventType(ctx, "gone", db.EventWindowGone); err != nil || !gone {
		t.Fatalf("window_gone for the window the backend reports absent = (%v, %v), want true\nbackend calls: %v\ntmux was run directly:\n%s",
			gone, err, fake.Calls(), tmuxCalls())
	}
	if live, err := s.HasEventType(ctx, "live", db.EventWindowGone); err != nil || live {
		t.Fatalf("window_gone for the window the backend reports present = (%v, %v), want false", live, err)
	}

	if !w.managerPresent() {
		t.Fatal("managerPresent = false with the manager window present in the backend")
	}
	if err := w.nudge("wk-live"); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if got, want := w.token(), instanceToken(os.Getpid(), processStartTime(os.Getpid())); got == "" || got != want {
		t.Fatalf("session token = %q, want %q, built from the manager pane pid the backend reports", got, want)
	}

	calls := fake.Calls()
	for _, want := range []string{
		"WindowExistsErr(watch-sess, wk-gone)",
		"WindowExistsErr(watch-sess, wk-live)",
		"CapturePane(watch-sess, wk-live, 6)",
		"WindowExists(watch-sess, manager)",
		"SendLine(watch-sess, wk-live, continue)",
		"PanePID(watch-sess, manager)",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("backend was never asked %s\ncalls:\n  %s", want, strings.Join(calls, "\n  "))
		}
	}
	if slices.Contains(calls, "CapturePane(watch-sess, wk-gone, 6)") {
		t.Error("captured the pane of a window the backend had just reported absent")
	}
	if got := tmuxCalls(); got != "" {
		t.Fatalf("the watcher ran tmux directly instead of through the backend:\n%s", got)
	}
}

// TestWake_GoesThroughBackend proves the manager-window seams NewDaemon wires reach the
// manager through the Backend it is given, with a failing tmux on PATH that must record
// nothing: the foreground check reads the pane pid, the wake is typed with TypeLine, and Enter
// is pressed with SendKey only after a fresh capture shows exactly the wake at an idle prompt.
// With the lead's draft in the input instead, the capture happens and no key is sent.
func TestWake_GoesThroughBackend(t *testing.T) {
	tmuxCalls := forbidTmux(t)
	w, s, _, _ := newWatcher(t)
	const sess = "wake-sess"
	fake := backendtest.New(sess)
	fake.AddWindow(sess, managerWindow, inputPane(wakeLine))
	fake.SetPanePID(sess, managerWindow, os.Getpid())
	d := NewDaemon(s, w.P, fake, sess, nil)

	if err := d.pressEnter(); err != nil {
		t.Fatalf("pressEnter with exactly the wake at an idle prompt: %v\nbackend calls: %v\ntmux was run directly:\n%s", err, fake.Calls(), tmuxCalls())
	}
	if err := d.typeWake(); err != nil {
		t.Fatalf("typeWake: %v\ntmux was run directly:\n%s", err, tmuxCalls())
	}
	d.managerForeground()

	capture := fmt.Sprintf("CapturePane(%s, %s, %d)", sess, managerWindow, captureLines)
	enter := fmt.Sprintf("SendKey(%s, %s, Enter)", sess, managerWindow)
	calls := fake.Calls()
	for _, want := range []string{
		capture,
		enter,
		fmt.Sprintf("TypeLine(%s, %s, %s)", sess, managerWindow, wakeLine),
		fmt.Sprintf("PanePID(%s, %s)", sess, managerWindow),
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("backend was never asked %s\ncalls:\n  %s", want, strings.Join(calls, "\n  "))
		}
	}
	if slices.Index(calls, capture) > slices.Index(calls, enter) {
		t.Errorf("Enter was pressed before the pane was captured:\n  %s", strings.Join(calls, "\n  "))
	}

	draft := backendtest.New(sess)
	draft.AddWindow(sess, managerWindow, inputPane("can you check the"))
	if err := NewDaemon(s, w.P, draft, sess, nil).pressEnter(); err == nil {
		t.Error("pressEnter with the lead's draft in the input returned nil, want a refusal")
	}
	if got := draft.Calls(); !slices.Contains(got, capture) || slices.Contains(got, enter) {
		t.Errorf("with a draft: calls = %v, want a capture and no Enter", got)
	}
	if got := tmuxCalls(); got != "" {
		t.Fatalf("the wake ran tmux directly instead of through the backend:\n%s", got)
	}
}

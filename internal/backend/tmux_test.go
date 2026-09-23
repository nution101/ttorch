package backend

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/tmux"
)

// stubTmux puts a tmux on PATH that appends each invocation's arguments to a log and
// answers the handful of queries the adapter makes, and returns a func that reads and
// clears the log. Session "absent" does not exist, session "broken" fails list-windows,
// window "missing" cannot be targeted, and anything addressed to session "hang" never
// answers.
func stubTmux(t *testing.T) func() string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_TMUX_LOG"
case "$*" in *" hang"*) exec sleep 5 ;; esac
case "$1" in
  has-session) [ "$3" = absent ] && exit 1 ;;
  list-windows)
    [ "$3" = broken ] && { echo 'list-windows failed' >&2; exit 1; }
    case "$*" in *window_id*) printf '@1 manager\n@2 wk-a\n' ;; *) printf 'manager\nwk-a\n' ;; esac ;;
  display-message)
    case "$*" in *pane_in_mode*) echo 0 ;; *pane_current_command*) echo sleep ;; esac ;;
  list-panes) echo 4242 ;;
  capture-pane|kill-window)
    case "$*" in *:missing*) echo "can't find window: missing" >&2; exit 1 ;; esac
    [ "$1" = capture-pane ] && echo 'pane text' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_TMUX_LOG", log)
	return func() string {
		b, _ := os.ReadFile(log)
		_ = os.Remove(log)
		return string(b)
	}
}

// results flattens return values so two calls can be compared: an error becomes its text,
// because two runs of the same failing command return equal messages but distinct values.
func results(vals ...any) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		if err, ok := v.(error); ok {
			out[i] = "error: " + err.Error()
			continue
		}
		out[i] = v
	}
	return out
}

// TestTmux_SameCommandsAsDirectCalls is the no-behaviour-change check for the adapter.
// For every Backend method it runs the adapter and then the package tmux call(s) the
// orchestrator and watcher made before the seam existed, against a stub tmux that logs
// its arguments, and requires the two to run the same tmux command lines in the same
// order and return the same values, errors included.
func TestTmux_SameCommandsAsDirectCalls(t *testing.T) {
	calls := stubTmux(t)
	t.Setenv("TTORCH_TMUX_SESSION", "parity")
	t.Setenv("TMUX", "/tmp/stub,1,0") // Attach switches the client rather than exec'ing tmux
	var b Backend = Tmux{}

	cases := []struct {
		name    string
		seam    func() []any
		direct  func() []any
		runsCmd bool // false for the two methods that answer without running tmux
	}{
		{"SessionName", func() []any { return results(b.SessionName()) }, func() []any { return results(tmux.SessionName()) }, false},
		{"Available", func() []any { return results(b.Available()) }, func() []any { return results(tmux.Available()) }, false},
		{"EnsureSession existing", func() []any { return results(b.EnsureSession("s")) }, func() []any { return results(tmux.EnsureSession("s")) }, true},
		{"EnsureSession absent", func() []any { return results(b.EnsureSession("absent")) }, func() []any { return results(tmux.EnsureSession("absent")) }, true},
		{"HasSession", func() []any { return results(b.HasSession("s")) }, func() []any { return results(tmux.HasSession("s")) }, true},
		{"HasSession absent", func() []any { return results(b.HasSession("absent")) }, func() []any { return results(tmux.HasSession("absent")) }, true},
		{"KillSession", func() []any { return results(b.KillSession("s")) }, func() []any { return results(tmux.KillSession("s")) }, true},
		{"ListWindows", func() []any { return results(b.ListWindows("s")) }, func() []any { return results(tmux.ListWindows("s")) }, true},
		{"ListWindows broken", func() []any { return results(b.ListWindows("broken")) }, func() []any { return results(tmux.ListWindows("broken")) }, true},
		{"NewWindow", func() []any { return results(b.NewWindow("s", "wk-a", "/work", "a")) }, func() []any {
			err := tmux.NewWindow("s", "wk-a", "/work")
			if err == nil {
				_ = tmux.LabelWindow("s", "wk-a", "a")
			}
			return results(err)
		}, true},
		{"WindowExists", func() []any { return results(b.WindowExists("s", "wk-a")) }, func() []any { return results(tmux.WindowExists("s", "wk-a")) }, true},
		{"WindowExists absent", func() []any { return results(b.WindowExists("s", "wk-z")) }, func() []any { return results(tmux.WindowExists("s", "wk-z")) }, true},
		{"WindowExists broken", func() []any { return results(b.WindowExists("broken", "wk-a")) }, func() []any { return results(tmux.WindowExists("broken", "wk-a")) }, true},
		{"WindowExistsErr", func() []any { return results(b.WindowExistsErr("s", "wk-a")) }, func() []any { return results(tmux.WindowExistsErr("s", "wk-a")) }, true},
		{"WindowExistsErr broken", func() []any { return results(b.WindowExistsErr("broken", "wk-a")) }, func() []any { return results(tmux.WindowExistsErr("broken", "wk-a")) }, true},
		{"SendLine", func() []any { return results(b.SendLine("s", "wk-a", "hello")) }, func() []any { return results(tmux.SendLine("s", "wk-a", "hello")) }, true},
		{"CapturePane", func() []any { return results(b.CapturePane("s", "wk-a", 6)) }, func() []any { return results(tmux.CapturePane("s", "wk-a", 6)) }, true},
		{"CapturePane missing", func() []any { return results(b.CapturePane("s", "missing", 6)) }, func() []any { return results(tmux.CapturePane("s", "missing", 6)) }, true},
		{"PaneCurrentCommand", func() []any { return results(b.PaneCurrentCommand("s", "wk-a")) }, func() []any { return results(tmux.PaneCurrentCommand("s", "wk-a")) }, true},
		{"PanePID", func() []any { return results(b.PanePID("s", "wk-a")) }, func() []any { return results(tmux.PanePID("s", "wk-a")) }, true},
		{"KillWindow", func() []any { return results(b.KillWindow("s", "wk-a")) }, func() []any { return results(tmux.KillWindow("s", "wk-a")) }, true},
		{"KillWindow missing", func() []any { return results(b.KillWindow("s", "missing")) }, func() []any { return results(tmux.KillWindow("s", "missing")) }, true},
		{"Attach", func() []any { return results(b.Attach("s", "manager")) }, func() []any { return results(tmux.Attach("s", "manager")) }, true},
	}
	for _, c := range cases {
		calls()
		gotRes := c.seam()
		gotCmds := calls()
		wantRes := c.direct()
		wantCmds := calls()
		if gotCmds != wantCmds {
			t.Errorf("%s: tmux commands through the backend differ from the direct call\nbackend:\n%s\ndirect:\n%s", c.name, gotCmds, wantCmds)
		}
		if c.runsCmd && gotCmds == "" {
			t.Errorf("%s: ran no tmux command, so the comparison proves nothing", c.name)
		}
		if !reflect.DeepEqual(gotRes, wantRes) {
			t.Errorf("%s: backend returned %v, direct call returned %v", c.name, gotRes, wantRes)
		}
	}
}

// TestTmux_TimeoutKeepsItsMeaning checks the one error the orchestrator branches on. A
// tmux that never answers must come back through the backend as an error matching
// ErrTimeout from WindowExistsErr, and as "present" from WindowExists, because the gate
// and Resume rely on that fold to avoid starting a second agent in an occupied worktree.
func TestTmux_TimeoutKeepsItsMeaning(t *testing.T) {
	stubTmux(t)
	t.Cleanup(tmux.SetRunTimeoutForTest(200 * time.Millisecond))
	var b Backend = Tmux{}

	exists, err := b.WindowExistsErr("hang", "wk-a")
	if exists || !errors.Is(err, ErrTimeout) {
		t.Fatalf("WindowExistsErr against a hung tmux = (%v, %v), want (false, an error matching ErrTimeout)", exists, err)
	}
	if !strings.Contains(err.Error(), "no answer after") {
		t.Fatalf("timeout error lost the tmux package's message: %v", err)
	}
	if !b.WindowExists("hang", "wk-a") {
		t.Fatal("WindowExists against a hung tmux = false, want true (an unanswered read is not a gone window)")
	}
}

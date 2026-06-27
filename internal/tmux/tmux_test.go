package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeTmuxScript stands in for the real tmux binary. It records each
// invocation's argv to $TTORCH_FAKE_LOG (args tab-separated, one invocation per
// line) and emits canned stdout / exit codes driven by FAKE_* env vars, so the
// package's command construction and output parsing can be asserted
// deterministically without a real tmux installed. Arguments are assumed
// tab-free, which holds for every argv ttorch hands tmux here.
const fakeTmuxScript = `#!/bin/sh
: "${TTORCH_FAKE_LOG:=/dev/null}"
{
  first=1
  for a in "$@"; do
    if [ "$first" = 1 ]; then
      printf '%s' "$a"
      first=0
    else
      printf '\t%s' "$a"
    fi
  done
  printf '\n'
} >>"$TTORCH_FAKE_LOG"

case "$1" in
has-session)     exit "${FAKE_HAS_SESSION_EXIT:-0}" ;;
list-windows)    printf '%s' "$FAKE_LIST_WINDOWS"; exit "${FAKE_LIST_WINDOWS_EXIT:-0}" ;;
display-message) printf '%s' "$FAKE_DISPLAY";      exit "${FAKE_DISPLAY_EXIT:-0}" ;;
list-panes)      printf '%s' "$FAKE_LIST_PANES";   exit "${FAKE_LIST_PANES_EXIT:-0}" ;;
capture-pane)    printf '%s' "$FAKE_CAPTURE";      exit "${FAKE_CAPTURE_EXIT:-0}" ;;
*)               exit "${FAKE_GENERIC_EXIT:-0}" ;;
esac
`

// installFakeTmux writes the fake tmux onto a temp dir, prepends it to PATH, and
// points the invocation log at a fresh file. It returns the log path. Because it
// uses t.Setenv the changes are scoped to (and undone after) the calling test,
// so integration tests in other functions still see the real environment.
func installFakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	if err := os.WriteFile(script, []byte(fakeTmuxScript), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "invocations.log")
	t.Setenv("TTORCH_FAKE_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// readInvocations returns the argv of every fake-tmux call, in order.
func readInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, "\t"))
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// --- pure functions (no exec) ---

func TestSessionName(t *testing.T) {
	t.Setenv("TTORCH_TMUX_SESSION", "")
	if got := SessionName(); got != "ttorch" {
		t.Errorf("default SessionName = %q, want ttorch", got)
	}
	t.Setenv("TTORCH_TMUX_SESSION", "custom-sess")
	if got := SessionName(); got != "custom-sess" {
		t.Errorf("override SessionName = %q, want custom-sess", got)
	}
}

func TestInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	if InsideTmux() {
		t.Error("InsideTmux = true with empty TMUX")
	}
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
	if !InsideTmux() {
		t.Error("InsideTmux = false with TMUX set")
	}
}

func TestTarget(t *testing.T) {
	if got := target("sess", "win"); got != "sess:win" {
		t.Errorf("target = %q, want sess:win", got)
	}
}

func TestTitleFormat(t *testing.T) {
	const want = "#{?#{@ttorch_label},#{@ttorch_label},#W}"
	if TitleFormat != want {
		t.Errorf("TitleFormat = %q, want %q", TitleFormat, want)
	}
}

func TestAvailableAndBin(t *testing.T) {
	// With the fake on PATH, tmux is "available" and bin resolves it.
	installFakeTmux(t)
	if !Available() {
		t.Error("Available = false with fake tmux on PATH")
	}
	if _, err := bin(); err != nil {
		t.Errorf("bin err = %v, want nil with fake on PATH", err)
	}
}

func TestUnavailableWhenAbsent(t *testing.T) {
	// PATH with no tmux at all.
	t.Setenv("PATH", t.TempDir())
	if Available() {
		t.Error("Available = true with empty PATH")
	}
	if _, err := bin(); err == nil {
		t.Error("bin err = nil, want error when tmux absent")
	}
	// run must surface the lookup failure rather than panic.
	if _, err := run("list-windows"); err == nil {
		t.Error("run err = nil, want error when tmux absent")
	}
}

// --- command construction / output parsing via fake tmux ---

func TestHasSession(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_HAS_SESSION_EXIT", "0")
	if !HasSession("s") {
		t.Error("HasSession = false, want true (exit 0)")
	}
	inv := readInvocations(t, log)
	if len(inv) != 1 || !reflect.DeepEqual(inv[0], []string{"has-session", "-t", "s"}) {
		t.Errorf("invocations = %v, want [[has-session -t s]]", inv)
	}

	t.Setenv("FAKE_HAS_SESSION_EXIT", "1")
	if HasSession("s") {
		t.Error("HasSession = true, want false (exit 1)")
	}
}

func TestEnsureSession_New(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_HAS_SESSION_EXIT", "1") // session does not exist
	if err := EnsureSession("s"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	want := [][]string{
		{"has-session", "-t", "s"},
		{"new-session", "-d", "-s", "s"},
		{"set-option", "-t", "s", "destroy-unattached", "off"},
		{"set-option", "-t", "s", "set-titles", "on"},
		{"set-option", "-t", "s", "set-titles-string", TitleFormat},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

func TestEnsureSession_Existing(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_HAS_SESSION_EXIT", "0") // session already exists
	if err := EnsureSession("s"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	inv := readInvocations(t, log)
	for _, call := range inv {
		if call[0] == "new-session" {
			t.Fatalf("new-session called for an existing session: %v", inv)
		}
	}
	// session options are still (re)applied: destroy-unattached off + 2 title options.
	if len(inv) != 4 {
		t.Fatalf("invocations = %v, want has-session + 3 set-option", inv)
	}
}

// TestEnsureSessionPinsDestroyUnattachedOff guards the disconnect-crash fix: the
// shared session ttorch owns must pin destroy-unattached off so a client disconnect
// (or an inherited global "destroy-unattached on") can never tear it down. Losing
// this session — which holds every worker's window — exits the server and kills the
// whole fleet. Covers both the freshly-created and the pre-existing paths.
func TestEnsureSessionPinsDestroyUnattachedOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hasExit string // FAKE_HAS_SESSION_EXIT: "1" => create, "0" => already exists
	}{
		{"new", "1"},
		{"existing", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := installFakeTmux(t)
			t.Setenv("FAKE_HAS_SESSION_EXIT", tc.hasExit)
			if err := EnsureSession("s"); err != nil {
				t.Fatalf("EnsureSession: %v", err)
			}
			want := []string{"set-option", "-t", "s", "destroy-unattached", "off"}
			found := false
			for _, call := range readInvocations(t, log) {
				if reflect.DeepEqual(call, want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("EnsureSession(%s path) did not pin destroy-unattached off", tc.name)
			}
		})
	}
}

func TestEnsureSession_NewSessionFails(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_HAS_SESSION_EXIT", "1") // proceed to create
	t.Setenv("FAKE_GENERIC_EXIT", "1")     // new-session fails
	err := EnsureSession("s")
	if err == nil || !strings.Contains(err.Error(), "tmux new-session") {
		t.Fatalf("EnsureSession err = %v, want a new-session failure", err)
	}
}

func TestNewWindow(t *testing.T) {
	log := installFakeTmux(t)
	if err := NewWindow("s", "w", "/work/dir"); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	want := [][]string{
		{"new-window", "-d", "-t", "s", "-n", "w", "-c", "/work/dir"},
		{"set-option", "-w", "-t", "s:w", "automatic-rename", "off"},
		{"set-option", "-w", "-t", "s:w", "allow-rename", "off"},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

func TestNewWindow_FailsBeforePinning(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_GENERIC_EXIT", "1") // new-window fails
	if err := NewWindow("s", "w", "/x"); err == nil {
		t.Fatal("NewWindow err = nil, want failure")
	}
	// It must return before pinning the window name.
	if got := readInvocations(t, log); len(got) != 1 || got[0][0] != "new-window" {
		t.Errorf("invocations = %v, want only new-window", got)
	}
}

func TestListWindows(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_LIST_WINDOWS", "alpha\nbeta\ngamma")
	got, err := ListWindows("s")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("ListWindows = %v", got)
	}
	if inv := readInvocations(t, log); len(inv) != 1 ||
		!reflect.DeepEqual(inv[0], []string{"list-windows", "-t", "s", "-F", "#{window_name}"}) {
		t.Errorf("invocation = %v", inv)
	}
}

func TestListWindows_Empty(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_LIST_WINDOWS", "")
	got, err := ListWindows("s")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if got != nil {
		t.Errorf("ListWindows = %v, want nil for no windows", got)
	}
}

func TestListWindows_Error(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_LIST_WINDOWS_EXIT", "1")
	if _, err := ListWindows("s"); err == nil {
		t.Error("ListWindows err = nil, want error")
	}
}

func TestWindowExists(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_LIST_WINDOWS", "w1\nw2")
	if !WindowExists("s", "w1") {
		t.Error("WindowExists w1 = false")
	}
	if WindowExists("s", "absent") {
		t.Error("WindowExists absent = true")
	}
}

func TestWindowExistsErr(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_LIST_WINDOWS", "w1\nw2")
	if ok, err := WindowExistsErr("s", "w1"); !ok || err != nil {
		t.Errorf("WindowExistsErr w1 = (%v,%v), want (true,nil)", ok, err)
	}
	if ok, err := WindowExistsErr("s", "absent"); ok || err != nil {
		t.Errorf("WindowExistsErr absent = (%v,%v), want (false,nil)", ok, err)
	}

	// A tmux read failure must be reported as an error, not "window gone".
	t.Setenv("FAKE_LIST_WINDOWS_EXIT", "1")
	if ok, err := WindowExistsErr("s", "w1"); ok || err == nil {
		t.Errorf("WindowExistsErr on read failure = (%v,%v), want (false,err)", ok, err)
	}
}

func TestLabelWindow(t *testing.T) {
	log := installFakeTmux(t)
	if err := LabelWindow("s", "w", "My Friendly Label"); err != nil {
		t.Fatalf("LabelWindow: %v", err)
	}
	want := []string{"set-option", "-w", "-t", "s:w", "@ttorch_label", "My Friendly Label"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestSendLine_PlainText(t *testing.T) {
	log := installFakeTmux(t)
	if err := SendLine("s", "w", "echo hi there"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	want := [][]string{
		{"send-keys", "-t", "s:w", "-l", "echo hi there"},
		{"send-keys", "-t", "s:w", "Enter"},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

func TestSendLine_SlashCommandUsesLongerSettle(t *testing.T) {
	log := installFakeTmux(t)
	start := time.Now()
	if err := SendLine("s", "w", "/clear"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	// Slash-commands get a >1s settle delay before Enter; a one-sided lower
	// bound confirms the branch without being timing-flaky.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("slash-command settle = %v, want >= 1s", elapsed)
	}
	want := [][]string{
		{"send-keys", "-t", "s:w", "-l", "/clear"},
		{"send-keys", "-t", "s:w", "Enter"},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

func TestSendLine_FirstSendFails(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_GENERIC_EXIT", "1") // the literal send-keys fails
	if err := SendLine("s", "w", "echo hi"); err == nil {
		t.Fatal("SendLine err = nil, want failure")
	}
	// Enter must not be sent if the text send failed.
	if got := readInvocations(t, log); len(got) != 1 {
		t.Errorf("invocations = %v, want only the failed literal send", got)
	}
}

func TestSendKey(t *testing.T) {
	log := installFakeTmux(t)
	if err := SendKey("s", "w", "C-c"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	want := []string{"send-keys", "-t", "s:w", "C-c"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestCapturePane(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_CAPTURE", "line1\nline2\n")
	out, err := CapturePane("s", "w", 120)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if out != "line1\nline2" { // run() trims the trailing newline
		t.Errorf("CapturePane = %q", out)
	}
	want := []string{"capture-pane", "-p", "-t", "s:w", "-S", "-120"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestCapturePane_Error(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_CAPTURE", "boom")
	t.Setenv("FAKE_CAPTURE_EXIT", "1")
	out, err := CapturePane("s", "w", 10)
	if err == nil {
		t.Fatal("CapturePane err = nil, want error")
	}
	// run() wraps the command and includes its combined output.
	if !strings.Contains(err.Error(), "tmux capture-pane") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to mention the command and output", err)
	}
	if out != "boom" {
		t.Errorf("CapturePane out = %q, want boom", out)
	}
}

func TestPaneCurrentCommand(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_DISPLAY", "claude\n")
	if got := PaneCurrentCommand("s", "w"); got != "claude" {
		t.Errorf("PaneCurrentCommand = %q, want claude", got)
	}
	want := []string{"display-message", "-p", "-t", "s:w", "#{pane_current_command}"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}

	t.Setenv("FAKE_DISPLAY_EXIT", "1")
	if got := PaneCurrentCommand("s", "w"); got != "" {
		t.Errorf("PaneCurrentCommand on error = %q, want empty", got)
	}
}

func TestPanePID(t *testing.T) {
	log := installFakeTmux(t)
	t.Setenv("FAKE_LIST_PANES", "4242\n9999")
	if got := PanePID("s", "w"); got != 4242 {
		t.Errorf("PanePID = %d, want 4242 (first line)", got)
	}
	want := []string{"list-panes", "-t", "s:w", "-F", "#{pane_pid}"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}

	t.Setenv("FAKE_LIST_PANES", "not-a-number")
	if got := PanePID("s", "w"); got != 0 {
		t.Errorf("PanePID on unparseable = %d, want 0", got)
	}

	t.Setenv("FAKE_LIST_PANES_EXIT", "1")
	if got := PanePID("s", "w"); got != 0 {
		t.Errorf("PanePID on error = %d, want 0", got)
	}
}

func TestKillWindow(t *testing.T) {
	log := installFakeTmux(t)
	if err := KillWindow("s", "w"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	want := []string{"kill-window", "-t", "s:w"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestKillSession(t *testing.T) {
	log := installFakeTmux(t)
	if err := KillSession("s"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	want := []string{"kill-session", "-t", "s"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestSelectWindow(t *testing.T) {
	log := installFakeTmux(t)
	if err := SelectWindow("s", "w"); err != nil {
		t.Fatalf("SelectWindow: %v", err)
	}
	want := []string{"select-window", "-t", "s:w"}
	if inv := readInvocations(t, log); len(inv) != 1 || !reflect.DeepEqual(inv[0], want) {
		t.Errorf("invocation = %v, want %v", inv, want)
	}
}

func TestKillSession_Error(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("FAKE_GENERIC_EXIT", "1")
	if err := KillSession("s"); err == nil {
		t.Error("KillSession err = nil, want error propagated")
	}
}

func TestAttach_InsideTmux(t *testing.T) {
	// Inside tmux, Attach selects the window then switches the client (no
	// process replacement), so the branch is deterministically testable.
	log := installFakeTmux(t)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	if err := Attach("s", "w"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	want := [][]string{
		{"select-window", "-t", "s:w"},
		{"switch-client", "-t", "s"},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

func TestAttach_InsideTmux_SwitchClientError(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("FAKE_GENERIC_EXIT", "1") // switch-client fails
	if err := Attach("s", "w"); err == nil {
		t.Error("Attach err = nil, want the switch-client failure propagated")
	}
}

// --- integration tests against a real tmux (skipped when absent) ---

func TestIntegration_SessionAndWindowLifecycle(t *testing.T) {
	if !Available() {
		t.Skip("tmux not installed")
	}
	session := fmt.Sprintf("ttorch-it-%s-%d", t.Name(), os.Getpid())
	t.Cleanup(func() { _ = KillSession(session) })

	if err := EnsureSession(session); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if !HasSession(session) {
		t.Fatal("HasSession = false after EnsureSession")
	}
	if err := EnsureSession(session); err != nil {
		t.Fatalf("EnsureSession (idempotent): %v", err)
	}

	cwd := t.TempDir()
	if err := NewWindow(session, "w1", cwd); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if !WindowExists(session, "w1") {
		t.Fatal("WindowExists w1 = false")
	}
	if WindowExists(session, "absent") {
		t.Fatal("WindowExists absent = true")
	}
	ws, err := ListWindows(session)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if !contains(ws, "w1") {
		t.Fatalf("ListWindows = %v, want to contain w1", ws)
	}
	if ok, err := WindowExistsErr(session, "w1"); !ok || err != nil {
		t.Fatalf("WindowExistsErr w1 = (%v,%v)", ok, err)
	}
	if err := LabelWindow(session, "w1", "My Label"); err != nil {
		t.Fatalf("LabelWindow: %v", err)
	}
	if err := SelectWindow(session, "w1"); err != nil {
		t.Fatalf("SelectWindow: %v", err)
	}
	if pid := PanePID(session, "w1"); pid <= 0 {
		t.Fatalf("PanePID = %d, want > 0", pid)
	}

	cmd := ""
	for i := 0; i < 40 && cmd == ""; i++ {
		if cmd = PaneCurrentCommand(session, "w1"); cmd == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if cmd == "" {
		t.Error("PaneCurrentCommand stayed empty for a live pane")
	}

	// A killed window must disappear from the listing.
	if err := NewWindow(session, "w2", cwd); err != nil {
		t.Fatalf("NewWindow w2: %v", err)
	}
	if err := KillWindow(session, "w2"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	if WindowExists(session, "w2") {
		t.Fatal("w2 still exists after KillWindow")
	}

	if err := KillSession(session); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if HasSession(session) {
		t.Fatal("HasSession = true after KillSession")
	}
}

func TestIntegration_SendLineRoundTrip(t *testing.T) {
	if !Available() {
		t.Skip("tmux not installed")
	}
	session := fmt.Sprintf("ttorch-it-%s-%d", t.Name(), os.Getpid())
	t.Cleanup(func() { _ = KillSession(session) })

	if err := EnsureSession(session); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := NewWindow(session, "w1", t.TempDir()); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	const marker = "TTORCH_RT_MARKER_OK"
	if err := SendLine(session, "w1", "echo "+marker); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	found := false
	for i := 0; i < 100 && !found; i++ {
		out, err := CapturePane(session, "w1", 200)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(out, marker) {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("marker not observed in pane after SendLine round-trip")
	}

	if err := SendKey(session, "w1", "Enter"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
}

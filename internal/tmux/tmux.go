// Package tmux drives the tmux binary via os/exec. Each worker (and the manager)
// lives in its own window of a shared session so the lead can watch or intervene.
package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SessionName is the tmux session ttorch uses (override with TTORCH_TMUX_SESSION).
func SessionName() string {
	if s := os.Getenv("TTORCH_TMUX_SESSION"); s != "" {
		return s
	}
	return "ttorch"
}

// TitleFormat is the set-titles-string ttorch applies to the sessions it creates
// so the terminal tab shows a window's friendly display label (the @ttorch_label
// window option set by LabelWindow), falling back to the window's tmux name when
// no label is set. Without set-titles the terminal shows its own fallback (e.g.
// iTerm's "tmux N"). The view sessions in package termtab set the same string.
const TitleFormat = "#{?#{@ttorch_label},#{@ttorch_label},#W}"

// Available reports whether tmux is installed.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// InsideTmux reports whether the current process is running inside tmux.
func InsideTmux() bool { return os.Getenv("TMUX") != "" }

func bin() (string, error) {
	p, err := exec.LookPath("tmux")
	if err != nil {
		return "", errors.New("tmux not found (run 'ttorch doctor')")
	}
	return p, nil
}

func run(args ...string) (string, error) {
	b, err := bin()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(b, args...).CombinedOutput()
	s := strings.TrimRight(string(out), "\n")
	if err != nil {
		return s, fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

func target(session, window string) string { return session + ":" + window }

// HasSession reports whether the named session exists.
func HasSession(session string) bool {
	_, err := run("has-session", "-t", session)
	return err == nil
}

// applySessionOptions applies the per-session options ttorch needs on a session it
// owns. set-option is per-session, so these are (re)applied to every session ttorch
// attaches (this shared session, and the view sessions in package termtab).
// Best-effort: a failure never blocks a spawn.
//
// It first pins destroy-unattached off: this shared session holds every worker's
// window, so it must NEVER self-destruct when no client is attached. A client
// disconnect (e.g. the lead's remote tmux client dropping) leaves the session
// momentarily unattached, and without this pin a global "destroy-unattached on"
// inherited from the user's tmux config would tear the session down, exit the
// server, and kill every live worker along with its uncommitted work. Pinning off
// at the session level overrides any such global default. It then turns on
// terminal-title reporting and points it at TitleFormat so the terminal tab shows a
// window's friendly display label (the @ttorch_label window option set by
// LabelWindow), falling back to the window's tmux name.
func applySessionOptions(session string) {
	_, _ = run("set-option", "-t", session, "destroy-unattached", "off")
	_, _ = run("set-option", "-t", session, "set-titles", "on")
	_, _ = run("set-option", "-t", session, "set-titles-string", TitleFormat)
}

// EnsureSession creates the session detached if it does not exist. It (re)applies
// ttorch's session options each call so a pre-existing session also pins
// destroy-unattached off and reports titles.
func EnsureSession(session string) error {
	if HasSession(session) {
		applySessionOptions(session)
		return nil
	}
	if _, err := run("new-session", "-d", "-s", session); err != nil {
		return err
	}
	applySessionOptions(session)
	return nil
}

// ListWindows returns the window names in a session.
func ListWindows(session string) ([]string, error) {
	out, err := run("list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// WindowExists reports whether a named window exists in the session.
func WindowExists(session, window string) bool {
	exists, _ := WindowExistsErr(session, window)
	return exists
}

// WindowExistsErr reports whether a named window exists, distinguishing a window that
// is genuinely absent (false, nil) from a tmux read that failed (false, err). Callers
// that must not treat a transient `tmux list-windows` hiccup as "the window is gone"
// (e.g. Spawn's readiness wait) use this instead of WindowExists.
func WindowExistsErr(session, window string) (bool, error) {
	ws, err := ListWindows(session)
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if w == window {
			return true, nil
		}
	}
	return false, nil
}

// NewWindow creates a detached window with the given working directory. The
// window's name is pinned (automatic-rename and allow-rename off) so the running
// command cannot overwrite it: ttorch uses the name as a stable tmux target and as
// a discovery key, while the friendly tab title is carried separately via the
// @ttorch_label window option (see LabelWindow and TitleFormat). Pinning is
// best-effort; only the window creation can fail the caller.
func NewWindow(session, window, cwd string) error {
	if _, err := run("new-window", "-d", "-t", session, "-n", window, "-c", cwd); err != nil {
		return err
	}
	t := target(session, window)
	_, _ = run("set-option", "-w", "-t", t, "automatic-rename", "off")
	_, _ = run("set-option", "-w", "-t", t, "allow-rename", "off")
	return nil
}

// LabelWindow sets a window's friendly display title — the terminal tab name
// rendered by TitleFormat — via the @ttorch_label window option. It does not touch
// the window's tmux name, which stays the stable target. Best-effort.
func LabelWindow(session, window, label string) error {
	_, err := run("set-option", "-w", "-t", target(session, window), "@ttorch_label", label)
	return err
}

// SendLine types a line into a window then presses Enter. The text and the Enter
// are sent separately (a combined send can submit before a TUI has rendered the
// input); a short settle delay precedes Enter, longer for slash-commands which may
// open a completion popup.
func SendLine(session, window, text string) error {
	t := target(session, window)
	if _, err := run("send-keys", "-t", t, "-l", text); err != nil {
		return err
	}
	delay := 300 * time.Millisecond
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		delay = 1200 * time.Millisecond
	}
	time.Sleep(delay)
	_, err := run("send-keys", "-t", t, "Enter")
	return err
}

// SendKey sends a single named key (e.g. "Escape", "C-c") to a window.
func SendKey(session, window, key string) error {
	_, err := run("send-keys", "-t", target(session, window), key)
	return err
}

// CapturePane returns the last n lines of a window's pane.
func CapturePane(session, window string, n int) (string, error) {
	return run("capture-pane", "-p", "-t", target(session, window), "-S", "-"+strconv.Itoa(n))
}

// PaneCurrentCommand returns the command name of the process in the foreground of a
// window's pane (tmux's #{pane_current_command}) — e.g. "zsh" while the window is
// still a bare shell, and "claude"/"node" once a harness has taken over. It returns
// "" if the window or pane cannot be read (e.g. the window has already exited). Spawn
// polls this to tell when a launched worker command is actually up before returning.
func PaneCurrentCommand(session, window string) string {
	out, err := run("display-message", "-p", "-t", target(session, window), "#{pane_current_command}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// PanePID returns the PID of the process running in a window's pane, or 0.
func PanePID(session, window string) int {
	out, err := run("list-panes", "-t", target(session, window), "-F", "#{pane_pid}")
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	pid, err := strconv.Atoi(line)
	if err != nil {
		return 0
	}
	return pid
}

// KillWindow removes a window (best effort).
func KillWindow(session, window string) error {
	_, err := run("kill-window", "-t", target(session, window))
	return err
}

// KillSession tears down an entire session and all its windows.
func KillSession(session string) error {
	_, err := run("kill-session", "-t", session)
	return err
}

// SelectWindow makes a window active.
func SelectWindow(session, window string) error {
	_, err := run("select-window", "-t", target(session, window))
	return err
}

// Attach focuses the window and hands the terminal to the session: switching the
// client if already inside tmux, otherwise replacing this process with
// `tmux attach`.
func Attach(session, window string) error {
	_ = SelectWindow(session, window)
	if InsideTmux() {
		_, err := run("switch-client", "-t", session)
		return err
	}
	b, err := bin()
	if err != nil {
		return err
	}
	return syscall.Exec(b, []string{"tmux", "attach-session", "-t", session}, os.Environ())
}

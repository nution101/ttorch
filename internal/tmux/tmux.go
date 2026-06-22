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

// SessionName is the tmux session orcha uses (override with ORCHA_TMUX_SESSION).
func SessionName() string {
	if s := os.Getenv("ORCHA_TMUX_SESSION"); s != "" {
		return s
	}
	return "orcha"
}

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
		return "", errors.New("tmux not found (run 'orcha doctor')")
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

// EnsureSession creates the session detached if it does not exist.
func EnsureSession(session string) error {
	if HasSession(session) {
		return nil
	}
	_, err := run("new-session", "-d", "-s", session)
	return err
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
	ws, err := ListWindows(session)
	if err != nil {
		return false
	}
	for _, w := range ws {
		if w == window {
			return true
		}
	}
	return false
}

// NewWindow creates a detached window with the given working directory.
func NewWindow(session, window, cwd string) error {
	_, err := run("new-window", "-d", "-t", session, "-n", window, "-c", cwd)
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

// KillWindow removes a window (best effort).
func KillWindow(session, window string) error {
	_, err := run("kill-window", "-t", target(session, window))
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

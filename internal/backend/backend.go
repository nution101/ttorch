// Package backend is the seam between ttorch and the program that hosts its sessions.
// Every worker, reviewer and manager runs in a named window of one shared session, and
// the orchestrator and the watcher reach that session only through the Backend interface
// here, so a second host can be added by implementing it rather than by editing each
// call site. tmux is the only implementation and the default.
package backend

import (
	"fmt"
	"os"
	"strings"

	"github.com/nution101/ttorch/internal/tmux"
)

// Backend is what ttorch does to a session host. The method set is taken from the call
// sites in internal/orchestrator and internal/watch, nothing more: session lifecycle,
// creating a window in the session, typing into it, reading it back, asking whether it
// is still there, killing it, and reading its pane process.
//
// A session is addressed by name and a window by (session, name). Window names are the
// stable targets ttorch keys on ("manager", "wk-<id>", "rv-<id>-<dim>"); a label is only
// the friendly title a terminal tab shows.
type Backend interface {
	// SessionName is the shared session ttorch runs in.
	SessionName() string
	// Available reports whether the host program is installed.
	Available() bool

	// EnsureSession creates the session if it does not exist.
	EnsureSession(session string) error
	// HasSession reports whether the session exists.
	HasSession(session string) bool
	// KillSession tears down the session and every window in it.
	KillSession(session string) error
	// ListWindows returns the window names in the session. A failed read is an error,
	// never an empty list.
	ListWindows(session string) ([]string, error)

	// NewWindow creates a detached window in the session, working in cwd, and gives it
	// a display label. Only the creation can fail the caller; the label is best-effort.
	NewWindow(session, window, cwd, label string) error
	// WindowExists reports whether the window is present. A read that did not answer in
	// time must report true, and any other failed read false: the callers are shaped
	// "create it if it is not there", and a timeout read as "gone" would start a second
	// agent in a worktree that already has one.
	WindowExists(session, window string) bool
	// WindowExistsErr is WindowExists with the failure kept: (false, nil) means the
	// window is definitely absent, and a read that did not answer in time returns an
	// error that matches ErrTimeout.
	WindowExistsErr(session, window string) (bool, error)
	// SendLine types text into the window's pane and presses Enter.
	SendLine(session, window, text string) error
	// CapturePane returns the last n lines of the window's pane.
	CapturePane(session, window string, n int) (string, error)
	// PaneCurrentCommand returns the name of the pane's foreground process, or "" when
	// it cannot be read.
	PaneCurrentCommand(session, window string) string
	// PanePID returns the pid of the process running in the window's pane, or 0.
	PanePID(session, window string) int
	// KillWindow removes the window.
	KillWindow(session, window string) error
	// Attach hands the caller's terminal to the session with the window focused.
	Attach(session, window string) error
}

// ErrTimeout marks a host command that did not answer in time, so a caller can tell "the
// host did not answer" from "the host answered no" with errors.Is. It is the tmux
// package's own sentinel, so the tmux backend's errors match it unchanged; another backend
// wraps its timeouts with it.
var ErrTimeout = tmux.ErrTimeout

// Env is the environment variable that selects the backend.
const Env = "TTORCH_BACKEND"

// FromEnv returns the backend TTORCH_BACKEND names (see Resolve).
func FromEnv() (Backend, error) {
	return Resolve(os.Getenv(Env))
}

// Resolve returns the backend with the given name. Empty selects the default, tmux.
// Anything it does not recognize is refused rather than falling back to the default, so
// a misspelled value cannot quietly run the fleet on a host the lead did not choose.
func Resolve(name string) (Backend, error) {
	switch strings.TrimSpace(name) {
	case "", "tmux":
		return Tmux{}, nil
	}
	return nil, fmt.Errorf("%s=%q is not a session backend ttorch supports; use \"tmux\" or leave it unset", Env, name)
}

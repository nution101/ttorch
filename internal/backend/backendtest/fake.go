// Package backendtest provides a recording fake of backend.Backend for the tests of the
// packages that drive sessions. Nothing outside tests imports it.
package backendtest

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nution101/ttorch/internal/backend"
)

// Fake is a Backend that runs no program. It keeps the sessions and windows it has been
// asked to create, models a window's pane as the lines typed into it, and records every
// call in order so a test can assert which calls a caller made and with what arguments.
//
// Its failures follow tmux where a caller depends on the difference: listing the windows
// of a session that does not exist is an error (so WindowExistsErr reports an error, and
// WindowExists false), and targeting a window that does not exist is an error. It never
// times out.
type Fake struct {
	Session     string // SessionName's answer
	Unavailable bool   // Available's answer is the negation

	mu       sync.Mutex
	calls    []string
	sessions map[string]bool
	windows  map[string]*window // keyed by "session:window"
}

type window struct {
	pane    []string
	command string
	pid     int
}

var _ backend.Backend = (*Fake)(nil)

// New returns a Fake whose SessionName is session.
func New(session string) *Fake {
	return &Fake{Session: session, sessions: map[string]bool{}, windows: map[string]*window{}}
}

// AddWindow creates session (if needed) and a window in it whose pane shows pane,
// without recording a call, so a test can set up the state a caller then observes.
func (f *Fake) AddWindow(session, name, pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[session] = true
	f.windows[key(session, name)] = &window{pane: splitLines(pane), command: "zsh"}
}

// SetPanePID sets the pid PanePID reports for an existing window.
func (f *Fake) SetPanePID(session, name string, pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.windows[key(session, name)]; w != nil {
		w.pid = pid
	}
}

// HasWindow reports whether the window exists, without recording a call.
func (f *Fake) HasWindow(session, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.windows[key(session, name)] != nil
}

// Calls returns every call so far, oldest first, each rendered as
// "Method(arg, arg, ...)".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func key(session, name string) string { return session + ":" + name }

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// record appends a call. The caller holds f.mu.
func (f *Fake) record(method string, args ...string) {
	f.calls = append(f.calls, method+"("+strings.Join(args, ", ")+")")
}

// target returns the named window, or the error tmux gives for a missing one. The caller
// holds f.mu.
func (f *Fake) target(session, name string) (*window, error) {
	if w := f.windows[key(session, name)]; w != nil {
		return w, nil
	}
	return nil, fmt.Errorf("can't find window: %s", key(session, name))
}

func (f *Fake) SessionName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SessionName")
	return f.Session
}

func (f *Fake) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Available")
	return !f.Unavailable
}

func (f *Fake) EnsureSession(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureSession", session)
	f.sessions[session] = true
	return nil
}

func (f *Fake) HasSession(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("HasSession", session)
	return f.sessions[session]
}

func (f *Fake) KillSession(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("KillSession", session)
	if !f.sessions[session] {
		return fmt.Errorf("can't find session: %s", session)
	}
	delete(f.sessions, session)
	for k := range f.windows {
		if strings.HasPrefix(k, session+":") {
			delete(f.windows, k)
		}
	}
	return nil
}

// listWindows is ListWindows without recording. The caller holds f.mu.
func (f *Fake) listWindows(session string) ([]string, error) {
	if !f.sessions[session] {
		return nil, errors.New("can't find session: " + session)
	}
	var names []string
	for k := range f.windows {
		if name, ok := strings.CutPrefix(k, session+":"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *Fake) ListWindows(session string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListWindows", session)
	return f.listWindows(session)
}

// NewWindow creates the window as a bare shell, replacing any window of the same name
// the way the tmux backend does.
func (f *Fake) NewWindow(session, name, cwd, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("NewWindow", session, name, cwd, label)
	if !f.sessions[session] {
		return errors.New("can't find session: " + session)
	}
	f.windows[key(session, name)] = &window{command: "zsh"}
	return nil
}

func (f *Fake) WindowExists(session, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WindowExists", session, name)
	return f.windows[key(session, name)] != nil
}

func (f *Fake) WindowExistsErr(session, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WindowExistsErr", session, name)
	if _, err := f.listWindows(session); err != nil {
		return false, err
	}
	return f.windows[key(session, name)] != nil, nil
}

// SendLine appends text to the pane and treats its first word as the program that now
// holds the pane's foreground, which is what typing a command into a shell does.
func (f *Fake) SendLine(session, name, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SendLine", session, name, text)
	w, err := f.target(session, name)
	if err != nil {
		return err
	}
	w.pane = append(w.pane, text)
	if fields := strings.Fields(text); len(fields) > 0 {
		w.command = fields[0]
	}
	return nil
}

func (f *Fake) CapturePane(session, name string, n int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CapturePane", session, name, strconv.Itoa(n))
	w, err := f.target(session, name)
	if err != nil {
		return "", err
	}
	lines := w.pane
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func (f *Fake) PaneCurrentCommand(session, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneCurrentCommand", session, name)
	if w := f.windows[key(session, name)]; w != nil {
		return w.command
	}
	return ""
}

func (f *Fake) PanePID(session, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PanePID", session, name)
	if w := f.windows[key(session, name)]; w != nil {
		return w.pid
	}
	return 0
}

func (f *Fake) KillWindow(session, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("KillWindow", session, name)
	if _, err := f.target(session, name); err != nil {
		return err
	}
	delete(f.windows, key(session, name))
	return nil
}

func (f *Fake) Attach(session, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Attach", session, name)
	return nil
}

package backend

import "github.com/nution101/ttorch/internal/tmux"

// Tmux is the Backend that drives the tmux binary. Every method delegates to package
// tmux, which owns the command lines, the per-command deadline and the error text, so
// going through the seam runs exactly the tmux commands a direct call runs.
type Tmux struct{}

var _ Backend = Tmux{}

// SessionName is tmux.SessionName: TTORCH_TMUX_SESSION, else "ttorch".
func (Tmux) SessionName() string { return tmux.SessionName() }

func (Tmux) Available() bool { return tmux.Available() }

func (Tmux) EnsureSession(session string) error { return tmux.EnsureSession(session) }

func (Tmux) HasSession(session string) bool { return tmux.HasSession(session) }

func (Tmux) KillSession(session string) error { return tmux.KillSession(session) }

func (Tmux) ListWindows(session string) ([]string, error) { return tmux.ListWindows(session) }

// NewWindow creates the window, then sets its label. A failed label is dropped, as it
// was when the orchestrator made the two calls itself.
func (Tmux) NewWindow(session, window, cwd, label string) error {
	if err := tmux.NewWindow(session, window, cwd); err != nil {
		return err
	}
	_ = tmux.LabelWindow(session, window, label)
	return nil
}

func (Tmux) WindowExists(session, window string) bool { return tmux.WindowExists(session, window) }

func (Tmux) WindowExistsErr(session, window string) (bool, error) {
	return tmux.WindowExistsErr(session, window)
}

func (Tmux) SendLine(session, window, text string) error {
	return tmux.SendLine(session, window, text)
}

func (Tmux) CapturePane(session, window string, n int) (string, error) {
	return tmux.CapturePane(session, window, n)
}

func (Tmux) PaneCurrentCommand(session, window string) string {
	return tmux.PaneCurrentCommand(session, window)
}

func (Tmux) PanePID(session, window string) int { return tmux.PanePID(session, window) }

func (Tmux) KillWindow(session, window string) error { return tmux.KillWindow(session, window) }

func (Tmux) Attach(session, window string) error { return tmux.Attach(session, window) }

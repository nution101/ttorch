// Package termtab opens a native macOS terminal tab or window that attaches a
// view onto a worker's tmux window, so the lead can watch a worker without
// leaving tmux. The worker process itself never leaves its tmux window in the
// shared session: the native tab only attaches a grouped (view) session that
// shares the source session's window list. Closing the tab destroys the view
// session and leaves the worker's window and process intact.
//
// The whole feature is macOS-only and best-effort: Open returns nil when the
// feature is disabled, the OS is not macOS, or the underlying osascript fails
// (it logs a one-line note to stderr in the latter case). It must never fail a
// spawn.
package termtab

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Open opens a native terminal tab (iTerm) or window (Terminal.app) that
// attaches a tmux view onto session:window. It is best-effort: it returns nil
// when disabled, on non-macOS, or on a soft failure (logged to stderr).
func Open(session, window string) error {
	if !enabled() {
		return nil
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	term := terminalChoice()
	if term == "auto" {
		term = detectTerminal()
	}
	tmuxCmd := viewCommand(session, window)
	script := appleScript(term, tmuxCmd)
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not open a terminal view for %s (workers still run in tmux; Ctrl-b w to navigate): %v\n", window, err)
		return nil
	}
	return nil
}

// Enabled reports whether native-terminal behavior (worker views and the
// manager-in-iTerm launch) is turned on. It is the exported gate for callers.
func Enabled() bool { return enabled() }

// ITermAvailable reports whether iTerm2 is installed. iTerm2 ships no CLI on
// PATH, so its presence is detected by the app bundle. macOS-only.
func ITermAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat("/Applications/iTerm.app")
	return err == nil
}

// managerCommand builds the tmux command a fresh iTerm window runs to attach the
// lead to the manager: it selects the manager window first (the session also
// carries a default window 0 and the manager window is created detached), then
// attaches the session. It is executed by the new window's interactive shell, so
// every operand is single-quoted with shq — session and window names are
// untrusted (session = $TTORCH_TMUX_SESSION).
func managerCommand(session, window string) string {
	return fmt.Sprintf(
		"tmux select-window -t %s:%s \\; attach-session -t %s",
		shq(session), shq(window), shq(session),
	)
}

// itermNewWindowScript returns the AppleScript that opens a NEW iTerm window and
// runs cmd in it. The command is embedded as an AppleScript string literal, so
// backslashes and double-quotes are escaped.
func itermNewWindowScript(cmd string) string {
	lit := escapeAppleScript(cmd)
	return `tell application "iTerm"
	activate
	create window with default profile
	tell current session of current window to write text "` + lit + `"
end tell`
}

// OpenManagerSession opens the manager in a NEW iTerm2 window on macOS so the
// manager tab and the per-worker view tabs share one iTerm window. It returns
// true only when it actually launched iTerm; otherwise it returns false and the
// caller falls back to tmux.Attach.
//
// It is a no-op (returns false) unless every gate holds: the feature is enabled;
// the OS is macOS; the user did not force Terminal.app; iTerm is installed; we
// are not already inside iTerm (attach in place); and we are not already inside
// tmux (switch-client in place). It never panics and never blocks.
func OpenManagerSession(session, window string) bool {
	if !enabled() ||
		runtime.GOOS != "darwin" ||
		terminalChoice() == "terminal" ||
		!ITermAvailable() ||
		os.Getenv("TERM_PROGRAM") == "iTerm.app" ||
		os.Getenv("TMUX") != "" {
		return false
	}
	script := itermNewWindowScript(managerCommand(session, window))
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not open the manager in iTerm2 (attaching in tmux instead): %v\n", err)
		return false
	}
	return true
}

// enabled reports whether worker tabs are turned on. Default enabled; the values
// "0", "off", "false", "no" disable it.
func enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_WORKER_TABS"))) {
	case "0", "off", "false", "no":
		return false
	default:
		return true
	}
}

// terminalChoice reports the requested terminal: "auto" (default), "iterm", or
// "terminal". Any unrecognized value falls back to "auto".
func terminalChoice() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_TERMINAL"))); v {
	case "iterm", "terminal":
		return v
	default:
		return "auto"
	}
}

// detectTerminal picks a terminal when the choice is "auto": iTerm if installed,
// otherwise Terminal.app (always present on macOS).
func detectTerminal() string {
	if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
		return "iterm"
	}
	return "terminal"
}

// viewName returns the sanitized tmux session name for the view that attaches to
// a worker window. It keeps only [A-Za-z0-9_-] (tmux session names must not
// contain '.', ':', or whitespace) and prefixes "ttv-".
func viewName(window string) string {
	var b strings.Builder
	for _, r := range window {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return "ttv-" + b.String()
}

// viewCommand builds the tmux command the native tab runs. It creates (or
// attaches to) a grouped view session sharing the source session's windows,
// sets destroy-unattached (scoped explicitly to the view session) so closing the
// tab tears down only the view, and selects the worker's window. new-session
// WITHOUT -d attaches immediately, so destroy-unattached only fires when the user
// closes the tab.
//
// This command is executed by the terminal's interactive shell (via osascript
// `do script` / `write text`), so every interpolated operand is single-quoted
// with shq to neutralize shell metacharacters — the session and window names are
// untrusted (window = "wk-"+taskID, session = $TTORCH_TMUX_SESSION).
func viewCommand(session, window string) string {
	view := viewName(window)
	return fmt.Sprintf(
		"tmux new-session -A -s %s -t %s \\; set-option -t %s destroy-unattached on \\; select-window -t %s:%s",
		shq(view), shq(session), shq(view), shq(view), shq(window),
	)
}

// shq single-quotes s into exactly one shell word, neutralizing every shell
// metacharacter. An embedded single quote is closed, escaped as \', and reopened.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Close best-effort tears down the native terminal view session for a worker
// window so its watcher tab closes cleanly when the worker is torn down. It is a
// no-op when the feature is disabled or off macOS; killing a non-existent session
// is ignored. It never affects the worker, which lives in the source session.
func Close(window string) {
	if !enabled() || runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("tmux", "kill-session", "-t", viewName(window)).Run()
}

// appleScript returns the AppleScript passed to `osascript -e`. For iTerm it
// opens a proper new tab and writes the command; for Terminal.app it opens a new
// window via `do script`. The tmux command is embedded as an AppleScript string
// literal, so backslashes and double-quotes are escaped.
func appleScript(term, tmuxCmd string) string {
	lit := escapeAppleScript(tmuxCmd)
	if term == "iterm" {
		return `tell application "iTerm"
	activate
	tell current window
		create tab with default profile
		tell current session to write text "` + lit + `"
	end tell
end tell`
	}
	return `tell application "Terminal"
	activate
	do script "` + lit + `"
end tell`
}

// escapeAppleScript escapes a string for embedding inside an AppleScript double-
// quoted literal: backslashes first, then double-quotes.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

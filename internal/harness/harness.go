// Package harness detects the coding-agent harness and builds its launch command.
// Claude Code is the primary target; sessions launch with --dangerously-skip-permissions
// because all work is confined to isolated worktrees.
package harness

import (
	"os"
	"os/exec"
	"strings"
)

// DetectOwn returns the harness running the current process, or "" if unknown.
func DetectOwn() string {
	if os.Getenv("CLAUDECODE") != "" {
		return "claude"
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return "claude"
	}
	return ""
}

// Resolve returns the harness to use, defaulting to claude.
func Resolve() string {
	if h := DetectOwn(); h != "" {
		return h
	}
	return "claude"
}

// Available reports whether the harness binary is on PATH.
func Available(kind string) bool {
	_, err := exec.LookPath(kind)
	return err == nil
}

// InteractiveCommand is the shell command to start an interactive session.
func InteractiveCommand(kind string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions"
	default:
		return kind
	}
}

// BriefCommand starts the harness with a task brief as its initial prompt.
func BriefCommand(kind, briefPath string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions \"$(cat " + quote(briefPath) + ")\""
	default:
		return kind
	}
}

// quote single-quotes a path for safe interpolation in a shell command.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

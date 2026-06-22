// Package harness detects the coding-agent harness and builds its launch command.
// Claude Code is the primary target; sessions launch with --dangerously-skip-permissions
// because all work is confined to isolated worktrees.
package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// InstallTurnEndHook arranges for the harness to touch markerPath at each turn
// boundary, so the supervisor can detect activity. For Claude Code this writes a
// worktree-local Stop hook and keeps it out of git's view. Other harnesses are
// no-ops for now.
func InstallTurnEndHook(kind, worktree, markerPath string) error {
	if kind != "claude" {
		return nil
	}
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookEntry struct {
		Hooks []hookCmd `json:"hooks"`
	}
	type settings struct {
		Hooks map[string][]hookEntry `json:"hooks"`
	}
	cfg := settings{Hooks: map[string][]hookEntry{
		"Stop": {{Hooks: []hookCmd{{Type: "command", Command: "touch " + quote(markerPath)}}}},
	}}
	dir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), b, 0o644); err != nil {
		return err
	}
	return excludeInWorktree(worktree, ".claude/settings.local.json")
}

// excludeInWorktree appends a pattern to the worktree's git info/exclude so the
// hook file never shows as dirty or gets committed.
func excludeInWorktree(worktree, pattern string) error {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		return nil // best effort: not fatal if git layout is unusual
	}
	excl := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(worktree, excl)
	}
	if b, err := os.ReadFile(excl); err == nil && strings.Contains(string(b), pattern) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(pattern + "\n")
	return err
}

// quote single-quotes a path for safe interpolation in a shell command.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

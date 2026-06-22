// Package harness detects the coding-agent harness and builds its launch command.
// Claude Code is the primary target; sessions launch with --dangerously-skip-permissions
// because all work is confined to isolated worktrees.
package harness

import (
	"encoding/json"
	"fmt"
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

// EffortArgs returns the extra `claude` arguments that set the reasoning effort
// for a spawned session. The level is read from TTORCH_EFFORT (default
// "ultracode"); set TTORCH_EFFORT=off to leave Claude's own default untouched.
//
// "ultracode" is not an --effort level: it is a session feature (xhigh reasoning
// plus dynamic workflow orchestration) enabled via --settings. The discrete
// levels (low|medium|high|xhigh|max) go through the --effort flag. An
// unrecognized value falls back to ultracode rather than passing something
// claude would reject.
func EffortArgs(kind string) string {
	if kind != "claude" {
		return ""
	}
	switch level := effortLevel(); level {
	case "off", "none", "default":
		return ""
	case "low", "medium", "high", "xhigh", "max":
		return " --effort " + level
	default: // "ultracode" and anything unrecognized
		return ` --settings '{"ultracode":true}'`
	}
}

func effortLevel() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_EFFORT")))
	if v == "" {
		return "ultracode"
	}
	return v
}

// InteractiveCommand is the shell command to start an interactive session.
func InteractiveCommand(kind string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind)
	default:
		return kind
	}
}

// BriefCommand starts the harness with a task brief as its initial prompt.
func BriefCommand(kind, briefPath string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind) + " \"$(cat " + quote(briefPath) + ")\""
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
		// IncludeCoAuthoredBy=false stops the agent from adding an AI co-author
		// trailer to commits — work is authored as the repo's git user, not the agent.
		IncludeCoAuthoredBy bool                   `json:"includeCoAuthoredBy"`
		Hooks               map[string][]hookEntry `json:"hooks"`
	}
	cfg := settings{
		IncludeCoAuthoredBy: false,
		Hooks: map[string][]hookEntry{
			"Stop": {{Hooks: []hookCmd{{Type: "command", Command: "touch " + quote(markerPath)}}}},
		},
	}
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

// TrustWorktree pre-accepts Claude Code's one-time folder-trust prompt for a
// worker's repo and worktree, so a spawned worker runs without an interactive
// prompt blocking it. It sets hasTrustDialogAccepted in Claude's config
// (~/.claude.json) for the given paths. Best-effort and idempotent: it never fails
// a spawn, never overwrites an unparseable config, and is a no-op when the harness
// is not claude or TTORCH_NO_AUTOTRUST is set.
func TrustWorktree(kind string, paths ...string) {
	if kind != "claude" || os.Getenv("TTORCH_NO_AUTOTRUST") != "" {
		return
	}
	var resolved []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			p = rp // match how Claude keys projects (resolved real path)
		}
		resolved = append(resolved, p)
	}
	_ = ensureTrusted(claudeConfigPath(), resolved)
}

func claudeConfigPath() string {
	if v := os.Getenv("TTORCH_CLAUDE_JSON"); v != "" { // test/override hook
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// ensureTrusted marks each path trusted in the Claude config at cfgPath, preserving
// all other content. It returns an error (without writing) if the config exists but
// is not valid JSON, so a malformed config is never clobbered.
func ensureTrusted(cfgPath string, paths []string) error {
	if cfgPath == "" || len(paths) == 0 {
		return nil
	}
	cfg := map[string]any{}
	if b, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("claude config %s is not valid JSON; leaving it untouched: %w", cfgPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	changed := false
	for _, p := range paths {
		entry, _ := projects[p].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			projects[p] = entry
		}
		if v, _ := entry["hasTrustDialogAccepted"].(bool); !v {
			entry["hasTrustDialogAccepted"] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(cfgPath, b)
}

func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ttorch-claude-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

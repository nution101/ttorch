// Package paths centralizes orcha's on-disk layout. Every location is overridable
// via an environment variable so tests can redirect into a temp directory.
package paths

import (
	"os"
	"path/filepath"
)

// Paths describes where orcha keeps its own state and where it installs the
// managed Claude Code / agent content.
type Paths struct {
	Home     string // ~/.orcha (ORCHA_HOME)
	Claude   string // ~/.claude (ORCHA_CLAUDE_DIR)
	Agents   string // ~/.agents (ORCHA_AGENTS_DIR)
	LocalBin string // ~/.local/bin (ORCHA_BIN_DIR) — where the PATH symlink lives
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Default resolves the standard layout, honoring environment overrides.
func Default() Paths {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return Paths{
		Home:     envOr("ORCHA_HOME", filepath.Join(home, ".orcha")),
		Claude:   envOr("ORCHA_CLAUDE_DIR", filepath.Join(home, ".claude")),
		Agents:   envOr("ORCHA_AGENTS_DIR", filepath.Join(home, ".agents")),
		LocalBin: envOr("ORCHA_BIN_DIR", filepath.Join(home, ".local", "bin")),
	}
}

// BinDir is the user-owned directory holding the real orcha binary.
// It is intentionally user-owned so macOS can atomically self-replace it.
func (p Paths) BinDir() string { return filepath.Join(p.Home, "bin") }

// Binary is the path to the installed orcha executable.
func (p Paths) Binary() string { return filepath.Join(p.BinDir(), "orcha") }

// ManifestFile is the ledger of every managed file and its sha256.
func (p Paths) ManifestFile() string { return filepath.Join(p.Home, "manifest.json") }

// VersionFile records the installed content/binary version.
func (p Paths) VersionFile() string { return filepath.Join(p.Home, "version") }

// UpdateCheckFile caches the 24h "new version available" check.
func (p Paths) UpdateCheckFile() string { return filepath.Join(p.Home, "update-check.json") }

// StateDir holds volatile runtime state (never touched by updates).
func (p Paths) StateDir() string { return filepath.Join(p.Home, "state") }

// DataDir holds durable manager records (never touched by updates).
func (p Paths) DataDir() string { return filepath.Join(p.Home, "data") }

// Worktrees is the root for per-task isolated git worktrees.
func (p Paths) Worktrees() string { return filepath.Join(p.Home, "worktrees") }

// BriefPath is where a task's brief lives.
func (p Paths) BriefPath(id string) string { return filepath.Join(p.DataDir(), id, "brief.md") }

// ClaudeSkills is the managed-skills install root for Claude Code.
func (p Paths) ClaudeSkills() string { return filepath.Join(p.Claude, "skills") }

// AgentsSkills mirrors skills for vendor-neutral harnesses.
func (p Paths) AgentsSkills() string { return filepath.Join(p.Agents, "skills") }

// ClaudeAgents is the subagent install root.
func (p Paths) ClaudeAgents() string { return filepath.Join(p.Claude, "agents") }

// ClaudeCommands is the slash-command install root.
func (p Paths) ClaudeCommands() string { return filepath.Join(p.Claude, "commands") }

// GlobalAgentsMD is the global guidance file (CLAUDE.md symlinks to it).
func (p Paths) GlobalAgentsMD() string { return filepath.Join(p.Claude, "AGENTS.md") }

// GlobalClaudeMD is the symlink target name (-> AGENTS.md).
func (p Paths) GlobalClaudeMD() string { return filepath.Join(p.Claude, "CLAUDE.md") }

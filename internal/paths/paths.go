// Package paths centralizes ttorch's on-disk layout. Every location is overridable
// via an environment variable so tests can redirect into a temp directory.
package paths

import (
	"os"
	"path/filepath"
)

// Paths describes where ttorch keeps its own state and where it installs the
// managed Claude Code / agent content.
type Paths struct {
	Home     string // ~/.ttorch (TTORCH_HOME)
	Claude   string // ~/.claude (TTORCH_CLAUDE_DIR)
	Agents   string // ~/.agents (TTORCH_AGENTS_DIR)
	LocalBin string // ~/.local/bin (TTORCH_BIN_DIR) — where the PATH symlink lives
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
		Home:     envOr("TTORCH_HOME", filepath.Join(home, ".ttorch")),
		Claude:   envOr("TTORCH_CLAUDE_DIR", filepath.Join(home, ".claude")),
		Agents:   envOr("TTORCH_AGENTS_DIR", filepath.Join(home, ".agents")),
		LocalBin: envOr("TTORCH_BIN_DIR", filepath.Join(home, ".local", "bin")),
	}
}

// BinDir is the user-owned directory holding the real ttorch binary.
// It is intentionally user-owned so macOS can atomically self-replace it.
func (p Paths) BinDir() string { return filepath.Join(p.Home, "bin") }

// Binary is the path to the installed ttorch executable.
func (p Paths) Binary() string { return filepath.Join(p.BinDir(), "ttorch") }

// ManifestFile is the ledger of every managed file and its sha256.
func (p Paths) ManifestFile() string { return filepath.Join(p.Home, "manifest.json") }

// VersionFile records the installed content/binary version.
func (p Paths) VersionFile() string { return filepath.Join(p.Home, "version") }

// ManagerCharterFile holds the manager's system-prompt charter, passed to claude
// via --append-system-prompt-file so the launched command stays short.
func (p Paths) ManagerCharterFile() string { return filepath.Join(p.Home, "manager-charter.md") }

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

// PIDFile holds the running supervisor's PID.
func (p Paths) PIDFile() string { return filepath.Join(p.StateDir(), "daemon.pid") }

// Beacon is touched each supervisor cycle as a liveness signal.
func (p Paths) Beacon() string { return filepath.Join(p.StateDir(), ".beacon") }

// WakeQueue is the durable, append-only queue the supervisor writes and the
// manager drains.
func (p Paths) WakeQueue() string { return filepath.Join(p.StateDir(), "wake-queue") }

// DaemonLog captures detached supervisor output.
func (p Paths) DaemonLog() string { return filepath.Join(p.Home, "daemon.log") }

// TurnEndMarker is touched by a worker's harness at each turn boundary.
func (p Paths) TurnEndMarker(id string) string {
	return filepath.Join(p.StateDir(), id+".turn-ended")
}

// ApprovalFile holds a short-lived approval token authorizing a merge for a task.
func (p Paths) ApprovalFile(id string) string {
	return filepath.Join(p.StateDir(), id+".approve")
}

// AuditLog records state-changing delivery actions (merges).
func (p Paths) AuditLog() string { return filepath.Join(p.Home, "audit.log") }

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

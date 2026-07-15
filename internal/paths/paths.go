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

// StateDB is the global SQLite database — the single source of truth for all
// orchestration state. Overridable via TTORCH_DB so tests can redirect into a
// temp file.
func (p Paths) StateDB() string { return envOr("TTORCH_DB", filepath.Join(p.Home, "state.db")) }

// DataDir holds durable manager records (never touched by updates).
func (p Paths) DataDir() string { return filepath.Join(p.Home, "data") }

// ValidateCacheDir is the content-addressed cache of GREEN trust-gate validate results,
// keyed by (git tree hash + gate-definition identity) so every re-validation of an identical
// tree under an identical gate collapses to a single real run (a rebase mints a new commit
// sha but not a new tree, so a commit-keyed cache missed on every rebase). Overridable via
// TTORCH_VALIDATE_CACHE_DIR (set and non-empty ⇒ used verbatim) so it can later point at a
// shared/networked volume. Entries are content-addressed, so a stale one is never served.
func (p Paths) ValidateCacheDir() string {
	return envOr("TTORCH_VALIDATE_CACHE_DIR", filepath.Join(p.Home, "validate-cache"))
}

// Worktrees is the root for per-task isolated git worktrees.
func (p Paths) Worktrees() string { return filepath.Join(p.Home, "worktrees") }

// BriefPath is where a task's brief lives.
func (p Paths) BriefPath(id string) string { return filepath.Join(p.DataDir(), id, "brief.md") }

// WatchPIDFile holds the running `ttorch watch` singleton's PID. The watcher takes
// an exclusive flock on it (flock-as-truth) so at most one watcher blocks at a time;
// the recorded pid is for `ttorch watch --reset`'s observability and pid-reuse-guarded
// reap (§4.5).
func (p Paths) WatchPIDFile() string { return filepath.Join(p.StateDir(), "watch.pid") }

// SchedulerPIDFile holds the auto-started scheduler daemon's singleton lock + pid (one daemon
// per ~/.ttorch). The daemon takes an exclusive flock on it; a redundant start (e.g. a second
// `ttorch` session) finds the lock held and exits quietly, so the manager never runs two daemons.
func (p Paths) SchedulerPIDFile() string { return filepath.Join(p.StateDir(), "scheduler.pid") }

// SchedulerLog is where the auto-started scheduler daemon's diagnostic output goes — a file under
// ~/.ttorch, NEVER the manager pane (no TTY injection).
func (p Paths) SchedulerLog() string { return filepath.Join(p.Home, "scheduler.log") }

// ApprovalFile holds a short-lived approval token authorizing a merge for a task.
// The adversarial-review VERDICT it sits beside is no longer a file — it is durable,
// content-pinned SQLite state (the verdicts table), so a merge is never forced to
// re-gate by file-TTL expiry. The approval token stays a short-lived file: its TTL is
// the deliberate human-approval freshness window, distinct from the verdict so an audit
// can always tell "a human approved" from "the reviewers passed".
func (p Paths) ApprovalFile(id string) string {
	return filepath.Join(p.StateDir(), id+".approve")
}

// ReviewInputsDir holds the materialized inputs the review subagents read for a task
// (the diff, brief, validate results, reviewed HEAD) and the per-dimension findings
// reports they write back. It is durable manager data, beside BriefPath.
func (p Paths) ReviewInputsDir(id string) string {
	return filepath.Join(p.DataDir(), id, "review")
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

// ClaudeHooks is the install root for ttorch's shipped hook scripts. The global
// settings file references these scripts by absolute path.
func (p Paths) ClaudeHooks() string { return filepath.Join(p.Claude, "hooks") }

// GlobalAgentsMD is the global guidance file (CLAUDE.md symlinks to it).
func (p Paths) GlobalAgentsMD() string { return filepath.Join(p.Claude, "AGENTS.md") }

// GlobalClaudeMD is the symlink target name (-> AGENTS.md).
func (p Paths) GlobalClaudeMD() string { return filepath.Join(p.Claude, "CLAUDE.md") }

// GlobalSettingsFile is Claude Code's global settings file. ttorch merges a
// ttorch-owned block of keys/hooks into it in place (clobber-safe), never parking
// it as a side-file because it is a shared live file other tools also write.
func (p Paths) GlobalSettingsFile() string { return filepath.Join(p.Claude, "settings.json") }

// GlobalSettingsBackup is the one-time snapshot ttorch writes before its first
// modification of GlobalSettingsFile, so the developer's original is recoverable.
func (p Paths) GlobalSettingsBackup() string {
	return filepath.Join(p.Claude, "settings.json.ttorch-backup")
}

// GlobalSettingsLedger records which leaves of GlobalSettingsFile ttorch owns (and
// the exact value it wrote), so updates upgrade only ttorch-owned keys and uninstall
// removes only them — developer-set keys are never touched. It is ttorch state, so
// it lives under ~/.ttorch and is never part of the content manifest.
func (p Paths) GlobalSettingsLedger() string {
	return filepath.Join(p.Home, "global-settings-ledger.json")
}

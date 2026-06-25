// Package harness detects the coding-agent harness and builds its launch command.
// Claude Code is the primary target; sessions launch with --dangerously-skip-permissions
// because all work is confined to isolated worktrees.
package harness

import (
	"crypto/rand"
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
// for a spawned worker (and the `ttorch cc` session). The level is read from
// TTORCH_EFFORT (default "ultracode"); set TTORCH_EFFORT=off to leave Claude's
// own default untouched.
func EffortArgs(kind string) string {
	if kind != "claude" {
		return ""
	}
	return effortArgsForLevel(effortLevel())
}

// effortArgsForLevel maps an effort level to claude flags. "ultracode" is not an
// --effort level: it is a session feature (xhigh reasoning plus dynamic workflow
// orchestration) enabled via --settings. The discrete levels (low|medium|high|
// xhigh|max) go through the --effort flag; off/none/default add nothing; an
// unrecognized value falls back to ultracode rather than passing something claude
// would reject.
func effortArgsForLevel(level string) string {
	switch level {
	case "off", "none", "default":
		return ""
	case "low", "medium", "high", "xhigh", "max":
		return " --effort " + level
	default:
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

// managerEffortLevel is the manager's effort, from TTORCH_MANAGER_EFFORT (default
// "high"). The manager only plans and delegates, so it runs lighter than workers
// and is deliberately NOT ultracode by default: ultracode pushes a session to do
// deep work itself (and spawn its own internal sub-agents), which fights the
// manager's delegate-only role.
func managerEffortLevel() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_MANAGER_EFFORT")))
	if v == "" {
		return "high"
	}
	return v
}

// managerCharter is appended to the manager session's system prompt so the
// session always acts as the orchestrator regardless of how the lead phrases a
// request. It is one line (it is sent through tmux).
const managerCharter = "You are the ttorch MANAGER for this tmux session; the person you talk to is the lead. You PLAN and DELEGATE — you do not write code, edit files, or run a project's build/test/lint commands in this window. Run the team as a continuous loop, not a one-shot checklist. (1) The live board is your source of truth: at every check-in re-derive current state from ttorch status, ttorch peek <id>, and git/PR state plus your task list before you report, dispatch, land, or yield — never act on remembered or assumed state. When a worker looks stuck, idle, or slow, ASK it for its state or keep observing — never assert a stall you cannot verify (a repeated-looking progress counter is not evidence) and never command a worker to stop, abandon, or discard work on inference; the worker has ground truth about its own execution, you (reading pane output) do not, so phrase steers as questions or options unless you have direct evidence. (2) Lead-manager only: the lead talks only to you in this manager tab, and you own all worker interaction (dispatch, steer, review, land) and surface every decision and question here — never expect the lead to open or type into a worker tab. (3) This manager tab is for orchestration only: no substantive work, including code review, runs here — delegate it to worker tabs, and run adversarial review in an INDEPENDENT worker, never the author of the code. (4) Keep the fleet moving: model the work as a queue, keep every worker slot that safely can be busy busy, and before ending any turn dispatch every backlog task whose files are DISJOINT from all in-flight workers — serialize only on real file-overlap or dependencies; idling a slot while disjoint work waits is a defect. Run a PRE-YIELD checklist so reporting is a gate, not a stop. (5) Autonomy loop: when woken (the supervisor pokes you on actionable wakes) drain the wake queue and advance ALL actionable state — land green workers, answer or redispatch blocked ones, dispatch disjoint backlog — then idle; the lead is an INTERRUPT, not the sole driver. Dispatch each task with: ttorch spawn <task-id> <repo-path> (add --scout for investigation-only). Supervise with ttorch status, ttorch peek <id>, and ttorch send <id> <text>; validate with ttorch validate <id>; review with ttorch review-diff <id>; and report plain outcomes (ready, blocked, or needs-your-decision). The only commands you run in this window are ttorch orchestration commands. Never merge or deliver without the lead's explicit approval. The SOLE exception is a repository the lead has explicitly set to TRUSTED delivery mode: there, a passing adversarial-review verdict (ttorch trust prep|record, per the ttorch-review skill) plus a fresh green validate auto-authorizes ttorch merge-local without a separate lead approval; every other mode still requires the lead's go-ahead. Follow the ttorch-manager and ttorch-review skills for the full protocol."

// InteractiveCommand starts an interactive session (used by `ttorch cc`).
func InteractiveCommand(kind string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind)
	default:
		return kind
	}
}

// ManagerCommand starts the manager session. It runs lighter than workers
// (managerEffortLevel, default "high") and carries the manager charter so the
// session plans and delegates via `ttorch spawn` rather than doing the work
// itself. The session is launched with a stable session id so a later restore can
// resume this exact conversation. charterFile, when set, is passed via
// --append-system-prompt-file so the launched command stays short instead of
// inlining the whole charter on the command line.
func ManagerCommand(kind, sessionID, charterFile string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + effortArgsForLevel(managerEffortLevel()) +
			managerCharterArg(charterFile) +
			" --session-id " + shq(sessionID)
	default:
		return kind
	}
}

// managerCharterArg applies the manager charter: as a file reference when a path
// is given (keeps the launched command short), otherwise inlined.
func managerCharterArg(charterFile string) string {
	if charterFile != "" {
		return " --append-system-prompt-file " + shq(charterFile)
	}
	return " --append-system-prompt " + shq(managerCharter)
}

// WriteManagerCharter writes the manager charter to path so it can be passed via
// --append-system-prompt-file. It overwrites any existing file (the charter is
// ttorch-managed and may change across versions).
func WriteManagerCharter(path string) error {
	return os.WriteFile(path, []byte(managerCharter+"\n"), 0o644)
}

// ManagerResumeCommand resumes the manager conversation after a stop/reboot/
// upgrade. It keeps the manager's effort and charter but resumes the prior
// conversation rather than starting a new one: it uses --resume <sessionID>, or
// --continue (most recent conversation in the launch directory) when no id is
// known (legacy state).
func ManagerResumeCommand(kind, sessionID, charterFile string) string {
	switch kind {
	case "claude":
		base := "claude --dangerously-skip-permissions" + effortArgsForLevel(managerEffortLevel()) +
			managerCharterArg(charterFile)
		if sessionID == "" {
			return base + " --continue"
		}
		return base + " --resume " + shq(sessionID)
	default:
		return kind
	}
}

// BriefCommand starts the harness with a task brief as its initial prompt. It is
// launched with a stable session id so a later restore can resume this exact
// worker conversation.
func BriefCommand(kind, briefPath, sessionID string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind) +
			" --session-id " + shq(sessionID) +
			" \"$(cat " + quote(briefPath) + ")\""
	default:
		return kind
	}
}

// ResumeCommand resumes a worker conversation after a stop/reboot/upgrade with no
// brief: it continues the exact conversation via --resume <sessionID>, or
// --continue (most recent conversation in the worktree) when no id is known
// (legacy state).
func ResumeCommand(kind, sessionID string) string {
	switch kind {
	case "claude":
		base := "claude --dangerously-skip-permissions" + EffortArgs(kind)
		if sessionID == "" {
			return base + " --continue"
		}
		return base + " --resume " + shq(sessionID)
	default:
		return kind
	}
}

// ManagerResumeOrFresh resumes the manager conversation, falling back to a fresh
// manager (same session id) if the resume fails — e.g. the saved conversation was
// never persisted, or its id/directory no longer resolves. Without the fallback a
// failed `--resume` would exit and strand the lead at a shell prompt.
func ManagerResumeOrFresh(kind, sessionID, charterFile string) string {
	if kind != "claude" {
		return ManagerResumeCommand(kind, sessionID, charterFile)
	}
	return ManagerResumeCommand(kind, sessionID, charterFile) + " || " + ManagerCommand(kind, sessionID, charterFile)
}

// WorkerResumeOrFresh resumes a worker conversation, falling back to relaunching
// it from its brief (same session id) if the resume fails, so a worker is never
// left at a dead shell after a stop/reboot/upgrade.
func WorkerResumeOrFresh(kind, sessionID, briefPath string) string {
	if kind != "claude" {
		return ResumeCommand(kind, sessionID)
	}
	return ResumeCommand(kind, sessionID) + " || " + BriefCommand(kind, briefPath, sessionID)
}

// NewSessionID returns a random RFC-4122 version-4 UUID, used as a stable Claude
// Code session id so a session can be resumed later. It uses crypto/rand and no
// external dependency.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; if it does, panic rather than emit a
		// predictable id that could collide with a real conversation.
		panic("harness: crypto/rand: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// shq single-quotes s into one shell word, neutralizing shell metacharacters
// (an embedded single quote is closed, escaped as \', and reopened).
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

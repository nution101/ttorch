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
	"regexp"
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

// EffortArgs returns the extra `claude` arguments that set the reasoning effort for a
// spawned worker (or the `ttorch cc` session). level is the explicit per-task choice
// (e.g. from `spawn --effort`); when it is empty the level falls back to TTORCH_EFFORT
// and finally the "ultracode" default (effortLevel). A level (or TTORCH_EFFORT) of off
// leaves Claude's own default untouched. Non-claude harnesses never get effort args.
func EffortArgs(kind, level string) string {
	if kind != "claude" {
		return ""
	}
	if l := strings.ToLower(strings.TrimSpace(level)); l != "" {
		return effortArgsForLevel(l)
	}
	return effortArgsForLevel(effortLevel())
}

// EffortLevels are the per-task reasoning-effort levels `ttorch spawn --effort` accepts.
// low|medium|high|xhigh|max go through claude's --effort flag; ultracode enables the
// ultracode session feature (xhigh reasoning + dynamic workflow orchestration); off
// leaves claude's own default untouched.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max", "ultracode", "off"}

// ValidEffort reports whether s names a recognized effort level (case-insensitive). The
// empty string is NOT valid here — it means "unset", which callers handle separately —
// so a flag value that is set is always one of EffortLevels.
func ValidEffort(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, l := range EffortLevels {
		if l == s {
			return true
		}
	}
	return false
}

// ResolveWorkerEffort resolves the reasoning-effort level to launch a worker with. The
// order is: the explicit per-task choice (e.g. `spawn --effort`), then the global
// TTORCH_EFFORT env, then the kind default — "high" for a scout (investigation only, it
// does not need ultracode) and "ultracode" for a ship worker. The result is a concrete
// level the spawn path persists on the task row so a later resume restores the same
// effort. It is always non-empty.
func ResolveWorkerEffort(explicit string, scout bool) string {
	if e := strings.ToLower(strings.TrimSpace(explicit)); e != "" {
		return e
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_EFFORT"))); v != "" {
		return v
	}
	if scout {
		return "high"
	}
	return "ultracode"
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

// ModelAliases are the short model names ttorch recognizes for --model without a full id.
// They mirror claude's own aliases; the alias→concrete-model mapping (and its drift across
// releases) is claude's to own, not ttorch's.
var ModelAliases = []string{"haiku", "sonnet", "opus", "fable", "opusplan", "default"}

// modelIDRe matches a full/prefixed model id (e.g. claude-opus-4-8,
// us.anthropic.claude-3-5-..., anthropic/claude-...): a conservative id charset. It is only
// consulted for a value that carries a separator, so a bare-word typo (e.g. "opuss") is
// rejected while a real full id is accepted.
var modelIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)

// isModelAlias reports whether lower (already lowercased) is one of the known aliases.
func isModelAlias(lower string) bool {
	for _, a := range ModelAliases {
		if a == lower {
			return true
		}
	}
	return false
}

// NormalizeModel canonicalizes a model value so the validated, persisted, resolved, and
// launched forms all agree: a known alias is lowercased (claude's aliases are lowercase and
// matched exactly), while a full/prefixed model id keeps its original case (ids can be
// case-sensitive). This mirrors the effort dial, which lowercases end-to-end. Empty (and a
// whitespace-only value) normalizes to "".
func NormalizeModel(s string) string {
	s = strings.TrimSpace(s)
	if lower := strings.ToLower(s); isModelAlias(lower) {
		return lower
	}
	return s
}

// ValidModel reports whether s names a model ttorch will pass to claude: a known alias
// (case-insensitive) or a full/prefixed model id. A bare word that is not a known alias is
// treated as a typo and rejected (so `--model opuss` fails loudly, like an unknown
// --effort). The empty string is NOT valid here — it means "unset", which callers handle
// separately. Note the "default" alias IS valid (ModelArgs maps it, like the "off"/"none"
// sentinels, to no --model flag); only "off"/"none" are rejected here.
func ValidModel(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	if isModelAlias(s) {
		return true
	}
	// A bare word (no separator) that is not a known alias is a typo; only a value shaped
	// like a full/prefixed id is accepted beyond the alias list.
	if strings.ContainsAny(s, "-._:/") {
		return modelIDRe.MatchString(s)
	}
	return false
}

// ModelArgs returns the extra `claude` arguments that pin the model for a spawned worker
// (or the manager / `ttorch cc` session). model is the resolved choice; an empty value — or
// the "off"/"none"/"default" sentinels — emits nothing, leaving claude's own default
// untouched (so the model dial stays opt-in and non-breaking). Non-claude harnesses never
// get model args. This mirrors EffortArgs; model and effort are orthogonal (which brain vs
// how hard it thinks) and compose on the launch command.
func ModelArgs(kind, model string) string {
	if kind != "claude" {
		return ""
	}
	m := NormalizeModel(model)
	switch strings.ToLower(m) {
	case "", "off", "none", "default":
		return ""
	}
	return " --model " + shq(m)
}

// ResolveWorkerModel resolves the model to launch a worker with: the explicit per-task
// choice (e.g. `spawn --model`) wins, then the global TTORCH_MODEL env, else "" (unset).
//
// Unlike ResolveWorkerEffort — which carries a hard kind default (ultracode) inherited from
// before the model dial existed — an unset model resolves to "" so no --model is passed and
// claude's own default applies. This keeps the model dial opt-in and non-breaking; the
// dispatch-time tier policy (a cheap model per task class) lives in the scheduler's
// classifier, which fills the model only when it is unset on the task row.
func ResolveWorkerModel(explicit string) string {
	if e := NormalizeModel(explicit); e != "" {
		return e
	}
	return EnvWorkerModel()
}

// managerModelLevel is the manager's model, from TTORCH_MANAGER_MODEL (default "" — claude's
// own default). It is separate from the worker default (TTORCH_MODEL) so the plan-heavy
// manager and the workers can run on different models, mirroring the TTORCH_MANAGER_EFFORT /
// TTORCH_EFFORT split.
func managerModelLevel() string {
	return NormalizeModel(os.Getenv("TTORCH_MANAGER_MODEL"))
}

// EnvWorkerEffort and EnvWorkerModel expose the raw worker env defaults (TTORCH_EFFORT /
// TTORCH_MODEL, "" if unset) so the scheduler's dispatch-time tier classifier can slot BELOW
// an explicit per-task value but ABOVE its own complexity guess. Keeping these here makes the
// launch path the single source of truth for the env var names, rather than the scheduler
// re-reading os.Getenv itself. EnvWorkerEffort lowercases to match ResolveWorkerEffort.
func EnvWorkerEffort() string { return strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_EFFORT"))) }
func EnvWorkerModel() string  { return NormalizeModel(os.Getenv("TTORCH_MODEL")) }

// managerCharter is appended to the manager session's system prompt so the
// session always acts as the orchestrator regardless of how the lead phrases a
// request. It is one line (it is sent through tmux).
const managerCharter = "You are the ttorch MANAGER for this tmux session; the person you talk to is the lead. You PLAN and DELEGATE — you do not write code, edit files, or run a project's build/test/lint commands in this window. Run the team as a continuous loop, not a one-shot checklist. A deterministic SCHEDULER runs alongside you by default (disable with TTORCH_SCHEDULER_AUTOSTART=0) and drives the mechanical loop: it dispatches ready backlog (launching each worker with the brief you stored on the task), lands already-gated work, and recovers workers that verifiably died — so you no longer hand-dispatch disjoint backlog or hand-run ttorch land each turn. Your job is the judgment it cannot do: plan and brief the work, GATE finished work (it lands only already-gated work), answer blocked/needs-input workers, surface non-trusted merges for the lead's approval (the lead approves — you never self-approve), and report. (1) The live board is your source of truth: at every check-in re-derive current state from ttorch status, ttorch peek <id>, and git/PR state plus your task list before you report, dispatch, land, or yield — never act on remembered or assumed state. When a worker looks stuck, idle, or slow, ASK it for its state or keep observing — never assert a stall you cannot verify (a repeated-looking progress counter is not evidence) and never command a worker to stop, abandon, or discard work on inference; the worker has ground truth about its own execution, you (reading pane output) do not, so phrase steers as questions or options unless you have direct evidence. (2) Lead-manager only: the lead talks only to you in this manager tab, and you own all worker interaction (steer, review, gate, relay the lead's input — the scheduler dispatches/lands/recovers on your behalf, never the lead's) and surface every decision and question here — never expect the lead to open or type into a worker tab. (3) This manager tab is for orchestration only: no substantive work, including code review, runs here — delegate it to worker tabs, and run adversarial review in an INDEPENDENT worker, never the author of the code. (4) Keep the fleet moving — by planning, not hand-dispatching: the scheduler continuously dispatches every backlog task whose files are DISJOINT from all in-flight workers, so your part is to plan tasks it can dispatch hands-off — give each a precise, file-granular footprint and a stored brief (ttorch task add <id> --touches ... --brief-file ...); serialize only on real file-overlap or dependencies, and spawn directly only for work it will not pick up. Run a PRE-YIELD checklist so reporting is a gate, not a stop. (5) Autonomy loop: the scheduler advances the mechanical work on its own, but only you gate finished work, answer a blocked worker, and surface a non-trusted merge for the lead's approval — so after each turn in which you are NOT awaiting the lead, arm ttorch watch as a background task; when it returns an actionable batch, re-derive from the DB and advance the decisions only you can make — gate finished workers so the scheduler can land them, answer or redispatch blocked ones, surface for the lead's approval any non-trusted merge that waits — then re-arm ttorch watch. When you surface a decision to the lead, FIRST cancel any in-flight watcher and do not re-arm — the window waits silently until the lead returns; the lead is an INTERRUPT, not the sole driver. Plan and store each task with ttorch task add <id> --project <p> --touches ... --brief-file ... so the scheduler dispatches it; spawn one yourself with ttorch spawn <task-id> <repo-path> (add --scout for investigation-only) only when you want it started now or the scheduler will not pick it up. Supervise by exception with ttorch peek <id> and ttorch send <id> <text> (the scheduler recovers crashed workers; you step in on blocked/needs-input/off-track); validate with ttorch validate <id>; gate with ttorch trust prep|record (adversarial review in an independent worker); and report plain outcomes (ready, blocked, or needs-your-decision). The only commands you run in this window are ttorch orchestration commands. Never merge or deliver without the lead's explicit approval. The SOLE exception is a repository the lead has explicitly set to TRUSTED delivery mode: there, a passing adversarial-review verdict (ttorch trust prep|record, per the ttorch-review skill) plus a fresh green validate auto-authorizes the merge without a separate lead approval; every other mode still requires the lead's go-ahead. The scheduler performs the land in each case, only after the gate — and, outside trusted, the lead's approval — is satisfied, never ungated. Follow the ttorch-manager and ttorch-review skills for the full protocol."

// InteractiveCommand starts an interactive session (used by `ttorch cc`). It honors the
// worker model default (TTORCH_MODEL) and effort default, matching a spawned worker.
func InteractiveCommand(kind string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind, "") + ModelArgs(kind, ResolveWorkerModel(""))
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
			ModelArgs(kind, managerModelLevel()) +
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
			ModelArgs(kind, managerModelLevel()) +
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
// launched with a stable session id so a later restore can resume this exact worker
// conversation. effort is the resolved per-task reasoning effort (see
// ResolveWorkerEffort); "" defers to TTORCH_EFFORT / the default. model is the resolved
// per-task model (see ResolveWorkerModel); "" passes no --model, leaving claude's default.
func BriefCommand(kind, briefPath, sessionID, effort, model string) string {
	switch kind {
	case "claude":
		return "claude --dangerously-skip-permissions" + EffortArgs(kind, effort) + ModelArgs(kind, model) +
			" --session-id " + shq(sessionID) +
			" \"$(cat " + quote(briefPath) + ")\""
	default:
		return kind
	}
}

// ResumeCommand resumes a worker conversation after a stop/reboot/upgrade with no
// brief: it continues the exact conversation via --resume <sessionID>, or
// --continue (most recent conversation in the worktree) when no id is known
// (legacy state). effort is the persisted per-task reasoning effort the resumed
// session relaunches at; "" defers to TTORCH_EFFORT / the default (legacy rows). model is
// the persisted per-task model; "" passes no --model (legacy rows / claude's default).
func ResumeCommand(kind, sessionID, effort, model string) string {
	switch kind {
	case "claude":
		base := "claude --dangerously-skip-permissions" + EffortArgs(kind, effort) + ModelArgs(kind, model)
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
// left at a dead shell after a stop/reboot/upgrade. effort and model are the persisted
// per-task reasoning effort and model both the resume and the re-brief relaunch at.
func WorkerResumeOrFresh(kind, sessionID, briefPath, effort, model string) string {
	if kind != "claude" {
		return ResumeCommand(kind, sessionID, effort, model)
	}
	return ResumeCommand(kind, sessionID, effort, model) + " || " + BriefCommand(kind, briefPath, sessionID, effort, model)
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

// WriteWorkerSettings writes a worktree-local Claude Code settings file that
// disables the AI co-author trailer, so a worker's commits are authored as the
// repo's git user. For Claude Code it writes <worktree>/.claude/settings.local.json
// and keeps it out of git's view. Other harnesses are no-ops for now.
//
// Workers report their turn boundaries and status through the DB (`ttorch report`),
// so no Stop hook is installed; this file carries only the co-author setting.
func WriteWorkerSettings(kind, worktree string) error {
	if kind != "claude" {
		return nil
	}
	type settings struct {
		// IncludeCoAuthoredBy=false stops the agent from adding an AI co-author
		// trailer to commits — work is authored as the repo's git user, not the agent.
		IncludeCoAuthoredBy bool `json:"includeCoAuthoredBy"`
	}
	dir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(settings{IncludeCoAuthoredBy: false}, "", "  ")
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

// WorkerLaunchPrefix builds the leading environment assignments prepended to a
// worker's launch command so the worker's `ttorch report/stage/note/follow-on`
// resolve their task and DB from the environment without a flag (§3.1). Each value
// is single-quoted so a path or id with shell metacharacters survives verbatim; an
// empty value is omitted. The returned string ends in a trailing space (ready to
// prepend) or is empty when nothing is set.
func WorkerLaunchPrefix(taskID, dbPath string) string {
	var parts []string
	if taskID != "" {
		parts = append(parts, "TTORCH_TASK_ID="+shq(taskID))
	}
	if dbPath != "" {
		parts = append(parts, "TTORCH_DB="+shq(dbPath))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// workerTaskFileRel is the worktree-relative path of the git-excluded file a spawned
// worker carries so its `ttorch report/stage/note/follow-on` can resolve the task +
// DB by walking up from cwd, independent of any environment (§3.1).
const workerTaskFileRel = ".ttorch/task"

// WriteWorkerTaskFile writes <worktree>/.ttorch/task recording the task id and DB
// path, then keeps it out of git's view via the same exclusion mechanism as the
// Stop hook (excludeInWorktree). The format is one `key=value` line per field so it
// stays human-readable and forward-extensible; ReadWorkerTaskFile is its reader.
func WriteWorkerTaskFile(worktree, taskID, dbPath string) error {
	dir := filepath.Join(worktree, ".ttorch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body := "task_id=" + taskID + "\ndb=" + dbPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "task"), []byte(body), 0o644); err != nil {
		return err
	}
	return excludeInWorktree(worktree, workerTaskFileRel)
}

// ReadWorkerTaskFile parses a <dir>/.ttorch/task file, returning the recorded task
// id and DB path. ok is false when the file is absent or carries no task id. It does
// not walk up the tree — the caller decides the search policy (§3.1). Unknown keys
// and blank lines are ignored so the format can grow without breaking older readers.
func ReadWorkerTaskFile(dir string) (taskID, dbPath string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".ttorch", "task"))
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "task_id":
			taskID = strings.TrimSpace(val)
		case "db":
			dbPath = strings.TrimSpace(val)
		}
	}
	return taskID, dbPath, taskID != ""
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

// Package cli implements ttorch's command dispatch. Milestone M0 ships the
// distribution surface (install / update / uninstall / doctor / version);
// orchestration verbs are stubbed with a clear "coming soon" message.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	ttorchembed "github.com/nution101/ttorch"
	"github.com/nution101/ttorch/internal/buildinfo"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/doctor"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/installer"
	"github.com/nution101/ttorch/internal/learnings"
	"github.com/nution101/ttorch/internal/manifest"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/profile"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/scheduler"
	"github.com/nution101/ttorch/internal/selfupdate"
	"github.com/nution101/ttorch/internal/singleton"
	"github.com/nution101/ttorch/internal/skills"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/watch"
	"github.com/nution101/ttorch/internal/worktree"
)

// repo is the GitHub slug releases are fetched from. Update when publishing.
const repo = "nution101/ttorch"

func assetName(tag string) string {
	return fmt.Sprintf("ttorch-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// Main runs ttorch and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		// Bare `ttorch` launches the manager session (auto-restoring saved state).
		m, err := mgr()
		if err != nil {
			return run(err)
		}
		defer m.Close()
		skills.EnsureInstalled(os.Stderr)
		return run(m.StartManager())
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(buildinfo.String())
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "doctor":
		return run(cmdDoctor(rest))
	case "install":
		return run(cmdInstall())
	case "update":
		return run(cmdUpdate(rest))
	case "uninstall":
		return run(cmdUninstall(rest))
	case "init":
		return run(cmdInit(rest))
	case "profile":
		return run(cmdProfile(rest))
	case "skills":
		return run(cmdSkills(rest))
	case "manager":
		m, err := mgr()
		if err != nil {
			return run(err)
		}
		defer m.Close()
		skills.EnsureInstalled(os.Stderr)
		return run(m.StartManager())
	case "resume":
		return run(cmdResume())
	case "reset":
		return run(cmdReset(rest))
	case "stop":
		return run(cmdStop())
	case "cc":
		return run(cmdCC(rest))
	case "spawn":
		return run(cmdSpawn(rest))
	case "report":
		return run(cmdReport(rest))
	case "stop-hook":
		return run(cmdStopHook(os.Stdin, os.Stdout))
	case "stage":
		return run(cmdStage(rest))
	case "note":
		return run(cmdNote(rest))
	case "follow-on":
		return run(cmdFollowOn(rest))
	case "status":
		return run(cmdStatus())
	case "tasks":
		return run(cmdTasks(rest))
	case "project":
		return run(cmdProject(rest))
	case "epic":
		return run(cmdEpic(rest))
	case "phase":
		return run(cmdPhase(rest))
	case "task":
		return run(cmdTask(rest))
	case "check-overlap":
		return run(cmdCheckOverlap(rest))
	case "peek":
		return run(cmdPeek(rest))
	case "send":
		return run(cmdSend(rest))
	case "teardown":
		return run(cmdTeardown(rest))
	case "wait":
		return run(cmdWait(rest))
	case "watch":
		return run(cmdWatch(rest))
	case "await-lead":
		return run(cmdAwaitLead(rest))
	case "watchdog":
		return run(cmdWatchdog(rest))
	case "scheduler":
		return run(cmdScheduler(rest))
	case "validate":
		return run(cmdValidate(rest))
	case "ci-parity":
		return run(cmdCIParity(rest))
	case "review-diff":
		return run(cmdReviewDiff(rest))
	case "approve":
		return run(cmdApprove(rest))
	case "trust":
		return run(cmdTrust(rest))
	case "security-review":
		return run(cmdSecurityReview(rest))
	case "qa-review":
		return run(cmdQAReview(rest))
	case "merge-local":
		return run(cmdMergeLocal(rest))
	case "land":
		return run(cmdLand(rest))
	case "promote":
		return run(cmdPromote(rest))
	case "pr-check":
		return run(cmdPRCheck(rest))
	case "fleet-sync":
		return run(cmdFleetSync(rest))
	case "recovery":
		return run(cmdRecovery())
	case "learn":
		return run(cmdLearn(rest))
	case "learnings":
		return run(cmdLearnings(rest))
	case "worker", "skill":
		fmt.Fprintf(os.Stderr, "ttorch %s: not available yet — arrives in a later milestone.\n", cmd)
		return 3
	default:
		fmt.Fprintf(os.Stderr, "ttorch: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

func run(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "install missing tools without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return doctor.Run(os.Stdout, os.Stdin, *yes)
}

func cmdInstall() error {
	p := paths.Default()
	res, err := installer.Apply(ttorchembed.Content, p, buildinfo.CurrentVersion())
	if err != nil {
		return err
	}
	printResult(os.Stdout, res)
	fmt.Printf("Installed into %s and %s.\n", p.Claude, p.Agents)
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	contentOnly := fs.Bool("content-only", false, "re-apply embedded content without replacing the binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths.Default()

	if *contentOnly {
		return reapplyContent(p)
	}

	latest, err := selfupdate.Latest(repo, nil)
	if errors.Is(err, selfupdate.ErrNoReleases) {
		fmt.Println("No releases published yet; refreshing content from the current binary.")
		return reapplyContent(p)
	}
	if err != nil {
		return err
	}
	cur := buildinfo.CurrentVersion()
	if selfupdate.CompareVersions(latest, cur) <= 0 {
		fmt.Printf("Already up to date (%s).\n", cur)
		return reapplyContent(p)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Printf("Updating %s -> %s ...\n", cur, latest)
	cfg := selfupdate.Config{
		Repo:           repo,
		CurrentVersion: cur,
		ExecutablePath: exe,
		AssetName:      assetName(latest),
	}
	if err := cfg.Apply(latest); err != nil {
		return err
	}
	fmt.Println("Binary updated. Applying new content ...")
	// Re-exec the freshly written binary to apply its embedded content.
	c := exec.Command(exe, "install")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove ~/.ttorch state and data")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := installer.Uninstall(paths.Default(), *purge)
	if err != nil {
		return err
	}
	fmt.Printf("Removed %d managed file(s).\n", res.Report.Count(manifest.Retired))
	for _, f := range res.Report.Conflicts() {
		fmt.Printf("  kept: %s (%s)\n", f.Path, f.Note)
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Println("  " + n)
		}
	}
	return nil
}

func cmdSkills(args []string) error {
	if len(args) > 0 && args[0] == "install" {
		fs := flag.NewFlagSet("skills", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "install without prompting")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return skills.Install(os.Stdout, os.Stdin, *yes)
	}
	skills.List(os.Stdout)
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	mode := fs.String("mode", "pr", "delivery mode for this repo: pr | local | validated | trusted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	notes, err := projectinit.Init(dir, *mode)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	// Keep the DB delivery-mode DISPLAY cache in step with the block we just wrote, so
	// `ttorch project ls` stops showing a stale mode for an already-registered repo.
	// Best-effort: the gate always re-reads the authoritative mode from AGENTS.md
	// (§0.3), so a DB sync failure must never fail `init`.
	if note, err := syncProjectModeCache(dir, *mode); err != nil {
		fmt.Printf("  note: could not sync project delivery-mode cache: %v\n", err)
	} else if note != "" {
		fmt.Println("  " + note)
	}
	if p, err := profile.Apply(dir); err == nil {
		stack := p.Stack
		if stack == "" {
			stack = "unknown"
		}
		fmt.Printf("  wrote project profile (stack: %s) — commit AGENTS.md so workers pick it up\n", stack)
	}
	// Opt-in, default-off, evidence-gated codegraph worker code-navigation. When the feature
	// is disabled (the default) or codegraph is not installed, this is a clean no-op that
	// prints nothing — `ttorch init` behaves exactly as it does without codegraph.
	if notes, err := projectinit.SetupCodegraph(dir); err != nil {
		fmt.Printf("  note: codegraph setup: %v\n", err)
	} else {
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}
	return nil
}

// syncProjectModeCache refreshes the DB delivery-mode DISPLAY cache for the repo at
// dir to match the block `ttorch init` just wrote. It is best-effort and never the
// source of truth: a dir outside any git repo, or a repo with no registered project
// row, is a benign no-op; the gate always resolves the enforced mode from AGENTS.md
// (§0.3), never from this cache. It returns a human-readable note only when a row was
// actually updated.
func syncProjectModeCache(dir, mode string) (string, error) {
	repoRoot, err := worktree.RepoRoot(dir)
	if err != nil {
		return "", nil // not inside a git repo → no project row to refresh
	}
	m, err := mgr()
	if err != nil {
		return "", err
	}
	defer m.Close()
	updated, err := m.Store.SetProjectModeByRepo(context.Background(), repoRoot, mode)
	if err != nil {
		return "", err
	}
	if !updated {
		return "", nil // repo not registered as a project (run `ttorch project add`)
	}
	return "synced project delivery-mode cache → " + mode, nil
}

func cmdProfile(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	p, err := profile.Apply(dir)
	if err != nil {
		return err
	}
	stack := p.Stack
	if stack == "" {
		stack = "unknown"
	}
	fmt.Printf("Updated the project profile in %s/AGENTS.md (stack: %s).\n", dir, stack)
	if len(p.Exemplars) > 0 {
		fmt.Printf("  exemplars: %s\n", strings.Join(p.Exemplars, ", "))
	}
	fmt.Println("  Commit AGENTS.md so workers pick it up.")
	return nil
}

// mgr opens a Manager (and so the SQLite state store, which can fail to open or
// migrate). Each short-lived CLI command opens exactly one and closes it with
// defer m.Close() — one Open + Close per process (§2.4). The error propagates to the
// shared run(err) exit path.
func mgr() (*orchestrator.Manager, error) { return orchestrator.New(paths.Default()) }

func cmdSpawn(args []string) error {
	// Task id and repo are the first two positionals; flags follow (the stdlib
	// flag parser stops at the first positional, so parse the remainder).
	if len(args) < 2 {
		return errors.New(`usage: ttorch spawn <task-id> <repo-path> [--scout] [--init] [--touches "a,b"] [--brief-file <path> | --brief "..."] [--effort <level>] [--model <m>] [--force-overlap] [--cmd "..."]`)
	}
	id, repo := args[0], args[1]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	scout := fs.Bool("scout", false, "investigation task: report only, no code changes")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh|max|ultracode|off (default: $TTORCH_EFFORT, else ultracode for ship / high for scout)")
	model := fs.String("model", "", "model: haiku|sonnet|opus|fable|opusplan or a full model id (default: $TTORCH_MODEL, else claude's own default)")
	doInit := fs.Bool("init", false, "force first-use setup (AGENTS.md block + CLAUDE.md symlink) even when the repo tracks AGENTS.md, which auto-init declines; plain spawn auto-inits only when tracked-file-safe (TTORCH_NO_AUTOINIT=1 to skip)")
	touches := fs.String("touches", "", `comma-separated file paths/prefixes this task will touch; refuses to dispatch onto files a live worker already holds`)
	briefFile := fs.String("brief-file", "", "path to a file whose contents become the worker's initial prompt (the full brief), instead of the generic stub")
	brief := fs.String("brief", "", "inline brief text used as the worker's initial prompt, instead of the generic stub")
	forceOverlap := fs.Bool("force-overlap", false, "dispatch even if the footprint overlaps a live worker (override the conflict refusal)")
	raw := fs.String("cmd", "", "raw command to run instead of the default harness launch")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	// Reject an unknown --effort before any side effect, naming the accepted levels, so a
	// typo fails loudly rather than silently launching the worker at the ultracode default.
	if *effort != "" && !harness.ValidEffort(*effort) {
		return fmt.Errorf("spawn: invalid --effort %q (want one of: %s)", *effort, strings.Join(harness.EffortLevels, "|"))
	}
	// Reject an unknown --model before any side effect too, so a typo fails loudly rather
	// than launching the worker with a --model claude will reject.
	if *model != "" && !harness.ValidModel(*model) {
		return fmt.Errorf("spawn: invalid --model %q (want an alias %s, or a full model id)", *model, strings.Join(harness.ModelAliases, "|"))
	}
	// Resolve the brief before any side effect so a bad --brief/--brief-file fails the
	// spawn loudly rather than silently launching the worker on the generic stub.
	briefContent, err := resolveBrief("spawn", *brief, *briefFile)
	if err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if *doInit {
		notes, err := m.InitRepo(repo, "pr")
		if err != nil {
			return err
		}
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}
	// Write the manager-supplied brief to the task's brief path BEFORE launching, so the
	// worker starts with the full brief as its initial prompt (Spawn reads this file and
	// only falls back to the stub when it is absent).
	if briefContent != "" {
		if err := m.WriteBrief(id, briefContent); err != nil {
			return fmt.Errorf("spawn: writing brief for %q: %w", id, err)
		}
	}
	// Install any missing recommended agent skills before the worker's tmux session comes
	// up, so it inherits them from its first turn. Best-effort: never blocks the spawn.
	skills.EnsureInstalled(os.Stderr)
	footprint := parseTouches(*touches)
	// Resolve model + effort through the SAME dispatch-time tier policy the autonomous
	// scheduler uses, so an interactive `ttorch spawn` gets cheap-by-default tiering (a scout
	// on haiku, a normal ship on sonnet, a risk-path ship on opus) instead of falling through
	// to claude's default model at ultracode. Precedence is preserved: an explicit --model/
	// --effort (or a value persisted on the task row) wins over the env default, which wins
	// over the classifier. The task row (if it exists) supplies the title/footprint the
	// classifier reads; the flags override those signals for this dispatch.
	tierTask, _, _ := m.Store.GetTask(context.Background(), id)
	tierTask.Kind = db.KindShip
	if *scout {
		tierTask.Kind = db.KindScout
	}
	if len(footprint) > 0 {
		tierTask.Footprint = footprint
	}
	if *effort != "" {
		tierTask.Effort = *effort
	}
	if *model != "" {
		tierTask.Model = *model
	}
	tierModel, tierEffort, autoTiered := scheduler.ResolveDispatchTier(tierTask)
	t, err := m.SpawnWithEffort(id, repo, *scout, *raw, footprint, *forceOverlap, tierEffort, tierModel)
	if err != nil {
		return err
	}
	// Record whether the tier was classifier-derived so a later retry may escalate the model
	// (auto) or must respect a user/env pin — mirroring the scheduler's autonomous dispatch.
	_ = m.Store.SetTaskAutoTiered(context.Background(), id, autoTiered)
	modelLabel := t.Model
	if modelLabel == "" {
		modelLabel = "default"
	}
	fmt.Printf("spawned %s (%s, model %s, effort %s) in window %s\n  worktree: %s\n", t.ID, t.Kind, modelLabel, t.Effort, t.Window, t.Worktree)
	if len(t.Footprint) > 0 {
		note := ""
		if *forceOverlap {
			note = " (overlap forced)"
		}
		fmt.Printf("  touches: %s%s\n", strings.Join(t.Footprint, ", "), note)
	}
	return nil
}

// parseTouches splits a --touches value into a normalized footprint: entries are
// comma-separated, trimmed, path-cleaned (so "internal/cli/" and "./internal/cli"
// tidy to "internal/cli"), de-duplicated, and empties dropped. Order is preserved,
// so the result is deterministic.
func parseTouches(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(s, ",") {
		f := strings.TrimSpace(raw)
		if f == "" {
			continue
		}
		f = path.Clean(f)
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// resolveBrief returns the brief content from the --brief / --brief-file flags, or "" when
// neither is set (the worker then gets the generic stub). cmd names the calling subcommand for
// error messages (e.g. "spawn", "task add"), which `ttorch spawn` and `ttorch task add` share.
// It errors if both are set (ambiguous) or if --brief-file is unreadable or empty, so a bad
// invocation fails before any side effect rather than silently launching the worker on the stub.
func resolveBrief(cmd, brief, briefFile string) (string, error) {
	if brief != "" && briefFile != "" {
		return "", fmt.Errorf("%s: pass only one of --brief or --brief-file", cmd)
	}
	if briefFile != "" {
		b, err := os.ReadFile(briefFile)
		if err != nil {
			return "", fmt.Errorf("%s: reading --brief-file: %w", cmd, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("%s: --brief-file %q is empty", cmd, briefFile)
		}
		return string(b), nil
	}
	return brief, nil
}

// --- worker-facing reporting (§3.1) -----------------------------------------
//
// report/stage/note/follow-on let the CALLING worker write to its OWN task without
// the orchestrator/tmux machinery: each is short-lived (one db.Open + defer Close),
// mutates exactly one task, and validates its input.
//
// The audit ACTOR is the real calling worker's identity — resolved from
// $TTORCH_TASK_ID, then the worktree's .ttorch/task (cwd walk-up), both written by the
// manager at spawn — and is NEVER derived from the caller-supplied --task. That keeps
// attribution unforgeable: a worker cannot pass --task <other> to record an event as
// another worker, and the mutation is scoped to its own task (a --task naming a
// different task is rejected). A caller with no worker identity is the manager/lead
// context: it may target any task with --task, attributed to the manager. The actor is
// worker:<id>, so transitioning into done/blocked/needs-input — and only that — wakes
// the manager (§1.3); manager-authored events are non-actionable by construction.
// The DB is resolved by $TTORCH_DB → the .ttorch/task file's db → the default StateDB().

const reportUsage = `usage: ttorch report <done|blocked|needs-input|active> [--task <id>] [-m "msg"]`

func cmdReport(args []string) error {
	if len(args) < 1 {
		return errors.New(reportUsage)
	}
	status, ok := reportStatusValue(args[0])
	if !ok {
		return fmt.Errorf("report: unknown status %q\n%s", args[0], reportUsage)
	}
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	taskFlag := fs.String("task", "", "your own task id (default: $TTORCH_TASK_ID or the worktree's .ttorch/task)")
	msg := fs.String("m", "", "note recorded with the status change (also carried on the event)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	taskID, dbPath, actor, err := resolveWorkerAuth(*taskFlag)
	if err != nil {
		return err
	}
	store, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ev, err := store.ReportStatus(context.Background(), taskID, status, actor, *msg)
	if err != nil {
		return err
	}
	tail := ""
	if ev.Actionable {
		tail = " (the manager has been notified)"
	}
	fmt.Printf("%s → %s%s\n", taskID, status, tail)
	return nil
}

// cmdStopHook is the Claude Code Stop-hook handler installed on every worker session
// (see harness.WriteWorkerSettings). Claude fires it when the worker finishes a turn and is
// about to go idle. If the worker's task is still `active` — it has not reported a
// terminal/blocking status — the hook BLOCKS the stop and reminds the worker to run
// `ttorch report done|blocked|needs-input`. This closes the gap where a worker committed its
// work and idled WITHOUT reporting, leaving finished work invisible to the scheduler (which
// lands only reported/gated work) and the manager (which gates only reported-done work).
//
// It is FAIL-OPEN: any ambiguity — unreadable payload, not a worker context, DB error, or a
// task that already left `active` — allows the stop (no output), so the hook can never wedge
// a session. `stop_hook_active` (Claude sets it when this stop already follows a prior block)
// short-circuits to avoid a block loop. Opt out entirely with TTORCH_NO_STOP_REPORT.
func cmdStopHook(in io.Reader, out io.Writer) error {
	if os.Getenv("TTORCH_NO_STOP_REPORT") != "" {
		return nil
	}
	// The only field we need is stop_hook_active: re-blocking a stop that already follows a
	// prior block would loop, so we stand down in that case.
	var payload struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	if data, err := io.ReadAll(io.LimitReader(in, 1<<20)); err == nil {
		_ = json.Unmarshal(data, &payload) // best-effort; a malformed payload leaves it false
	}
	if payload.StopHookActive {
		return nil
	}
	// Resolve THIS worker's own task from its unforgeable identity ($TTORCH_TASK_ID / the
	// worktree's .ttorch/task), exactly as `ttorch report` does. Not a worker context → allow.
	taskID, dbPath, _, err := resolveWorkerAuth("")
	if err != nil {
		return nil
	}
	store, err := db.Open(dbPath)
	if err != nil {
		return nil
	}
	defer store.Close()
	task, ok, err := store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return nil
	}
	// A task that has reported done/blocked/needs-input (or otherwise left `active`) is already
	// visible to the advance machinery — there is nothing to enforce.
	if task.Status != db.StatusActive {
		return nil
	}
	reason := "ttorch: you are about to go idle without reporting this task's status, so the manager " +
		"and scheduler cannot advance your work (they act only on reported/gated work). Run exactly " +
		"one of these now, from your worktree, then stop:\n" +
		"  - `ttorch report done -m \"<summary>\"` — the work is complete and committed\n" +
		"  - `ttorch report blocked -m \"<why>\"` — you cannot proceed\n" +
		"  - `ttorch report needs-input -m \"<question>\"` — you need a decision\n" +
		"If the task is not actually finished, keep working instead."
	resp := struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{Decision: "block", Reason: reason}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	fmt.Fprintln(out, string(b))
	return nil
}

// reportStatusValue maps a CLI report verb to its db task status. The hyphenated
// needs-input verb maps to the needs_input status enum.
func reportStatusValue(verb string) (string, bool) {
	switch verb {
	case "done":
		return db.StatusDone, true
	case "blocked":
		return db.StatusBlocked, true
	case "needs-input":
		return db.StatusNeedsInput, true
	case "active":
		return db.StatusActive, true
	}
	return "", false
}

const stageUsage = `usage: ttorch stage <text> [--task <id>]`

func cmdStage(args []string) error {
	taskFlag, rest, err := extractTaskFlag(args)
	if err != nil {
		return err
	}
	stage := strings.TrimSpace(strings.Join(rest, " "))
	if stage == "" {
		return errors.New(stageUsage)
	}
	taskID, dbPath, actor, err := resolveWorkerAuth(taskFlag)
	if err != nil {
		return err
	}
	store, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.SetStage(context.Background(), taskID, stage, actor); err != nil {
		return err
	}
	fmt.Printf("%s stage: %s\n", taskID, stage)
	return nil
}

const noteUsage = "usage: ttorch note <text...> | - | --message-file <path> [--task <id>]"

func cmdNote(args []string) error {
	taskFlag, rest, err := extractTaskFlag(args)
	if err != nil {
		return err
	}
	// Reuse send's safe message resolution — inline text, stdin (-), or --message-file —
	// so a note body with shell metacharacters is carried verbatim, never re-evaluated.
	body, err := resolveSendMessage(rest, os.Stdin, stdinIsTerminal())
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("note: empty message — nothing to record")
	}
	taskID, dbPath, actor, err := resolveWorkerAuth(taskFlag)
	if err != nil {
		return err
	}
	store, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.AddNote(context.Background(), taskID, actor, body); err != nil {
		return err
	}
	fmt.Printf("noted on %s\n", taskID)
	return nil
}

const followOnUsage = `usage: ttorch follow-on <new-id> --title "…" [--touches "a,b"] [--task <parent>]`

func cmdFollowOn(args []string) error {
	if len(args) < 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return errors.New(followOnUsage)
	}
	newID := args[0]
	fs := flag.NewFlagSet("follow-on", flag.ContinueOnError)
	title := fs.String("title", "", "one-line title for the follow-on task (required)")
	touches := fs.String("touches", "", "comma-separated files/prefixes the follow-on will touch")
	taskFlag := fs.String("task", "", "parent task id — your own task (default: $TTORCH_TASK_ID or the worktree's .ttorch/task)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" {
		return errors.New("follow-on: --title is required")
	}
	parentID, dbPath, actor, err := resolveWorkerAuth(*taskFlag)
	if err != nil {
		return err
	}
	store, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	parent, ok, err := store.GetTask(ctx, parentID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("follow-on: parent task %q not found", parentID)
	}
	if _, exists, err := store.GetTask(ctx, newID); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("follow-on: task %q already exists", newID)
	}
	// A pending backlog child OF the parent, created BY the caller (worker:<id> or the
	// manager), filed atomically:
	// db.CreateFollowOn writes the row, its canonical 'created' event, and the typed
	// follow_on_created event in ONE transaction (the §1.4 fix tracked from inc2 —
	// previously this path used two separate transactions). Both events are
	// non-actionable: a follow-on is backlog the manager surfaces on its next re-derive,
	// never an interrupt (the lead's decision, §9).
	if _, err := store.CreateFollowOn(ctx, db.Task{
		ID: newID, ProjectID: parent.ProjectID, ParentTaskID: &parentID,
		CreatedBy: actor, Title: *title, Kind: db.KindShip,
		Status: db.StatusPending, Footprint: parseTouches(*touches),
	}, actor, *title); err != nil {
		return err
	}
	fmt.Printf("filed follow-on %s (parent %s)\n", newID, parentID)
	return nil
}

// workerActor is the events.actor / notes.author label for a task's own worker. Only
// a worker actor makes a status transition actionable (§1.3).
func workerActor(taskID string) string { return "worker:" + taskID }

// extractTaskFlag pulls an optional --task <id> (or --task=<id>) out of args,
// returning it plus the remaining arguments in order. The text-bearing verbs (stage,
// note) need it because their free-form body cannot go through the stdlib flag parser
// (which stops at the first positional). Any other flags are left in rest untouched.
func extractTaskFlag(args []string) (taskFlag string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--task":
			if i+1 >= len(args) {
				return "", nil, errors.New("--task: missing <id>")
			}
			taskFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--task="):
			taskFlag = args[i][len("--task="):]
		default:
			rest = append(rest, args[i])
		}
	}
	return taskFlag, rest, nil
}

// resolveWorkerAuth resolves, for the worker-facing verbs (§3.1), the TARGET task to
// mutate, the DB to write it to, and the audit ACTOR — and enforces that attribution is
// not forgeable.
//
// The actor is derived from the REAL caller identity (callerIdentity: $TTORCH_TASK_ID →
// the worktree's .ttorch/task, NEVER --task):
//
//   - Worker context (an identity resolves): the actor is worker:<callerID> and the
//     mutation is scoped to that task. --task may be omitted (defaults to the caller's
//     own id) or must equal it; a --task naming a different task is rejected, so a
//     worker can neither forge another worker's attribution nor touch its row.
//   - Manager/lead context (no identity — not a spawned worker): the actor is the
//     manager and --task selects any task. --task is required here, since there is no
//     own-task default. Manager-authored events are non-actionable by construction
//     (§1.3), so a manager actor cannot be abused to wake the watcher.
//
// DB precedence: $TTORCH_DB → the .ttorch/task file's db → the default StateDB().
func resolveWorkerAuth(taskFlag string) (taskID, dbPath, actor string, err error) {
	callerID, fileDB := callerIdentity()
	dbPath = resolveDBPath(fileDB)
	taskFlag = strings.TrimSpace(taskFlag)
	if callerID == "" {
		// Manager/lead context: target any task, attributed to the manager.
		if taskFlag == "" {
			return "", "", "", errors.New("could not resolve the task: pass --task <id>, set TTORCH_TASK_ID, or run inside a worktree containing .ttorch/task")
		}
		return taskFlag, dbPath, db.ActorManager, nil
	}
	// Worker context: a worker may write only its own task.
	if taskFlag != "" && taskFlag != callerID {
		return "", "", "", fmt.Errorf("refusing to write task %q: a worker may report only its own task (%s) — drop --task or pass --task %s", taskFlag, callerID, callerID)
	}
	return callerID, dbPath, workerActor(callerID), nil
}

// callerIdentity resolves the REAL caller's worker identity — what the manager wrote at
// spawn — from $TTORCH_TASK_ID first, then the worktree's .ttorch/task (cwd walk-up). It
// deliberately ignores --task, so identity (and thus the audit actor) is never
// caller-supplied. An empty id means the caller is not a spawned worker: the
// manager/lead context. The file's recorded DB path is returned too, to serve as the DB
// fallback even when the id came from the environment.
func callerIdentity() (callerID, fileDB string) {
	fileID, fileDB := findTaskFile()
	if env := strings.TrimSpace(os.Getenv("TTORCH_TASK_ID")); env != "" {
		return env, fileDB
	}
	return fileID, fileDB
}

// resolveDBPath picks the state DB: $TTORCH_DB → the .ttorch/task file's db → the
// default StateDB() (§3.1).
func resolveDBPath(fileDB string) string {
	switch {
	case os.Getenv("TTORCH_DB") != "":
		return os.Getenv("TTORCH_DB")
	case fileDB != "":
		return fileDB
	default:
		return paths.Default().StateDB()
	}
}

// findTaskFile walks up from cwd to the first .ttorch/task file, returning its
// recorded task id and DB path (both empty when none is found). The walk stops at the
// filesystem root.
func findTaskFile() (taskID, dbPath string) {
	dir, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	for {
		if id, dbp, ok := harness.ReadWorkerTaskFile(dir); ok {
			return id, dbp
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func cmdStatus() error {
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	live, err := m.Status()
	if err != nil {
		return err
	}
	// statusView splits the FULL live set into the windowed display rows and the free
	// worktree-pool capacity (derived from the full set, see its doc). Pending backlog
	// (task add / follow-on, no window) is excluded from the table; it belongs in
	// `ttorch tasks`, which lists the full hierarchy including backlog.
	rows, free := statusView(live, m.TaskState, m.Pool)
	if len(rows) == 0 {
		fmt.Println("no active workers. dispatch with: ttorch spawn <task-id> <repo-path>")
		return nil
	}
	renderStatus(os.Stdout, rows, free)
	return nil
}

// statusView assembles what `ttorch status` renders, from the FULL live task set: the
// display rows — only spawned/windowed workers, each tagged with its live STATE from
// state (m.TaskState in production) — and the free worktree-pool capacity per repo.
// Capacity is computed over the FULL live set, NOT the windowed row subset: a live
// worktree-holding task that carries no window still occupies a pool slot, so it must
// count against capacity. That mirrors the orchestrator's in-use accounting
// (Manager.inUseWorktrees, which filters on Worktree!="" over liveTasks() and never
// looks at the window), so the summary never overstates what a fresh dispatch could
// acquire. Split from cmdStatus so this windowed-rows-vs-full-capacity split is
// unit-testable without a tmux session.
func statusView(live []db.Task, state func(db.Task) string, pool worktree.Pool) ([]statusRow, map[string]int) {
	windowed := windowedTasks(live)
	rows := make([]statusRow, len(windowed))
	for i, t := range windowed {
		// The row set and lifecycle columns (STATUS/STAGE/OWNER) come from the DB;
		// STATE is derived live from the task's tmux window.
		rows[i] = statusRow{
			ID: t.ID, Kind: t.Kind, State: state(t),
			Status: t.Status, Stage: t.Stage, Owner: t.Owner,
			Window: t.Window, Project: t.Project, Footprint: t.Footprint,
		}
	}
	return rows, freeSlotsByRepo(pool, live)
}

// freeSlotsByRepo maps each repo with a live task to its free worktree-pool capacity —
// the pool cap minus the slots its live workers currently hold (worktree.Pool.FreeSlots).
// The in-use accounting mirrors the orchestrator's (Manager.inUseWorktrees): a live task
// occupies a slot for as long as it holds a worktree, including after its window has gone
// but before teardown releases the slot, so the count never overstates what a fresh
// dispatch could actually acquire.
func freeSlotsByRepo(pool worktree.Pool, live []db.Task) map[string]int {
	inUse := map[string][]string{}
	for _, t := range live {
		if t.Worktree != "" {
			inUse[t.Project] = append(inUse[t.Project], t.Worktree)
		}
	}
	free := map[string]int{}
	for _, t := range live {
		if _, ok := free[t.Project]; !ok {
			free[t.Project] = pool.FreeSlots(inUse[t.Project])
		}
	}
	return free
}

// windowedTasks keeps only tasks that were spawned — those carrying a tmux window.
// ttorch status reports spawned/live workers (deriving working/idle/gone from the
// window); a pending backlog task has no window and is surfaced by `ttorch tasks`
// instead (§3.3).
func windowedTasks(tasks []db.Task) []db.Task {
	var out []db.Task
	for _, t := range tasks {
		if t.Window != "" {
			out = append(out, t)
		}
	}
	return out
}

// statusRow is one worker's line in `ttorch status`: its LIVE tmux state plus the
// DB-backed lifecycle columns (status/stage/owner) and declared footprint. Split
// from cmdStatus so the rendering (the columns, footprint display, and summary
// line) is unit-testable without tmux.
type statusRow struct {
	ID, Kind, State, Status, Stage, Owner, Window, Project string
	Footprint                                              []string
}

// renderStatus prints the worker table — each worker's declared footprint on an
// indented continuation line beneath it — followed by a summary line whose headline is
// FREE DISPATCH CAPACITY: how many more disjoint tasks the worktree pool can take right
// now. free maps each repo to that count (see freeSlotsByRepo). live/idle/with-footprints
// describe the CURRENT fleet — counted over LIVE workers only (idle or working), so
// "with footprints" agrees with the conflict gate, which ignores gone workers; a gone
// worker's footprint still shows on its row for context. "idle" is parenthesised as a
// subset of live so it is never misread as free capacity (the old "idle slots" wording
// conflated the two and hid genuinely-free worktree slots from operators).
func renderStatus(w io.Writer, rows []statusRow, free map[string]int) {
	const format = "%-16s %-6s %-8s %-12s %-14s %-16s %-10s %s\n"
	fmt.Fprintf(w, format, "TASK", "KIND", "STATE", "STATUS", "STAGE", "OWNER", "WINDOW", "PROJECT")
	var live, idle, declared int
	var repos []string
	seen := map[string]bool{}
	for _, r := range rows {
		fmt.Fprintf(w, format, r.ID, r.Kind, r.State, dash(r.Status), dash(r.Stage), dash(r.Owner), dash(r.Window), r.Project)
		if len(r.Footprint) > 0 {
			fmt.Fprintf(w, "%-16s touches: %s\n", "", strings.Join(r.Footprint, ", "))
		}
		if !seen[r.Project] {
			seen[r.Project] = true
			repos = append(repos, r.Project) // first-seen order, matching the table
		}
		if r.State != "idle" && r.State != "working" {
			continue // gone (or unknown): not a live slot, not counted in the summary
		}
		live++
		if r.State == "idle" {
			idle++
		}
		if len(r.Footprint) > 0 {
			declared++
		}
	}
	fmt.Fprintf(w, "%d live (%d idle) · %s · %d with footprints\n", live, idle, freeSummary(repos, free), declared)
}

// freeSummary renders the free worktree-pool capacity for the status summary. With a
// single repo (the common case) it reads "%d free slots". Capacity is per repo, so across
// multiple repos it breaks the count down ("free slots: 14 in cli, 16 in orcha") rather
// than report a single fleet-wide total that no one repo could actually absorb. repos is
// in table order; free is keyed by repo path (freeSlotsByRepo), and a repo absent from
// free reads as zero free slots.
func freeSummary(repos []string, free map[string]int) string {
	switch len(repos) {
	case 0:
		return "0 free slots"
	case 1:
		return fmt.Sprintf("%d free slots", free[repos[0]])
	default:
		parts := make([]string, len(repos))
		for i, repo := range repos {
			parts[i] = fmt.Sprintf("%d in %s", free[repo], filepath.Base(repo))
		}
		return "free slots: " + strings.Join(parts, ", ")
	}
}

// cmdCheckOverlap reports which live workers a proposed footprint would conflict
// with, so the manager can plan parallel dispatch without guessing. Footprints are
// repo-relative, so it always scopes to a concrete repo: --repo, or the repo
// containing the current directory. It refuses loudly if neither resolves, rather
// than silently widening to every repo (which would invent cross-repo conflicts).
func cmdCheckOverlap(args []string) error {
	footprint, repoFlag, err := checkOverlapArgs(args)
	if err != nil {
		return err
	}
	scope := repoFlag
	if scope == "" {
		scope = "."
	}
	repo, err := worktree.RepoRoot(scope)
	if err != nil {
		return fmt.Errorf("check-overlap: %s is not inside a git repository; cd into the repo or pass --repo <dir> (footprints are repo-relative)", scope)
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	conflicts, err := m.CheckOverlap(repo, footprint)
	if err != nil {
		// Fail closed: report the read failure rather than render a misleading "no conflicts".
		return fmt.Errorf("check-overlap: cannot read the live fleet to check for conflicts: %w", err)
	}
	renderOverlap(os.Stdout, footprint, conflicts)
	return nil
}

// checkOverlapArgs parses check-overlap's arguments into the proposed footprint and
// the --repo flag value. Split from cmdCheckOverlap so the arg handling — paths
// given comma- or space-separated, and the empty-footprint usage error — is
// testable without touching the filesystem.
func checkOverlapArgs(args []string) (footprint []string, repoFlag string, err error) {
	fs := flag.NewFlagSet("check-overlap", flag.ContinueOnError)
	rf := fs.String("repo", "", "repo to scope to (default: the repo containing the current directory)")
	if e := fs.Parse(args); e != nil {
		return nil, "", e
	}
	fp := parseTouches(strings.Join(fs.Args(), ","))
	if len(fp) == 0 {
		return nil, "", errors.New(`usage: ttorch check-overlap [--repo dir] "<paths>"   (comma- or space-separated)`)
	}
	return fp, *rf, nil
}

// renderOverlap prints a check-overlap report: a clear "safe to dispatch" line
// when disjoint, else each conflicting live worker and the overlapping paths.
func renderOverlap(w io.Writer, footprint []string, conflicts []orchestrator.Conflict) {
	joined := strings.Join(footprint, ", ")
	if len(conflicts) == 0 {
		fmt.Fprintf(w, "no conflicts: %q is disjoint from every live worker — safe to dispatch in parallel\n", joined)
		return
	}
	fmt.Fprintf(w, "%q conflicts with %d live worker(s):\n", joined, len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(w, "  %s (window %s, %s): %s\n", c.TaskID, c.Window, c.Project, c.OverlapString())
	}
}

func cmdPeek(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch peek <task-id> [lines]")
	}
	lines := 40
	if len(args) > 1 {
		if n, err := strconv.Atoi(args[1]); err == nil {
			lines = n
		}
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := m.Peek(args[0], lines)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

const sendUsage = "usage: ttorch send <task-id> <text...> | <task-id> - | <task-id> --message-file <path>"

func cmdSend(args []string) error {
	if len(args) < 1 {
		return errors.New(sendUsage)
	}
	id := args[0]
	msg, err := resolveSendMessage(args[1:], os.Stdin, stdinIsTerminal())
	if err != nil {
		return err
	}
	// A send that resolves to nothing is a silent no-op waiting to happen: the
	// worker would get a bare Enter and the caller would believe a brief landed.
	// Fail loudly instead.
	if msg == "" {
		return errors.New("send: empty message — nothing to deliver")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Send(id, msg); err != nil {
		return err
	}
	fmt.Printf("sent to %s\n", id)
	return nil
}

// resolveSendMessage determines the message body for `ttorch send` from the
// arguments that follow the task id. It supports three mutually exclusive
// sources, all of which carry the message as raw bytes that never reach a
// shell command line — so backticks, $(...), quotes, and angle brackets survive
// verbatim:
//
//   - inline:        send <id> <text...>            (joined with spaces; unchanged)
//   - stdin:         send <id> -                    (or no text at all, when piped)
//   - message file:  send <id> --message-file PATH  (also --message-file=PATH)
//
// Reading from stdin or a file is the safe path for arbitrary text: the bytes
// are passed to the worker untouched (tmux send-keys -l, never re-evaluated by a
// shell). For those two sources a single trailing newline run is trimmed so a
// plain `echo msg | ttorch send <id> -` does not deliver a spurious extra Enter;
// the inline form is left exactly as before. stdinIsTerminal guards the no-text
// case from blocking forever on an interactive terminal.
func resolveSendMessage(rest []string, stdin io.Reader, stdinIsTerminal bool) (string, error) {
	// --message-file <path> | --message-file=<path>
	if len(rest) > 0 && (rest[0] == "--message-file" || strings.HasPrefix(rest[0], "--message-file=")) {
		var path string
		var extra []string
		if eq := strings.IndexByte(rest[0], '='); eq >= 0 {
			path, extra = rest[0][eq+1:], rest[1:]
		} else {
			if len(rest) < 2 {
				return "", errors.New("send --message-file: missing <path>")
			}
			path, extra = rest[1], rest[2:]
		}
		if path == "" {
			return "", errors.New("send --message-file: empty <path>")
		}
		if len(extra) > 0 {
			return "", fmt.Errorf("send --message-file takes a single path; unexpected extra arguments: %v", extra)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("send: read message file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	// Explicit stdin (`-`), or no text given at all: read the whole of stdin.
	// With no text and an interactive terminal, fail loudly rather than hang.
	if (len(rest) == 1 && rest[0] == "-") || len(rest) == 0 {
		if len(rest) == 0 && stdinIsTerminal {
			return "", errors.New(sendUsage)
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("send: read message from stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	// Inline text — preserved byte-for-byte as before.
	return strings.Join(rest, " "), nil
}

// stdinIsTerminal reports whether ttorch's stdin is an interactive terminal (as
// opposed to a pipe or a redirected file). Used to decide whether a textless
// `ttorch send <id>` should read stdin or refuse rather than block on a TTY.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func cmdTeardown(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch teardown <task-id> [--force]")
	}
	id := args[0]
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	force := fs.Bool("force", false, "discard unlanded work")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.Teardown(id, *force)
	if err != nil {
		return err
	}
	fmt.Printf("tore down %s\n", id)
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

func cmdCC(args []string) error {
	fs := flag.NewFlagSet("cc", flag.ContinueOnError)
	isolated := fs.Bool("isolated", false, "open in its own isolated worktree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	return m.OpenCC(*isolated)
}

func cmdStop() error {
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.StopSession()
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return err
}

// cmdResume forces a rebuild of the manager window and every worker tab from
// saved state, then attaches the lead to the manager.
func cmdResume() error {
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.Resume()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return m.StartManager()
}

// cmdReset discards the saved session (manager record + task records) for a clean
// start. It confirms first unless --yes is given. Worktrees and branches are kept.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes && !confirm(os.Stdout, os.Stdin, "Discard the saved ttorch session (manager + task records)? Worktrees and branches are kept.") {
		fmt.Println("aborted")
		return nil
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.Reset()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

// confirm prompts the user for a yes/no answer, defaulting to no.
func confirm(out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// cmdWait is a retired alias. The supervisor wake-queue it used to drain is gone;
// the manager now arms `ttorch watch` (event-driven on the DB, zero injection into
// any session). It is kept as a thin shim — rather than dropped to an "unknown
// command" — so a manager session resumed from before the retirement, or a habit of
// typing `ttorch wait`, gets a clear pointer to the replacement. It takes no action.
func cmdWait(_ []string) error {
	fmt.Println("`ttorch wait` is retired; use `ttorch watch` (event-driven on the DB, zero injection).")
	return nil
}

// cmdWatch arms the event-driven watcher (§4): it blocks on actionable DB
// transitions and, when one occurs, prints the coalesced batch + WATCH_WATERMARK and
// exits 0 so the harness re-invokes the manager through its background-task-completion
// channel — no keystroke ever reaches the manager window. It is short-lived only in
// the sense that it returns on the first wake; until then it holds its store and the
// singleton flock for its lifetime. `--reset` reaps an orphan watcher instead of
// watching (manager restart, §4.5).
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	since := fs.Int64("since", -1, "only surface events with id greater than this (default: manager.watch_watermark)")
	timeout := fs.Duration("timeout", 0, "give up after this long with WATCH_TIMEOUT (0 = block forever)")
	coalesce := fs.Duration("coalesce", 750*time.Millisecond, "wait this long after the first actionable event to absorb a burst")
	reset := fs.Bool("reset", false, "reap an orphan watcher and confirm the singleton is free, then return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths.Default()
	store, err := db.Open(p.StateDB())
	if err != nil {
		return err
	}
	defer store.Close()

	// SIGTERM (how `--reset` reaps an orphan) and Ctrl-C cancel the loop; the deferred
	// flock release then frees the singleton, which is what lets a reset proceed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := watch.New(store, p, tmux.SessionName())
	if *reset {
		return w.Reset(ctx)
	}
	// Arming the watcher means the manager is back in the loop — clear the
	// awaiting-lead backstop (§4.6: "cleared on the next watch arm"). The protocol
	// only arms watch when NOT awaiting the lead, so this never races the backstop.
	if err := store.SetAwaitingLead(ctx, false); err != nil {
		return err
	}
	w.Since = *since
	w.Timeout = *timeout
	w.Coalesce = *coalesce
	if _, err := w.Run(ctx); err != nil {
		// A clean cancel/SIGTERM (e.g. a reset reaping us) is a normal exit, not an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	return nil
}

// cmdAwaitLead sets (or, with --clear, clears) manager.awaiting_lead — the silent
// backstop in §4.6. While set, a still-running watcher keeps blocking instead of
// surfacing, so an actionable event can never pull the manager off a decision it has
// put to the lead. The manager sets it as it surfaces a decision and clears it when
// the lead returns (re-arming `ttorch watch` also clears it).
func cmdAwaitLead(args []string) error {
	fs := flag.NewFlagSet("await-lead", flag.ContinueOnError)
	clear := fs.Bool("clear", false, "clear the flag — the lead has returned; the watcher may be re-armed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := db.Open(paths.Default().StateDB())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetAwaitingLead(context.Background(), !*clear); err != nil {
		return err
	}
	if *clear {
		fmt.Println("awaiting-lead cleared — the watcher may be re-armed")
	} else {
		fmt.Println("awaiting-lead set — waiting for the lead; do not arm the watcher")
	}
	return nil
}

// cmdWatchdog runs the EXTERNAL manager-liveness net (§4.7): the counterpart to
// `ttorch watch` (which recovers stalled WORKERS) for the manager itself. It detects a
// manager that is stalled — its own LLM turn died on an API error — WHILE actionable
// work waits, and re-pokes it by appending a single actionable event that an armed
// `ttorch watch` surfaces, waking the manager through the harness's background-task-
// completion channel. It never writes to the manager console and never injects a
// keystroke; the DB row is the only out-of-band lever. It is idle-aware: a check with
// nothing to do is a cheap no-op, so it is safe to run on a short launchd/cron interval.
// One-shot by default (ideal for `StartInterval`); --interval D runs its own loop (the
// "small ttorch daemon" form). Its own status lines go to ITS stdout (the cron/launchd
// log), not the manager — suppress them with --quiet for an unattended schedule.
func cmdWatchdog(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	stall := fs.Duration("stall", 5*time.Minute, "treat the manager as stalled after this long with no manager action")
	interval := fs.Duration("interval", 0, "run continuously, checking every interval (0 = one check, then exit)")
	quiet := fs.Bool("quiet", false, "suppress per-check status output (for unattended cron/launchd schedules)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths.Default()
	store, err := db.Open(p.StateDB())
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	wd := watch.NewWatchdog(store, p)
	wd.Stall = *stall

	check := func() error {
		res, err := wd.Check(ctx)
		if err != nil {
			return err
		}
		if !*quiet {
			if res.Poked {
				fmt.Println("watchdog: re-poked the stalled manager —", res.Reason)
			} else {
				fmt.Println("watchdog: standing by —", res.Reason)
			}
		}
		return nil
	}

	if *interval <= 0 {
		return check()
	}
	for {
		if err := check(); err != nil {
			return err
		}
		t := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// cmdScheduler runs the deterministic dispatch+gate+land+supervise daemon (roadmap item A). Each
// tick it runs whichever passes are enabled, in this order:
//
//   - SUPERVISE (phase 3, opt-in via --supervise): reclaim workers that have VERIFIABLY died —
//     a tmux window the watcher confirmed gone, or an expired lease (never pane-output
//     inference) — and re-dispatch them within a bounded retry ceiling; a task that exceeds
//     the ceiling is poison-pilled to the terminal 'failed' status with an actionable event
//     for the lead, never restarted forever. It NEVER reclaims a live worker. Runs first so a
//     reclaimed task re-dispatches in the same tick.
//   - DISPATCH (phase 1, on by default): re-derive ready pending backlog from SQLite, atomically
//     claim the tasks it can prove safe to run in parallel (a declared footprint disjoint from
//     every live and just-claimed worker, within free worktree capacity), and dispatch them
//     through the SAME spawn path the manager uses — so disjoint ready work (including a
//     supervisor-reclaimed task) never sits idle.
//   - GATE (opt-in via --gate): find done tasks in a TRUSTED repo that do NOT already carry a
//     passing verdict, run the trust prep, dispatch the adversarial reviewers in independent
//     workers, and on an all-pass RECORD the verdict via the SAME commit-pinned trust-record
//     path the manager uses — so trusted done-work is gated without the manager doing it by
//     hand. It FAILS CLOSED: a blocking finding, a prep refusal, or a stalled reviewer records
//     nothing and surfaces an actionable gate_blocked event for the manager to adjudicate.
//     Non-trusted repos and already-passing tasks are skipped. Runs before LAND so a verdict
//     recorded this tick can land in the same tick.
//   - LAND (phase 2a, opt-in via --land): find done tasks that ALREADY carry a passing durable
//     verdict and land them through the SAME pipeline `ttorch land` runs (rebase, re-validate,
//     carry verdict+approval, per-repo fast-forward, teardown) — so green, gated work merges
//     without the manager doing it by hand. It NEVER lands ungated work: a passing verdict is
//     required to attempt a task and the land path's own commit-pinned gate is the authority.
//
// The four passes are independent (--dispatch defaults on; --gate, --land, and --supervise
// opt-in), so it can run as a dispatch-only daemon, a land-only daemon, a supervisor, or any
// combination. The manager AUTO-STARTS this daemon by default with all four passes on (config-gated by
// TTORCH_SCHEDULER_AUTOSTART; see Manager.StartManager), so a normal `ttorch` session is already
// driving the board — this subcommand stays for running it by hand or with a different pass mix.
// --singleton takes a per-~/.ttorch lock and exits quietly if another daemon already holds it
// (how the manager's auto-start avoids running two); a plain `ttorch scheduler` stays safe to run
// as multiple instances. Its log lines go to ITS stdout, never the manager pane (no TTY
// injection). --once runs a single tick then exits (tests / cron); otherwise it loops on
// --interval until Ctrl-C/SIGTERM.
func cmdScheduler(args []string) error {
	// `scheduler status` is a read-only observability subcommand (not a daemon mode), so it is
	// dispatched before the daemon flags are parsed.
	if len(args) > 0 && args[0] == "status" {
		return cmdSchedulerStatus(args[1:])
	}
	fs := flag.NewFlagSet("scheduler", flag.ContinueOnError)
	interval := fs.Duration("interval", scheduler.DefaultInterval, "tick cadence: re-derive the board and act every interval")
	once := fs.Bool("once", false, "run a single tick (act on what is ready now), then exit")
	dispatch := fs.Bool("dispatch", true, "claim + dispatch ready pending backlog (on by default; pass --dispatch=false for a land-/supervise-only daemon)")
	land := fs.Bool("land", false, "also LAND done tasks that already carry a passing verdict, via the same pipeline as 'ttorch land' (off by default)")
	supervise := fs.Bool("supervise", false, "also SUPERVISE the fleet: reclaim workers that verifiably died (window gone / lease expired) and re-dispatch within a bounded retry ceiling, poison-pilling the rest (off by default)")
	gate := fs.Bool("gate", false, "also GATE done tasks in a trusted repo with no passing verdict: run the trust prep, dispatch the adversarial reviewers, and on an all-pass record the verdict via the unchanged trust-record path — fails closed (a blocking finding/prep refusal/stalled reviewer is surfaced for the manager, never recorded) (off by default)")
	single := fs.Bool("singleton", false, "hold a per-~/.ttorch singleton lock and exit quietly if another scheduler daemon already holds it (used by the manager's auto-start; a plain 'ttorch scheduler' stays safe to run as multiple instances)")
	// Backpressure governor (H4): machine-load throttle on TOP of the worktree-pool cap. Each
	// defaults to -1 (sentinel for "not set on the command line"); when set it OVERRIDES the
	// env-or-default value scheduler.New resolved (TTORCH_MAX_ACTIVE_WORKERS / TTORCH_LOAD_CEILING
	// / TTORCH_MAX_LAND_CONCURRENCY). A value <= 0 DISABLES that knob.
	maxActive := fs.Int("max-active", -1, "max heavy workers running concurrently across the fleet before dispatch defers this tick (a machine-load throttle BELOW the worktree-pool cap); <=0 disables it. Default: NumCPU/2, floored at 2, capped at the pool size (env: TTORCH_MAX_ACTIVE_WORKERS)")
	loadCeiling := fs.Float64("load-ceiling", -1, "defer a dispatch tick whose 1-minute system load average exceeds this value (fail-open: an unreadable loadavg never defers); <=0 disables it (the default). Set relative to core count (env: TTORCH_LOAD_CEILING)")
	maxLand := fs.Int("max-land-concurrency", -1, "max gated tasks the land pass hands to the concurrent land pipeline per tick, bounding how many heavy validate suites launch at once; <=0 disables the per-tick bound. Default 2 (env: TTORCH_MAX_LAND_CONCURRENCY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*dispatch && !*land && !*supervise && !*gate {
		return errors.New("scheduler: nothing to do — enable at least one of --dispatch (default), --land, --gate, or --supervise")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()

	// --singleton: take the per-~/.ttorch scheduler lock so the manager's auto-start can never run
	// two daemons. If another daemon already holds it, exit quietly (a no-op success). A plain
	// `ttorch scheduler` (no --singleton) is unaffected and remains safe to run as multiple
	// instances, since the DB's atomic claims already prevent double-dispatch/double-land.
	if *single {
		lock, acquired, err := singleton.Acquire(m.P.SchedulerPIDFile())
		if err != nil {
			return fmt.Errorf("scheduler: could not take the singleton lock: %w", err)
		}
		if !acquired {
			fmt.Fprintln(os.Stdout, "scheduler: another scheduler daemon already holds the singleton lock for this ~/.ttorch; exiting.")
			return nil
		}
		defer singleton.Release(lock)
	}

	// The daemon spawns workers autonomously; install any missing recommended agent skills
	// once here, before the dispatch loop brings up worker tmux sessions. Best-effort.
	skills.EnsureInstalled(os.Stderr)

	sch := scheduler.New(m, *interval, os.Stdout)
	sch.Dispatch = *dispatch
	sch.Land = *land
	sch.Supervise = *supervise
	sch.Gate = *gate
	// Override the governor's env-or-default knobs only when the flag was EXPLICITLY passed, so a
	// flag beats the env, the env beats the built-in default, and an unset flag leaves New()'s
	// resolution intact (fs.Visit only visits flags that were set). The auto-start daemon passes
	// none of these, so it runs on the env-or-default governor.
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "max-active":
			sch.MaxActiveWorkers = *maxActive
		case "load-ceiling":
			sch.LoadCeiling = *loadCeiling
		case "max-land-concurrency":
			sch.MaxLandConcurrency = *maxLand
		}
	})
	// Ctrl-C and SIGTERM cancel the loop and let it exit cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		// Supervise FIRST (mirroring runTick): reclaim dead workers so the dispatch pass can
		// re-dispatch them in this same one-shot tick.
		if *supervise {
			n, err := sch.RunSuperviseOnce(ctx)
			// A clean cancel mid-tick is a normal exit, not an error.
			if err != nil && ctx.Err() == nil {
				return err
			}
			fmt.Printf("scheduler: reclaimed %d task(s) for retry\n", n)
			// Idle-nudge runs in the same supervise step as the loop's runTick, so the one-shot
			// path matches the daemon: nudge alive-but-idle workers after reclaiming dead ones.
			if ctx.Err() == nil {
				n, err := sch.RunNudgeIdleOnce(ctx)
				if err != nil && ctx.Err() == nil {
					return err
				}
				fmt.Printf("scheduler: nudged %d idle worker(s)\n", n)
			}
		}
		if *dispatch && ctx.Err() == nil {
			n, err := sch.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				return err
			}
			fmt.Printf("scheduler: dispatched %d task(s)\n", n)
		}
		// Gate runs between dispatch and land (mirroring runTick): a task whose reviewers' reports
		// are all in this tick is recorded by the gate pass and then landed by the land pass.
		if *gate && ctx.Err() == nil {
			n, err := sch.RunGateOnce(ctx)
			if err != nil && ctx.Err() == nil {
				return err
			}
			fmt.Printf("scheduler: gated %d task(s)\n", n)
		}
		if *land && ctx.Err() == nil {
			n, err := sch.RunLandOnce(ctx)
			if err != nil && ctx.Err() == nil {
				return err
			}
			fmt.Printf("scheduler: landed %d task(s)\n", n)
		}
		return nil
	}
	fmt.Fprintf(os.Stdout, "scheduler: %s every %s (Ctrl-C to stop)\n", schedulerModes(*dispatch, *land, *supervise, *gate), *interval)
	if err := sch.Run(ctx); err != nil {
		// A clean cancel/SIGTERM is a normal exit, not an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	return nil
}

// cmdSchedulerStatus prints the daemon's durable observability row — last-tick age, cumulative
// counters (dispatched / landed / gated / recovered / deferred / errors), and the most recent
// swallowed error — alongside the live board gauges (queue depth + active workers) and whether
// the scheduler singleton lock is currently held (a daemon process is alive). It exits non-zero
// when the daemon is STALLED: its last tick is older than --stale-after (default
// scheduler.StaleAfterDefault), or no tick has ever been recorded. That lets the manager's
// watchdog shell out to it to distinguish a DAEMON stall (held lock but stale last_tick, or no
// daemon at all) from a MANAGER stall. It is READ-ONLY: it never starts the daemon or writes the
// board, so it is safe to poll.
func cmdSchedulerStatus(args []string) error {
	fs := flag.NewFlagSet("scheduler status", flag.ContinueOnError)
	staleAfter := fs.Duration("stale-after", scheduler.StaleAfterDefault, "report the daemon stalled (exit non-zero) when its last recorded tick is older than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()

	ctx := context.Background()
	row, has, err := m.Store.GetSchedulerStatus(ctx)
	if err != nil {
		return err
	}
	pending, active := scheduler.BoardGauges(ctx, m.Store)
	view := scheduler.StatusView{
		Row:        row,
		HasRow:     has,
		Pending:    pending,
		Active:     active,
		LockHeld:   singleton.Held(m.P.SchedulerPIDFile()),
		Now:        time.Now(),
		StaleAfter: *staleAfter,
	}
	fmt.Fprint(os.Stdout, view.Render())
	if view.Stalled() {
		// A non-zero exit is the watchdog's machine-readable signal; the full report is already on
		// stdout. Keep the error line concise and greppable.
		if has {
			return fmt.Errorf("daemon stalled: last tick %s ago (threshold %s)",
				time.Since(row.LastTickAt).Round(time.Second), *staleAfter)
		}
		return errors.New("daemon stalled: no tick recorded yet")
	}
	return nil
}

// schedulerModes describes the enabled passes for the daemon's startup banner, in tick order
// (supervise → dispatch → gate → land). The caller has already refused an all-off config, so
// the result is never empty.
func schedulerModes(dispatch, land, supervise, gate bool) string {
	var parts []string
	if supervise {
		parts = append(parts, "recovering dead workers")
	}
	if dispatch {
		parts = append(parts, "dispatching ready backlog")
	}
	if gate {
		parts = append(parts, "gating finished work")
	}
	if land {
		parts = append(parts, "landing gated work")
	}
	return strings.Join(parts, ", ")
}

func cmdValidate(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch validate <task-id>")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	results, err := m.Validate(args[0])
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Println("no checks detected for this worktree (add .ttorch/validate.sh to define them)")
		return nil
	}
	failed := printResults(os.Stdout, results)
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(results))
	}
	fmt.Printf("all %d checks passed\n", len(results))
	return nil
}

// printResults renders check results in the shared PASS/FAIL format (reused by
// `validate` and `trust show`) and returns how many failed.
func printResults(w io.Writer, results []validate.Result) int {
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "  [%s] %s\n", status, r.Name)
		if !r.Passed && r.Output != "" {
			fmt.Fprintln(w, indentTail(r.Output, 15))
		}
	}
	return failed
}

// indentTail returns the last n lines of s, each indented for readability.
func indentTail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}

func cmdReviewDiff(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch review-diff <task-id> [--stat]")
	}
	id := args[0]
	fs := flag.NewFlagSet("review-diff", flag.ContinueOnError)
	stat := fs.Bool("stat", false, "summary (diffstat) only")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := m.ReviewDiff(id, *stat)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		fmt.Println("(no changes against the default branch)")
		return nil
	}
	fmt.Println(out)
	return nil
}

func cmdApprove(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch approve <task-id> [--ttl 10m]")
	}
	id := args[0]
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	ttl := fs.Duration("ttl", 10*time.Minute, "how long the approval stays valid")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Approve(id, *ttl); err != nil {
		return err
	}
	fmt.Printf("approved %s for %s — now run: ttorch merge-local %s\n", id, *ttl, id)
	return nil
}

func cmdTrust(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch trust prep|record|show <task-id> [flags]")
	}
	sub, id := args[0], args[1]
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	switch sub {
	case "prep":
		dir, err := m.TrustPrep(id)
		if err != nil {
			return err
		}
		dims := m.ReviewersFor(id)
		fmt.Printf("prepared review inputs for %s in %s\n", id, dir)
		fmt.Printf("  run the %d reviewer(s) for this diff (%s), then: ttorch trust record %s\n",
			len(dims), strings.Join(dims, " | "), id)
		return nil
	case "record":
		fs := flag.NewFlagSet("trust record", flag.ContinueOnError)
		sha := fs.String("sha", "", "commit the review covers (default: the worker's current HEAD)")
		ttl := fs.Duration("ttl", 30*time.Minute, "how long the verdict stays valid")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		v, err := m.TrustRecord(id, *sha, *ttl)
		if err != nil {
			return err
		}
		printResults(os.Stdout, review.ToResults(v))
		fmt.Printf("recorded %s verdict for %s (valid %s)\n", v.Overall, id, *ttl)
		return nil
	case "show":
		v, ok := m.TrustShow(id)
		if !ok {
			fmt.Printf("no valid verdict for %s — run 'ttorch trust record %s'\n", id, id)
			return nil
		}
		printResults(os.Stdout, review.ToResults(v))
		fmt.Printf("verdict: %s\n", v.Overall)
		return nil
	default:
		return errors.New("usage: ttorch trust prep|record|show <task-id> [flags]")
	}
}

func cmdMergeLocal(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch merge-local <task-id> [--require-verdict]")
	}
	id := args[0]
	fs := flag.NewFlagSet("merge-local", flag.ContinueOnError)
	requireVerdict := fs.Bool("require-verdict", false,
		"also require a passing adversarial-review verdict + a fresh green validate (implied by trusted mode)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := m.MergeLocal(id, *requireVerdict)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// cmdLand delivers one task, several named tasks, or the whole done set (--all). A single
// explicit id takes the exact single-land path; two or more tasks — or --all — go through the
// async pipelined land queue (LandSet), which lands each as soon as it is individually ready,
// serializing only the per-repo fast-forward. Bounded by file-disjointness: same-package tasks
// serialize the actual landing (one re-rebases onto the other and re-gates), while disjoint
// tasks land concurrently.
func cmdLand(args []string) error {
	var ids []string
	requireVerdict, all := false, false
	for _, a := range args {
		switch a {
		case "--require-verdict", "-require-verdict":
			requireVerdict = true
		case "--all", "-all":
			all = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("land: unknown flag %q; usage: ttorch land <task-id>... | --all [--require-verdict]", a)
			}
			ids = append(ids, a)
		}
	}
	if all && len(ids) > 0 {
		return errors.New("land: pass either --all or explicit task ids, not both")
	}
	if !all && len(ids) == 0 {
		return errors.New("usage: ttorch land <task-id>... | --all [--require-verdict]")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()

	if all {
		ids, err = doneTaskIDs(m)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Println("land --all: no done tasks to land")
			return nil
		}
	}

	// One explicit task keeps the single-land path verbatim (same output, no queue overhead).
	if len(ids) == 1 && !all {
		out, err := m.Land(ids[0], requireVerdict)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}

	results := m.LandSet(context.Background(), ids, requireVerdict)
	return reportLandResults(results)
}

// doneTaskIDs lists the ids of every task awaiting landing — status done, excluding ad-hoc cc
// sessions — for `ttorch land --all`. The queue then lands each per its repo's delivery mode.
func doneTaskIDs(m *orchestrator.Manager) ([]string, error) {
	tasks, err := m.Store.ListTasks(context.Background(), db.TaskFilter{
		Status:      []string{db.StatusDone},
		ExcludeKind: []string{db.KindCC},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// reportLandResults prints each task's land summary or failure in input order and returns a
// non-nil error when any task did not land, so the exit code reflects a partial landing.
func reportLandResults(results []orchestrator.LandResult) error {
	landed := 0
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("  %s: %v\n", r.TaskID, r.Err)
			continue
		}
		landed++
		fmt.Println("  " + strings.ReplaceAll(r.Output, "\n", "\n  "))
	}
	fmt.Printf("landed %d of %d task(s)\n", landed, len(results))
	if landed < len(results) {
		return fmt.Errorf("land: %d of %d task(s) did not land", len(results)-landed, len(results))
	}
	return nil
}

func cmdPromote(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch promote <task-id>")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Promote(args[0]); err != nil {
		return err
	}
	fmt.Printf("promoted %s to a ship task\n", args[0])
	return nil
}

func cmdPRCheck(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch pr-check <task-id> <pr-url>")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.ArmPRCheck(args[0], args[1]); err != nil {
		return err
	}
	fmt.Printf("watching %s for merge of %s\n", args[0], args[1])
	return nil
}

func cmdFleetSync(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.FleetSync(dir)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

func cmdRecovery() error {
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	notes, err := m.Recovery()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

// resolveRepo picks the repo root for a learnings command: an explicit --repo, else
// the --task's project, else the repo containing the current directory.
func resolveRepo(repoFlag, taskFlag string) (string, error) {
	if repoFlag != "" {
		return worktree.RepoRoot(repoFlag)
	}
	if taskFlag != "" {
		m, err := mgr()
		if err != nil {
			return "", err
		}
		defer m.Close()
		t, ok, err := m.Store.GetTask(context.Background(), taskFlag)
		if err != nil || !ok {
			return "", fmt.Errorf("unknown task %q", taskFlag)
		}
		if t.Project != "" {
			return t.Project, nil
		}
	}
	return worktree.RepoRoot(".")
}

func cmdLearn(args []string) error {
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository (default: current repo, or --task's repo)")
	task := fs.String("task", "", "attribute the lesson to a task (resolves its repo)")
	glob := fs.String("glob", "", "path scope, e.g. internal/api/**")
	pin := fs.Bool("pin", false, "always include this lesson (don't wait for recurrence)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return errors.New(`usage: ttorch learn [--task id] [--glob pat] [--pin] "<lesson>"`)
	}
	dir, err := resolveRepo(*repo, *task)
	if err != nil {
		return err
	}
	e, err := learnings.Apply(dir, text, *glob, *task, *pin)
	if err != nil {
		return err
	}
	status := "recorded"
	if e.Pinned || e.Count >= learnings.PromoteThreshold {
		status = "recorded + promoted to AGENTS.md"
	}
	fmt.Printf("%s (seen %dx): %s\n", status, e.Count, e.Text)
	return nil
}

func cmdLearnings(args []string) error {
	fs := flag.NewFlagSet("learnings", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := resolveRepo(*repo, "")
	if err != nil {
		return err
	}
	entries, err := (learnings.Store{Dir: dir}).Load()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no learnings recorded yet")
		return nil
	}
	promoted := map[string]bool{}
	for _, e := range learnings.Promoted(entries) {
		promoted[e.Text] = true
	}
	for _, e := range entries {
		tag := "ledger"
		if promoted[e.Text] {
			tag = "in AGENTS.md"
		}
		fmt.Printf("  [%dx · %s] %s\n", e.Count, tag, e.Text)
	}
	return nil
}

func reapplyContent(p paths.Paths) error {
	res, err := installer.Apply(ttorchembed.Content, p, buildinfo.CurrentVersion())
	if err != nil {
		return err
	}
	printResult(os.Stdout, res)
	return nil
}

func printResult(w io.Writer, res *installer.Result) {
	r := res.Report
	fmt.Fprintf(w, "Content: %d added, %d updated, %d unchanged, %d retired.\n",
		r.Count(manifest.Added), r.Count(manifest.Updated), r.Count(manifest.Unchanged), r.Count(manifest.Retired))
	if c := r.Conflicts(); len(c) > 0 {
		fmt.Fprintf(w, "%d file(s) need your attention (your changes were preserved):\n", len(c))
		for _, f := range c {
			fmt.Fprintf(w, "  - %s: %s\n", f.Path, f.Note)
		}
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Fprintln(w, "  "+n)
		}
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ttorch — manage a team of Claude Code agents

Usage: ttorch [command] [flags]   (bare 'ttorch' launches/restores the manager session)

Team:
  (bare) ttorch           start/attach the manager (one persistent session). If a
                          saved session exists (after stop/reboot/upgrade) it rebuilds
                          the manager + all worker tabs, each resumed to its prior
                          conversation; otherwise a new manager starts in this folder
  resume                  force a rebuild of the manager + all worker tabs, then attach
  reset [--yes]           discard the saved session for a clean start (keeps worktrees)
  stop                    stop the manager session (resumable: run 'ttorch')
  cc [--isolated]         open a Claude session attached to the team
  spawn <id> <repo>       start a worker on a task in an isolated worktree
                          (auto-inits the repo when tracked-file-safe;
                          TTORCH_NO_AUTOINIT=1 to skip)
    --scout                 investigation only (report, no code changes)
    --init                  force first-use setup (AGENTS.md block + CLAUDE.md
                            symlink) even when the repo tracks AGENTS.md
    --touches "a,b"         file paths/prefixes this task will touch; refuses to
                            dispatch onto files a live worker already holds
    --brief-file <path>     launch the worker with this file's contents as its
                            initial prompt (the full brief) instead of the stub
    --brief "..."           launch the worker with this inline text as its brief
    --effort <level>        reasoning effort: low|medium|high|xhigh|max|ultracode|off
                            (default: $TTORCH_EFFORT, else ultracode for ship / high
                            for scout); persisted so a resume restores it
    --model <m>             model: haiku|sonnet|opus|fable|opusplan or a full id
                            (default: $TTORCH_MODEL, else claude's own default);
                            persisted so a resume restores it
    --force-overlap         dispatch anyway when --touches overlaps a live worker
    --cmd "..."             run a raw command instead of the default harness
  status                  list active workers (live tmux state + DB status/stage/owner)
  check-overlap "<paths>" show which live workers a proposed footprint conflicts
    [--repo dir]            with, to plan disjoint parallel dispatch (scopes to the
                            cwd's repo, or --repo)
  peek <id> [lines]       read recent output from a worker
  send <id> <text...>     type a message into a worker (delivered verbatim)
    send <id> -             read the message body from stdin (safe for any chars)
    send <id> --message-file <path>   read the message body from a file
  teardown <id> [--force] finish a worker (refuses to discard unlanded work)

Worker reporting (run by a worker about its own task; resolves the task from
--task, else $TTORCH_TASK_ID, else the worktree's .ttorch/task):
  report <done|blocked|needs-input|active> [-m "msg"]
                          set the task's status (done/blocked/needs-input wake the
                          manager); -m records a note in the same transaction
  stage "<text>"          set a free-text progress stage (does not wake the manager)
  note <text...> | - | --message-file <path>
                          record freeform activity (does not wake the manager)
  follow-on <new-id> --title "…" [--touches "a,b"]
                          file a child task into the backlog (does not spawn)

Backlog & planning (read the DB; includes pending backlog tasks):
  tasks                   list tasks (DB-backed), with filters and views
    [--project id]          scope to a project (see 'project ls')
    [--epic id]             scope to an epic (see 'epic ls')
    [--status s[,s…]]       comma-separated statuses, e.g. active,blocked,done
    [--tree]                print the projects→epics→phases→tasks hierarchy
    [--timeline <task-id>]  print one task's events ∪ notes by time
  project add <repo> [--name n]   register a repo (caches its delivery mode for display)
  project ls              list registered projects
  epic add --project <id> --title "…"     create an epic under a project
  epic ls [--project id]                  list epics
  epic set-status <id> <status>           planned|in_progress|blocked|done|cancelled
  phase add --epic <id> --title "…"       create a phase under an epic
  phase ls [--epic id]                    list phases
  phase set-status <id> <status>          planned|in_progress|blocked|done|cancelled
  task add <id> --project <id> [--epic id] [--phase id] [--title "…"] [--touches "a,b"]
                          create a pending backlog task (does not spawn)

Supervision:
  watch                   block until an actionable DB event, print the coalesced
    [--since n]             batch + WATCH_WATERMARK, then exit (the manager arms this
    [--timeout d]           as a background task each non-blocking turn; --since
    [--coalesce d]          defaults to the stored watermark; --timeout 0 blocks)
  watch --reset           reap an orphan watcher and confirm the singleton is free
  await-lead [--clear]    mark the manager as awaiting the lead (the watcher stays
                          silent and never surfaces); --clear when the lead returns
  watchdog                external manager-liveness net: re-poke a STALLED manager that
    [--stall d]             has actionable work waiting (silently, via the same DB-event
    [--interval d]          channel 'watch' uses — never a keystroke). Idle-aware, so it
    [--quiet]               no-ops when nothing waits; one-shot unless --interval loops
                            it as a daemon. Run from launchd/cron.
  scheduler               deterministic dispatch+gate+land+supervise daemon (opt-in; nothing
    [--interval d]          auto-starts it). DISPATCH (on): each tick re-derive ready pending
    [--once]                backlog and atomically claim + dispatch the tasks it can prove
    [--dispatch]            safe — a declared footprint, disjoint from every live and
    [--land]                just-claimed worker, within free worktree capacity — via the
    [--gate]                manager's own spawn path. Skips (never fails) overlapping or
    [--supervise]           capacity-blocked and footprint-less tasks (left for the manager).
                            GATE (--gate, off): also gate done tasks in a TRUSTED repo with no
                            passing verdict — run the trust prep, dispatch the adversarial
                            reviewers, and on an all-pass record the verdict via the unchanged
                            trust-record path; fails closed (a blocking finding / prep refusal /
                            stalled reviewer is surfaced for the manager, NEVER recorded).
                            LAND (--land, off): also land done tasks that ALREADY carry a
                            passing verdict, via the same pipeline as 'ttorch land'; never
                            lands ungated work. SUPERVISE (--supervise, off): reclaim workers
                            that verifiably died — a confirmed-gone window or an expired lease,
                            never pane inference — and re-dispatch them within a bounded retry
                            ceiling, poison-pilling a task that exceeds it to 'failed' with an
                            actionable event; never reclaims a live worker. The four toggle
                            independently (--dispatch=false --land = land-only). Logs to its
                            own stdout, never the manager pane. --once runs one tick.
    [--max-active n]        BACKPRESSURE GOVERNOR (H4): a machine-load throttle layered ON TOP
    [--load-ceiling x]      of the worktree-pool cap — it only ever dispatches FEWER workers,
    [--max-land-concurrency n]  never more. --max-active caps heavy workers running concurrently
                            (default NumCPU/2, floored at 2, capped at the pool; <=0 disables);
                            at the cap a tick defers + LOGS, leaving tasks pending. --load-ceiling
                            (off by default) defers a tick whose 1-min load average exceeds it,
                            failing OPEN if loadavg is unreadable. --max-land-concurrency (default
                            2) bounds gated tasks handed to the land pipeline per tick so a burst
                            cannot launch many heavy validates at once. Each also reads an env var
                            (TTORCH_MAX_ACTIVE_WORKERS / TTORCH_LOAD_CEILING /
                            TTORCH_MAX_LAND_CONCURRENCY); an explicit flag overrides the env.
  scheduler status        show the daemon's observability: last-tick age, cumulative counters,
    [--stale-after d]       last error, queue depth + active, and whether the singleton lock is
                            held. Exits non-zero when the last tick is older than --stale-after
                            (default 90s) so a watchdog can detect a stalled daemon. Read-only.

Delivery:
  validate <id>               run the repo's build/test/lint checks on a worker
  ci-parity [dir] [--list]    reproduce the repo's actual CI run-steps locally
                              (green here == green in CI); --list shows the plan only
  review-diff <id> [--stat]   show a worker's changes vs the default branch
  approve <id> [--ttl 10m]    grant a time-boxed approval (run by the lead)
  trust prep|record|show <id> prep/record/show the adversarial-review verdict
  security-review prep|record|show <id>
                              run the security reviewer in ANY delivery mode (advisory;
                              does not block) — prep inputs, review, then record
  qa-review prep|record|show <id>
                              run the optional test-adequacy (QA) reviewer (advisory;
                              does not block) — prep inputs, review, then record
  merge-local <id> [--require-verdict]
                              fast-forward the local default branch (needs approval;
                              --require-verdict also gates on a passing verdict + validate)
  land <id>... | --all [--require-verdict]
                              one safe atomic delivery: fetch, rebase onto the current
                              default (abort on conflict), re-validate, integrate per the
                              repo's delivery mode honoring the existing gates, verify the
                              landed tip matches the validated commit, and fast-forward
                              the local default. Pass several ids or --all (the whole done
                              set) to land concurrently: each lands as soon as it is ready,
                              serializing only the per-repo fast-forward — disjoint tasks
                              land in parallel, same-package tasks serialize
  promote <id>                turn a scout task into a ship task
  pr-check <id> <url>         arm a PR-merge check (surfaced by 'ttorch watch')
  fleet-sync [dir]            refresh local default from origin; prune gone branches
  recovery                    reconcile tracked tasks against live windows
  learn [--task id] "<msg>"   record a durable repo lesson (auto-promotes when recurring)
  learnings                   list recorded lessons

Setup:
  install                 install/update managed skills, agents, and guidance
  update [--content-only] self-update the binary, then re-apply content
  uninstall [--purge]     remove managed files (keeps files you edited)
  doctor [--yes]          check/install tmux, git, gh, claude (+ optional codegraph)
  skills [install]        list / install recommended agent skills (e.g. axi, ponytail)
  init [--mode m]         set up a repo's AGENTS.md + CLAUDE.md + delivery mode
                          (+ codegraph nav when TTORCH_CODEGRAPH=1; opt-in, default off)
  profile [dir]           derive the repo's stack/commands/conventions into AGENTS.md
  version | help          version / this message

Coming later: native-Windows polish
`)
}

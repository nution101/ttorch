package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/termtab"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/worktree"
)

// Spawn starts a worker for taskID against the repo containing projectPath. If
// rawCmd is empty, it launches the detected harness with the task brief; otherwise
// it runs rawCmd (used for testing and escape hatches). It declares no footprint,
// so it is exempt from overlap enforcement — the back-compat entry point.
func (m *Manager) Spawn(taskID, projectPath string, scout bool, rawCmd string) (db.Task, error) {
	return m.spawnWorker(taskID, projectPath, scout, rawCmd, nil, false, "", "", false)
}

// SpawnWithFootprint is Spawn plus deterministic overlap prevention (see
// SpawnWithEffort for the footprint semantics). It dispatches at the default/unset effort
// and model. It is a thin back-compat wrapper over SpawnAutonomous (autonomous=true) kept
// stable for the overlap-gate tests; the scheduler daemon forwards the per-task effort and
// model via SpawnAutonomous.
func (m *Manager) SpawnWithFootprint(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool) (db.Task, error) {
	return m.SpawnAutonomous(taskID, projectPath, scout, rawCmd, footprint, forceOverlap, "", "")
}

// SpawnAutonomous is the AUTONOMOUS (scheduler daemon) entry point: SpawnWithFootprint plus
// the explicit per-task reasoning effort AND model the scheduler forwards from the claimed
// task row. Each is resolved through harness.ResolveWorkerEffort / ResolveWorkerModel
// (explicit row value > env > default) and persisted on the row so a resume restores it.
//
// It dispatches with autonomous=true: a daemon dispatch must never write the manager-send
// brief stub (no `ttorch send` is coming for it), and a briefless task should already have
// been skipped by the scheduler — see spawnWorker / briefForLaunch.
func (m *Manager) SpawnAutonomous(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool, effort, model string) (db.Task, error) {
	return m.spawnWorker(taskID, projectPath, scout, rawCmd, footprint, forceOverlap, effort, model, true)
}

// SpawnWithEffort is SpawnWithFootprint plus an explicit per-task reasoning effort AND
// model (`ttorch spawn --effort/--model`). effort/model are the requested values, or "" to
// use the defaults; each is resolved through harness.ResolveWorkerEffort / ResolveWorkerModel
// (explicit > env > default; a scout's effort defaults to high, and an unset model passes no
// --model) and the resolved values are BOTH used for the launch command and persisted on the
// task row so a later resume restores them.
//
// footprint is the repo-relative paths/prefixes the task will touch. When it is
// non-empty and forceOverlap is false, the spawn is REFUSED if the footprint overlaps
// any live worker's footprint (in the same repo), naming the conflicting task — so two
// workers are never dispatched onto the same files. The check runs before any side
// effect (worktree, window), so a refusal leaves nothing behind. A declared footprint
// is recorded on the task; an empty footprint enforces nothing.
//
// The gate reads the persisted, live worker set and the task's own footprint is
// saved only after its window comes up, so the no-overlap guarantee holds for
// serial dispatch (the manager dispatches one worker at a time). This matches the
// unlocked worktree-pool semantics; two truly concurrent spawns of overlapping
// footprints are not serialized.
//
// It is the INTERACTIVE (manager / `ttorch spawn`) entry point, so it dispatches with
// autonomous=false: a brief-less interactive spawn launches the worker on the
// manager-send stub and waits for the manager's `ttorch send`. The autonomous daemon
// path is SpawnWithFootprint (autonomous=true).
func (m *Manager) SpawnWithEffort(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool, effort, model string) (db.Task, error) {
	return m.spawnWorker(taskID, projectPath, scout, rawCmd, footprint, forceOverlap, effort, model, false)
}

// spawnWorker is the shared spawn implementation behind every entry point. autonomous
// distinguishes a scheduler-daemon dispatch (true) from an interactive manager/`ttorch
// spawn` (false); it controls whether a missing brief is allowed to fall back to the
// manager-send stub (interactive only) or is refused (autonomous — the daemon must never
// strand a worker on a stub no `ttorch send` will satisfy). effort and model are the
// requested per-task levels ("" = default), resolved and persisted once below. All other
// behavior is identical across callers.
func (m *Manager) spawnWorker(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool, effort, model string, autonomous bool) (db.Task, error) {
	var zero db.Task
	if err := m.requireTmux(); err != nil {
		return zero, err
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return zero, err
	}
	repo, err := worktree.RepoRoot(abs)
	if err != nil {
		return zero, fmt.Errorf("%s is not inside a git repository", projectPath)
	}

	window := "wk-" + taskID
	if tmux.WindowExists(m.Session, window) {
		return zero, fmt.Errorf("task %q already has a window; tear it down first", taskID)
	}
	kind := "ship"
	if scout {
		kind = "scout"
	}
	// Resolve the reasoning effort once: explicit --effort > TTORCH_EFFORT > kind default
	// (scout ⇒ high, ship ⇒ ultracode). The resolved level drives the launch command AND
	// is persisted on the task row below, so a resume restores the same effort.
	resolvedEffort := harness.ResolveWorkerEffort(effort, scout)
	// Resolve the model the same way (explicit --model > TTORCH_MODEL > unset). Unset ("")
	// passes no --model, so claude keeps its own default; the resolved value drives the
	// launch command AND is persisted on the task row below, so a resume restores it.
	resolvedModel := harness.ResolveWorkerModel(model)

	// Deterministic overlap gate: refuse a dispatch that would put this worker onto
	// files a live worker already holds. Done before acquiring any resource so a
	// refusal has no side effects; --force-overlap is the explicit manager override.
	if len(footprint) > 0 && !forceOverlap {
		conflicts, err := m.CheckOverlap(repo, footprint)
		if err != nil {
			// Fail closed: the overlap gate could not read the live fleet, so it cannot prove
			// this footprint is disjoint. Refuse rather than dispatch onto possibly-shared files
			// (--force-overlap is the explicit override). Before any side effect, like a conflict.
			return zero, fmt.Errorf("spawn %q: cannot verify footprint %q is disjoint from live workers: %w", taskID, strings.Join(footprint, ","), err)
		}
		if len(conflicts) > 0 {
			return zero, ConflictError(footprint, conflicts)
		}
	}

	// Zero-config first-use setup so a worker always has AGENTS.md to read. autoInit is
	// safe by construction: it writes the managed block / CLAUDE.md symlink / profile only
	// when that introduces no tracked-file change, and otherwise nudges toward `ttorch
	// init` — so it can never re-break merge-local (a848c1c). Opt out with TTORCH_NO_AUTOINIT.
	// Placed AFTER the window and overlap refusal gates so a refused spawn — like the
	// read-only notice it replaced — leaves the lead's checkout untouched.
	autoInit(repo)

	// Fail closed: if the in-use worktree set can't be read, refuse rather than acquire on an
	// incomplete view (which could reuse a worktree a live worker still holds). This is after the
	// overlap gate but still before any side effect, so a refusal leaves nothing behind.
	inUse, err := m.inUseWorktrees(repo)
	if err != nil {
		return zero, fmt.Errorf("spawn %q: cannot read in-use worktrees: %w", taskID, err)
	}
	wt, err := m.Pool.Acquire(repo, inUse)
	if err != nil {
		return zero, err
	}
	// A pooled worktree may be handed back still on a prior task's (often already
	// merged) branch; start every worker on a FRESH branch cut from the up-to-date
	// default branch with a clean tree so it never inherits a previous task's branch
	// or state. Release the slot on failure so a bad start does not leak it.
	if err := worktree.StartBranch(repo, wt, taskBranch(taskID)); err != nil {
		_ = m.Pool.Release(repo, wt)
		return zero, fmt.Errorf("spawn %q: preparing a fresh task branch: %w", taskID, err)
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		_ = m.Pool.Release(repo, wt)
		return zero, err
	}
	if err := m.newWindow(window, wt, windowLabel(kind, taskID)); err != nil {
		_ = m.Pool.Release(repo, wt)
		return zero, err
	}

	h := harness.Resolve()
	// Assign a stable session id so a later restore resumes this exact conversation.
	sid := harness.NewSessionID()
	// Write the worker's trimmed Claude settings (no AI co-author trailer on commits)
	// and pre-accept the harness's folder-trust prompt so the worker runs autonomously.
	_ = harness.WriteWorkerSettings(h, wt)
	harness.TrustWorktree(h, repo, wt)
	// Give the worker its task identity (§3.1): a git-excluded <worktree>/.ttorch/task
	// file so `ttorch report/stage/note/follow-on` resolve the task + DB by walking up
	// from cwd even after a resume drops the launch env. Best-effort, like the settings.
	dbPath := m.P.StateDB()
	_ = harness.WriteWorkerTaskFile(wt, taskID, dbPath)
	harnessLaunch := rawCmd == ""
	cmd := rawCmd
	if cmd == "" {
		brief, err := m.briefForLaunch(taskID, kind, autonomous)
		if err != nil {
			// briefForLaunch only errors on an autonomous dispatch with no stored brief —
			// the scheduler should have skipped it. Unwind the half-started spawn (window +
			// worktree are already up) rather than strand a worker on a stub no send will
			// satisfy; the scheduler's revert returns the claim to pending.
			m.abortSpawn(window, repo, wt)
			return zero, fmt.Errorf("spawn %q: %w", taskID, err)
		}
		// Prepend TTORCH_TASK_ID/TTORCH_DB so the worker's reporting commands resolve
		// their task + DB from the launch env (the .ttorch/task file is the durable
		// fallback that also survives a resume). §3.1.
		cmd = harness.WorkerLaunchPrefix(taskID, dbPath) + harness.BriefCommand(h, brief, sid, resolvedEffort, resolvedModel)
	}
	if err := tmux.SendLine(m.Session, window, cmd); err != nil {
		m.abortSpawn(window, repo, wt)
		return zero, err
	}
	// Do not return — and so do not let the manager send a brief — until the worker is
	// actually up and able to receive input. A brief sent into a still-booting harness
	// is silently dropped, stranding the worker on its stub brief; if the worker never
	// comes up, fail the spawn loudly rather than hand back a phantom worker.
	if err := m.waitForLaunch(window); err != nil {
		m.abortSpawn(window, repo, wt)
		return zero, fmt.Errorf("spawn %q: %w", taskID, err)
	}
	// A real harness needs a moment after its process appears to finish wiring up its
	// interactive input before it will reliably accept typed text; the rawCmd escape
	// hatch (tests, plain commands) has no such TUI, so it skips the settle.
	if harnessLaunch && h == "claude" {
		time.Sleep(spawnSettle)
	}
	// Best-effort: open a native terminal tab/window that attaches a view onto
	// this worker's tmux window so the lead can watch it. The worker stays in
	// tmux regardless; never fail the spawn on this.
	_ = termtab.Open(m.Session, window)

	// Persist the task in the DB. The repo is upserted to a project first (FK), then
	// the task row is upserted: a `pending` backlog row created by `task add` is
	// UPDATEd in place (no TEXT-PRIMARY-KEY collision) and a fresh task is INSERTed in
	// status active. UpsertTask only syncs runtime/coupling fields on the update path,
	// so the backlog→active transition is driven separately by ReportStatus (§3.4).
	// Every event written here is manager-authored and non-actionable, so spawning
	// never wakes a watcher.
	ctx := context.Background()
	proj, err := m.Store.UpsertProject(ctx, repo, "")
	if err != nil {
		m.abortSpawn(window, repo, wt)
		return zero, err
	}
	prior, existed, err := m.Store.GetTask(ctx, taskID)
	if err != nil {
		m.abortSpawn(window, repo, wt)
		return zero, err
	}
	t, err := m.Store.UpsertTask(ctx, db.Task{
		ID: taskID, ProjectID: proj.ID, Window: window, Worktree: wt,
		Harness: h, Kind: kind, Created: time.Now(), SessionID: sid,
		Footprint: footprint, Status: db.StatusActive, Owner: "worker:" + taskID,
		Effort: resolvedEffort, Model: resolvedModel,
	}, db.ActorManager)
	if err != nil {
		// The worker is live by now (waitForLaunch confirmed it), so a failed save would
		// otherwise strand a running worker with no task record to tear it down — unwind
		// it like every other spawn failure rather than leak a phantom worker + slot.
		m.abortSpawn(window, repo, wt)
		return zero, err
	}
	// Flip a pre-existing (backlog) row to active with its status_changed event; a
	// freshly inserted row is already active (with its 'created' event), so skip it.
	if existed && prior.Status != db.StatusActive {
		if _, err := m.Store.ReportStatus(ctx, taskID, db.StatusActive, db.ActorManager, ""); err != nil {
			// UpsertTask already committed window/worktree onto the (still pending) row in
			// its own tx; blank them so the worktree abortSpawn is about to release is not
			// left aliased by a stale row (the same principle as teardown, §3.4). Best-effort
			// in this error path.
			_ = m.Store.SetTaskFields(ctx, taskID, db.TaskFields{Window: new(string), Worktree: new(string)})
			m.abortSpawn(window, repo, wt)
			return zero, err
		}
		// Return the canonical post-transition row (active + last_progress_at); surface a
		// read failure rather than hand back the pre-flip (still 'pending') UpsertTask result.
		refreshed, ok, err := m.Store.GetTask(ctx, taskID)
		if err != nil {
			m.abortSpawn(window, repo, wt)
			return zero, err
		}
		if ok {
			t = refreshed
		}
	}
	// Grant the worker a lease so a dead worker's task can later be reclaimed on
	// verifiable ground truth — an expired lease — rather than pane-output inference
	// (§roadmap 2). The worker is already live and persisted; a failed lease-set must not
	// strand it, and it degrades safely (the reclaim sweep only ever touches a task whose
	// lease genuinely expired, so a task with no lease is simply never reclaimed) — so
	// surface it and carry on, exactly like the spawned-event mirror below.
	if err := m.Store.GrantLease(ctx, taskID, "worker:"+taskID); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not grant a lease for %s: %v\n", taskID, err)
	}
	// Record the spawn as a typed, manager-authored, non-actionable event (§3.4). The
	// worker is already live and persisted, and the events row is a best-effort audit
	// mirror, so a failed append must not fail the spawn (which would strand a running
	// worker) — surface it and carry on.
	if _, err := m.Store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventSpawned, Actor: db.ActorManager,
		Payload: fmt.Sprintf("kind=%s window=%s", kind, window),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the spawned event for %s: %v\n", taskID, err)
	}
	return t, nil
}

// taskBranch is the local branch a freshly spawned worker starts on: a stable,
// per-task name so a reused pooled worktree never leaves the worker on a prior task's
// branch. It matches the branch the PR delivery path publishes (see integratePR), so
// the local and remote branch names stay aligned.
func taskBranch(taskID string) string { return "ttorch/" + taskID }

// Spawn-readiness tunables (vars so tests can shrink them): spawnReadyTimeout bounds
// how long Spawn waits for a launched worker command to take over its pane before
// giving up; spawnReadyInterval is the poll cadence; spawnSettle is an extra pause
// after a real harness's process appears, to let its TUI finish wiring up input
// handling before Spawn returns (and before the manager's brief is sent).
var (
	spawnReadyTimeout  = 20 * time.Second
	spawnReadyInterval = 200 * time.Millisecond
	spawnSettle        = 600 * time.Millisecond
)

// waitForLaunch blocks until the command launched in window has taken over the pane —
// i.e. its foreground process is no longer the bare shell that started in the window —
// so a spawn never returns (and a brief is never sent) while the worker is still a
// shell that would silently drop the input. It returns an error if the window exits or
// no non-shell command appears within spawnReadyTimeout, so a failed launch fails the
// spawn loudly instead of yielding a phantom worker. Comparing against a shell denylist
// (rather than the pre-launch command) is robust to the shell's own startup churn,
// where the foreground transitions through launchers like `env` before settling.
func (m *Manager) waitForLaunch(window string) error {
	deadline := time.Now().Add(spawnReadyTimeout)
	for {
		// Only a window CONFIRMED absent (read succeeded, window not listed) means the
		// launch died; a transient tmux read failure is retried until the deadline so a
		// momentary hiccup never tears down a healthy, just-launched worker.
		exists, err := tmux.WindowExistsErr(m.Session, window)
		if err == nil {
			if !exists {
				return errors.New("the worker's window exited before its command started")
			}
			if cur := tmux.PaneCurrentCommand(m.Session, window); cur != "" && !isShellCommand(cur) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("could not read the worker's window within %s: %w", spawnReadyTimeout, err)
			}
			return fmt.Errorf("the worker's command did not start within %s (its window is still a bare shell)", spawnReadyTimeout)
		}
		time.Sleep(spawnReadyInterval)
	}
}

// shellCommands are the foreground process names that mean a window is still a bare
// shell (or a shell launcher), not a worker command that has taken over the pane.
var shellCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "tcsh": true, "csh": true, "ash": true, "env": true, "login": true,
}

// isShellCommand reports whether a pane's foreground command name is a shell or shell
// launcher (tmux strips no leading login dash, but guard for it anyway).
func isShellCommand(cmd string) bool {
	return shellCommands[strings.TrimPrefix(cmd, "-")]
}

// abortSpawn unwinds a half-started spawn: it reaps the window's processes, kills the
// window, and returns the worktree to the pool, so a launch that fails midway leaves
// behind no phantom worker window or leaked pool slot.
func (m *Manager) abortSpawn(window, repo, wt string) {
	m.killPaneProcesses(window)
	_ = tmux.KillWindow(m.Session, window)
	if repo != "" && wt != "" {
		_ = m.Pool.Release(repo, wt)
	}
}

// briefForLaunch returns the path to the brief the worker launch command reads. A brief
// the manager supplied via `task add`/`spawn --brief` (written by WriteBrief before the
// spawn) is used as the worker's initial prompt, and a resume/respawn keeps an existing
// brief rather than clobbering it.
//
// When NO brief exists yet, behavior splits on dispatch provenance:
//
//   - interactive (autonomous=false): write the generic "wait for ttorch send" stub and
//     launch on it — the manager sends the real brief moments later via `ttorch send`.
//   - autonomous (autonomous=true): REFUSE. The scheduler must skip a briefless task (it
//     gates dispatch on the has_brief column), so reaching here means a regression; a
//     daemon dispatch must never strand a worker on a stub no `ttorch send` will satisfy.
//     Failing loud is the second guard behind the scheduler's skip.
func (m *Manager) briefForLaunch(taskID, kind string, autonomous bool) (string, error) {
	path := m.P.BriefPath(taskID)
	if _, err := os.Stat(path); err == nil {
		return path, nil // a brief already exists (real brief, or a stub kept across a respawn)
	}
	if autonomous {
		return "", fmt.Errorf("task %s has no stored brief; refusing to dispatch it onto the manager-send stub (a briefless task is left for the manager)", taskID)
	}
	if err := writeBriefStub(path, taskID, kind); err != nil {
		return "", err
	}
	return path, nil
}

// WriteBrief stores content as task taskID's brief (paths.BriefPath) so the next Spawn
// launches the worker with it as the initial prompt rather than the generic stub, and
// records brief presence on the task row (has_brief) so the scheduler can gate
// auto-dispatch on a stored brief without probing the filesystem. It creates the data
// dir and overwrites any existing brief for the id. `ttorch task add --brief` /
// `spawn --brief` call it.
//
// The file is written FIRST, then the flag is raised, so has_brief is never true without
// a brief actually on disk (a conservative false-negative is safe — the scheduler simply
// skips — while a false-positive would re-introduce the briefless-stall bug). The flag
// update is a no-op when the task row does not exist yet — `ttorch spawn --brief` writes
// the brief before the spawn creates the row, and that spawn launches on the file
// regardless — so it never errors there. The Store is always present in production
// (orchestrator.New); the nil guard keeps the file-write seam unit-testable with a bare
// Manager.
func (m *Manager) WriteBrief(taskID, content string) error {
	if err := writeBrief(m.P.BriefPath(taskID), content); err != nil {
		return err
	}
	if m.Store != nil {
		return m.Store.SetBriefStored(context.Background(), taskID)
	}
	return nil
}

// writeBrief writes body to path as a task brief, creating its parent directory.
func writeBrief(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func writeBriefStub(path, id, kind string) error {
	body := fmt.Sprintf(`# Task %s (%s)

Ramp up first: read this repo's AGENTS.md / CLAUDE.md for project context and
conventions, and use any relevant skills available to you, before changing code.

Your full task brief is on its way — the manager sends it via `+"`ttorch send`"+` moments
after spawn. Wait for it, ramping up (as above) in the meantime. Do NOT report needs-input
asking for the brief — it is coming, and a premature ping is a false alarm that wakes the
manager for nothing. Work only within this worktree; commit on a feature branch. Do not
address the lead directly.

## Reporting (mandatory — this is how the manager sees you)

The manager is woken ONLY by your status reports; run them from this worktree (they
resolve your task automatically):

- `+"`ttorch report active`"+` when you start the work.
- `+"`ttorch report done`"+` the moment the work is complete and ready for review,
  `+"`ttorch report blocked -m \"why\"`"+` when you cannot proceed, and
  `+"`ttorch report needs-input -m \"the question\"`"+` when you need a decision. You
  MUST call one of these at the matching transition — the manager does not poll you.
- `+"`ttorch stage \"<phase>\"`"+` for fine progress (e.g. implementing, testing,
  validating) and `+"`ttorch note \"<text>\"`"+` for activity worth recording. Neither
  wakes the manager.
- `+"`ttorch follow-on <new-id> --title \"…\" [--touches \"a,b\"]`"+` to file a child
  task for out-of-scope work you discover; it lands in the backlog, it does not spawn.
`, id, kind)
	return writeBrief(path, body)
}

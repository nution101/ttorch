// Package orchestrator composes the runtime primitives (tmux, worktree, harness,
// state) into the operations the manager drives: spawn, status, peek, send,
// teardown, plus launching the manager session and ad-hoc cc sessions.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/profile"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/termtab"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// requiredReviewers are the adversarial-review dimensions every trust verdict must
// cover. It is the single source of truth for both recording (which per-dimension
// reports to aggregate) and completeness (a missing dimension fails closed).
var requiredReviewers = []string{review.DimensionCorrectness, review.DimensionScope, review.DimensionSecurity}

// Manager performs runtime operations against a tmux session and the state store.
type Manager struct {
	P       paths.Paths
	Session string
	Store   *db.Store
	Pool    worktree.Pool
}

// New builds a Manager from the standard paths. It opens the SQLite state store
// (running migrations, which can fail) and runs the one-shot legacy-JSON import, so
// it now returns an error. A short-lived CLI command opens one Manager and closes it
// (Close) at the end of the process; the long-blocking watcher holds its store for
// its lifetime.
func New(p paths.Paths) (*Manager, error) {
	store, err := db.Open(p.StateDB())
	if err != nil {
		return nil, err
	}
	m := &Manager{
		P:       p,
		Session: tmux.SessionName(),
		Store:   store,
		Pool:    worktree.Pool{Root: p.Worktrees(), Max: worktree.MaxFromEnv()},
	}
	// Migrate any pre-SQLite JSON state into the DB (one-shot, idempotent — §2.5).
	// A task's tmux window decides its imported status (active vs torn_down). This is
	// best-effort: the legacy source is preserved either way (state.migrated/), so a
	// transient import hiccup must not brick startup.
	if _, err := db.ImportLegacy(context.Background(), store, p.StateDir(), func(window string) bool {
		return tmux.WindowExists(m.Session, window)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: legacy state import skipped: %v\n", err)
	}
	return m, nil
}

// Close releases the underlying state store. Short-lived CLI commands defer it.
func (m *Manager) Close() error {
	if m.Store == nil {
		return nil
	}
	return m.Store.Close()
}

// liveTasks returns the tracked tasks that are not in a terminal state — the
// DB-backed equivalent of the old state.List(), which only ever held live records
// because Teardown deleted them. Retained rows (torn_down/abandoned) are filtered
// out so callers see exactly today's live fleet.
func (m *Manager) liveTasks() []db.Task {
	tasks, _ := m.Store.ListTasks(context.Background(), db.TaskFilter{})
	var out []db.Task
	for _, t := range tasks {
		if t.Status == db.StatusTornDown || t.Status == db.StatusAbandoned {
			continue
		}
		out = append(out, t)
	}
	return out
}

// inUseWorktrees returns the worktree paths held by active tasks for a repo.
func (m *Manager) inUseWorktrees(repo string) []string {
	var out []string
	for _, t := range m.liveTasks() {
		if t.Project == repo && t.Worktree != "" {
			out = append(out, t.Worktree)
		}
	}
	return out
}

// killPaneProcesses reaps a window's pane process group so a returned worktree is
// not held by lingering children.
func (m *Manager) killPaneProcesses(window string) {
	pid := tmux.PanePID(m.Session, window)
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(150 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func (m *Manager) requireTmux() error {
	if !tmux.Available() {
		return errors.New("tmux is required (run 'ttorch doctor' to install it)")
	}
	return nil
}

// windowLabel returns the friendly tab title for a task window: scouts are tagged
// so they stand out; ship and cc tasks read as just the task id. The window's tmux
// name (e.g. "wk-<id>") is unaffected — it stays the stable target.
func windowLabel(kind, id string) string {
	if kind == "scout" {
		return "scout · " + id
	}
	return id
}

// newWindow creates a ttorch window and gives it a friendly display title (the
// terminal tab name) while keeping its tmux name as the stable target. Labeling is
// best-effort, so only the window creation can fail the caller.
func (m *Manager) newWindow(window, cwd, label string) error {
	if err := tmux.NewWindow(m.Session, window, cwd); err != nil {
		return err
	}
	_ = tmux.LabelWindow(m.Session, window, label)
	return nil
}

// Spawn starts a worker for taskID against the repo containing projectPath. If
// rawCmd is empty, it launches the detected harness with the task brief; otherwise
// it runs rawCmd (used for testing and escape hatches). It declares no footprint,
// so it is exempt from overlap enforcement — the back-compat entry point.
func (m *Manager) Spawn(taskID, projectPath string, scout bool, rawCmd string) (db.Task, error) {
	return m.SpawnWithFootprint(taskID, projectPath, scout, rawCmd, nil, false)
}

// SpawnWithFootprint is Spawn plus deterministic overlap prevention: footprint is
// the repo-relative paths/prefixes the task will touch. When it is non-empty and
// forceOverlap is false, the spawn is REFUSED if the footprint overlaps any live
// worker's footprint (in the same repo), naming the conflicting task — so two
// workers are never dispatched onto the same files. The check runs before any side
// effect (worktree, window), so a refusal leaves nothing behind. A declared
// footprint is recorded on the task; an empty footprint enforces nothing.
//
// The gate reads the persisted, live worker set and the task's own footprint is
// saved only after its window comes up, so the no-overlap guarantee holds for
// serial dispatch (the manager dispatches one worker at a time). This matches the
// unlocked worktree-pool semantics; two truly concurrent spawns of overlapping
// footprints are not serialized.
func (m *Manager) SpawnWithFootprint(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool) (db.Task, error) {
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

	// Deterministic overlap gate: refuse a dispatch that would put this worker onto
	// files a live worker already holds. Done before acquiring any resource so a
	// refusal has no side effects; --force-overlap is the explicit manager override.
	if len(footprint) > 0 && !forceOverlap {
		if conflicts := m.CheckOverlap(repo, footprint); len(conflicts) > 0 {
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

	wt, err := m.Pool.Acquire(repo, m.inUseWorktrees(repo))
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
		brief := m.briefForLaunch(taskID, kind)
		// Prepend TTORCH_TASK_ID/TTORCH_DB so the worker's reporting commands resolve
		// their task + DB from the launch env (the .ttorch/task file is the durable
		// fallback that also survives a resume). §3.1.
		cmd = harness.WorkerLaunchPrefix(taskID, dbPath) + harness.BriefCommand(h, brief, sid)
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

// Live reports whether a task's tmux window is still present.
func (m *Manager) Live(t db.Task) bool {
	return tmux.WindowExists(m.Session, t.Window)
}

// Status returns the live (non-terminal) tracked tasks.
func (m *Manager) Status() ([]db.Task, error) { return m.liveTasks(), nil }

// DeriveState classifies a worker from observable inputs: whether its tmux window
// is live and a recent capture of its pane. It is the pure core of TaskState, kept
// separate so it can be tested without tmux. The states are:
//   - "gone":    the window is no longer present
//   - "working": the pane shows a busy indicator (mid-turn)
//   - "idle":    the window is live but not busy (finished / awaiting input)
//
// It shares the busy-detection heuristic (livestate.Busy) with the watcher's
// liveness sweeps so `ttorch status` and the events the manager receives never
// disagree about whether a worker is mid-turn.
func DeriveState(live bool, pane string) string {
	if !live {
		return "gone"
	}
	if livestate.Busy(pane) {
		return "working"
	}
	return "idle"
}

// TaskState reports a worker's live state for `ttorch status` (see DeriveState).
// A live pane that can't be captured falls back to "idle".
func (m *Manager) TaskState(t db.Task) string {
	if !tmux.WindowExists(m.Session, t.Window) {
		return DeriveState(false, "")
	}
	out, _ := tmux.CapturePane(m.Session, t.Window, 6)
	return DeriveState(true, out)
}

// Peek returns the last n lines of a worker's pane.
func (m *Manager) Peek(taskID string, lines int) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	return tmux.CapturePane(m.Session, t.Window, lines)
}

// Send types a line into a worker's pane. It refuses loudly when the worker has no
// live window (torn down or never came up) rather than letting the keystrokes vanish
// into a dead target — a dropped message must fail, never silently no-op.
func (m *Manager) Send(taskID, text string) error {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if !tmux.WindowExists(m.Session, t.Window) {
		return fmt.Errorf("task %q has no live window to receive %q; it was torn down or never started", taskID, text)
	}
	return tmux.SendLine(m.Session, t.Window, text)
}

// closeTermTab closes a worker's native terminal view tab on teardown. It is a
// package-level seam (like landIntegrate) so a test can confirm Teardown closes the
// tab without driving real macOS GUI scripting (which termtab.Close gates off under
// test anyway).
var closeTermTab = termtab.Close

// Teardown finishes a task: it refuses to discard UNLANDED work — a worktree with
// uncommitted changes OR a task branch with commits not yet merged into the default
// branch — unless force is set, then kills the worker's tmux window, closes its native
// terminal view tab, returns the worktree to the pool, and retains the row as torn_down
// with its worktree blanked (§3.4). On --force, any committed-but-unmerged work is first
// stashed under a recovery ref so a forced teardown is never an unrecoverable delete.
func (m *Manager) Teardown(taskID string, force bool) ([]string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	var notes []string
	// The git dir to evaluate the task branch from: prefer the repo (stable across the
	// teardown), fall back to the live worktree. Both share one ref store, so the task
	// branch (ttorch/<id>) and the default branch resolve from either.
	gitDir := t.Project
	if gitDir == "" {
		gitDir = t.Worktree
	}
	branch := taskBranch(taskID)
	if !force {
		if dirty, _ := worktree.IsDirty(t.Worktree); dirty {
			return nil, fmt.Errorf("task %q has uncommitted changes; review it, then 'ttorch teardown %s --force'", taskID, taskID)
		}
		// Committed-but-unmerged work is as destructible as uncommitted work: a clean
		// worktree can still hold commits on ttorch/<id> that no merge target contains,
		// and Pool.Release deletes the task branch — so without this guard those commits
		// are lost. Refuse, naming the commits; --force (which stashes a recovery ref
		// below) is the explicit override.
		if gitDir != "" && worktree.RefExists(gitDir, branch) {
			def := worktree.DefaultBranch(gitDir)
			unmerged, uerr := worktree.UnmergedCommits(gitDir, branch, def, "origin/"+def)
			if uerr != nil {
				return nil, fmt.Errorf("task %q: could not verify %s is merged into %s (%v); review it, then 'ttorch teardown %s --force' to discard", taskID, branch, def, uerr, taskID)
			}
			if len(unmerged) > 0 {
				return nil, fmt.Errorf("task %q has %d committed change(s) on %s not merged into %s:\n  %s\nmerge or land them, then 'ttorch teardown %s --force' to discard (the discarded commits are saved to a recovery ref)",
					taskID, len(unmerged), branch, def, strings.Join(unmerged, "\n  "), taskID)
			}
		}
	} else if gitDir != "" && worktree.RefExists(gitDir, branch) {
		// --force: stash any committed-but-unmerged work under a recovery ref BEFORE the
		// branch is deleted, so a forced discard stays recoverable.
		if note := m.stashDiscardedBranch(gitDir, taskID, branch); note != "" {
			notes = append(notes, note)
		}
	}
	// Kill the worker's tmux window AND close its native terminal view tab so teardown
	// leaves no zombie tab behind (Part B). closeTermTab tears down the view session;
	// the view's exec'd tmux (see termtab.viewCommand) then exits and the terminal
	// closes the now-empty tab.
	m.killPaneProcesses(t.Window)
	_ = tmux.KillWindow(m.Session, t.Window)
	closeTermTab(t.Window)
	if t.Project != "" && t.Worktree != "" {
		if err := m.Pool.Release(t.Project, t.Worktree); err != nil {
			notes = append(notes, "worktree: "+err.Error())
		} else {
			notes = append(notes, "worktree returned to pool for reuse")
		}
	}
	// The DB retains the row (rows are never hard-deleted, §3.4/§7): mark it torn_down
	// with a typed, non-actionable event so it drops out of the live fleet (liveTasks)
	// while its history is preserved, and BLANK the worktree so the retained row never
	// aliases a worktree the pool may reassign (§3.4). Status + worktree-blank + event
	// commit atomically in one transaction.
	fields := db.TaskFields{}
	if t.Worktree != "" {
		blank := ""
		fields.Worktree = &blank
	}
	if _, err := m.Store.RecordTransition(context.Background(), taskID, db.StatusTornDown, fields, db.EventTornDown, db.ActorManager, ""); err != nil {
		notes = append(notes, "state: "+err.Error())
	}
	return notes, nil
}

// stashDiscardedBranch saves a task branch's committed-but-unmerged commits under a
// recovery ref (refs/ttorch/discarded/<id>-<sha>) before a forced teardown deletes the
// branch, returning a human note describing where they went (or the failure). It returns
// "" ONLY when the branch is provably fully merged/landed (its commits live on a base
// ref) — nothing to preserve. The unmerged set is computed against the same bases the
// non-force guard uses, so the two paths agree on what counts as unlanded. Crucially,
// when merge status CANNOT be determined (UnmergedCommits errors), it still preserves the
// branch tip and warns: the feature's whole purpose is that a forced teardown is never an
// unrecoverable delete, so an "unknown" status must fail safe toward preservation — the
// mirror of the non-force path, which refuses on that same error.
func (m *Manager) stashDiscardedBranch(gitDir, taskID, branch string) string {
	def := worktree.DefaultBranch(gitDir)
	unmerged, err := worktree.UnmergedCommits(gitDir, branch, def, "origin/"+def)
	if err == nil && len(unmerged) == 0 {
		return "" // provably merged/landed — its commits live on a base ref already
	}
	// Either the branch holds unique commits, or merge status is undeterminable: preserve
	// the tip rather than risk discarding committed work unrecoverably. If even the tip
	// cannot be resolved, surface that loudly (mirroring the SetRef-failure path) rather
	// than returning "" — a silent "" reads as "nothing to preserve" and the branch is
	// then deleted, the exact unrecoverable delete this feature exists to prevent.
	tip, rerr := worktree.ResolveRef(gitDir, branch)
	if rerr != nil {
		return "could not save discarded commits: " + rerr.Error()
	}
	ref := "refs/ttorch/discarded/" + taskID + "-" + short(tip)
	if serr := worktree.SetRef(gitDir, ref, tip); serr != nil {
		return "could not save discarded commits: " + serr.Error()
	}
	if err != nil {
		return fmt.Sprintf("could not verify %s is merged (%v); saved its tip to %s (recover with: git -C %s log %s)", branch, err, ref, gitDir, ref)
	}
	return fmt.Sprintf("saved %d discarded commit(s) to %s (recover with: git -C %s log %s)", len(unmerged), ref, gitDir, ref)
}

// uninitNotice returns the one-line nudge to print when path is inside a git repo that
// has not been `ttorch init`'d AND auto-init declined to write it (because the repo
// tracks AGENTS.md/CLAUDE.md — see autoInit), or "" when no nudge is warranted (a non-git
// path or an already-initialized repo). Splitting the decision from the printing keeps it
// testable without tmux. The mode reported is whatever projectinit.ReadMode resolves,
// which defaults to "pr" for an uninitialized repo. It is strictly read-only.
func uninitNotice(path string) string {
	repo, err := worktree.RepoRoot(path)
	if err != nil || projectinit.Initialized(repo) {
		return ""
	}
	return fmt.Sprintf("%s not ttorch-init'd; using delivery-mode=%s (run \"ttorch init\" to persist).",
		repo, projectinit.ReadMode(repo))
}

// tracksConventionFile reports whether the repo git-tracks AGENTS.md or CLAUDE.md. When
// it does, auto-init must NOT write: both projectinit.Init and profile.Apply upsert a
// managed block into AGENTS.md (and Init re-links CLAUDE.md), so writing would modify a
// tracked file, dirtying the checkout and tripping merge-local's HasTrackedChanges gate —
// the exact regression that retired the original auto-init (a848c1c). `git ls-files`
// lists only tracked paths, so a non-empty result for either means it is tracked. A git
// error fails safe (treated as tracked): decline to write rather than risk dirtying.
func tracksConventionFile(repo string) bool {
	out, err := exec.Command("git", "-C", repo, "ls-files", "--", "AGENTS.md", "CLAUDE.md").Output()
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}

// autoInit restores ttorch's zero-config first-use setup for the repo containing path:
// it writes the AGENTS.md managed block, the CLAUDE.md symlink, and the project profile
// so a worker always has AGENTS.md to read, without the lead having to run `ttorch init`.
//
// It is safe by construction — it writes ONLY when doing so introduces no change to a
// git-tracked file. The original auto-init (removed in a848c1c) injected the managed
// block into a tracked, committed AGENTS.md, which left the lead's checkout with an
// uncommitted tracked-file change and blocked merge-local's clean fast-forward
// (worktree.HasTrackedChanges). Here, when the repo tracks AGENTS.md/CLAUDE.md, auto-init
// declines to write and instead prints the one-line nudge directing the lead to
// `ttorch init` (which they then commit themselves). For the common case — a repo that
// does not commit these convention files — the new AGENTS.md/CLAUDE.md are untracked,
// which the merge gate explicitly tolerates (see MergeLocal). The write itself is
// clobber-safe and idempotent (projectinit.Init preserves developer content).
//
// It no-ops outside a git repo, on an already-initialized repo, and when
// TTORCH_NO_AUTOINIT is set.
func autoInit(path string) {
	if os.Getenv("TTORCH_NO_AUTOINIT") != "" {
		return
	}
	repo, err := worktree.RepoRoot(path)
	if err != nil || projectinit.Initialized(repo) {
		return
	}
	// Safety gate: never modify a tracked file (the a848c1c regression). If the repo
	// commits AGENTS.md/CLAUDE.md, decline and nudge the lead toward explicit `ttorch
	// init` rather than dirtying the tree and breaking merge-local.
	if tracksConventionFile(repo) {
		if msg := uninitNotice(repo); msg != "" {
			fmt.Fprintln(os.Stderr, "ttorch: "+msg)
		}
		return
	}
	notes, err := projectinit.Init(repo, "pr")
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "ttorch: set up %s for ttorch (set TTORCH_NO_AUTOINIT=1 to skip)\n", repo)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "  "+n)
	}
	if p, err := profile.Apply(repo); err == nil {
		stack := p.Stack
		if stack == "" {
			stack = "unknown"
		}
		fmt.Fprintf(os.Stderr, "  wrote project profile (stack: %s)\n", stack)
	}
}

// InitRepo performs the explicit, consented first-use setup for the repo containing
// projectPath: it writes the AGENTS.md managed block, the CLAUDE.md symlink (both
// clobber-safe), and the project profile. This is the opt-in write path behind
// `ttorch init` and `ttorch spawn --init`; plain spawn never calls it. It returns
// human-readable notes describing what changed.
func (m *Manager) InitRepo(projectPath, mode string) ([]string, error) {
	repo, err := worktree.RepoRoot(projectPath)
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", projectPath)
	}
	notes, err := projectinit.Init(repo, mode)
	if err != nil {
		return nil, err
	}
	if p, err := profile.Apply(repo); err == nil {
		stack := p.Stack
		if stack == "" {
			stack = "unknown"
		}
		notes = append(notes, fmt.Sprintf("wrote project profile (stack: %s)", stack))
	}
	return notes, nil
}

// StartManager attaches the lead to the manager. If the manager window is already
// live it just attaches; otherwise it rebuilds. When saved state exists (a manager
// record or any task), it restores the whole team — the manager window plus every
// worker tab, each resumed to its prior conversation — so a stop, reboot, crash, or
// `ttorch update` is recoverable by simply running `ttorch`. With no saved state it
// starts a fresh manager in the current directory.
func (m *Manager) StartManager() error {
	if err := m.requireTmux(); err != nil {
		return err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	if tmux.WindowExists(m.Session, "manager") {
		fmt.Fprintln(os.Stderr, "ttorch: attaching to your running manager — 'ttorch stop' to end it (then 'ttorch' in another folder to restart there).")
		return m.attachManager()
	}

	_, ok, _ := m.Store.GetManager(context.Background())
	tasks := m.liveTasks()
	if ok || len(tasks) > 0 {
		notes := m.restore()
		fmt.Fprintln(os.Stderr, "ttorch: restoring your saved session — manager and workers resume where they left off.")
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, "  "+n)
		}
		return m.attachManager()
	}

	// Fresh start: no saved session.
	dir := cwd()
	sid := harness.NewSessionID()
	if err := m.Store.SetManager(context.Background(), db.Manager{Dir: dir, SessionID: sid}); err != nil {
		return err
	}
	autoInit(dir) // zero-config first-use setup of the launch dir; tracked-file-safe (a848c1c)
	if err := m.newWindow("manager", dir, "manager"); err != nil {
		return err
	}
	_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(harness.Resolve(), sid, m.charterFile()))
	fmt.Fprintf(os.Stderr, "ttorch: manager started in %s — tell it the repo to work on; 'ttorch stop' to end.\n", dir)
	return m.attachManager()
}

// charterFile ensures the manager charter file exists and returns its path; on a
// write failure it returns "" so the launch falls back to an inline charter.
func (m *Manager) charterFile() string {
	p := m.P.ManagerCharterFile()
	if err := harness.WriteManagerCharter(p); err != nil {
		return ""
	}
	return p
}

// attachManager opens the manager window for the lead: it prefers a native iTerm2
// window on macOS, falling back to a plain tmux attach.
func (m *Manager) attachManager() error {
	if termtab.OpenManagerSession(m.Session, "manager") {
		fmt.Fprintln(os.Stderr, "ttorch: opened the manager in a new iTerm2 window (now in front) — workers open as tabs there; this terminal is free.")
		return nil
	}
	return tmux.Attach(m.Session, "manager")
}

// restore rebuilds any missing windows from saved state, resuming each session to
// its prior conversation. It is best-effort: a single window that fails to rebuild
// is noted but never aborts the rest. It returns human-readable notes.
func (m *Manager) restore() []string {
	var notes []string
	if err := tmux.EnsureSession(m.Session); err != nil {
		return []string{"could not ensure tmux session: " + err.Error()}
	}
	h := harness.Resolve()

	// Manager window first, so there is always a manager to talk to.
	if !tmux.WindowExists(m.Session, "manager") {
		mgr, ok, _ := m.Store.GetManager(context.Background())
		if ok {
			if err := m.newWindow("manager", mgr.Dir, "manager"); err != nil {
				notes = append(notes, "skipped manager ("+err.Error()+")")
			} else {
				_ = tmux.SendLine(m.Session, "manager", harness.ManagerResumeOrFresh(h, mgr.SessionID, m.charterFile()))
				notes = append(notes, "restored manager")
			}
		} else {
			// No manager record (legacy state): start a fresh manager so the lead
			// always has one, and persist it for next time.
			dir := cwd()
			sid := harness.NewSessionID()
			if err := m.Store.SetManager(context.Background(), db.Manager{Dir: dir, SessionID: sid}); err != nil {
				notes = append(notes, "could not persist new manager record: "+err.Error())
			}
			if err := m.newWindow("manager", dir, "manager"); err != nil {
				notes = append(notes, "skipped manager ("+err.Error()+")")
			} else {
				_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(h, sid, m.charterFile()))
				notes = append(notes, "started a fresh manager (no saved manager record)")
			}
		}
	}

	// Workers: rebuild each task window whose worktree still exists. liveTasks already
	// excludes torn-down rows; cc sessions are skipped (ad-hoc, lead-driven, §7).
	tasks := m.liveTasks()
	for _, t := range tasks {
		if t.Kind == "cc" {
			continue // ad-hoc, lead-driven sessions are not auto-restored
		}
		if tmux.WindowExists(m.Session, t.Window) {
			continue
		}
		if _, err := os.Stat(t.Worktree); err != nil {
			notes = append(notes, fmt.Sprintf("skipped %s (worktree gone)", t.ID))
			continue
		}
		if err := m.newWindow(t.Window, t.Worktree, windowLabel(t.Kind, t.ID)); err != nil {
			notes = append(notes, fmt.Sprintf("skipped %s (%s)", t.ID, err.Error()))
			continue
		}
		_ = tmux.SendLine(m.Session, t.Window, harness.WorkerResumeOrFresh(h, t.SessionID, m.P.BriefPath(t.ID)))
		_ = termtab.Open(m.Session, t.Window)
		notes = append(notes, "restored "+t.ID)
	}
	return notes
}

// Resume rebuilds the manager and every worker tab from saved state, resuming each
// to its prior conversation. The caller attaches afterwards.
func (m *Manager) Resume() ([]string, error) {
	if err := m.requireTmux(); err != nil {
		return nil, err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return nil, err
	}
	return m.restore(), nil
}

// Reset discards the saved session for a clean start: it kills the tmux session
// (if present) and removes the manager record and every task record. It never
// deletes worktrees or branches.
func (m *Manager) Reset() ([]string, error) {
	var notes []string
	if tmux.Available() && tmux.HasSession(m.Session) {
		if err := tmux.KillSession(m.Session); err != nil {
			notes = append(notes, "tmux: "+err.Error())
		} else {
			notes = append(notes, "killed the ttorch tmux session")
		}
	}
	// Discard the saved session by dropping and recreating the schema (a true clean
	// slate). The store has no per-row delete by design (the model is append-only /
	// status-based), so the reset is a schema down+up via the reversible migrations.
	// This wipes the global DB — the same scope as the old global state/ wipe — and,
	// like the old reset, NEVER touches worktrees or branches.
	ctx := context.Background()
	tasks, _ := m.Store.ListTasks(ctx, db.TaskFilter{})
	if err := m.Store.MigrateDown(ctx, 0); err != nil {
		notes = append(notes, "state reset (drop): "+err.Error())
		return notes, nil
	}
	if err := m.Store.Migrate(ctx); err != nil {
		notes = append(notes, "state reset (recreate): "+err.Error())
		return notes, nil
	}
	notes = append(notes, fmt.Sprintf("discarded the saved session (%d task record(s)); worktrees and branches were kept", len(tasks)))
	return notes, nil
}

// StopSession tears down the ttorch tmux session (and all its windows). It does
// NOT clear state, so the session can be resumed later with `ttorch` or
// `ttorch resume`.
func (m *Manager) StopSession() ([]string, error) {
	if !tmux.Available() || !tmux.HasSession(m.Session) {
		return []string{"no ttorch session was running"}, nil
	}
	windows, _ := tmux.ListWindows(m.Session)
	workers := 0
	for _, w := range windows {
		if strings.HasPrefix(w, "wk-") {
			workers++
		}
	}
	if err := tmux.KillSession(m.Session); err != nil {
		return nil, err
	}
	notes := []string{fmt.Sprintf("stopped the ttorch session %q (%d window(s))", m.Session, len(windows))}
	if workers > 0 {
		notes = append(notes, fmt.Sprintf("%d worker(s) were running; their worktrees remain in the pool", workers))
	}
	notes = append(notes, "run 'ttorch' to resume where you left off, or 'ttorch reset' to discard the saved session")
	return notes, nil
}

// OpenCC opens an interactive harness session inside the ttorch session as a tracked
// window, so the manager is aware of it. With isolated, it gets its own worktree.
// ccSuffix returns a short random hex tag that disambiguates two cc sessions opened
// in the same wall-clock second (their HHMMSS prefixes are identical), so their ids
// never collide on the tasks TEXT PRIMARY KEY (§3.4). It is a var so a test can make
// it deterministic.
var ccSuffix = func() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; degrade to a fixed tag rather than panic so
		// an ad-hoc cc session still opens (its DB row is best-effort anyway).
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

// ccID builds the id for an ad-hoc cc session: cc-HHMMSS-<4hex> (§3.4).
func ccID() string { return "cc-" + time.Now().Format("150405") + "-" + ccSuffix() }

func (m *Manager) OpenCC(isolated bool) error {
	if err := m.requireTmux(); err != nil {
		return err
	}
	dir := cwd()
	id := ccID()
	window := id
	if isolated {
		if repo, err := worktree.RepoRoot(dir); err == nil {
			if wt, err := m.Pool.Acquire(repo, m.inUseWorktrees(repo)); err == nil {
				dir = wt
			}
		}
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return err
	}
	if err := m.newWindow(window, dir, windowLabel("cc", id)); err != nil {
		return err
	}
	h := harness.Resolve()
	if repo, err := worktree.RepoRoot(dir); err == nil {
		harness.TrustWorktree(h, repo, dir)
	} else {
		harness.TrustWorktree(h, dir)
	}
	_ = tmux.SendLine(m.Session, window, harness.InteractiveCommand(h))
	// Track the cc session (best-effort, as before). Every task needs a project (FK),
	// so the session's directory is upserted as the project — a cc session may not be
	// in a git repo, and repo_path is only a grouping/display key here.
	ctx := context.Background()
	if proj, err := m.Store.UpsertProject(ctx, dir, ""); err == nil {
		_, _ = m.Store.CreateTask(ctx, db.Task{
			ID: id, ProjectID: proj.ID, Window: window, Worktree: dir, Harness: h,
			Kind: db.KindCC, Created: time.Now(), Status: db.StatusActive,
		}, db.ActorManager)
	}
	return tmux.Attach(m.Session, window)
}

// --- delivery lifecycle (M4) ---

// ReviewDiff returns a worker's changes against the repo's default branch.
func (m *Manager) ReviewDiff(taskID string, stat bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	base := worktree.DefaultBranch(t.Project)
	return worktree.Diff(t.Worktree, base, stat)
}

// Validate runs the worktree's detected checks for a task. It returns nil results
// when no checks are detected (the caller reports that distinctly from a pass).
func (m *Manager) Validate(taskID string) ([]validate.Result, error) {
	ctx := context.Background()
	t, ok, err := m.Store.GetTask(ctx, taskID)
	if err != nil || !ok {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	steps := validate.Detect(t.Worktree)
	if len(steps) == 0 {
		return nil, nil
	}
	results := validate.Run(t.Worktree, steps)
	// Record a typed, manager-authored, non-actionable 'validated' event carrying the
	// pass/fail tally (§3.4). Best-effort: the results are already produced for the
	// caller, so a failed audit append must not mask them.
	passed := len(results) - len(validate.Failures(results))
	if _, err := m.Store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventValidated, Actor: db.ActorManager,
		Payload: fmt.Sprintf("%d/%d checks passed", passed, len(results)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the validated event for %s: %v\n", taskID, err)
	}
	return results, nil
}

// Approve grants a short-lived approval token authorizing a merge for taskID.
// This is intended for the lead to run, not the manager.
func (m *Manager) Approve(taskID string, ttl time.Duration) error {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return err
	}
	if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("human", head)); err != nil {
		return err
	}
	// Record that THIS approval was the lead's — both in the token's provenance (the
	// authority for the merge's audit label) and in the persisted task state (overwriting
	// any prior auto-mint marker so the two never drift). RecordDelivery preserves the
	// gate/sha fields it is given; the 'approved' event is manager/lead-authored and
	// non-actionable, so it never wakes a watcher.
	if err := m.Store.RecordDelivery(context.Background(), taskID, db.Delivery{
		GatePassed: t.GatePassed, ApprovedBy: "human", ReviewedSHA: t.ReviewedSHA,
		EventType: db.EventApproved, Actor: db.ActorLead,
	}); err != nil {
		return err
	}
	m.audit(fmt.Sprintf("approve task=%s commit=%s ttl=%s", taskID, short(head), ttl))
	return nil
}

// gitOut runs `git -C dir <args...>` and returns its raw stdout, enriching the error with
// git's stderr on failure. The trust-prep path uses it to shell `git rev-list` and `git
// diff` directly: the equivalent reads live in internal/worktree, but a sibling change owns
// that file, so the trust gate runs these here rather than collide with it.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// mergeBaseDiff returns the COMMITTED three-dot diff `git diff base...rev`: the diff from
// the merge-base of base and rev to rev, i.e. ONLY rev's own changes. The trust gate stages
// this for the reviewers so any lead base gained since rev was cut never appears — a two-dot
// `git diff base rev` renders that lead as phantom reverts, which burned a full
// three-reviewer pass and nearly masked a real bug (the cosign-strict / liveness-dwell
// near-miss). Reads committed objects only, never a working tree.
func mergeBaseDiff(dir, base, rev string) (string, error) {
	return gitOut(dir, "diff", base+"..."+rev)
}

// TrustPrep materializes the inputs the adversarial reviewers read for taskID into
// ReviewInputsDir: the COMMITTED three-dot diff against the default branch (diff.patch),
// the brief (brief.md, if one was written), a fresh validate of the committed sha
// (validate.json), and the reviewed HEAD (head.txt). It refuses a dirty worktree and reads
// only committed objects, so the reviewers see exactly the commit that will fast-forward —
// a worker cannot present a benign working tree while a different commit merges.
//
// It also refuses a STALE BASE up front: if the default branch carries commits the worker's
// HEAD lacks, prep fails (staging nothing) and tells the manager to rebase the worker first.
// Reviewing a stale-base branch diffs against a base that no longer matches what merges, the
// merge gate would refuse the fast-forward anyway, and the diff would otherwise surface the
// default's own lead as phantom reverts. It returns the inputs dir.
func (m *Manager) TrustPrep(taskID string) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	// Reviewed state must equal the committed state that will merge.
	if clean, err := worktree.IsClean(t.Worktree); err != nil || !clean {
		return "", fmt.Errorf("worktree for %q is not clean; commit or discard changes before review so the reviewers see exactly the committed diff that will merge", taskID)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
	}
	def := worktree.DefaultBranch(t.Project)

	// Stale-base guard — run BEFORE staging any inputs or dispatching reviewers. If the
	// default branch carries commits the worker's HEAD lacks, the branch was cut from an
	// older base: the merge gate would refuse the fast-forward anyway, and a base-relative
	// review diff would render the default's own lead as phantom reverts — which burned a
	// full three-reviewer pass and nearly masked a real bug (the cosign-strict /
	// liveness-dwell near-miss). `git rev-list <head>..<def>` lists exactly the commits the
	// default has that the worker lacks; any output means the base is stale. Fail loudly so
	// the manager rebases the worker onto the current default first, and stage nothing.
	behind, err := gitOut(t.Worktree, "rev-list", head+".."+def)
	if err != nil {
		return "", fmt.Errorf("trust prep %q: could not check whether the branch is based on the current %s: %w", taskID, def, err)
	}
	if behind = strings.TrimSpace(behind); behind != "" {
		return "", fmt.Errorf("trust prep: the branch for %q is %d commit(s) behind %s and its base is stale; have the worker rebase onto the current %s before review, then re-run 'ttorch trust prep %s'", taskID, len(strings.Split(behind, "\n")), def, def, taskID)
	}

	dir := m.P.ReviewInputsDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The reviewers' diff is the COMMITTED three-dot diff `git diff <def>...<head>` (the
	// merge-base diff), so it contains ONLY the branch's own changes — never any lead the
	// default gained since the branch was cut. The stale-base guard above makes <def> an
	// ancestor of <head> here, but the three-dot form is the correct, intent-revealing way
	// to diff a branch against its base, and is defense in depth against a phantom-revert
	// diff. Reads committed objects only, never the working tree.
	diff, err := mergeBaseDiff(t.Worktree, def, head)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "diff.patch"), []byte(diff), 0o644); err != nil {
		return "", err
	}
	// A worker may run without a written brief; copy it only when present.
	if b, err := os.ReadFile(m.P.BriefPath(taskID)); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), b, 0o644); err != nil {
			return "", err
		}
	}
	// validate.json reflects the gate's own check of the committed sha (default-branch
	// definition, immutable checkout) — the same notion of "green" the gate enforces.
	_, results, _ := validateCommitted(t.Project, head) // nil when no checks are detected
	vb, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "validate.json"), append(vb, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), []byte(head+"\n"), 0o644); err != nil {
		return "", err
	}
	m.audit(fmt.Sprintf("trust-prep task=%s commit=%s", taskID, short(head)))
	return dir, nil
}

// TrustRecord aggregates the reviewers' per-dimension reports for taskID into a
// commit-pinned verdict and persists it, recording GatePassed/ReviewedSHA on the
// task. The sha it covers must still be the worker's HEAD (a record-time pin against
// a commit landing after review).
//
// In every mode except trusted the verdict authorizes nothing on its own — it is
// advisory, and a merge still requires the human approval token. In trusted mode a
// PASS verdict whose worktree is also fresh-validate green auto-mints the approval
// token (ApprovedBy="auto"): this is the "merge without a human reading the diff"
// path. A no-checks-detected repo is NOT green (an empty Failures() must never read as
// a pass), so it never auto-approves; the same fail-closed re-check runs again at the
// merge gate in MergeLocal.
func (m *Manager) TrustRecord(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("review covers %s but the worker HEAD is now %s; re-run 'ttorch trust prep %s' and review again", short(sha), short(head), taskID)
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, requiredReviewers)
	if err != nil {
		return zero, err
	}
	// Pin the reviewed diff's content identity onto the verdict (the committed three-dot diff
	// the reviewers read) so a later clean rebase onto an advanced default can carry the
	// verdict forward without re-running the reviewers — see carryVerdictForward. Computed from
	// committed objects, so it is independent of the worktree state.
	patch, derr := mergeBaseDiff(t.Worktree, worktree.DefaultBranch(t.Project), sha)
	if derr != nil {
		return zero, fmt.Errorf("trust record %q: could not compute the reviewed diff identity: %w", taskID, derr)
	}
	verdict.DiffID = review.DiffID([]byte(patch))
	if err := review.Write(m.P.ReviewVerdictFile(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	t.ReviewedSHA = sha
	t.GatePassed = verdict.Overall == review.Pass
	t.ApprovedBy = ""
	// Trusted mode is the sole carve-out: a PASS verdict auto-mints the approval token so
	// the lead need not read the diff — but ONLY when the worktree is clean (reviewed
	// state == the committed HEAD that will merge), the worktree passes the gate's fresh
	// validate resolved from the DEFAULT BRANCH (not the worker's own copy), and the diff
	// does not touch the gate definition itself (changing the gate requires a human). The
	// token is bound to the reviewed sha so a later commit invalidates it. All of these
	// are re-checked at the merge in MergeLocal — minting here is an optimization, not the
	// authority. Any non-trusted mode leaves the verdict advisory.
	if verdict.Overall == review.Pass && projectinit.ReadMode(t.Project) == "trusted" {
		base := worktree.DefaultBranch(t.Project)
		clean, cerr := worktree.IsClean(t.Worktree)
		touched, _, terr := diffTouchesGateConfig(t.Project, base, sha)
		green := false
		// A trusted auto-mint's green authority MUST be the default-branch gate script,
		// never ecosystem detection on the worker's checkout (which the worker controls
		// via go.mod/package.json). Without it, leave the verdict advisory — a human must
		// approve — and skip validation entirely so no worker-defined checks run.
		if cerr == nil && terr == nil && clean && !touched && hasDefaultBranchGateScript(t.Project) {
			green, _, _ = validateCommitted(t.Project, sha)
		}
		if green {
			if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("auto", sha)); err != nil {
				return zero, err
			}
			t.ApprovedBy = "auto"
		}
	}
	// Persist the verdict provenance (gate/approval/sha) in one write. The
	// review_recorded event is manager-authored and non-actionable (§1.3). t was
	// mutated above as the accumulator for the auto-mint decision.
	if err := m.Store.RecordDelivery(context.Background(), taskID, db.Delivery{
		GatePassed: t.GatePassed, ApprovedBy: t.ApprovedBy, ReviewedSHA: t.ReviewedSHA,
		EventType: db.EventReviewRecorded, Actor: db.ActorManager,
	}); err != nil {
		return zero, err
	}
	autoMinted := "no"
	if t.ApprovedBy == "auto" {
		autoMinted = "yes"
	}
	m.audit(fmt.Sprintf("trust-record task=%s commit=%s verdict=%s mode=%s auto-approved=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project), autoMinted))
	return verdict, nil
}

// TrustShow returns the current valid (unexpired) verdict for taskID, if any,
// without consuming it.
func (m *Manager) TrustShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.P.ReviewVerdictFile(taskID))
}

// securityVerdictPath is where the standalone, advisory security-everywhere verdict
// lives — beside the review inputs the security reviewer reads, and DISTINCT from the
// trust gate's ReviewVerdictFile so the two never interfere: the advisory pass can
// never mint an approval or satisfy the trusted gate, and recording it never disturbs a
// trust verdict.
func (m *Manager) securityVerdictPath(taskID string) string {
	return filepath.Join(m.P.ReviewInputsDir(taskID), "security-verdict.json")
}

// SecurityReview folds ONLY the security reviewer's report for taskID into a
// commit-pinned verdict and persists it as an advisory result — the security-everywhere
// pass that runs in every delivery mode, not just trusted. It reuses the same inputs
// (materialized by TrustPrep) and the same internal/review aggregation as the trust
// gate, but it is purely advisory: it never mints an approval, never touches the trust
// gate's verdict or the task's gate state, and never blocks a merge. The manager
// surfaces its findings; in non-trusted modes the human approval still governs delivery,
// and in trusted mode the full three-dimension gate (which already includes security) is
// unchanged.
//
// Like TrustRecord it is commit-pinned: the sha it covers must still be the worker's
// HEAD (so a commit landing after the security reviewer ran is rejected rather than
// silently passed). A missing or malformed security.json folds to a "block" advisory
// verdict (fail closed) telling the manager to actually run the reviewer.
func (m *Manager) SecurityReview(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("security review covers %s but the worker HEAD is now %s; re-run 'ttorch security-review prep %s' and review again", short(sha), short(head), taskID)
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, []string{review.DimensionSecurity})
	if err != nil {
		return zero, err
	}
	if err := review.Write(m.securityVerdictPath(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	// Record a typed, manager-authored, non-actionable 'security_recorded' event (§3.4).
	// It is a PURE event append — NOT RecordDelivery — because the security-everywhere
	// pass is advisory and must never touch the task's gate state (gate_passed/
	// approved_by/reviewed_sha); it only notes that the audit ran and its outcome.
	// Best-effort: the verdict is already persisted, so a failed append must not mask it.
	if _, err := m.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventSecurityRecorded, Actor: db.ActorManager,
		Payload: fmt.Sprintf("verdict=%s sha=%s", verdict.Overall, short(sha)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the security_recorded event for %s: %v\n", taskID, err)
	}
	m.audit(fmt.Sprintf("security-review task=%s commit=%s verdict=%s mode=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project)))
	return verdict, nil
}

// SecurityReviewShow returns the current valid (unexpired) advisory security verdict for
// taskID, if any, without consuming it.
func (m *Manager) SecurityReviewShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.securityVerdictPath(taskID))
}

// qaVerdictPath is where the standalone, advisory test-adequacy (QA) verdict lives — beside
// the review inputs the QA reviewer reads, and DISTINCT from both the trust gate's
// ReviewVerdictFile and the security audit's verdict, so none of the three interfere: the QA
// pass can never mint an approval or satisfy the trusted gate, and recording it never
// disturbs a trust or security verdict.
func (m *Manager) qaVerdictPath(taskID string) string {
	return filepath.Join(m.P.ReviewInputsDir(taskID), "qa-verdict.json")
}

// QAReview folds ONLY the QA reviewer's report for taskID into a commit-pinned verdict and
// persists it as an advisory result — the optional test-adequacy audit. It reuses the same
// inputs (materialized by TrustPrep) and the same internal/review aggregation as the trust
// gate, but it is purely advisory: it never mints an approval, never touches the trust gate's
// verdict or the task's gate state, and never blocks a merge. The manager surfaces its
// findings; delivery is still governed by the human approval (or, in trusted mode, the
// unchanged three-dimension gate, which does not include QA).
//
// Like TrustRecord it is commit-pinned: the sha it covers must still be the worker's HEAD (so
// a commit landing after the QA reviewer ran is rejected rather than silently passed). A
// missing or malformed qa.json folds to a "block" advisory verdict (fail closed) telling the
// manager to actually run the reviewer.
func (m *Manager) QAReview(taskID, sha string, ttl time.Duration) (review.Verdict, error) {
	var zero review.Verdict
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return zero, fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return zero, fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return zero, err
	}
	if sha == "" {
		sha = head
	}
	if sha != head {
		return zero, fmt.Errorf("qa review covers %s but the worker HEAD is now %s; re-run 'ttorch qa-review prep %s' and review again", short(sha), short(head), taskID)
	}
	verdict, err := review.Aggregate(m.P.ReviewInputsDir(taskID), sha, []string{review.DimensionQA})
	if err != nil {
		return zero, err
	}
	if err := review.Write(m.qaVerdictPath(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	// Record a typed, manager-authored, non-actionable 'qa_recorded' event (§3.4). Like the
	// security audit it is a PURE event append — NOT RecordDelivery — because the QA pass is
	// advisory and must never touch the task's gate state (gate_passed/approved_by/
	// reviewed_sha); it only notes that the audit ran and its outcome. Best-effort: the
	// verdict is already persisted, so a failed append must not mask it.
	if _, err := m.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventQARecorded, Actor: db.ActorManager,
		Payload: fmt.Sprintf("verdict=%s sha=%s", verdict.Overall, short(sha)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the qa_recorded event for %s: %v\n", taskID, err)
	}
	m.audit(fmt.Sprintf("qa-review task=%s commit=%s verdict=%s mode=%s",
		taskID, short(sha), verdict.Overall, projectinit.ReadMode(t.Project)))
	return verdict, nil
}

// QAReviewShow returns the current valid (unexpired) advisory QA verdict for taskID, if any,
// without consuming it.
func (m *Manager) QAReviewShow(taskID string) (review.Verdict, bool) {
	return review.Load(m.qaVerdictPath(taskID))
}

// recordDelivered marks a task delivered and appends a typed, manager-authored,
// non-actionable delivery event in one transaction (§3.4): `delivered` for a local
// fast-forward (MergeLocal, standalone or via Land's local/validated/trusted path) and
// `merged` for a PR merge (Land's pr path). It is best-effort by design: the merge has
// already happened and is irreversible, and the events row is only a best-effort mirror
// of the must-succeed audit.log (whose abort-on-failure semantics, §1105, are unchanged),
// so a failed append is surfaced — never allowed to fail a completed merge.
func (m *Manager) recordDelivered(taskID, eventType, payload string) {
	if _, err := m.Store.RecordTransition(context.Background(), taskID, db.StatusDelivered, db.TaskFields{}, eventType, db.ActorManager, payload); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the %s event for %s: %v\n", eventType, taskID, err)
	}
}

// MergeLocal fast-forwards the repo's local default branch to the worker's HEAD —
// the sole sanctioned state-changing write to a real checkout. It always requires a
// valid approval token, the default branch checked out and clean, and a clean
// fast-forward.
//
// The trust gate is layered on top: when requireVerdict is set, or the repo is in
// trusted delivery mode, the merge ADDITIONALLY requires a passing, commit-pinned
// review.Verdict and a fresh, green validate run — and treats no-checks-detected as a
// hard BLOCK (an empty Failures() must never read as a pass). The verdict, like the
// approval, is consumed only immediately before the merge and pinned to the exact
// commit being merged (verdict.ReviewedSHA==workerHead), closing the TOCTOU window
// where a commit lands after review. Every merge is recorded in the audit log.
func (m *Manager) MergeLocal(taskID string, requireVerdict bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	if !approval.Valid(m.P.ApprovalFile(taskID)) {
		return "", fmt.Errorf("no valid approval for %q; the lead must run 'ttorch approve %s' first", taskID, taskID)
	}
	repo := t.Project
	gated := requireVerdict || projectinit.ReadMode(repo) == "trusted"
	// Read the approval token's provenance (human|auto) from the token itself, not from
	// mutable task state, so a crash between minting and saving can never relabel a merge.
	tokData, _ := approval.Data(m.P.ApprovalFile(taskID))
	tokBy, _ := splitApprovalPayload(tokData)
	// Fail closed: an AUTO-minted approval is only valid through the active gate. If the
	// gate is not active here (the repo no longer reads as trusted — e.g. a degraded
	// AGENTS.md silently dropped the mode — and no --require-verdict), an auto token must
	// not merge ungated.
	if !gated && tokBy == "auto" {
		return "", fmt.Errorf("%q carries an auto-approval that is only valid through the trust gate, but the gate is not active (repo not in trusted mode and no --require-verdict); refusing to merge ungated", taskID)
	}
	// The committed object that will fast-forward. Everything the gate validates and pins
	// is THIS sha — never the mutable worktree, which a running worker could change.
	workerHead, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
	}
	def := worktree.DefaultBranch(repo)
	if gated {
		// Defense in depth: the worktree must be clean (a clean signal that no worker is
		// mid-edit), though correctness no longer depends on it — the gate validates the
		// committed sha, not the worktree.
		if clean, err := worktree.IsClean(t.Worktree); err != nil || !clean {
			return "", fmt.Errorf("trust gate: the worktree for %q is not clean; commit or discard all changes before merging", taskID)
		}
		// A trusted AUTO-merge's green authority must be the default-branch gate script —
		// never ecosystem detection on the worker's checkout (go.mod/package.json, which
		// the worker controls). Without it, require a human approval; refuse here before
		// any worker-defined validation runs. (A human-approved gated merge may use the
		// detection fallback.)
		if tokBy == "auto" && !hasDefaultBranchGateScript(repo) {
			return "", fmt.Errorf("trust gate: %q has no .ttorch/validate.sh on the default branch, so a trusted auto-merge's checks would be worker-defined; the lead must approve it explicitly with 'ttorch approve %s'", taskID, taskID)
		}
		// Require a passing, unexpired verdict (load, not yet consume — a recoverable
		// refusal below must leave it intact for a retry). Absent/expired/blocking all
		// fail closed.
		v, ok := review.Load(m.P.ReviewVerdictFile(taskID))
		if !ok {
			return "", fmt.Errorf("trust gate: no valid review verdict for %q; run 'ttorch trust prep %s', review, then 'ttorch trust record %s'", taskID, taskID, taskID)
		}
		if v.Overall != review.Pass {
			return "", fmt.Errorf("trust gate: the review verdict for %q is %q, not pass; resolve the blocking findings and re-record", taskID, v.Overall)
		}
		// Fresh validate of the COMMITTED sha (an immutable detached checkout), using the
		// gate definition from the DEFAULT BRANCH. No checks detected is a hard BLOCK.
		green, results, err := validateCommitted(repo, workerHead)
		if err != nil {
			return "", err
		}
		if !green {
			if len(results) == 0 {
				return "", fmt.Errorf("trust gate: no checks detected for %q; the gate requires a build/test/lint suite on the default branch (add .ttorch/validate.sh)", taskID)
			}
			return "", fmt.Errorf("trust gate: %d of %d checks failed for %q; fix them, re-validate, and re-record the verdict", len(validate.Failures(results)), len(results), taskID)
		}
		// A trusted AUTO-merge may not change the gate's own definition — that requires a
		// human. Checked against the COMMITTED diff, so reverting the bytes in the worktree
		// cannot hide it. (A human-approved gated merge, tokBy=="human", is allowed to.)
		if tokBy == "auto" {
			if touched, name, err := diffTouchesGateConfig(repo, def, workerHead); err != nil {
				return "", err
			} else if touched {
				return "", fmt.Errorf("trust gate: %q changes a gate-definition file (%s); a trusted auto-merge cannot alter its own gate — the lead must approve it explicitly with 'ttorch approve %s'", taskID, name, taskID)
			}
		}
		// HEAD-unchanged bracket: the worker must not have advanced HEAD during the gate,
		// so the sha we validated and pinned is still the sha that merges.
		if cur, err := worktree.Head(t.Worktree); err != nil || cur != workerHead {
			return "", fmt.Errorf("trust gate: the worker for %q advanced during review; re-prep, re-review, and re-record", taskID)
		}
	}
	cur, _ := worktree.CurrentBranch(repo)
	if cur != def {
		return "", fmt.Errorf("repo is on %q, not the default branch %q", cur, def)
	}
	// Only tracked changes block a fast-forward; untracked files (e.g. an `ttorch init`
	// AGENTS.md the developer hasn't committed) are fine, and git's own --ff-only
	// guards the rare untracked-collision case.
	if changed, _ := worktree.HasTrackedChanges(repo); changed {
		return "", fmt.Errorf("repo has uncommitted changes to tracked files; commit or stash before merging")
	}
	defHead, err := worktree.Head(repo)
	if err != nil {
		return "", err
	}
	if !worktree.IsAncestor(repo, defHead, workerHead) {
		return "", fmt.Errorf("worker %q is not a fast-forward of %q; have the worker rebase first", taskID, def)
	}
	// Consume the approval only now — immediately before the state change — so a
	// recoverable refusal above leaves it intact for a retry, and require it to
	// authorize exactly the commit being merged (no changes since the lead reviewed).
	approvedData, ok := approval.Consume(m.P.ApprovalFile(taskID))
	if !ok {
		return "", fmt.Errorf("approval for %q expired before merge; run 'ttorch approve %s' again", taskID, taskID)
	}
	approvedBy, approvedHead := splitApprovalPayload(approvedData)
	if approvedHead != workerHead {
		return "", fmt.Errorf("worker %q changed since approval (approved %s, now %s); re-review with 'ttorch review-diff %s' and approve again", taskID, short(approvedHead), short(workerHead), taskID)
	}
	if !gated {
		if err := worktree.MergeFastForward(repo, workerHead); err != nil {
			return "", err
		}
		m.audit(fmt.Sprintf("merge-local task=%s repo=%s %s -> %s", taskID, repo, def, short(workerHead)))
		m.recordDelivered(taskID, db.EventDelivered, fmt.Sprintf("%s -> %s", def, short(workerHead)))
		return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
	}
	// Consume the verdict beside the approval and pin it to the merged commit — the
	// second commit-pin, parallel to approvedHead==workerHead, so a commit pushed after
	// review can never ride in unreviewed. Consume re-checks pass, so a verdict that
	// turned blocking since the load above fails closed here too.
	cv, ok := review.Consume(m.P.ReviewVerdictFile(taskID))
	if !ok {
		return "", fmt.Errorf("trust gate: the review verdict for %q expired or is no longer passing; re-record it with 'ttorch trust record %s'", taskID, taskID)
	}
	if cv.ReviewedSHA != workerHead {
		return "", fmt.Errorf("trust gate: the verdict for %q covers %s but the worker is now %s; re-review and re-record", taskID, short(cv.ReviewedSHA), short(workerHead))
	}
	// Attribute the audit to the consumed token's provenance, and fail closed if it is
	// unknown (a legacy token with no provenance must not merge through the gate).
	var approver string
	switch approvedBy {
	case "auto", "human":
		approver = approvedBy
	default:
		return "", fmt.Errorf("trust gate: the approval for %q has no recorded provenance; re-approve with 'ttorch approve %s'", taskID, taskID)
	}
	// A trusted merge MUST be auditable (every trusted merge must be reconstructable):
	// write + flush the record BEFORE the fast-forward and abort if it cannot be
	// persisted — for finance, an unrecorded merge is not acceptable.
	auditLine := fmt.Sprintf("merge-local task=%s repo=%s %s -> %s gate=verdict approver=%s", taskID, repo, def, short(workerHead), approver)
	if err := m.writeAudit(auditLine); err != nil {
		return "", fmt.Errorf("trust gate: cannot record the merge for %q in the audit log (%v); refusing to merge unaudited", taskID, err)
	}
	if err := worktree.MergeFastForward(repo, workerHead); err != nil {
		return "", err
	}
	m.recordDelivered(taskID, db.EventDelivered, fmt.Sprintf("%s -> %s gate=verdict approver=%s", def, short(workerHead), approver))
	return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
}

// gateConfigFiles define the trust gate itself: the validation script and the repo's
// delivery-mode/gate config. A trusted AUTO-merge must never change them — altering the
// gate requires an explicit human approval.
var gateConfigFiles = []string{".ttorch/validate.sh", "AGENTS.md"}

// gateValidate runs the trust gate's fresh validation against the worker's worktree, but
// using the validation DEFINITION resolved from the repo's DEFAULT BRANCH: the
// .ttorch/validate.sh as it exists on the default branch (run from a temp copy so a
// worker cannot weaken its own gate by editing the script on its branch), or, when the
// default branch defines none, the built-in ecosystem steps (fixed ttorch commands the
// worker cannot redefine). It returns whether the worktree is green, the results for
// reporting, and any error. No detected checks => NOT green (a hard block): an empty
// Failures() must never be read as a pass.
func gateValidate(worktreeDir, repo string) (bool, []validate.Result, error) {
	def := worktree.DefaultBranch(repo)
	var steps []validate.Step
	if script, ok := worktree.ShowFile(repo, def, ".ttorch/validate.sh"); ok {
		tmp, err := os.CreateTemp("", "ttorch-gate-validate-*.sh")
		if err != nil {
			return false, nil, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(script); err != nil {
			tmp.Close()
			return false, nil, err
		}
		if err := tmp.Close(); err != nil {
			return false, nil, err
		}
		steps = []validate.Step{{Name: "gate", Cmd: []string{"sh", tmp.Name()}}}
	} else {
		steps = validate.DetectDefaults(worktreeDir)
	}
	if len(steps) == 0 {
		return false, nil, nil
	}
	results := validate.Run(worktreeDir, steps)
	return gateGreen(steps, results), results, nil
}

// gateGreen reports whether every detected step produced a passing result. A step that
// failed even to start is reported by validate.Run as a non-passing result, so it is
// treated as a failure (block), never conflated with a pass; a missing result for any
// step is likewise not green.
func gateGreen(steps []validate.Step, results []validate.Result) bool {
	return len(steps) > 0 && len(results) == len(steps) && len(validate.Failures(results)) == 0
}

// validateCommitted runs the trust gate's validation against an IMMUTABLE detached
// checkout of the committed sha — never the live worktree, which a running worker could
// mutate mid-run (race good bytes in, validate, revert) — using the validation
// definition from the default branch. This makes the thing validated exactly the thing
// that fast-forwards.
func validateCommitted(repo, sha string) (bool, []validate.Result, error) {
	parent, err := os.MkdirTemp("", "ttorch-gate-*")
	if err != nil {
		return false, nil, err
	}
	defer os.RemoveAll(parent)
	// A unique basename (the temp dir's own name) avoids linked-worktree id collisions
	// when gates run concurrently.
	co := filepath.Join(parent, filepath.Base(parent))
	if err := worktree.AddDetached(repo, co, sha); err != nil {
		return false, nil, err
	}
	defer worktree.RemoveWorktree(repo, co)
	return gateValidate(co, repo)
}

// hasDefaultBranchGateScript reports whether the repo's default branch defines the gate
// script (.ttorch/validate.sh). The trusted AUTO path requires it: without it,
// gateValidate falls back to ecosystem detection (go.mod / package.json) on the worker's
// own checkout, whose presence and scripts the worker controls — so "green" would be
// worker-influenced. A human `ttorch approve` is still allowed to use the detection
// fallback (a human is then in the loop).
func hasDefaultBranchGateScript(repo string) bool {
	_, ok := worktree.ShowFile(repo, worktree.DefaultBranch(repo), ".ttorch/validate.sh")
	return ok
}

// diffTouchesGateConfig reports whether the COMMITTED diff base..rev modifies any
// gate-definition file (and which one), so a trusted auto-merge of such a change can be
// refused in favor of an explicit human approval. It reads committed objects, not the
// working tree, so the check cannot be evaded by reverting the bytes in the worktree.
func diffTouchesGateConfig(repo, base, rev string) (bool, string, error) {
	names, err := worktree.ChangedFiles(repo, base, rev)
	if err != nil {
		return false, "", err
	}
	for _, n := range names {
		for _, g := range gateConfigFiles {
			if n == g {
				return true, n, nil
			}
		}
	}
	return false, "", nil
}

// approvalPayload packs the grant provenance ("human"|"auto") with the reviewed sha into
// the approval token's opaque data, so a merge attributes the audit from the token it
// actually consumes rather than from mutable task state (which a crash could desync).
func approvalPayload(by, sha string) string { return by + " " + sha }

// splitApprovalPayload unpacks approvalPayload. A token with no provenance prefix
// (legacy/plain sha) yields by=="" so the gated path can fail closed on it.
func splitApprovalPayload(data string) (by, sha string) {
	if i := strings.IndexByte(data, ' '); i >= 0 {
		return data[:i], data[i+1:]
	}
	return "", data
}

// --- one-command land (safe atomic delivery) ---

// landIntegrate performs the mode-appropriate integration step of Land and returns the
// commit now on the local default branch's tip. It is a package-level seam so a test can
// substitute a faulty integrator and exercise Land's post-merge verification abort path
// (a clean local fast-forward can never land a tree different from the validated commit,
// so the only way to drive the mismatch alarm in-process is to inject one).
var landIntegrate = func(m *Manager, t db.Task, mode string, requireVerdict bool, rebasedHead string) (string, error) {
	return m.integrate(t, mode, requireVerdict, rebasedHead)
}

// Land turns the manual push/PR/merge/fetch/ff/verify dance into one safe, atomic command
// for taskID, closing the near-misses of doing it by hand:
//
//  1. fetch origin so the rebase targets the current default-branch tip, not a stale local
//     copy (the "forgot-fetch stale sync" near-miss);
//  2. rebase the worker's committed work onto that tip — on conflict it ABORTS the rebase
//     and reports the real overlap rather than blind-merging a far-behind branch whose diff
//     reads as a huge phantom deletion;
//  3. re-run the validation gate on the REBASED tree (the immutable committed sha, gate
//     definition from the default branch) and require it green — no checks detected is a
//     hard block, exactly like the merge gate;
//  4. integrate honoring the repo's delivery mode and the EXISTING merge gates — pr mode
//     pushes + opens + merges a PR (GitHub's review/branch-protection is the gate); every
//     other mode does an approval-gated local fast-forward via MergeLocal, whose approval
//     and (trusted / --require-verdict) verdict checks are never bypassed;
//  5. POST-MERGE VERIFY that the worker's reviewed changes landed intact — byte-identity to
//     the validated commit for the pinned local fast-forward, or every worker-touched file
//     landing verbatim for a PR merge (where the base may legitimately have advanced) —
//     else abort and alarm;
//  6. leave the local default branch fast-forwarded to the landed commit.
//
// The rebase preserves the gate's bright line that "what merges is what was reviewed": if
// the rebase moves the worker onto an advanced default, the lead's approval of the pre-rebase
// commit no longer covers what would merge, so Land refuses (consuming nothing) and asks for
// a fresh approval of the rebased commit rather than carrying a stale one forward. The common
// case — the worker already current with the default — rebases to a no-op and lands in one
// command. Every failure is loud; Land never silently no-ops.
func (m *Manager) Land(taskID string, requireVerdict bool) (string, error) {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	repo, wt := t.Project, t.Worktree
	if repo == "" || wt == "" {
		return "", fmt.Errorf("land: task %q has no repo/worktree to land", taskID)
	}
	// Land must resolve the delivery mode authoritatively before routing the integration:
	// refuse a repo with no recorded mode rather than silently defaulting to pr (a degraded
	// or absent AGENTS.md would otherwise reroute a local/trusted repo onto the ungated PR
	// path, sidestepping the merge gate). projectinit.Initialized is the authority that
	// `ttorch init` writes.
	if !projectinit.Initialized(repo) {
		return "", fmt.Errorf("land: repo %s has no ttorch delivery mode configured; run 'ttorch init --mode <pr|local|validated|trusted>' first", repo)
	}
	mode := projectinit.ReadMode(repo)
	def := worktree.DefaultBranch(repo)
	gated := requireVerdict || mode == "trusted"

	// --require-verdict layers the adversarial-review verdict onto a LOCAL merge gate; pr
	// mode has no local merge to gate (GitHub review/branch-protection is its gate), so
	// honor the flag loudly rather than silently dropping it.
	if mode == "pr" && requireVerdict {
		return "", fmt.Errorf("land: --require-verdict applies to local/validated/trusted modes; repo %s is in pr mode, where GitHub review/branch-protection is the gate", repo)
	}

	// The rebased + validated commit must be exactly what merges: refuse a dirty worktree
	// up front (a worker mid-edit), the same contract trust prep enforces.
	if clean, err := worktree.IsClean(wt); err != nil || !clean {
		return "", fmt.Errorf("land: the worktree for %q is not clean; commit or discard changes first so the rebased, validated commit is exactly what lands", taskID)
	}

	// (1) Fetch origin. pr mode REQUIRES an origin to push to; other modes can land a
	// purely local repo (no remote), so a missing origin there is fine.
	hasOrigin := worktree.RemoteExists(repo, "origin")
	if mode == "pr" && !hasOrigin {
		return "", fmt.Errorf("land: repo %s is in pr delivery mode but has no 'origin' remote to push to", repo)
	}
	if hasOrigin {
		if err := worktree.Fetch(repo); err != nil {
			return "", fmt.Errorf("land: 'git fetch origin' failed for %s: %w; refusing to land against a stale origin", repo, err)
		}
	}

	// (2) Rebase the worker onto the current default tip — origin/<def> when a remote
	// default exists (the authoritative tip), else the local <def>.
	base := def
	if hasOrigin && worktree.RefExists(repo, "origin/"+def) {
		base = "origin/" + def
	}
	baseSha, err := worktree.ResolveRef(repo, base)
	if err != nil {
		return "", fmt.Errorf("land: could not resolve the rebase base %s: %w", base, err)
	}
	preRebase, err := worktree.Head(wt)
	if err != nil {
		return "", err
	}
	if err := worktree.Rebase(wt, base); err != nil {
		if abErr := worktree.RebaseAbort(wt); abErr != nil {
			return "", fmt.Errorf("land: rebasing %q onto %s hit conflicts AND the abort failed (%v); the worktree %s is left mid-rebase — run 'git -C %s rebase --abort' by hand, resolve the overlap in the worker, then re-run land: %w", taskID, base, abErr, wt, wt, err)
		}
		return "", fmt.Errorf("land: rebasing %q onto %s hit conflicts (real overlap with changes already on %s); aborted the rebase and restored the worktree — resolve the overlap in the worker, then re-run land: %w", taskID, base, def, err)
	}
	rebasedHead, err := worktree.Head(wt)
	if err != nil {
		return "", err
	}

	// (3) Validate the REBASED tree. Must be green; no checks detected is a hard block.
	green, results, err := validateCommitted(repo, rebasedHead)
	if err != nil {
		return "", fmt.Errorf("land: could not validate the rebased tree for %q: %w", taskID, err)
	}
	if !green {
		if len(results) == 0 {
			return "", fmt.Errorf("land: no checks detected for %q after rebasing onto %s; the gate requires a build/test/lint suite (add .ttorch/validate.sh on the default branch)", taskID, base)
		}
		return "", fmt.Errorf("land: %d of %d checks failed on the rebased tree for %q; fix them in the worker and re-run land", len(validate.Failures(results)), len(results), taskID)
	}

	// The approval (and, when gated, the verdict) authorize a SPECIFIC commit. If the rebase
	// moved the worker onto an advanced default, the prior approval of the pre-rebase commit
	// no longer covers what would merge — so require a fresh approval of the rebased commit
	// rather than carry a stale one forward. This pre-check consumes nothing (MergeLocal
	// remains the consuming authority), so the lead reviews the rebased diff, re-approves,
	// and re-runs land; a no-op rebase keeps the existing approval valid and lands at once.
	if mode != "pr" {
		// Verdict-portable fast-land: when the rebase moved the worker onto an advanced default
		// but its own three-dot diff is byte-identical to what the reviewers cleared, carry the
		// passing verdict forward to the rebased commit (re-pinning it, and the auto-minted
		// approval, to rebasedHead) instead of forcing a full re-gate. A rebase that changed the
		// reviewed content is never carried — carryVerdictForward returns a loud re-gate error.
		if gated && rebasedHead != preRebase {
			if _, err := m.carryVerdictForward(t, base, rebasedHead); err != nil {
				return "", err
			}
		}
		if err := m.gateCoversRebased(t, rebasedHead, gated); err != nil {
			return "", err
		}
	}

	// (4) Integrate, honoring the delivery mode and the existing merge gates.
	landed, err := landIntegrate(m, t, mode, requireVerdict, rebasedHead)
	if err != nil {
		return "", err
	}

	// (5) POST-MERGE VERIFY that the worker's reviewed changes landed intact. The local
	// fast-forward is byte-identical to the validated commit (strict); a PR merge may sit on
	// a base that legitimately advanced, so there we require only the worker's own files to
	// have landed verbatim.
	defAfter, err := verifyLanded(repo, def, baseSha, rebasedHead, mode != "pr")
	if err != nil {
		return "", fmt.Errorf("land: %q: %w", taskID, err)
	}

	// (6) The local default branch is now fast-forwarded to the landed commit.
	rebaseNote := "worker already current"
	if rebasedHead != preRebase {
		rebaseNote = fmt.Sprintf("rebased %s→%s onto %s", short(preRebase), short(rebasedHead), base)
	}
	m.audit(fmt.Sprintf("land task=%s repo=%s mode=%s %s -> %s verified", taskID, repo, mode, def, short(landed)))
	out := fmt.Sprintf("landed %s (%s mode): %s; %s fast-forwarded to %s and verified",
		taskID, mode, rebaseNote, def, short(defAfter))
	// Surface the security-everywhere audit status. This is purely ADVISORY and never
	// blocks: a gated land (trusted / --require-verdict) already ran the full review gate
	// — which includes security — so it needs no extra note; the other modes get a
	// non-blocking reminder of whether a fresh security audit covers the landed commit.
	if note := m.securityAuditNote(taskID, rebasedHead, gated); note != "" {
		out += "\n  " + note
	}
	return out, nil
}

// securityAuditNote returns a one-line, ADVISORY note on the security-everywhere audit
// for the commit being landed, or "" when none is warranted. It never blocks delivery:
// gated lands (trusted / --require-verdict) already cleared the full review gate, so they
// get no note; in every other mode it reports whether a fresh advisory security audit
// covers landedSHA, nudging the manager to run one when it does not.
func (m *Manager) securityAuditNote(taskID, landedSHA string, gated bool) string {
	if gated {
		return ""
	}
	v, ok := m.SecurityReviewShow(taskID)
	if !ok || v.ReviewedSHA != landedSHA {
		return fmt.Sprintf("advisory: no security audit covers %s — run 'ttorch security-review prep %s', review, then 'ttorch security-review record %s' (advisory, does not block delivery)",
			short(landedSHA), taskID, taskID)
	}
	if v.Overall != review.Pass {
		return fmt.Sprintf("advisory: security audit raised blocking findings for %s — review 'ttorch security-review show %s' (advisory, did not block this delivery)", short(landedSHA), taskID)
	}
	return fmt.Sprintf("advisory: security audit passed for %s", short(landedSHA))
}

// carryVerdictForward implements verdict-portable fast-land. When a gated land has rebased the
// worker onto an advanced default — moving the commit SHA the verdict was pinned to — it
// carries the existing PASSING verdict forward to the rebased commit, re-pinning the verdict
// (and, when the gate auto-minted it, the approval token) to rebasedHead WITHOUT re-running
// trust prep or the three reviewers — but ONLY when the worker's own three-dot diff against
// the rebase base is byte-identical to the diff the reviewers cleared, matched by the verdict's
// recorded content identity (review.DiffID). This is the throughput fix that stops re-gating a
// task every time an unrelated merge advances the default beneath it.
//
// A content change — anything but a clean, disjoint rebase — is NEVER carried: re-pinning a
// verdict onto changed content would be a trust hole, so it returns a loud re-gate error
// instead. When there is no passing verdict to carry, or one predating content identities (an
// empty DiffID), it carries nothing and returns (false, nil), letting gateCoversRebased issue
// the usual re-gate demand. The verdict expiry, findings, and content identity are preserved so
// a subsequent rebase can carry it again; the merge gate in MergeLocal re-validates and
// re-consumes the re-pinned tokens, so carry-forward is an optimization, never the authority.
func (m *Manager) carryVerdictForward(t db.Task, base, rebasedHead string) (bool, error) {
	v, ok := review.Load(m.P.ReviewVerdictFile(t.ID))
	if !ok || v.Overall != review.Pass {
		return false, nil // no carryable verdict — gateCoversRebased demands a fresh one
	}
	if v.ReviewedSHA == rebasedHead {
		return false, nil // already covers the rebased commit (e.g. a no-op re-run)
	}
	if v.DiffID == "" {
		return false, nil // a verdict recorded before content identities — fail safe to re-gate
	}
	patch, err := mergeBaseDiff(t.Worktree, base, rebasedHead)
	if err != nil {
		return false, fmt.Errorf("land: could not compute the rebased diff for %q to carry its verdict forward: %w", t.ID, err)
	}
	if review.DiffID([]byte(patch)) != v.DiffID {
		// The rebase onto the advanced default was not clean/disjoint: it changed the worker's
		// own diff, so the recorded verdict no longer covers what would merge. Never carry a
		// verdict onto changed content — force a full re-gate.
		return false, fmt.Errorf("land: rebasing %q onto the advanced %s changed its reviewed diff, so the recorded verdict no longer covers the rebased commit %s; re-run 'ttorch trust prep %s', review, and 'ttorch trust record %s', then re-run 'ttorch land %s'",
			t.ID, base, short(rebasedHead), t.ID, t.ID, t.ID)
	}
	// The worker's own diff is byte-identical to what the reviewers cleared: carry the verdict
	// forward by re-pinning it to the rebased commit, preserving its expiry, findings, and
	// content identity.
	reviewedSHA := v.ReviewedSHA
	ttl := time.Until(time.Unix(0, v.Expires))
	if ttl <= 0 {
		return false, nil // expired between Load and here — gateCoversRebased fails closed
	}
	v.ReviewedSHA = rebasedHead
	if err := review.Write(m.P.ReviewVerdictFile(t.ID), v, ttl); err != nil {
		return false, fmt.Errorf("land: could not re-pin the carried verdict for %q: %w", t.ID, err)
	}
	// The trusted gate auto-mints the approval token from the same review, pinned to the same
	// commit; re-pin it forward too so the merge proceeds without a human. A human-minted
	// approval is NEVER forged forward — the lead re-approves the rebased commit (a single
	// step), but the three reviewers do not re-run.
	if data, ok := approval.Data(m.P.ApprovalFile(t.ID)); ok {
		if by, _ := splitApprovalPayload(data); by == "auto" {
			if err := approval.Grant(m.P.ApprovalFile(t.ID), ttl, approvalPayload("auto", rebasedHead)); err != nil {
				return false, fmt.Errorf("land: could not re-pin the carried auto-approval for %q: %w", t.ID, err)
			}
		}
	}
	m.audit(fmt.Sprintf("fast-land task=%s carried verdict %s->%s base=%s (reviewed diff unchanged)",
		t.ID, short(reviewedSHA), short(rebasedHead), base))
	return true, nil
}

// gateCoversRebased verifies the existing approval (and, when gated, a passing review
// verdict) is pinned to the rebased commit that will actually merge — without consuming
// either (MergeLocal remains the consuming authority). It lets Land tell the lead clearly to
// approve the rebased commit when its own rebase moved the worker onto an advanced default,
// instead of MergeLocal later consuming a now-stale token and reporting a confusing generic
// mismatch.
func (m *Manager) gateCoversRebased(t db.Task, rebasedHead string, gated bool) error {
	data, ok := approval.Data(m.P.ApprovalFile(t.ID))
	_, approvedSha := splitApprovalPayload(data)
	if !ok || approvedSha != rebasedHead {
		return fmt.Errorf("land: no valid approval covers the rebased commit %s for %q (land rebased the worker onto the current default, so the prior approval no longer matches); review it with 'ttorch review-diff %s' and approve with 'ttorch approve %s', then re-run 'ttorch land %s'",
			short(rebasedHead), t.ID, t.ID, t.ID, t.ID)
	}
	if gated {
		v, vok := review.Load(m.P.ReviewVerdictFile(t.ID))
		if !vok || v.Overall != review.Pass || v.ReviewedSHA != rebasedHead {
			return fmt.Errorf("land: no passing review verdict covers the rebased commit %s for %q; re-run 'ttorch trust prep %s', review, and 'ttorch trust record %s', then re-run 'ttorch land %s'",
				short(rebasedHead), t.ID, t.ID, t.ID, t.ID)
		}
	}
	return nil
}

// integrate performs Land's mode-appropriate merge and returns the local default tip.
func (m *Manager) integrate(t db.Task, mode string, requireVerdict bool, rebasedHead string) (string, error) {
	repo := t.Project
	def := worktree.DefaultBranch(repo)
	if mode == "pr" {
		return m.integratePR(t, def, rebasedHead)
	}
	// local / validated / trusted: an approval-gated local fast-forward. MergeLocal
	// enforces the approval token and (in trusted mode or with --require-verdict) the
	// adversarial-review verdict + a fresh green validate; Land never bypasses those.
	if _, err := m.MergeLocal(t.ID, requireVerdict); err != nil {
		return "", fmt.Errorf("land: local merge gate refused %q: %w", t.ID, err)
	}
	return worktree.Head(repo)
}

// integratePR delivers via GitHub: it publishes EXACTLY the validated commit as a branch,
// opens (or reuses) a PR, merges it, then fast-forwards the local default branch to the
// merged tip. GitHub's required reviews / branch protection / status checks are the gate
// here, and a merge they block fails loudly.
func (m *Manager) integratePR(t db.Task, def, rebasedHead string) (string, error) {
	repo, wt := t.Project, t.Worktree
	// HEAD-unchanged bracket (mirrors MergeLocal): the worker must not have advanced past
	// the validated commit between validation and publish, so an unvalidated commit can
	// never reach the remote.
	if cur, err := worktree.Head(wt); err != nil || cur != rebasedHead {
		return "", fmt.Errorf("land: the worker for %q advanced past the validated commit %s before publish; re-run land", t.ID, short(rebasedHead))
	}
	branch := "ttorch/" + t.ID
	if err := worktree.Push(repo, "origin", rebasedHead+":refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("land: pushing %q to origin/%s failed: %w", t.ID, branch, err)
	}
	if err := ghEnsurePR(repo, branch, def, t.ID); err != nil {
		return "", err
	}
	if _, err := gh(repo, "pr", "merge", branch, "--merge", "--delete-branch"); err != nil {
		return "", fmt.Errorf("land: merging the PR for %q failed (required reviews / checks / branch protection?): %w", t.ID, err)
	}
	// Bring the local default branch to the merged tip.
	if err := worktree.Fetch(repo); err != nil {
		return "", fmt.Errorf("land: fetch after the PR merge of %q failed: %w", t.ID, err)
	}
	if cur, _ := worktree.CurrentBranch(repo); cur != def {
		return "", fmt.Errorf("land: repo is on %q, not the default branch %q; cannot fast-forward the local default after the PR merge", cur, def)
	}
	if changed, _ := worktree.HasTrackedChanges(repo); changed {
		return "", fmt.Errorf("land: repo has uncommitted tracked changes; cannot fast-forward the local default after the PR merge")
	}
	if err := worktree.MergeFastForward(repo, "origin/"+def); err != nil {
		return "", fmt.Errorf("land: fast-forwarding local %s to origin/%s after the PR merge failed: %w", def, def, err)
	}
	// The PR merged and the local default fast-forwarded: record the delivery as a
	// typed, non-actionable `merged` event (the PR-path counterpart of MergeLocal's
	// `delivered`, §3.4). Best-effort — the merge is already irreversible.
	m.recordDelivered(t.ID, db.EventMerged, fmt.Sprintf("pr merged: %s -> %s", branch, def))
	return worktree.Head(repo)
}

// verifyLanded asserts the worker's reviewed changes landed intact on the default branch and
// returns the new tip. When strict (a local fast-forward, where the base is pinned) it
// requires the tip to be byte-identical to the validated rebasedHead, catching any file
// changed or reverted outside the reviewed diff. When not strict (a PR merge, where the base
// may legitimately have advanced under a concurrent landing) it instead requires every file
// the worker changed (baseSha..rebasedHead) to be identical between rebasedHead and the
// landed tip — the worker's contribution landed verbatim, while concurrent changes to OTHER
// files are allowed. Either failure is a loud, file-naming alarm (a post-merge tripwire; it
// cannot un-merge, only refuse to bless).
func verifyLanded(repo, def, baseSha, rebasedHead string, strict bool) (string, error) {
	defAfter, err := worktree.ResolveRef(repo, def)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not resolve %s: %w", def, err)
	}
	if strict {
		drift, err := worktree.ChangedFiles(repo, defAfter, rebasedHead)
		if err != nil {
			return "", fmt.Errorf("post-merge verify could not diff the landed tip: %w", err)
		}
		if len(drift) > 0 {
			return "", fmt.Errorf("POST-MERGE VERIFY FAILED: %s now at %s is NOT identical to the validated commit %s; these files differ: %s — the integration changed or reverted files outside the reviewed diff; investigate before trusting this landing",
				def, short(defAfter), short(rebasedHead), strings.Join(drift, ", "))
		}
		return defAfter, nil
	}
	// PR path: every file the worker changed must have landed verbatim. Files that differ
	// between the validated commit and the landed tip but were NOT touched by the worker are
	// concurrent landings on the base and are allowed.
	workerFiles, err := worktree.ChangedFiles(repo, baseSha, rebasedHead)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not list the worker's changed files: %w", err)
	}
	landedDrift, err := worktree.ChangedFiles(repo, rebasedHead, defAfter)
	if err != nil {
		return "", fmt.Errorf("post-merge verify could not diff the landed tip: %w", err)
	}
	worker := make(map[string]bool, len(workerFiles))
	for _, f := range workerFiles {
		worker[f] = true
	}
	var bad []string
	for _, f := range landedDrift {
		if worker[f] {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("POST-MERGE VERIFY FAILED: the worker's reviewed changes did not land verbatim on %s (now %s); these worker-touched files differ from the validated commit %s: %s — the merge altered or dropped the worker's contribution; investigate before trusting this landing",
			def, short(defAfter), short(rebasedHead), strings.Join(bad, ", "))
	}
	return defAfter, nil
}

// gh runs a gh CLI command in repo, returning its trimmed combined output.
func gh(repo string, args ...string) (string, error) {
	c := exec.Command("gh", args...)
	c.Dir = repo
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("gh %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// ghEnsurePR opens a PR for branch into base, or succeeds if one is already open for it.
func ghEnsurePR(repo, branch, base, taskID string) error {
	if _, err := gh(repo, "pr", "view", branch, "--json", "number"); err == nil {
		return nil // a PR is already open for this branch
	}
	if _, err := gh(repo, "pr", "create", "--head", branch, "--base", base,
		"--title", "ttorch: land "+taskID,
		"--body", "Automated landing of ttorch task "+taskID+"."); err != nil {
		return fmt.Errorf("land: opening a PR for %q (%s -> %s) failed: %w", taskID, branch, base, err)
	}
	return nil
}

// Promote turns a scout task into a ship task (restoring teardown protection).
func (m *Manager) Promote(taskID string) error {
	t, ok, err := m.Store.GetTask(context.Background(), taskID)
	if err != nil || !ok {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if t.Kind != "scout" {
		return fmt.Errorf("task %q is not a scout task", taskID)
	}
	// Flip the kind to ship and record the manager-authored, non-actionable 'promoted'
	// event in one transaction (§3.4). The CHECK constraint validates the new kind.
	ship := db.KindShip
	_, err = m.Store.RecordTransition(context.Background(), taskID, "", db.TaskFields{Kind: &ship}, db.EventPromoted, db.ActorManager, "scout -> ship")
	return err
}

// ArmPRCheck records a PR URL on a task so the watcher polls for its merge (§4.4).
func (m *Manager) ArmPRCheck(taskID, url string) error {
	if _, ok, err := m.Store.GetTask(context.Background(), taskID); err != nil || !ok {
		return fmt.Errorf("unknown task %q", taskID)
	}
	// Record the PR url and the manager-authored, non-actionable 'pr_armed' event in one
	// transaction (§3.4).
	_, err := m.Store.RecordTransition(context.Background(), taskID, "", db.TaskFields{PR: &url}, db.EventPRArmed, db.ActorManager, url)
	return err
}

// FleetSync refreshes a repo's local default branch from origin when safe and
// prunes local branches whose upstream is gone.
func (m *Manager) FleetSync(repoPath string) ([]string, error) {
	repo, err := worktree.RepoRoot(repoPath)
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", repoPath)
	}
	var notes []string
	if err := worktree.Fetch(repo); err != nil {
		notes = append(notes, "fetch skipped (offline?)")
	}
	if gone, err := worktree.GoneBranches(repo); err == nil {
		cur, _ := worktree.CurrentBranch(repo)
		for _, b := range gone {
			if b == cur {
				continue
			}
			if err := worktree.DeleteBranch(repo, b); err == nil {
				notes = append(notes, "pruned branch "+b)
			}
		}
	}
	def := worktree.DefaultBranch(repo)
	if cur, _ := worktree.CurrentBranch(repo); cur == def {
		if dirty, _ := worktree.IsDirty(repo); !dirty {
			if err := worktree.MergeFastForward(repo, "origin/"+def); err == nil {
				notes = append(notes, def+" fast-forwarded to origin/"+def)
			}
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "already up to date")
	}
	return notes, nil
}

// Recovery reconciles tracked tasks against live tmux windows and reports drift.
func (m *Manager) Recovery() ([]string, error) {
	var notes []string
	windows, _ := tmux.ListWindows(m.Session)
	winSet := map[string]bool{}
	for _, w := range windows {
		winSet[w] = true
	}
	tasks := m.liveTasks()
	hasMeta := map[string]bool{}
	for _, t := range tasks {
		hasMeta[t.Window] = true
		if t.Kind != "cc" && !winSet[t.Window] {
			notes = append(notes, "dead worker (window gone): "+t.ID)
		}
	}
	for _, w := range windows {
		if strings.HasPrefix(w, "wk-") && !hasMeta[w] {
			notes = append(notes, "orphan window (no task record): "+w)
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "reconciled; nothing to recover")
	}
	return notes, nil
}

func (m *Manager) audit(line string) { _ = m.writeAudit(line) }

// writeAudit appends a timestamped record to the audit log and flushes it to disk,
// returning any error. Trusted merges call it directly and ABORT on failure — an
// unrecorded finance merge is not acceptable (every trusted merge must be
// reconstructable). Other call sites use audit() best-effort.
func (m *Manager) writeAudit(line string) error {
	if err := os.MkdirAll(m.P.Home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.P.AuditLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func cwd() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}

// briefForLaunch returns the path to the brief the worker launch command reads,
// writing the generic stub there first ONLY when no brief already exists. A brief the
// manager supplied via `spawn --brief/--brief-file` (written by WriteBrief before the
// spawn) is therefore used as the worker's initial prompt instead of the stub, and a
// resume/respawn keeps the original brief rather than clobbering it.
func (m *Manager) briefForLaunch(taskID, kind string) string {
	path := m.P.BriefPath(taskID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = writeBriefStub(path, taskID, kind)
	}
	return path
}

// WriteBrief stores content as task taskID's brief (paths.BriefPath) so the next Spawn
// launches the worker with it as the initial prompt rather than the generic stub. It
// creates the data dir and overwrites any existing brief for the id. `ttorch spawn
// --brief/--brief-file` calls it before spawning.
func (m *Manager) WriteBrief(taskID, content string) error {
	return writeBrief(m.P.BriefPath(taskID), content)
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

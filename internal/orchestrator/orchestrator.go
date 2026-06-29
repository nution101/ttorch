// Package orchestrator composes the runtime primitives (tmux, worktree, harness,
// state) into the operations the manager drives: spawn, status, peek, send,
// teardown, plus launching the manager session and ad-hoc cc sessions.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/livestate"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/profile"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/termtab"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/worktree"
)

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
// out so callers see exactly today's live fleet. 'failed' is terminal too (a task that
// exhausted its lease retries, §roadmap 2), so it is filtered out alongside the others —
// otherwise a poison-pilled task would keep occupying a fleet slot and holding its
// worktree in inUseWorktrees.
func (m *Manager) liveTasks() []db.Task {
	tasks, _ := m.Store.ListTasks(context.Background(), db.TaskFilter{})
	var out []db.Task
	for _, t := range tasks {
		if t.Status == db.StatusTornDown || t.Status == db.StatusAbandoned || t.Status == db.StatusFailed {
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
	// Drop any durable trust verdict for this task. It is consume-once gate state, not audit
	// history (the review_recorded event and audit.log preserve that a review happened), and
	// the task is being discarded — so the row must not outlive it. This restores the
	// self-cleanup the old TTL'd verdict file had: a torn-down id that is later re-spawned can
	// never inherit a stale verdict row pinned to the prior task's commit. Best-effort, like
	// the worktree release above.
	if err := m.Store.DeleteVerdict(context.Background(), taskID); err != nil {
		notes = append(notes, "verdict: "+err.Error())
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
	// Auto-start the deterministic scheduler daemon (config-gated, default-on, singleton) so a
	// normal `ttorch` session drives the board autonomously — dispatch + land + supervise — while
	// the LLM manager plans, gates, and answers decisions. Run for every start path (re-attach,
	// restore, fresh) so a missing daemon is (re)started; the singleton makes a redundant launch a
	// no-op. Best-effort and non-blocking: it never fails or delays the attach below.
	m.autoStartScheduler()
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
		_ = tmux.SendLine(m.Session, t.Window, harness.WorkerResumeOrFresh(h, t.SessionID, m.P.BriefPath(t.ID), t.Effort))
		_ = termtab.Open(m.Session, t.Window)
		// Refresh the supervisor's sign-of-life anchor for the worker just rebuilt in place. A
		// resume re-drives the window but, on its own, appends no event — so a window_gone
		// recorded before the resume would remain the worker's LATEST signal and the supervisor
		// (db.ReclaimWindowGone / hasFreshWindowGoneTx) could reclaim a worker that is in fact
		// alive again. Appending the SAME 'spawned' sign-of-life event the spawn path emits puts
		// a newer-than-window_gone anchor on the task, so the reclaim correctly skips the live
		// resumed worker. It does NOT mask a genuinely dead worker: one that dies AFTER the
		// resume gets a later window_gone, which again becomes the latest anchor. Like the spawn
		// path's mirror, this is a best-effort audit append — the window is already rebuilt and
		// the worker resuming — so a failure is surfaced and carried, never aborting the restore.
		if _, err := m.Store.AppendEvent(context.Background(), db.Event{
			EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventSpawned, Actor: db.ActorManager,
			Payload: fmt.Sprintf("resume kind=%s window=%s", t.Kind, t.Window),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ttorch: could not record the resume sign-of-life event for %s: %v\n", t.ID, err)
		}
		// Refresh the supervisor's OTHER recovery anchor: the lease. The sign-of-life event above
		// only outranks a stale window_gone (db.ReclaimWindowGone); a worker resumed in place after
		// a long pause can ALSO hold a lease that lapsed during the downtime, which the lease-expiry
		// sweep (db.ReclaimExpiredLeases) would read as ground-truth death and reclaim — the same
		// spurious, bounded restart of a now-live worker. Pushing the held lease forward to
		// now+DefaultLeaseDuration restores the worker's lease anchor so the sweep skips it.
		// ExtendLease is a no-op when no lease is held, so it is safe for any restored worker, and
		// it does NOT mask a genuine crash: a worker that dies after the resume stops heartbeating,
		// its refreshed lease lapses again, and the sweep reclaims it as before. Best-effort like
		// the append — the window is already rebuilt and the worker resuming — so a failure is
		// surfaced and carried, never aborting the restore.
		if err := m.Store.ExtendLease(context.Background(), t.ID); err != nil {
			fmt.Fprintf(os.Stderr, "ttorch: could not refresh the resume lease anchor for %s: %v\n", t.ID, err)
		}
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

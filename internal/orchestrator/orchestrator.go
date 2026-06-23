// Package orchestrator composes the runtime primitives (tmux, worktree, harness,
// state) into the operations the manager drives: spawn, status, peek, send,
// teardown, plus launching the manager session and ad-hoc cc sessions.
package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/profile"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/termtab"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// Manager performs runtime operations against a tmux session and the state store.
type Manager struct {
	P       paths.Paths
	Session string
	Store   state.Store
	Pool    worktree.Pool
}

// New builds a Manager from the standard paths.
func New(p paths.Paths) *Manager {
	return &Manager{
		P:       p,
		Session: tmux.SessionName(),
		Store:   state.Store{Dir: p.StateDir()},
		Pool:    worktree.Pool{Root: p.Worktrees(), Max: worktree.MaxFromEnv()},
	}
}

// inUseWorktrees returns the worktree paths held by active tasks for a repo.
func (m *Manager) inUseWorktrees(repo string) []string {
	tasks, _ := m.Store.List()
	var out []string
	for _, t := range tasks {
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

// Spawn starts a worker for taskID against the repo containing projectPath. If
// rawCmd is empty, it launches the detected harness with the task brief; otherwise
// it runs rawCmd (used for testing and escape hatches).
func (m *Manager) Spawn(taskID, projectPath string, scout bool, rawCmd string) (state.Task, error) {
	var zero state.Task
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
	autoInit(repo) // first-use setup so workers always have AGENTS.md to read

	window := "wk-" + taskID
	if tmux.WindowExists(m.Session, window) {
		return zero, fmt.Errorf("task %q already has a window; tear it down first", taskID)
	}

	wt, err := m.Pool.Acquire(repo, m.inUseWorktrees(repo))
	if err != nil {
		return zero, err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return zero, err
	}
	if err := tmux.NewWindow(m.Session, window, wt); err != nil {
		return zero, err
	}

	kind := "ship"
	if scout {
		kind = "scout"
	}
	h := harness.Resolve()
	// Assign a stable session id so a later restore resumes this exact conversation.
	sid := harness.NewSessionID()
	// Install the turn-end hook so the supervisor can detect this worker's turns,
	// and pre-accept the harness's folder-trust prompt so the worker runs autonomously.
	_ = harness.InstallTurnEndHook(h, wt, m.P.TurnEndMarker(taskID))
	harness.TrustWorktree(h, repo, wt)
	cmd := rawCmd
	if cmd == "" {
		brief := m.P.BriefPath(taskID)
		if _, err := os.Stat(brief); os.IsNotExist(err) {
			_ = writeBriefStub(brief, taskID, kind)
		}
		cmd = harness.BriefCommand(h, brief, sid)
	}
	if err := tmux.SendLine(m.Session, window, cmd); err != nil {
		return zero, err
	}
	// Best-effort: open a native terminal tab/window that attaches a view onto
	// this worker's tmux window so the lead can watch it. The worker stays in
	// tmux regardless; never fail the spawn on this.
	_ = termtab.Open(m.Session, window)

	t := state.Task{
		ID: taskID, Window: window, Worktree: wt, Project: repo,
		Harness: h, Kind: kind, Created: time.Now(), SessionID: sid,
	}
	if err := m.Store.Save(t); err != nil {
		return zero, err
	}
	return t, nil
}

// Live reports whether a task's tmux window is still present.
func (m *Manager) Live(t state.Task) bool {
	return tmux.WindowExists(m.Session, t.Window)
}

// Status returns all tracked tasks.
func (m *Manager) Status() ([]state.Task, error) { return m.Store.List() }

// Peek returns the last n lines of a worker's pane.
func (m *Manager) Peek(taskID string, lines int) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	return tmux.CapturePane(m.Session, t.Window, lines)
}

// Send types a line into a worker's pane.
func (m *Manager) Send(taskID, text string) error {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return fmt.Errorf("unknown task %q", taskID)
	}
	return tmux.SendLine(m.Session, t.Window, text)
}

// Teardown finishes a task: it refuses to discard a worktree with uncommitted
// changes unless force is set, then kills the window and returns the worktree.
func (m *Manager) Teardown(taskID string, force bool) ([]string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	var notes []string
	if !force {
		if dirty, _ := worktree.IsDirty(t.Worktree); dirty {
			return nil, fmt.Errorf("task %q has uncommitted changes; review it, then 'ttorch teardown %s --force'", taskID, taskID)
		}
	}
	m.killPaneProcesses(t.Window)
	_ = tmux.KillWindow(m.Session, t.Window)
	termtab.Close(t.Window)
	if t.Project != "" && t.Worktree != "" {
		if err := m.Pool.Release(t.Project, t.Worktree); err != nil {
			notes = append(notes, "worktree: "+err.Error())
		} else {
			notes = append(notes, "worktree returned to pool for reuse")
		}
	}
	if err := m.Store.Remove(taskID); err != nil {
		notes = append(notes, "state: "+err.Error())
	}
	return notes, nil
}

// autoInit sets a repository up for ttorch on first use, so the lead never has to
// remember to run `ttorch init`. If path is inside a git repo that has not been
// initialized yet, it writes the AGENTS.md managed block (+ CLAUDE.md symlink) and
// the project profile, printing what it did. It is best-effort and opt-out via
// TTORCH_NO_AUTOINIT; it no-ops on non-git dirs and already-initialized repos.
func autoInit(path string) {
	if os.Getenv("TTORCH_NO_AUTOINIT") != "" {
		return
	}
	repo, err := worktree.RepoRoot(path)
	if err != nil || projectinit.Initialized(repo) {
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

	_, ok, _ := m.Store.LoadManager()
	tasks, _ := m.Store.List()
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
	if err := m.Store.SaveManager(state.Manager{Dir: dir, SessionID: sid}); err != nil {
		return err
	}
	autoInit(dir) // first-use setup of the default project (the launch dir)
	if err := tmux.NewWindow(m.Session, "manager", dir); err != nil {
		return err
	}
	_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(harness.Resolve(), sid))
	fmt.Fprintf(os.Stderr, "ttorch: manager started in %s — tell it the repo to work on; 'ttorch stop' to end.\n", dir)
	return m.attachManager()
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
		mgr, ok, _ := m.Store.LoadManager()
		if ok {
			if err := tmux.NewWindow(m.Session, "manager", mgr.Dir); err != nil {
				notes = append(notes, "skipped manager ("+err.Error()+")")
			} else {
				_ = tmux.SendLine(m.Session, "manager", harness.ManagerResumeOrFresh(h, mgr.SessionID))
				notes = append(notes, "restored manager")
			}
		} else {
			// No manager record (legacy state): start a fresh manager so the lead
			// always has one, and persist it for next time.
			dir := cwd()
			sid := harness.NewSessionID()
			if err := m.Store.SaveManager(state.Manager{Dir: dir, SessionID: sid}); err != nil {
				notes = append(notes, "could not persist new manager record: "+err.Error())
			}
			if err := tmux.NewWindow(m.Session, "manager", dir); err != nil {
				notes = append(notes, "skipped manager ("+err.Error()+")")
			} else {
				_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(h, sid))
				notes = append(notes, "started a fresh manager (no saved manager record)")
			}
		}
	}

	// Workers: rebuild each task window whose worktree still exists.
	tasks, _ := m.Store.List()
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
		if err := tmux.NewWindow(m.Session, t.Window, t.Worktree); err != nil {
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
	if err := m.Store.RemoveManager(); err != nil {
		notes = append(notes, "manager record: "+err.Error())
	}
	tasks, _ := m.Store.List()
	for _, t := range tasks {
		if err := m.Store.Remove(t.ID); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %s", t.ID, err.Error()))
		}
	}
	notes = append(notes, fmt.Sprintf("discarded the saved session (%d task record(s)); worktrees and branches were kept", len(tasks)))
	return notes, nil
}

// StopSession tears down the ttorch tmux session (and all its windows). The
// supervisor is stopped separately by the caller. It does NOT clear state, so the
// session can be resumed later with `ttorch` or `ttorch resume`.
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
func (m *Manager) OpenCC(isolated bool) error {
	if err := m.requireTmux(); err != nil {
		return err
	}
	dir := cwd()
	id := "cc-" + time.Now().Format("150405")
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
	if err := tmux.NewWindow(m.Session, window, dir); err != nil {
		return err
	}
	h := harness.Resolve()
	if repo, err := worktree.RepoRoot(dir); err == nil {
		harness.TrustWorktree(h, repo, dir)
	} else {
		harness.TrustWorktree(h, dir)
	}
	_ = tmux.SendLine(m.Session, window, harness.InteractiveCommand(h))
	_ = m.Store.Save(state.Task{
		ID: id, Window: window, Worktree: dir, Harness: h,
		Kind: "cc", Created: time.Now(),
	})
	return tmux.Attach(m.Session, window)
}

// --- delivery lifecycle (M4) ---

// ReviewDiff returns a worker's changes against the repo's default branch.
func (m *Manager) ReviewDiff(taskID string, stat bool) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	base := worktree.DefaultBranch(t.Project)
	return worktree.Diff(t.Worktree, base, stat)
}

// Validate runs the worktree's detected checks for a task. It returns nil results
// when no checks are detected (the caller reports that distinctly from a pass).
func (m *Manager) Validate(taskID string) ([]validate.Result, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	steps := validate.Detect(t.Worktree)
	if len(steps) == 0 {
		return nil, nil
	}
	return validate.Run(t.Worktree, steps), nil
}

// Approve grants a short-lived approval token authorizing a merge for taskID.
// This is intended for the lead to run, not the manager.
func (m *Manager) Approve(taskID string, ttl time.Duration) error {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if ttl <= 0 {
		return fmt.Errorf("--ttl must be positive (got %s)", ttl)
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return err
	}
	if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, head); err != nil {
		return err
	}
	m.audit(fmt.Sprintf("approve task=%s commit=%s ttl=%s", taskID, short(head), ttl))
	return nil
}

// MergeLocal fast-forwards the repo's local default branch to the worker's HEAD —
// the sole sanctioned state-changing write to a real checkout. It requires a valid
// approval token, the default branch checked out and clean, and a clean
// fast-forward. Every merge is recorded in the audit log.
func (m *Manager) MergeLocal(taskID string) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	if !approval.Valid(m.P.ApprovalFile(taskID)) {
		return "", fmt.Errorf("no valid approval for %q; the lead must run 'ttorch approve %s' first", taskID, taskID)
	}
	repo := t.Project
	def := worktree.DefaultBranch(repo)
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
	workerHead, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
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
	approvedHead, ok := approval.Consume(m.P.ApprovalFile(taskID))
	if !ok {
		return "", fmt.Errorf("approval for %q expired before merge; run 'ttorch approve %s' again", taskID, taskID)
	}
	if approvedHead != workerHead {
		return "", fmt.Errorf("worker %q changed since approval (approved %s, now %s); re-review with 'ttorch review-diff %s' and approve again", taskID, short(approvedHead), short(workerHead), taskID)
	}
	if err := worktree.MergeFastForward(repo, workerHead); err != nil {
		return "", err
	}
	m.audit(fmt.Sprintf("merge-local task=%s repo=%s %s -> %s", taskID, repo, def, short(workerHead)))
	return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
}

// Promote turns a scout task into a ship task (restoring teardown protection).
func (m *Manager) Promote(taskID string) error {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return fmt.Errorf("unknown task %q", taskID)
	}
	if t.Kind != "scout" {
		return fmt.Errorf("task %q is not a scout task", taskID)
	}
	t.Kind = "ship"
	return m.Store.Save(t)
}

// ArmPRCheck records a PR URL on a task so the supervisor polls for its merge.
func (m *Manager) ArmPRCheck(taskID, url string) error {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return fmt.Errorf("unknown task %q", taskID)
	}
	t.PR = url
	return m.Store.Save(t)
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
	tasks, _ := m.Store.List()
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

func (m *Manager) audit(line string) {
	_ = os.MkdirAll(m.P.Home, 0o755)
	f, err := os.OpenFile(m.P.AuditLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
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

func writeBriefStub(path, id, kind string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`# Task %s (%s)

Ramp up first: read this repo's AGENTS.md / CLAUDE.md for project context and
conventions, and use any relevant skills available to you, before changing code.

The manager has not written a detailed brief yet. Wait for instructions, or ask
the manager via your window. Work only within this worktree; commit on a feature
branch. Do not address the lead directly.
`, id, kind)
	return os.WriteFile(path, []byte(body), 0o644)
}

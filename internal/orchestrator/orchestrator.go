// Package orchestrator composes the runtime primitives (tmux, worktree, harness,
// state) into the operations the manager drives: spawn, status, peek, send,
// teardown, plus launching the manager session and ad-hoc cc sessions.
package orchestrator

import (
	"encoding/json"
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
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/supervisor"
	"github.com/nution101/ttorch/internal/termtab"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// requiredReviewers are the adversarial-review dimensions every trust verdict must
// cover. It is the single source of truth for both recording (which per-dimension
// reports to aggregate) and completeness (a missing dimension fails closed).
var requiredReviewers = []string{"correctness", "scope", "security"}

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
	// Read-only with respect to the lead's checkout: announce the delivery mode and
	// never write tracked files. First-use setup (AGENTS.md managed block + CLAUDE.md
	// symlink + profile) is opt-in via `ttorch init` or `ttorch spawn --init`.
	noticeDeliveryMode(repo)

	window := "wk-" + taskID
	if tmux.WindowExists(m.Session, window) {
		return zero, fmt.Errorf("task %q already has a window; tear it down first", taskID)
	}
	kind := "ship"
	if scout {
		kind = "scout"
	}

	wt, err := m.Pool.Acquire(repo, m.inUseWorktrees(repo))
	if err != nil {
		return zero, err
	}
	if err := tmux.EnsureSession(m.Session); err != nil {
		return zero, err
	}
	if err := m.newWindow(window, wt, windowLabel(kind, taskID)); err != nil {
		return zero, err
	}

	h := harness.Resolve()
	// Assign a stable session id so a later restore resumes this exact conversation.
	sid := harness.NewSessionID()
	// Install the turn-end hook so the supervisor can detect this worker's turns,
	// and pre-accept the harness's folder-trust prompt so the worker runs autonomously.
	_ = harness.InstallTurnEndHook(h, wt, m.P.TurnEndMarker(taskID))
	harness.TrustWorktree(h, repo, wt)
	// The turn-end hook is useless without something reading it: make sure the
	// background supervisor is up so this worker's turn boundaries and idle become
	// wakes the manager is told about.
	m.ensureSupervisor()
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

// ensureSupervisor starts the background supervisor if it isn't already running,
// so the manager is told when a worker ends a turn or goes idle. It reuses the
// shared start path (supervisor.Start) and is best-effort: a failure is reported
// to stderr but never fails the spawn. Set TTORCH_NO_SUPERVISOR=1 to manage the
// supervisor yourself.
func (m *Manager) ensureSupervisor() {
	if os.Getenv("TTORCH_NO_SUPERVISOR") != "" {
		return
	}
	_, started, err := supervisor.Start(m.P)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not start the supervisor (run 'ttorch supervise'): %v\n", err)
		return
	}
	if started {
		fmt.Fprintln(os.Stderr, "ttorch: started the background supervisor to watch for turn-end/idle")
	}
}

// Live reports whether a task's tmux window is still present.
func (m *Manager) Live(t state.Task) bool {
	return tmux.WindowExists(m.Session, t.Window)
}

// Status returns all tracked tasks.
func (m *Manager) Status() ([]state.Task, error) { return m.Store.List() }

// DeriveState classifies a worker from observable inputs: whether its tmux window
// is live and a recent capture of its pane. It is the pure core of TaskState, kept
// separate so it can be tested without tmux. The states are:
//   - "gone":    the window is no longer present
//   - "working": the pane shows a busy indicator (mid-turn)
//   - "idle":    the window is live but not busy (finished / awaiting input)
//
// It mirrors the supervisor's stale-detection heuristic (supervisor.Busy) so
// `ttorch status` and the wakes the manager receives never disagree.
func DeriveState(live bool, pane string) string {
	if !live {
		return "gone"
	}
	if supervisor.Busy(pane) {
		return "working"
	}
	return "idle"
}

// TaskState reports a worker's live state for `ttorch status` (see DeriveState).
// A live pane that can't be captured falls back to "idle".
func (m *Manager) TaskState(t state.Task) string {
	if !tmux.WindowExists(m.Session, t.Window) {
		return DeriveState(false, "")
	}
	out, _ := tmux.CapturePane(m.Session, t.Window, 6)
	return DeriveState(true, out)
}

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

// uninitNotice returns the one-line notice to print on spawn (and manager start) when
// path is inside a git repo that has not been `ttorch init`'d, or "" when no notice is
// warranted — a non-git path or an already-initialized repo. Splitting the decision
// from the printing keeps the read-only-on-spawn contract testable without tmux.
//
// It is strictly read-only: it never writes AGENTS.md, the CLAUDE.md symlink, or the
// project profile. Setup is opt-in via `ttorch init` or `ttorch spawn --init`
// (see InitRepo). The mode reported is whatever projectinit.ReadMode resolves, which
// defaults to "pr" for an uninitialized repo.
func uninitNotice(path string) string {
	repo, err := worktree.RepoRoot(path)
	if err != nil || projectinit.Initialized(repo) {
		return ""
	}
	return fmt.Sprintf("%s not ttorch-init'd; using delivery-mode=%s (run \"ttorch init\" to persist).",
		repo, projectinit.ReadMode(repo))
}

// noticeDeliveryMode prints the uninitNotice for path to stderr, if any. It is the
// non-destructive replacement for the former auto-init: spawn and manager start tell
// the lead which delivery mode is in effect without ever mutating their checkout.
func noticeDeliveryMode(path string) {
	if msg := uninitNotice(path); msg != "" {
		fmt.Fprintln(os.Stderr, "ttorch: "+msg)
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
	noticeDeliveryMode(dir) // read-only: never mutate the launch dir's tracked files
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
		mgr, ok, _ := m.Store.LoadManager()
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
			if err := m.Store.SaveManager(state.Manager{Dir: dir, SessionID: sid}); err != nil {
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
	// Record that THIS approval was the lead's, overwriting any prior auto-mint marker
	// (TrustRecord) so a later gated merge attributes it correctly in the audit log. The
	// token and this field are always written together, so the consumed token's
	// provenance and t.ApprovedBy never drift.
	t.ApprovedBy = "human"
	if err := m.Store.Save(t); err != nil {
		return err
	}
	m.audit(fmt.Sprintf("approve task=%s commit=%s ttl=%s", taskID, short(head), ttl))
	return nil
}

// TrustPrep materializes the inputs the adversarial reviewers read for taskID into
// ReviewInputsDir: the diff against the default branch (diff.patch), the brief
// (brief.md, if one was written), a fresh validate run (validate.json), and the
// reviewed HEAD (head.txt). It is the read-only setup step before review subagents
// run and enforces nothing. It returns the inputs dir.
func (m *Manager) TrustPrep(taskID string) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	dir := m.P.ReviewInputsDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	head, err := worktree.Head(t.Worktree)
	if err != nil {
		return "", err
	}
	diff, err := worktree.Diff(t.Worktree, worktree.DefaultBranch(t.Project), false)
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
	results, _ := m.Validate(taskID) // nil when no checks are detected
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
	t, err := m.Store.Load(taskID)
	if err != nil {
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
	if err := review.Write(m.P.ReviewVerdictFile(taskID), verdict, ttl); err != nil {
		return zero, err
	}
	t.ReviewedSHA = sha
	t.GatePassed = verdict.Overall == review.Pass
	t.ApprovedBy = ""
	// Trusted mode is the sole carve-out: a PASS verdict over a green worktree
	// auto-mints the approval token so the lead need not read the diff. Bind it to the
	// reviewed sha (== HEAD, asserted above) so a later commit invalidates it, exactly
	// as a human approval would. Any other mode leaves the verdict advisory.
	if verdict.Overall == review.Pass && projectinit.ReadMode(t.Project) == "trusted" {
		if green, _ := validateGreen(t.Worktree); green {
			if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, sha); err != nil {
				return zero, err
			}
			t.ApprovedBy = "auto"
		}
	}
	if err := m.Store.Save(t); err != nil {
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
	t, err := m.Store.Load(taskID)
	if err != nil {
		return "", fmt.Errorf("unknown task %q", taskID)
	}
	if !approval.Valid(m.P.ApprovalFile(taskID)) {
		return "", fmt.Errorf("no valid approval for %q; the lead must run 'ttorch approve %s' first", taskID, taskID)
	}
	repo := t.Project
	gated := requireVerdict || projectinit.ReadMode(repo) == "trusted"
	if gated {
		// Load (do not yet consume) the verdict so a recoverable refusal below leaves it
		// intact for a retry. Absent/expired/blocking all fail closed.
		v, ok := review.Load(m.P.ReviewVerdictFile(taskID))
		if !ok {
			return "", fmt.Errorf("trust gate: no valid review verdict for %q; run 'ttorch trust prep %s', review, then 'ttorch trust record %s'", taskID, taskID, taskID)
		}
		if v.Overall != review.Pass {
			return "", fmt.Errorf("trust gate: the review verdict for %q is %q, not pass; resolve the blocking findings and re-record", taskID, v.Overall)
		}
		// Re-run validation fresh at the gate. No-checks-detected is a hard BLOCK: a
		// repo with no build/test/lint cannot satisfy the gate (see validateGreen).
		green, results := validateGreen(t.Worktree)
		if !green {
			if len(results) == 0 {
				return "", fmt.Errorf("trust gate: no checks detected for %q; the gate requires a build/test/lint suite (add .ttorch/validate.sh)", taskID)
			}
			return "", fmt.Errorf("trust gate: %d of %d checks failed for %q; fix them, re-validate, and re-record the verdict", len(validate.Failures(results)), len(results), taskID)
		}
	}
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
	approver := "human"
	if gated {
		// Consume the verdict beside the approval and pin it to the merged commit — the
		// second commit-pin, parallel to approvedHead==workerHead, so a commit pushed
		// after review can never ride in unreviewed.
		cv, ok := review.Consume(m.P.ReviewVerdictFile(taskID))
		if !ok {
			return "", fmt.Errorf("trust gate: the review verdict for %q expired before merge; re-record it with 'ttorch trust record %s'", taskID, taskID)
		}
		if cv.ReviewedSHA != workerHead {
			return "", fmt.Errorf("trust gate: the verdict for %q covers %s but the worker is now %s; re-review and re-record", taskID, short(cv.ReviewedSHA), short(workerHead))
		}
		if t.ApprovedBy == "auto" {
			approver = "auto"
		}
	}
	if err := worktree.MergeFastForward(repo, workerHead); err != nil {
		return "", err
	}
	if gated {
		m.audit(fmt.Sprintf("merge-local task=%s repo=%s %s -> %s gate=verdict approver=%s", taskID, repo, def, short(workerHead), approver))
	} else {
		m.audit(fmt.Sprintf("merge-local task=%s repo=%s %s -> %s", taskID, repo, def, short(workerHead)))
	}
	return fmt.Sprintf("fast-forwarded %s to %s for task %s", def, short(workerHead), taskID), nil
}

// validateGreen reports whether worktreeDir has at least one detected check and every
// detected check passes, returning the run's results for reporting. A repo with NO
// detectable checks is deliberately NOT green: Manager.Validate returns nil results in
// that case, and reading an empty Failures() as a pass would fail open (auto-merging a
// repo with no build/test/lint). Both the trusted-mode auto-approve and the merge gate
// rely on this fail-closed contract.
func validateGreen(worktreeDir string) (bool, []validate.Result) {
	steps := validate.Detect(worktreeDir)
	if len(steps) == 0 {
		return false, nil
	}
	results := validate.Run(worktreeDir, steps)
	return len(validate.Failures(results)) == 0, results
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

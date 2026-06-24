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
	if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("human", head)); err != nil {
		return err
	}
	// Record that THIS approval was the lead's — both in the token's provenance (the
	// authority for the merge's audit label) and in the persisted task state (overwriting
	// any prior auto-mint marker so the two never drift).
	t.ApprovedBy = "human"
	if err := m.Store.Save(t); err != nil {
		return err
	}
	m.audit(fmt.Sprintf("approve task=%s commit=%s ttl=%s", taskID, short(head), ttl))
	return nil
}

// TrustPrep materializes the inputs the adversarial reviewers read for taskID into
// ReviewInputsDir: the COMMITTED diff against the default branch (diff.patch), the brief
// (brief.md, if one was written), a fresh validate of the committed sha (validate.json),
// and the reviewed HEAD (head.txt). It refuses a dirty worktree and reads only committed
// objects, so the reviewers see exactly the commit that will fast-forward — a worker
// cannot present a benign working tree while a different commit merges. It returns the
// inputs dir.
func (m *Manager) TrustPrep(taskID string) (string, error) {
	t, err := m.Store.Load(taskID)
	if err != nil {
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
	dir := m.P.ReviewInputsDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The reviewers' diff is the COMMITTED diff (base..HEAD), never the working tree.
	diff, err := worktree.DiffCommitted(t.Worktree, worktree.DefaultBranch(t.Project), head)
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
		if cerr == nil && terr == nil && clean && !touched {
			green, _, _ = validateCommitted(t.Project, sha)
		}
		if green {
			if err := approval.Grant(m.P.ApprovalFile(taskID), ttl, approvalPayload("auto", sha)); err != nil {
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

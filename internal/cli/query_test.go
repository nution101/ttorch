package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/singleton"
)

// withSeedDB creates an isolated SQLite store at a temp path, points $TTORCH_DB at
// it (so mgr()/cmd* resolve to it via paths.StateDB), runs seed against an open
// store, then closes it. The package TestMain pins TTORCH_HOME, and db.Open's guard
// is the final backstop, so this never touches the real ~/.ttorch.
func withSeedDB(t *testing.T, seed func(ctx context.Context, s *db.Store)) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	t.Setenv("TTORCH_TASK_ID", "")
	s, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(context.Background(), s)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

// captureStdout swaps os.Stdout for a pipe for the duration of fn and returns what
// it wrote. A goroutine drains the pipe so a large write never blocks.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out, runErr
}

func i64(v int64) *int64 { return &v }

// --- pure render / helper tests ---------------------------------------------

func TestDash(t *testing.T) {
	if dash("") != "-" || dash("x") != "x" {
		t.Fatal("dash should map empty→- and pass non-empty through")
	}
}

// TestRenderStatusColumns: the table carries the DB-backed STATUS/STAGE/OWNER
// columns alongside the live STATE; an empty stage/owner renders as "-". Both rows
// are spawned workers (status only ever renders windowed tasks).
func TestRenderStatusColumns(t *testing.T) {
	var b strings.Builder
	renderStatus(&b, []statusRow{
		{ID: "a", Kind: "ship", State: "working", Status: "active", Stage: "implementing", Owner: "worker:a", Window: "wk-a", Project: "/repo"},
		{ID: "b", Kind: "ship", State: "gone", Status: "active", Stage: "", Owner: "", Window: "wk-b", Project: "/repo"},
	}, map[string]int{"/repo": 14})
	out := b.String()
	for _, want := range []string{"STATUS", "STAGE", "OWNER", "active", "implementing", "worker:a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status table missing %q, got:\n%s", want, out)
		}
	}
	// Row b's empty stage/owner collapse to dashes.
	if !strings.Contains(out, "-") {
		t.Fatalf("a sparse row should show a dash, got:\n%s", out)
	}
}

// TestStatusExcludesBacklog proves the spec split (§3.3): a pending backlog task is
// listed by `ttorch tasks` but EXCLUDED from `ttorch status`, which shows only
// spawned/windowed workers.
func TestStatusExcludesBacklog(t *testing.T) {
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		if _, err := s.CreateTask(ctx, db.Task{ID: "wkr", ProjectID: p.ID, Status: db.StatusActive, Window: "wk-wkr", Owner: "worker:wkr"}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTask(ctx, db.Task{ID: "backlog", ProjectID: p.ID, Status: db.StatusPending}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
	})
	store := reopen(t, dbPath)
	// `ttorch tasks` (unfiltered ListTasks) lists everything, backlog included.
	all, _ := store.ListTasks(context.Background(), db.TaskFilter{})
	if !containsTask(all, "wkr") || !containsTask(all, "backlog") {
		t.Fatalf("ttorch tasks should list both spawned + backlog, got %v", taskIDs(all))
	}
	// `ttorch status` applies windowedTasks: only the spawned worker survives.
	live := windowedTasks(all)
	if !containsTask(live, "wkr") {
		t.Fatalf("status should include the spawned worker, got %v", taskIDs(live))
	}
	if containsTask(live, "backlog") {
		t.Fatalf("status must EXCLUDE the pending backlog task, got %v", taskIDs(live))
	}
}

func TestWindowedTasksFilter(t *testing.T) {
	got := windowedTasks([]db.Task{{ID: "a", Window: "wk-a"}, {ID: "b"}, {ID: "c", Window: "wk-c"}})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("windowedTasks = %v, want [a c]", taskIDs(got))
	}
}

// TestRenderTasks proves the flat list renders the lifecycle columns and INCLUDES a
// pending backlog task (window-less), with title/footprint on continuation lines.
func TestRenderTasks(t *testing.T) {
	var b strings.Builder
	renderTasks(&b, []db.Task{
		{ID: "live-1", Kind: "ship", Status: "active", Stage: "testing", Owner: "worker:live-1", Project: "/repo", Title: "the live one"},
		{ID: "backlog-1", Kind: "ship", Status: "pending", Project: "/repo", Title: "queued", Footprint: []string{"internal/cli", "internal/db"}},
	})
	out := b.String()
	for _, want := range []string{"live-1", "active", "testing", "backlog-1", "pending", "title: queued", "touches: internal/cli, internal/db", "2 task(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tasks table missing %q, got:\n%s", want, out)
		}
	}
}

func TestRenderTasksEmpty(t *testing.T) {
	var b strings.Builder
	renderTasks(&b, nil)
	if !strings.Contains(b.String(), "no tasks match") {
		t.Fatalf("empty task list should print a hint, got:\n%s", b.String())
	}
}

func TestParseStatusList(t *testing.T) {
	// comma list, trimming, and the needs-input→needs_input alias.
	got, err := parseStatusList(" done , blocked ,needs-input")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "done,blocked,needs_input" {
		t.Fatalf("parseStatusList = %v, want [done blocked needs_input]", got)
	}
	if v, _ := parseStatusList(""); v != nil {
		t.Fatalf("empty --status = %v, want nil (no constraint)", v)
	}
	if _, err := parseStatusList("done,bogus"); err == nil {
		t.Fatal("an unknown status must be rejected")
	}
}

// TestBuildTreePlacement (pure): a task hangs at the deepest level its ids resolve
// to — under its phase, else loose under its epic, else loose under its project —
// and ordering follows the input slices.
func TestBuildTreePlacement(t *testing.T) {
	projects := []db.Project{{ID: 1, RepoPath: "/r", Name: "r"}}
	epics := []db.Epic{{ID: 10, ProjectID: 1, Title: "E"}}
	phases := []db.Phase{{ID: 100, EpicID: 10, Title: "P"}}
	tasks := []db.Task{
		{ID: "t-phase", ProjectID: 1, EpicID: i64(10), PhaseID: i64(100)},
		{ID: "t-epic", ProjectID: 1, EpicID: i64(10)},
		{ID: "t-proj", ProjectID: 1},
	}
	roots := buildTree(projects, epics, phases, tasks, 0)
	if len(roots) != 1 || len(roots[0].epics) != 1 || len(roots[0].epics[0].phases) != 1 {
		t.Fatalf("tree shape wrong: %+v", roots)
	}
	pr := roots[0]
	if len(pr.epics[0].phases[0].tasks) != 1 || pr.epics[0].phases[0].tasks[0].ID != "t-phase" {
		t.Fatalf("t-phase should be under the phase, got %+v", pr.epics[0].phases[0].tasks)
	}
	if len(pr.epics[0].loose) != 1 || pr.epics[0].loose[0].ID != "t-epic" {
		t.Fatalf("t-epic should be loose under the epic, got %+v", pr.epics[0].loose)
	}
	if len(pr.loose) != 1 || pr.loose[0].ID != "t-proj" {
		t.Fatalf("t-proj should be loose under the project, got %+v", pr.loose)
	}
	// A non-zero projectID filter restricts the roots.
	if got := buildTree(projects, epics, phases, tasks, 999); len(got) != 0 {
		t.Fatalf("buildTree with an unknown project filter = %v, want empty", got)
	}
}

// TestRunTaskTreeOrdering exercises the full tree path against a seeded store and
// asserts the projects→epics→phases→tasks order in the rendered output.
func TestRunTaskTreeOrdering(t *testing.T) {
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		e, _ := s.CreateEpic(ctx, p.ID, "the epic", "")
		ph, _ := s.CreatePhase(ctx, e.ID, "the phase", "")
		mk := func(id string, epic, phase *int64) {
			if _, err := s.CreateTask(ctx, db.Task{ID: id, ProjectID: p.ID, EpicID: epic, PhaseID: phase, Status: db.StatusPending}, db.ActorManager); err != nil {
				t.Fatal(err)
			}
		}
		mk("t-phase", i64(e.ID), i64(ph.ID))
		mk("t-epic", i64(e.ID), nil)
		mk("t-proj", nil, nil)
	})
	store := reopen(t, dbPath)
	var b strings.Builder
	if err := runTaskTree(context.Background(), store, 0, nil, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	order := []string{"project ", "epic ", "phase ", "t-phase", "t-epic", "t-proj"}
	last := -1
	for _, marker := range order {
		idx := strings.Index(out, marker)
		if idx < 0 {
			t.Fatalf("tree output missing %q, got:\n%s", marker, out)
		}
		if idx < last {
			t.Fatalf("tree output out of order at %q, got:\n%s", marker, out)
		}
		last = idx
	}
}

// TestRenderTimelineOrdering: the timeline renders created→stage→status events then
// the note, in the ts order Timeline returns (events before notes on a tie, §2.2).
func TestRenderTimelineOrdering(t *testing.T) {
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		if _, err := s.CreateTask(ctx, db.Task{ID: "t", ProjectID: p.ID, Status: db.StatusActive}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetStage(ctx, "t", "implementing", "worker:t"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReportStatus(ctx, "t", db.StatusBlocked, "worker:t", "need a decision"); err != nil {
			t.Fatal(err)
		}
	})
	store := reopen(t, dbPath)
	items, err := store.Timeline(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	renderTimeline(&b, "t", items)
	out := b.String()
	order := []string{"created", "stage_changed", "implementing", "status_changed", "active→blocked", "need a decision"}
	last := -1
	for _, marker := range order {
		idx := strings.Index(out, marker)
		if idx < 0 {
			t.Fatalf("timeline missing %q, got:\n%s", marker, out)
		}
		if idx < last {
			t.Fatalf("timeline out of order at %q, got:\n%s", marker, out)
		}
		last = idx
	}
}

func TestRenderTimelineEmpty(t *testing.T) {
	var b strings.Builder
	renderTimeline(&b, "ghost", nil)
	if !strings.Contains(b.String(), "no events or notes") {
		t.Fatalf("empty timeline should say so, got:\n%s", b.String())
	}
}

// --- command-level (DB round-trip) tests -------------------------------------

// TestCmdProjectAddPopulatesModeCache proves project add registers the repo AND
// caches its delivery mode from AGENTS.md (§3.3 / §0.3 display cache).
func TestCmdProjectAddPopulatesModeCache(t *testing.T) {
	repo := initGitRepo(t)
	agents := "# repo\n\n<!-- BEGIN ttorch-managed -->\n- delivery-mode: trusted\n<!-- END ttorch-managed -->\n"
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := withSeedDB(t, nil)
	if _, err := captureStdout(t, func() error { return cmdProjectAdd([]string{repo}) }); err != nil {
		t.Fatal(err)
	}
	store := reopen(t, dbPath)
	projects, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("want exactly one project, got %d", len(projects))
	}
	if projects[0].DeliveryMode != "trusted" {
		t.Fatalf("delivery_mode = %q, want trusted (cached from AGENTS.md)", projects[0].DeliveryMode)
	}
}

// TestCmdTaskAddPendingBacklog: task add creates a window-less PENDING row that the
// tasks query then includes (the backlog), and it never spawns.
func TestCmdTaskAddPendingBacklog(t *testing.T) {
	var projID int64
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
	})
	out, err := captureStdout(t, func() error {
		return cmdTaskAdd([]string{"backlog-1", "--project", itoa(projID), "--title", "do later", "--touches", "internal/cli"})
	})
	if err != nil {
		t.Fatalf("task add: %v", err)
	}
	if !strings.Contains(out, "backlog task backlog-1") {
		t.Fatalf("task add output = %q", out)
	}
	store := reopen(t, dbPath)
	task := mustTask(t, store, "backlog-1")
	if task.Status != db.StatusPending {
		t.Fatalf("status = %q, want pending", task.Status)
	}
	if task.Window != "" || task.Worktree != "" {
		t.Fatalf("backlog task must not be spawned (no window/worktree), got window=%q worktree=%q", task.Window, task.Worktree)
	}
	// The pending row is included by the default (unfiltered) tasks query.
	all, _ := store.ListTasks(context.Background(), db.TaskFilter{})
	if len(all) != 1 || all[0].ID != "backlog-1" {
		t.Fatalf("ListTasks = %+v, want [backlog-1] (pending backlog included)", all)
	}
}

// TestCmdTaskAddStoresBrief: `task add --brief/--brief-file` persists the brief keyed by task
// id (paths.BriefPath), so the scheduler daemon's dispatch — and a later manual spawn — launch
// the worker WITH the full brief as its initial prompt instead of the stub. A task added with
// no brief stores none (and dispatches on the stub, unchanged); both flags at once is a loud
// error that creates no task.
func TestCmdTaskAddStoresBrief(t *testing.T) {
	var projID int64
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
	})
	const inline = "# Real brief\n\nImplement part C.\n"
	if _, err := captureStdout(t, func() error {
		return cmdTaskAdd([]string{"brief-inline", "--project", itoa(projID), "--brief", inline})
	}); err != nil {
		t.Fatalf("task add --brief: %v", err)
	}
	if got := mustReadFile(t, paths.Default().BriefPath("brief-inline")); got != inline {
		t.Fatalf("stored brief = %q, want %q", got, inline)
	}

	// --brief-file stores the file's contents.
	file := filepath.Join(t.TempDir(), "brief.md")
	const fromFile = "# File brief\n\nImplement part C from a file.\n"
	if err := os.WriteFile(file, []byte(fromFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdTaskAdd([]string{"brief-file", "--project", itoa(projID), "--brief-file", file})
	}); err != nil {
		t.Fatalf("task add --brief-file: %v", err)
	}
	if got := mustReadFile(t, paths.Default().BriefPath("brief-file")); got != fromFile {
		t.Fatalf("stored brief = %q, want %q", got, fromFile)
	}

	// No brief flag stores no brief — dispatch then falls back to the stub.
	if _, err := captureStdout(t, func() error {
		return cmdTaskAdd([]string{"brief-none", "--project", itoa(projID)})
	}); err != nil {
		t.Fatalf("task add (no brief): %v", err)
	}
	if _, err := os.Stat(paths.Default().BriefPath("brief-none")); !os.IsNotExist(err) {
		t.Fatalf("a task added with no brief must not store one; stat err = %v", err)
	}

	// Both flags at once is ambiguous: a loud error, and nothing is written (resolveBrief
	// refuses before the task row or brief file is created).
	if err := cmdTaskAdd([]string{"brief-both", "--project", itoa(projID), "--brief", inline, "--brief-file", file}); err == nil {
		t.Fatal("task add with both --brief and --brief-file must error")
	}
	if _, err := os.Stat(paths.Default().BriefPath("brief-both")); !os.IsNotExist(err) {
		t.Fatalf("the ambiguous add must not store a brief; stat err = %v", err)
	}
}

// TestCmdSchedulerSingletonExitsWhenHeld: `ttorch scheduler --singleton` exits quietly (a no-op
// success, with a clear notice) when another daemon already holds the per-~/.ttorch lock — the
// guard that lets the manager auto-start the daemon without ever running two. The lock is
// pre-held here to simulate a running daemon, and is left held after the command returns (the
// command never took it).
func TestCmdSchedulerSingletonExitsWhenHeld(t *testing.T) {
	withSeedDB(t, nil) // points TTORCH_DB at a temp store; TestMain pins TTORCH_HOME
	lock, acquired, err := singleton.Acquire(paths.Default().SchedulerPIDFile())
	if err != nil || !acquired {
		t.Fatalf("setup: pre-acquire the scheduler singleton: acquired=%v err=%v", acquired, err)
	}
	defer singleton.Release(lock)

	out, err := captureStdout(t, func() error {
		return cmdScheduler([]string{"--once", "--singleton"})
	})
	if err != nil {
		t.Fatalf("scheduler --singleton must exit cleanly when the lock is held: %v", err)
	}
	if !strings.Contains(out, "already holds the singleton") {
		t.Fatalf("expected the singleton-held notice, got: %q", out)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestCmdTaskAddValidations(t *testing.T) {
	var projID, otherEpicID int64
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
		other, _ := s.UpsertProject(ctx, "/other", "other")
		oe, _ := s.CreateEpic(ctx, other.ID, "other epic", "")
		otherEpicID = oe.ID
		if _, err := s.CreateTask(ctx, db.Task{ID: "exists", ProjectID: p.ID, Status: db.StatusPending}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
	})
	cases := map[string][]string{
		"missing project":    {"x"},
		"unknown project":    {"x", "--project", "9999"},
		"duplicate id":       {"exists", "--project", itoa(projID)},
		"unknown epic":       {"y", "--project", itoa(projID), "--epic", "9999"},
		"cross-project epic": {"z", "--project", itoa(projID), "--epic", itoa(otherEpicID)},
	}
	for name, args := range cases {
		if err := cmdTaskAdd(args); err == nil {
			t.Fatalf("%s: cmdTaskAdd(%v) must return an error", name, args)
		}
	}
}

// TestCmdTaskAddPhaseAdoptsEpic: --phase without --epic adopts the phase's parent
// epic, so the row is never left with phase_id set and epic_id NULL.
func TestCmdTaskAddPhaseAdoptsEpic(t *testing.T) {
	var projID, epicID, phaseID int64
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
		e, _ := s.CreateEpic(ctx, p.ID, "E", "")
		epicID = e.ID
		ph, _ := s.CreatePhase(ctx, e.ID, "P", "")
		phaseID = ph.ID
	})
	if _, err := captureStdout(t, func() error {
		return cmdTaskAdd([]string{"t", "--project", itoa(projID), "--phase", itoa(phaseID)})
	}); err != nil {
		t.Fatalf("task add --phase (no --epic): %v", err)
	}
	task := mustTask(t, reopen(t, dbPath), "t")
	if task.PhaseID == nil || *task.PhaseID != phaseID {
		t.Fatalf("phase_id = %v, want %d", task.PhaseID, phaseID)
	}
	if task.EpicID == nil || *task.EpicID != epicID {
		t.Fatalf("epic_id = %v, want %d (adopted from the phase's parent)", task.EpicID, epicID)
	}
}

// TestCmdTaskAddPhaseEpicMismatch: a --phase whose parent epic differs from an
// explicit --epic is rejected (the row would be incoherent).
func TestCmdTaskAddPhaseEpicMismatch(t *testing.T) {
	var projID, otherEpicID, phaseID int64
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
		e1, _ := s.CreateEpic(ctx, p.ID, "E1", "")
		e2, _ := s.CreateEpic(ctx, p.ID, "E2", "")
		otherEpicID = e2.ID
		ph, _ := s.CreatePhase(ctx, e1.ID, "P", "") // phase under E1
		phaseID = ph.ID
	})
	if err := cmdTaskAdd([]string{"t", "--project", itoa(projID), "--epic", itoa(otherEpicID), "--phase", itoa(phaseID)}); err == nil {
		t.Fatal("a --phase whose parent epic != --epic must be rejected")
	}
}

// TestCmdTasksFlagCombos: ambiguous flag combinations fail loudly rather than
// silently dropping a filter the caller supplied.
func TestCmdTasksFlagCombos(t *testing.T) {
	withSeedDB(t, nil) // pin TTORCH_DB so a regressed guard can't reach the real home
	for _, args := range [][]string{
		{"--timeline", "x", "--tree"},
		{"--timeline", "x", "--project", "1"},
		{"--tree", "--epic", "5"},
	} {
		if err := cmdTasks(args); err == nil {
			t.Fatalf("cmdTasks(%v) must reject the ambiguous flag combination", args)
		}
	}
}

// TestBuildTreeRespectsProject: a task whose epic ref points into ANOTHER project is
// anchored under its OWN project's loose bucket, identically whether the tree is
// project-filtered or not — never migrated under the foreign epic.
func TestBuildTreeRespectsProject(t *testing.T) {
	projects := []db.Project{{ID: 1, RepoPath: "/r1"}, {ID: 2, RepoPath: "/r2"}}
	epics := []db.Epic{{ID: 20, ProjectID: 2, Title: "foreign"}}
	tasks := []db.Task{{ID: "stray", ProjectID: 1, EpicID: i64(20)}} // epic 20 lives in project 2
	for _, filter := range []int64{0, 1} {
		roots := buildTree(projects, epics, nil, tasks, filter)
		var p1 *projTree
		for _, r := range roots {
			if r.p.ID == 1 {
				p1 = r
			}
		}
		if p1 == nil {
			t.Fatalf("filter=%d: project 1 missing from tree", filter)
		}
		if len(p1.loose) != 1 || p1.loose[0].ID != "stray" {
			t.Fatalf("filter=%d: stray task should be loose under its own project 1, got %+v", filter, p1.loose)
		}
	}
}

// TestCmdTasksMultiStatusFilter drives the full command: a comma-separated --status
// becomes TaskFilter.Status (→ status IN (?,…)) so only matching rows surface.
func TestCmdTasksMultiStatusFilter(t *testing.T) {
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		for _, tk := range []struct{ id, status string }{
			{"act", db.StatusActive}, {"blk", db.StatusBlocked}, {"dn", db.StatusDone},
		} {
			if _, err := s.CreateTask(ctx, db.Task{ID: tk.id, ProjectID: p.ID, Status: tk.status}, db.ActorManager); err != nil {
				t.Fatal(err)
			}
		}
	})
	out, err := captureStdout(t, func() error { return cmdTasks([]string{"--status", "done,blocked"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "blk") || !strings.Contains(out, "dn") {
		t.Fatalf("multi-status filter should include blocked+done rows, got:\n%s", out)
	}
	if strings.Contains(out, "act") {
		t.Fatalf("multi-status filter must exclude the active row, got:\n%s", out)
	}
	// An invalid status fails loudly.
	if err := cmdTasks([]string{"--status", "nonsense"}); err == nil {
		t.Fatal("tasks --status with an unknown value must error")
	}
}

// TestCmdEpicPhaseLifecycle: add an epic + phase under a project and drive their
// set-status verbs end to end.
func TestCmdEpicPhaseLifecycle(t *testing.T) {
	var projID int64
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, "/r", "r")
		projID = p.ID
	})
	if _, err := captureStdout(t, func() error {
		return cmdEpicAdd([]string{"--project", itoa(projID), "--title", "Build it"})
	}); err != nil {
		t.Fatalf("epic add: %v", err)
	}
	store := reopen(t, dbPath)
	epics, _ := store.ListEpics(context.Background(), projID)
	if len(epics) != 1 || epics[0].Title != "Build it" {
		t.Fatalf("epic ls = %+v, want one 'Build it'", epics)
	}
	epicID := epics[0].ID
	_ = store.Close()

	if _, err := captureStdout(t, func() error {
		return cmdPhaseAdd([]string{"--epic", itoa(epicID), "--title", "Phase one"})
	}); err != nil {
		t.Fatalf("phase add: %v", err)
	}
	store = reopen(t, dbPath)
	phases, _ := store.ListPhases(context.Background(), epicID)
	if len(phases) != 1 {
		t.Fatalf("phase ls = %+v, want one", phases)
	}
	phaseID := phases[0].ID
	_ = store.Close()

	if _, err := captureStdout(t, func() error { return cmdEpicSetStatus([]string{itoa(epicID), "in_progress"}) }); err != nil {
		t.Fatalf("epic set-status: %v", err)
	}
	if _, err := captureStdout(t, func() error { return cmdPhaseSetStatus([]string{itoa(phaseID), "done"}) }); err != nil {
		t.Fatalf("phase set-status: %v", err)
	}
	store = reopen(t, dbPath)
	if e, _, _ := store.GetEpic(context.Background(), epicID); e.Status != "in_progress" {
		t.Fatalf("epic status = %q, want in_progress", e.Status)
	}
	if ph, _, _ := store.GetPhase(context.Background(), phaseID); ph.Status != "done" {
		t.Fatalf("phase status = %q, want done", ph.Status)
	}

	// Guards: add requires a parent + title; set-status rejects an unknown status.
	if err := cmdEpicAdd([]string{"--title", "no project"}); err == nil {
		t.Fatal("epic add without --project must error")
	}
	if err := cmdEpicAdd([]string{"--project", itoa(projID)}); err == nil {
		t.Fatal("epic add without --title must error")
	}
	if err := cmdEpicSetStatus([]string{itoa(epicID), "bogus"}); err == nil {
		t.Fatal("epic set-status with an unknown status must error")
	}
	if err := cmdPhaseAdd([]string{"--epic", "9999", "--title", "x"}); err == nil {
		t.Fatal("phase add under a missing epic must error")
	}
}

// --- helpers -----------------------------------------------------------------

func itoa(v int64) string { return fmtID(v) }

func taskIDs(ts []db.Task) []string {
	out := make([]string, len(ts))
	for i, x := range ts {
		out[i] = x.ID
	}
	return out
}

func containsTask(ts []db.Task, id string) bool {
	for _, x := range ts {
		if x.ID == id {
			return true
		}
	}
	return false
}

// initGitRepo creates a throwaway git repo so worktree.RepoRoot (git rev-parse
// --show-toplevel) resolves. No commits are needed for show-toplevel.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v: %s", err, out)
	}
	return dir
}

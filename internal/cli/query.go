package cli

// Manager query/management surfaces (§3.3): the read side of the SQLite store
// (`tasks`, the project/epic/phase listings, the timeline and tree views) plus the
// management verbs that create backlog hierarchy WITHOUT spawning a worker
// (`project add`, `epic add`, `phase add`, `task add`, and the epic/phase
// set-status verbs). Each is short-lived — it opens one Manager (one db.Open +
// defer Close, §2.4) and reuses the existing store methods. Every query is
// parameterized; no value is concatenated into SQL (§1.4). The delivery-mode the
// gate enforces still comes from AGENTS.md — the project's cached delivery_mode
// here is DISPLAY ONLY (§0.3).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/worktree"
)

// dash renders an empty string as "-" so a sparse table column stays visible.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// fmtID renders a surrogate id for a table cell.
func fmtID(id int64) string { return strconv.FormatInt(id, 10) }

// deref returns the pointed-to string, or "" when nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- ttorch tasks ------------------------------------------------------------

const tasksUsage = `usage: ttorch tasks [--project <id>] [--epic <id>] [--status s[,s…]] [--tree] [--timeline <task-id>]`

// cmdTasks is the DB-backed task query (incl. the pending backlog). Default mode is
// a flat, filtered list; --tree prints the projects→epics→phases→tasks hierarchy;
// --timeline <id> prints one task's events∪notes by time (§3.3).
func cmdTasks(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	project := fs.Int64("project", 0, "filter to a project id (see 'ttorch project ls')")
	epic := fs.Int64("epic", 0, "filter to an epic id (see 'ttorch epic ls')")
	status := fs.String("status", "", "comma-separated statuses to include, e.g. active,blocked,done")
	tree := fs.Bool("tree", false, "print the projects→epics→phases→tasks hierarchy")
	timeline := fs.String("timeline", "", "print one task's timeline (events ∪ notes by time)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	statuses, err := parseStatusList(*status)
	if err != nil {
		return err
	}
	// Reject ambiguous flag combinations rather than silently honoring one and
	// dropping the rest: --timeline addresses a single task, and --tree models the
	// epic hierarchy itself (scope it with --project, never --epic).
	if *timeline != "" && (*tree || *project != 0 || *epic != 0 || *status != "") {
		return errors.New("tasks --timeline <id> addresses one task; it cannot be combined with --tree/--project/--epic/--status")
	}
	if *tree && *epic != 0 {
		return errors.New("tasks --tree shows the epic hierarchy; scope it with --project, not --epic")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()

	if *timeline != "" {
		return runTimeline(ctx, m.Store, *timeline, os.Stdout)
	}
	if *tree {
		return runTaskTree(ctx, m.Store, *project, statuses, os.Stdout)
	}
	tasks, err := m.Store.ListTasks(ctx, db.TaskFilter{Status: statuses, ProjectID: *project, EpicID: *epic})
	if err != nil {
		return err
	}
	renderTasks(os.Stdout, tasks)
	return nil
}

// validTaskStatus is the closed set of task statuses (§1.2), used to validate the
// --status filter so a typo fails loudly instead of silently matching nothing.
var validTaskStatus = map[string]bool{
	db.StatusPending: true, db.StatusActive: true, db.StatusNeedsInput: true,
	db.StatusBlocked: true, db.StatusDone: true, db.StatusDelivered: true,
	db.StatusTornDown: true, db.StatusAbandoned: true,
}

const taskStatusList = "pending, active, needs_input (or needs-input), blocked, done, delivered, torn_down, abandoned"

// parseStatusList splits a --status value into a normalized, validated slice for
// TaskFilter.Status (→ status IN (?,…), §1.4). Entries are comma-separated and
// trimmed; the hyphenated CLI spelling needs-input maps to the needs_input enum so
// the filter accepts the same vocabulary the report verb uses. A nil result means
// "no status constraint". An unknown status is a loud error.
func parseStatusList(s string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if v == "needs-input" {
			v = db.StatusNeedsInput
		}
		if !validTaskStatus[v] {
			return nil, fmt.Errorf("tasks --status: unknown status %q (valid: %s)", v, taskStatusList)
		}
		out = append(out, v)
	}
	return out, nil
}

// renderTasks prints a flat task table. The lifecycle columns (status/stage/owner)
// are DB-backed; the title and declared footprint follow on indented continuation
// lines (mirroring `ttorch status`). Pending backlog tasks are included.
func renderTasks(w io.Writer, tasks []db.Task) {
	if len(tasks) == 0 {
		fmt.Fprintln(w, "no tasks match. add backlog with: ttorch task add <id> --project <id>")
		return
	}
	const format = "%-16s %-6s %-12s %-14s %-16s %s\n"
	fmt.Fprintf(w, format, "ID", "KIND", "STATUS", "STAGE", "OWNER", "PROJECT")
	for _, t := range tasks {
		fmt.Fprintf(w, format, t.ID, t.Kind, t.Status, dash(t.Stage), dash(t.Owner), t.Project)
		if t.Title != "" {
			fmt.Fprintf(w, "%-16s title: %s\n", "", t.Title)
		}
		if len(t.Footprint) > 0 {
			fmt.Fprintf(w, "%-16s touches: %s\n", "", strings.Join(t.Footprint, ", "))
		}
	}
	fmt.Fprintf(w, "%d task(s)\n", len(tasks))
}

// --- ttorch tasks --timeline -------------------------------------------------

func runTimeline(ctx context.Context, store *db.Store, taskID string, w io.Writer) error {
	if _, ok, err := store.GetTask(ctx, taskID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("tasks --timeline: unknown task %q", taskID)
	}
	items, err := store.Timeline(ctx, taskID)
	if err != nil {
		return err
	}
	renderTimeline(w, taskID, items)
	return nil
}

// renderTimeline prints a task's merged events∪notes in the order Timeline returns
// them (by ts; events before notes on a tie, §2.2).
func renderTimeline(w io.Writer, taskID string, items []db.TimelineItem) {
	if len(items) == 0 {
		fmt.Fprintf(w, "timeline for %s — no events or notes yet\n", taskID)
		return
	}
	fmt.Fprintf(w, "timeline for %s — %d item(s):\n", taskID, len(items))
	for _, it := range items {
		ts := it.TS.UTC().Format(time.RFC3339)
		switch it.Kind {
		case "event":
			e := it.Event
			line := fmt.Sprintf("  %s  event  %-16s by %s", ts, e.Type, e.Actor)
			if e.FromStatus != nil || e.ToStatus != nil {
				line += fmt.Sprintf(" (%s→%s)", dash(deref(e.FromStatus)), dash(deref(e.ToStatus)))
			}
			fmt.Fprintln(w, line)
			if e.Payload != "" {
				fmt.Fprintf(w, "          %s\n", e.Payload)
			}
		case "note":
			n := it.Note
			fmt.Fprintf(w, "  %s  note   by %s\n", ts, n.Author)
			fmt.Fprintf(w, "          %s\n", n.Body)
		}
	}
}

// --- ttorch tasks --tree -----------------------------------------------------

// projTree/epicTree/phaseTree assemble the projects→epics→phases→tasks hierarchy.
// A task attaches at the deepest level its ids resolve to: under its phase, else
// (epic set, no phase) loose under its epic, else loose under its project.
type projTree struct {
	p     db.Project
	epics []*epicTree
	loose []db.Task
}
type epicTree struct {
	e      db.Epic
	phases []*phaseTree
	loose  []db.Task
}
type phaseTree struct {
	ph    db.Phase
	tasks []db.Task
}

func runTaskTree(ctx context.Context, store *db.Store, projectID int64, statuses []string, w io.Writer) error {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return err
	}
	epics, err := store.ListEpics(ctx, 0)
	if err != nil {
		return err
	}
	phases, err := store.ListPhases(ctx, 0)
	if err != nil {
		return err
	}
	tasks, err := store.ListTasks(ctx, db.TaskFilter{ProjectID: projectID, Status: statuses})
	if err != nil {
		return err
	}
	renderTaskTree(w, buildTree(projects, epics, phases, tasks, projectID))
	return nil
}

// buildTree groups the ordered input slices into the hierarchy. It preserves the
// input order at every level (the store already returns projects/epics/phases in a
// deterministic order, and tasks oldest-first), so the result is deterministic. A
// non-zero projectID restricts the tree to that one project.
func buildTree(projects []db.Project, epics []db.Epic, phases []db.Phase, tasks []db.Task, projectID int64) []*projTree {
	// Index the phase→epic and epic→project links so a task's deepest valid anchor
	// can be found, and tasks bucketed by where they hang.
	phaseByID := map[int64]*phaseTree{}
	epicByID := map[int64]*epicTree{}
	projByID := map[int64]*projTree{}

	var roots []*projTree
	for _, p := range projects {
		if projectID != 0 && p.ID != projectID {
			continue
		}
		node := &projTree{p: p}
		projByID[p.ID] = node
		roots = append(roots, node)
	}
	for i := range epics {
		e := epics[i]
		parent, ok := projByID[e.ProjectID]
		if !ok {
			continue
		}
		node := &epicTree{e: e}
		epicByID[e.ID] = node
		parent.epics = append(parent.epics, node)
	}
	for i := range phases {
		ph := phases[i]
		parent, ok := epicByID[ph.EpicID]
		if !ok {
			continue
		}
		node := &phaseTree{ph: ph}
		phaseByID[ph.ID] = node
		parent.phases = append(parent.phases, node)
	}
	for _, t := range tasks {
		// Anchor a task only under an epic/phase that belongs to its OWN project, so
		// a malformed cross-project ref can never migrate a task under the wrong
		// project — and the placement is identical whether or not the tree is
		// project-filtered. Otherwise the task falls back to its project's bucket.
		placed := false
		if t.PhaseID != nil {
			if pn := phaseByID[*t.PhaseID]; pn != nil {
				if en := epicByID[pn.ph.EpicID]; en != nil && en.e.ProjectID == t.ProjectID {
					pn.tasks = append(pn.tasks, t)
					placed = true
				}
			}
		}
		if !placed && t.EpicID != nil {
			if en := epicByID[*t.EpicID]; en != nil && en.e.ProjectID == t.ProjectID {
				en.loose = append(en.loose, t)
				placed = true
			}
		}
		if !placed {
			if pn := projByID[t.ProjectID]; pn != nil {
				pn.loose = append(pn.loose, t)
			}
		}
	}
	return roots
}

// renderTaskTree prints the assembled hierarchy with indentation.
func renderTaskTree(w io.Writer, roots []*projTree) {
	if len(roots) == 0 {
		fmt.Fprintln(w, "no projects yet. add one with: ttorch project add <repo>")
		return
	}
	for _, pt := range roots {
		name := projectDisplayName(pt.p)
		fmt.Fprintf(w, "project %d · %s · mode=%s · %s\n", pt.p.ID, name, pt.p.DeliveryMode, pt.p.Status)
		for _, et := range pt.epics {
			fmt.Fprintf(w, "  epic %d · %s · %s\n", et.e.ID, et.e.Title, et.e.Status)
			for _, pht := range et.phases {
				fmt.Fprintf(w, "    phase %d · %s · %s\n", pht.ph.ID, pht.ph.Title, pht.ph.Status)
				for _, t := range pht.tasks {
					renderTreeTask(w, "      ", t)
				}
			}
			for _, t := range et.loose {
				renderTreeTask(w, "    ", t)
			}
		}
		for _, t := range pt.loose {
			renderTreeTask(w, "  ", t)
		}
	}
}

func renderTreeTask(w io.Writer, indent string, t db.Task) {
	line := fmt.Sprintf("%stask %s · %s · %s", indent, t.ID, t.Status, t.Kind)
	if t.Title != "" {
		line += " · " + t.Title
	}
	fmt.Fprintln(w, line)
}

// --- ttorch project ----------------------------------------------------------

const projectUsage = `usage: ttorch project add <repo> [--name n] | ttorch project ls`

func cmdProject(args []string) error {
	if len(args) < 1 {
		return errors.New(projectUsage)
	}
	switch args[0] {
	case "add":
		return cmdProjectAdd(args[1:])
	case "ls", "list":
		return cmdProjectLs(args[1:])
	default:
		return errors.New(projectUsage)
	}
}

func cmdProjectAdd(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ttorch project add <repo> [--name n]")
	}
	repoArg := args[0]
	fs := flag.NewFlagSet("project add", flag.ContinueOnError)
	name := fs.String("name", "", "display name (defaults to the repo's basename)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Projects are keyed by repo ROOT (= state.Task.Project), so resolve the arg the
	// same way spawn does — registering "." and "/abs/path" must not create two rows.
	repoRoot, err := worktree.RepoRoot(repoArg)
	if err != nil {
		return fmt.Errorf("project add: %q is not inside a git repository (projects are keyed by repo root): %w", repoArg, err)
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	// Pass the name through verbatim: an empty name leaves an existing name untouched
	// (UpsertProject keeps it) rather than clobbering it with the basename.
	proj, err := m.Store.UpsertProject(ctx, repoRoot, *name)
	if err != nil {
		return err
	}
	// Populate the delivery-mode DISPLAY cache from AGENTS.md so a trusted repo does
	// not mis-display as the 'pr' default. The merge/land gates still read AGENTS.md
	// for the authoritative mode — this column is never consulted by a gate (§0.3).
	mode := projectinit.ReadMode(repoRoot)
	if err := m.Store.SetProjectMode(ctx, proj.ID, mode); err != nil {
		return err
	}
	fmt.Printf("project %d: %s (%s) · mode=%s\n", proj.ID, proj.RepoPath, projectDisplayName(proj), mode)
	return nil
}

func cmdProjectLs(args []string) error {
	if err := noFlags("project ls", args); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	projects, err := m.Store.ListProjects(context.Background())
	if err != nil {
		return err
	}
	renderProjects(os.Stdout, projects)
	return nil
}

// projectDisplayName falls back to the repo basename when no name is cached.
func projectDisplayName(p db.Project) string {
	if p.Name != "" {
		return p.Name
	}
	return filepath.Base(p.RepoPath)
}

func renderProjects(w io.Writer, projects []db.Project) {
	if len(projects) == 0 {
		fmt.Fprintln(w, "no projects yet. add one with: ttorch project add <repo>")
		return
	}
	const format = "%-4s %-16s %-10s %-9s %s\n"
	fmt.Fprintf(w, format, "ID", "NAME", "MODE", "STATUS", "REPO")
	for _, p := range projects {
		fmt.Fprintf(w, format, fmtID(p.ID), projectDisplayName(p), p.DeliveryMode, p.Status, p.RepoPath)
	}
}

// --- ttorch epic -------------------------------------------------------------

const epicUsage = `usage: ttorch epic add --project <id> --title "…" | ttorch epic ls [--project <id>] | ttorch epic set-status <id> <status>`

func cmdEpic(args []string) error {
	if len(args) < 1 {
		return errors.New(epicUsage)
	}
	switch args[0] {
	case "add":
		return cmdEpicAdd(args[1:])
	case "ls", "list":
		return cmdEpicLs(args[1:])
	case "set-status":
		return cmdEpicSetStatus(args[1:])
	default:
		return errors.New(epicUsage)
	}
}

func cmdEpicAdd(args []string) error {
	fs := flag.NewFlagSet("epic add", flag.ContinueOnError)
	project := fs.Int64("project", 0, "project id this epic belongs to (required)")
	title := fs.String("title", "", "one-line epic title (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == 0 {
		return errors.New("epic add: --project <id> is required")
	}
	if strings.TrimSpace(*title) == "" {
		return errors.New("epic add: --title is required")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, ok, err := m.Store.GetProject(ctx, *project); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("epic add: no such project %d (see 'ttorch project ls')", *project)
	}
	e, err := m.Store.CreateEpic(ctx, *project, strings.TrimSpace(*title), "")
	if err != nil {
		return err
	}
	fmt.Printf("epic %d created under project %d: %s\n", e.ID, e.ProjectID, e.Title)
	return nil
}

func cmdEpicLs(args []string) error {
	fs := flag.NewFlagSet("epic ls", flag.ContinueOnError)
	project := fs.Int64("project", 0, "filter to a project id (0 = all projects)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	epics, err := m.Store.ListEpics(context.Background(), *project)
	if err != nil {
		return err
	}
	renderEpics(os.Stdout, epics)
	return nil
}

func renderEpics(w io.Writer, epics []db.Epic) {
	if len(epics) == 0 {
		fmt.Fprintln(w, `no epics yet. add one with: ttorch epic add --project <id> --title "…"`)
		return
	}
	const format = "%-4s %-8s %-12s %s\n"
	fmt.Fprintf(w, format, "ID", "PROJECT", "STATUS", "TITLE")
	for _, e := range epics {
		fmt.Fprintf(w, format, fmtID(e.ID), fmtID(e.ProjectID), e.Status, e.Title)
	}
}

func cmdEpicSetStatus(args []string) error {
	id, status, err := parseSetStatusArgs("epic", args)
	if err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, ok, err := m.Store.GetEpic(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("epic set-status: no such epic %d", id)
	}
	if err := m.Store.SetEntityStatus(ctx, db.EntityEpic, id, status, db.ActorManager); err != nil {
		return err
	}
	fmt.Printf("epic %d → %s\n", id, status)
	return nil
}

// --- ttorch phase ------------------------------------------------------------

const phaseUsage = `usage: ttorch phase add --epic <id> --title "…" | ttorch phase ls [--epic <id>] | ttorch phase set-status <id> <status>`

func cmdPhase(args []string) error {
	if len(args) < 1 {
		return errors.New(phaseUsage)
	}
	switch args[0] {
	case "add":
		return cmdPhaseAdd(args[1:])
	case "ls", "list":
		return cmdPhaseLs(args[1:])
	case "set-status":
		return cmdPhaseSetStatus(args[1:])
	default:
		return errors.New(phaseUsage)
	}
}

func cmdPhaseAdd(args []string) error {
	fs := flag.NewFlagSet("phase add", flag.ContinueOnError)
	epic := fs.Int64("epic", 0, "epic id this phase belongs to (required)")
	title := fs.String("title", "", "one-line phase title (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *epic == 0 {
		return errors.New("phase add: --epic <id> is required")
	}
	if strings.TrimSpace(*title) == "" {
		return errors.New("phase add: --title is required")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, ok, err := m.Store.GetEpic(ctx, *epic); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("phase add: no such epic %d (see 'ttorch epic ls')", *epic)
	}
	ph, err := m.Store.CreatePhase(ctx, *epic, strings.TrimSpace(*title), "")
	if err != nil {
		return err
	}
	fmt.Printf("phase %d created under epic %d: %s\n", ph.ID, ph.EpicID, ph.Title)
	return nil
}

func cmdPhaseLs(args []string) error {
	fs := flag.NewFlagSet("phase ls", flag.ContinueOnError)
	epic := fs.Int64("epic", 0, "filter to an epic id (0 = all epics)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	phases, err := m.Store.ListPhases(context.Background(), *epic)
	if err != nil {
		return err
	}
	renderPhases(os.Stdout, phases)
	return nil
}

func renderPhases(w io.Writer, phases []db.Phase) {
	if len(phases) == 0 {
		fmt.Fprintln(w, `no phases yet. add one with: ttorch phase add --epic <id> --title "…"`)
		return
	}
	const format = "%-4s %-6s %-12s %s\n"
	fmt.Fprintf(w, format, "ID", "EPIC", "STATUS", "TITLE")
	for _, p := range phases {
		fmt.Fprintf(w, format, fmtID(p.ID), fmtID(p.EpicID), p.Status, p.Title)
	}
}

func cmdPhaseSetStatus(args []string) error {
	id, status, err := parseSetStatusArgs("phase", args)
	if err != nil {
		return err
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, ok, err := m.Store.GetPhase(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("phase set-status: no such phase %d", id)
	}
	if err := m.Store.SetEntityStatus(ctx, db.EntityPhase, id, status, db.ActorManager); err != nil {
		return err
	}
	fmt.Printf("phase %d → %s\n", id, status)
	return nil
}

// hierarchyStatus is the closed set of epic/phase statuses (§1.2). set-status
// validates against it before touching the DB (the CHECK constraint is the backstop).
var hierarchyStatus = map[string]bool{
	"planned": true, "in_progress": true, "blocked": true, "done": true, "cancelled": true,
}

const hierarchyStatusList = "planned, in_progress, blocked, done, cancelled"

// parseSetStatusArgs parses the shared `<kind> set-status <id> <status>` form.
func parseSetStatusArgs(kind string, args []string) (int64, string, error) {
	if len(args) != 2 {
		return 0, "", fmt.Errorf("usage: ttorch %s set-status <id> <status>  (status: %s)", kind, hierarchyStatusList)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%s set-status: id must be a number, got %q", kind, args[0])
	}
	status := args[1]
	if !hierarchyStatus[status] {
		return 0, "", fmt.Errorf("%s set-status: unknown status %q (valid: %s)", kind, status, hierarchyStatusList)
	}
	return id, status, nil
}

// --- ttorch task add ---------------------------------------------------------

const taskAddUsage = `usage: ttorch task add <id> --project <id> [--epic <id>] [--phase <id>] [--title "…"] [--touches "a,b"]`

func cmdTask(args []string) error {
	if len(args) < 1 || args[0] != "add" {
		return errors.New(taskAddUsage)
	}
	return cmdTaskAdd(args[1:])
}

// cmdTaskAdd creates a PENDING backlog task without spawning a worker (§3.3). It
// sets no window/worktree — spawn later UpsertTasks this row to active (§3.4).
func cmdTaskAdd(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(taskAddUsage)
	}
	id := args[0]
	fs := flag.NewFlagSet("task add", flag.ContinueOnError)
	project := fs.Int64("project", 0, "project id (required)")
	epic := fs.Int64("epic", 0, "optional epic id")
	phase := fs.Int64("phase", 0, "optional phase id")
	title := fs.String("title", "", "one-line task title")
	touches := fs.String("touches", "", "comma-separated files/prefixes the task will touch")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *project == 0 {
		return errors.New("task add: --project <id> is required")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, ok, err := m.Store.GetProject(ctx, *project); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("task add: no such project %d (see 'ttorch project ls')", *project)
	}
	// Resolve and cross-validate the hierarchy refs so the row is coherent: an
	// --epic must live under --project, and a --phase must live under that epic
	// (a phase always has a parent epic). When only --phase is given we adopt its
	// parent epic, so a task can never end up with phase_id set and epic_id NULL —
	// which would otherwise render under an epic in --tree yet be invisible to
	// `tasks --epic` (the two read surfaces must agree).
	var epicID, phaseID *int64
	if *epic != 0 {
		e, ok, err := m.Store.GetEpic(ctx, *epic)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("task add: no such epic %d (see 'ttorch epic ls')", *epic)
		}
		if e.ProjectID != *project {
			return fmt.Errorf("task add: epic %d belongs to project %d, not %d", *epic, e.ProjectID, *project)
		}
		ev := *epic
		epicID = &ev
	}
	if *phase != 0 {
		ph, ok, err := m.Store.GetPhase(ctx, *phase)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("task add: no such phase %d (see 'ttorch phase ls')", *phase)
		}
		if epicID != nil && *epicID != ph.EpicID {
			return fmt.Errorf("task add: phase %d belongs to epic %d, not the given epic %d", *phase, ph.EpicID, *epicID)
		}
		if epicID == nil {
			// Adopt the phase's parent epic and verify it belongs to --project.
			pe, ok, err := m.Store.GetEpic(ctx, ph.EpicID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("task add: phase %d references missing epic %d", *phase, ph.EpicID)
			}
			if pe.ProjectID != *project {
				return fmt.Errorf("task add: phase %d belongs to project %d, not %d", *phase, pe.ProjectID, *project)
			}
			ev := ph.EpicID
			epicID = &ev
		}
		pv := *phase
		phaseID = &pv
	}
	if _, exists, err := m.Store.GetTask(ctx, id); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("task add: task %q already exists", id)
	}
	t, err := m.Store.CreateTask(ctx, db.Task{
		ID: id, ProjectID: *project, EpicID: epicID, PhaseID: phaseID,
		CreatedBy: db.ActorManager, Title: strings.TrimSpace(*title),
		Kind: db.KindShip, Status: db.StatusPending, Footprint: parseTouches(*touches),
	}, db.ActorManager)
	if err != nil {
		return err
	}
	fmt.Printf("added backlog task %s (project %d, status %s) — spawn it with: ttorch spawn %s <repo>\n", t.ID, t.ProjectID, t.Status, t.ID)
	return nil
}

// noFlags rejects stray arguments for a verb that takes none, so a typo'd flag is a
// loud error instead of being silently ignored.
func noFlags(verb string, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("ttorch %s takes no arguments; got %v", verb, args)
	}
	return nil
}

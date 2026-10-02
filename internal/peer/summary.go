// Package peer holds what one coordinator reports about itself to another. Today that is the
// summary: a versioned snapshot of the coordinator's fleet, built from the reads `ttorch
// board`, `ttorch status` and `ttorch scheduler status` already make, so a script or another
// coordinator gets the same state as data instead of screen text.
//
// The summary carries counts, ids and ages, and no free text: no worker's question, no gate
// reason or finding, no approval reason, no scheduler error message, no escalation body.
// Escalation text is listed by `ttorch decisions`, which escapes and caps it (decisions.go).
package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nution101/ttorch/internal/board"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/scheduler"
)

// SchemaVersion is the summary's schema. A consumer refuses a version it does not know rather
// than guess at fields; any change to a field's name, type or meaning moves it. Version 2 added
// escalations.
const SchemaVersion = 2

// Summary is one coordinator's state at GeneratedAt. Every age is whole seconds before
// GeneratedAt, floored at zero, and null when the thing it measures has never happened. The
// strings it does carry (task ids, repo paths, statuses, modes) come from the DB or a repo and
// are escaped (see escape), so the summary is safe to print to a terminal in either rendering.
type Summary struct {
	SchemaVersion int          `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Tasks         TaskCounts   `json:"tasks"`
	Workers       WorkerCounts `json:"workers"`
	Decisions     Decisions    `json:"decisions"`
	Escalations   Escalations  `json:"escalations"`
	Scheduler     Scheduler    `json:"scheduler"`
	Manager       Manager      `json:"manager"`
	Repos         []Repo       `json:"repos"`
}

// TaskCounts counts fleet tasks by status. Every known status is present, at zero when no
// task has it. Ad-hoc lead sessions (kind cc) are not fleet work and are not counted.
type TaskCounts struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
}

// WorkerCounts counts the board's live workers by the state `ttorch status` derives from
// each one's window: working, idle, gone, and the rarer states the fleet reports.
type WorkerCounts struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"by_state"`
}

// Decisions is what waits on the lead, from the board: escalated gates, workers asking a
// question, and tasks awaiting approval. Each item names its task and the event that raised
// it, never the text that came with it. NewestEventID is the highest event id among the items,
// 0 when none has one.
type Decisions struct {
	Pending       int        `json:"pending"`
	NewestEventID int64      `json:"newest_event_id"`
	Items         []Decision `json:"items"`
}

// Escalations counts the open escalations (db.Escalation) and gives the highest open id, 0 when
// none is open. A parent compares HighestOpenID with the last id it has seen to know whether
// `decisions` has anything new.
type Escalations struct {
	Open          int   `json:"open"`
	HighestOpenID int64 `json:"highest_open_id"`
}

// The kinds of decision.
const (
	KindGate       = "gate"        // a gate the daemon escalated for the lead to adjudicate
	KindNeedsInput = "needs_input" // a worker waiting on an answer
	KindBlocked    = "blocked"     // a worker that reported itself blocked
	KindApproval   = "approval"    // a done task that needs the lead's own approval
)

// Decision is one item waiting on the lead. EventID is the event that raised it: the
// gate_blocked event for a gate, the status event for a question. It is 0, with a null age,
// when there is none, which is always the case for an approval.
type Decision struct {
	Kind       string `json:"kind"`
	TaskID     string `json:"task_id"`
	Project    string `json:"project"`
	EventID    int64  `json:"event_id"`
	AgeSeconds *int64 `json:"age_seconds"`
}

// Scheduler is the daemon's health: whether its process holds the singleton lock, and what
// its scheduler_status row says about its ticks. Stalled is `ttorch scheduler status`'s
// verdict: no tick ever, or the last one older than the threshold. The last error's message
// is not carried, only its age; `ttorch scheduler status` shows it.
type Scheduler struct {
	Running             bool   `json:"daemon_running"`
	Ticked              bool   `json:"has_ticked"`
	LastTickAgeSeconds  *int64 `json:"last_tick_age_seconds"`
	Stalled             bool   `json:"stalled"`
	Ticks               int64  `json:"tick_count"`
	Errors              int64  `json:"errors"`
	LastErrorAgeSeconds *int64 `json:"last_error_age_seconds"`
}

// Manager is the manager session's liveness. LastActionAgeSeconds is the age of the manager
// row's last write, the same "acted recently" signal the watchdog reads.
type Manager struct {
	Registered           bool   `json:"registered"`
	WindowPresent        bool   `json:"window_present"`
	AwaitingLead         bool   `json:"awaiting_lead"`
	LastActionAgeSeconds *int64 `json:"last_action_age_seconds"`
}

// Repo is one registered, unarchived project: its delivery mode and how many more workers
// its pool could take now.
type Repo struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	FreeSlots int    `json:"free_slots"`
}

// Snapshotter reads the board's snapshot; *board.Server satisfies it.
type Snapshotter interface {
	Snapshot(ctx context.Context) (board.Snapshot, error)
}

// Sources are the reads a summary is built from. Store and Board are required; a nil
// function reads as its zero answer (pr mode, no free slots, no window, no daemon).
type Sources struct {
	Store *db.Store
	Board Snapshotter
	// Mode resolves a repo's delivery mode (projectinit.ReadMode in production).
	Mode func(repo string) string
	// FreeSlots is how many more workers a repo's pool could take, as `ttorch status`
	// counts it.
	FreeSlots func(repo string) int
	// ManagerWindow reports whether the manager's window exists.
	ManagerWindow func() bool
	// SchedulerRunning reports whether a daemon holds the scheduler's singleton lock.
	SchedulerRunning func() bool
	// StaleAfter is the tick age past which the daemon counts as stalled. Zero means
	// scheduler.StaleAfterDefault.
	StaleAfter time.Duration
}

// statuses is every task status, in lifecycle order.
var statuses = []string{
	db.StatusPending, db.StatusActive, db.StatusNeedsInput, db.StatusBlocked, db.StatusDone,
	db.StatusDelivered, db.StatusTornDown, db.StatusAbandoned, db.StatusFailed,
}

// Build reads the summary as of now. Build itself only reads through src; whatever opened
// the store may have written to it first (orchestrator.New migrates, imports legacy state and
// seeds default branches).
func Build(ctx context.Context, src Sources, now time.Time) (Summary, error) {
	if src.Store == nil || src.Board == nil {
		return Summary{}, fmt.Errorf("peer: a summary needs a store and a board")
	}
	sum := Summary{SchemaVersion: SchemaVersion, GeneratedAt: now.UTC()}

	tasks, err := src.Store.ListTasks(ctx, db.TaskFilter{ExcludeKind: []string{db.KindCC}})
	if err != nil {
		return Summary{}, fmt.Errorf("peer: listing tasks: %w", err)
	}
	sum.Tasks = TaskCounts{Total: len(tasks), ByStatus: map[string]int{}}
	for _, s := range statuses {
		sum.Tasks.ByStatus[s] = 0
	}
	for _, t := range tasks {
		sum.Tasks.ByStatus[escape(t.Status)]++
	}

	snap, err := src.Board.Snapshot(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("peer: reading the board: %w", err)
	}
	sum.Workers = workers(snap)
	sum.Decisions = decisions(snap, now)
	if sum.Escalations.Open, sum.Escalations.HighestOpenID, err = src.Store.EscalationOpenCounts(ctx); err != nil {
		return Summary{}, fmt.Errorf("peer: counting escalations: %w", err)
	}

	if sum.Scheduler, err = schedulerHealth(ctx, src, now); err != nil {
		return Summary{}, err
	}
	if sum.Manager, err = manager(ctx, src, now); err != nil {
		return Summary{}, err
	}
	if sum.Repos, err = repos(ctx, src); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

func workers(snap board.Snapshot) WorkerCounts {
	w := WorkerCounts{Total: len(snap.Workers), ByState: map[string]int{}}
	for _, wk := range snap.Workers {
		w.ByState[escape(wk.State)]++
	}
	return w
}

func decisions(snap board.Snapshot, now time.Time) Decisions {
	d := Decisions{Pending: snap.Pending(), Items: []Decision{}}
	add := func(kind, task, project string, eventID int64, at *time.Time) {
		it := Decision{Kind: kind, TaskID: escape(task), Project: escape(project), EventID: eventID}
		if at != nil {
			it.AgeSeconds = age(now, *at)
		}
		d.Items = append(d.Items, it)
		d.NewestEventID = max(d.NewestEventID, eventID)
	}
	for _, g := range snap.Gates {
		add(KindGate, g.TaskID, g.Project, g.EventID, &g.At)
	}
	for _, q := range snap.Questions {
		kind := KindNeedsInput
		if q.Status == db.StatusBlocked {
			kind = KindBlocked
		}
		var at *time.Time
		if q.EventID != 0 {
			at = &q.At
		}
		add(kind, q.TaskID, q.Project, q.EventID, at)
	}
	for _, a := range snap.Approvals {
		add(KindApproval, a.TaskID, a.Project, 0, nil)
	}
	return d
}

func schedulerHealth(ctx context.Context, src Sources, now time.Time) (Scheduler, error) {
	row, has, err := src.Store.GetSchedulerStatus(ctx)
	if err != nil {
		return Scheduler{}, fmt.Errorf("peer: reading scheduler status: %w", err)
	}
	stale := src.StaleAfter
	if stale <= 0 {
		stale = scheduler.StaleAfterDefault
	}
	view := scheduler.StatusView{Row: row, HasRow: has, Now: now, StaleAfter: stale}
	s := Scheduler{Running: call(src.SchedulerRunning), Ticked: has, Stalled: view.Stalled()}
	if !has {
		return s, nil
	}
	s.LastTickAgeSeconds = age(now, row.LastTickAt)
	s.Ticks, s.Errors = row.TickCount, row.Errors
	s.LastErrorAgeSeconds = age(now, row.LastErrorAt)
	return s, nil
}

func manager(ctx context.Context, src Sources, now time.Time) (Manager, error) {
	m, ok, err := src.Store.GetManager(ctx)
	if err != nil {
		return Manager{}, fmt.Errorf("peer: reading the manager row: %w", err)
	}
	out := Manager{Registered: ok, WindowPresent: call(src.ManagerWindow)}
	if ok {
		out.AwaitingLead = m.AwaitingLead
		out.LastActionAgeSeconds = age(now, m.UpdatedAt)
	}
	return out, nil
}

func repos(ctx context.Context, src Sources) ([]Repo, error) {
	projects, err := src.Store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("peer: listing projects: %w", err)
	}
	out := []Repo{}
	for _, p := range projects {
		if p.Status == "archived" {
			continue
		}
		r := Repo{Path: escape(p.RepoPath), Name: escape(filepath.Base(p.RepoPath)), Mode: "pr"}
		if src.Mode != nil {
			r.Mode = escape(src.Mode(p.RepoPath))
		}
		if src.FreeSlots != nil {
			r.FreeSlots = src.FreeSlots(p.RepoPath)
		}
		out = append(out, r)
	}
	return out, nil
}

func call(f func() bool) bool { return f != nil && f() }

// age is the whole seconds from t to now, floored at zero so a clock behind a stored stamp
// never reads as negative. A zero t has never happened and has no age.
func age(now, t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	s := int64(now.Sub(t) / time.Second)
	if s < 0 {
		s = 0
	}
	return &s
}

// escape renders DB text as one line of printable runes. Every rune that is not graphic is
// written out the way a Go string literal writes it: \n, \r and \t; \xNN below U+0080 (C0 and
// DEL); \uNNNN or \UNNNNNNNN above (C1, bidi overrides and other format characters, line and
// paragraph separators, private use). Invalid UTF-8 becomes U+FFFD. This escapes the C0, C1
// and DEL runes sanitizeAuditLine does in internal/orchestrator, and also the invisible runes
// that are not control characters but still move or hide text on a terminal.
//
// A backslash is doubled, so a backslash in the output always starts an escape that this
// function wrote: text typed as `\x1b` comes out as `\\x1b` and cannot pass for an escaped
// ESC. The result is for reading; a consumer must not unescape it and print the result. The
// JSON encoder then quotes it, and since nothing non-graphic is left, decoding the JSON gives
// back text that is just as safe to print.
func escape(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexFunc(s, needsEscape) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case !needsEscape(r):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < utf8.RuneSelf:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xFFFF:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

// needsEscape is every rune escape writes out: a backslash, and anything that is not a letter,
// mark, number, punctuation, symbol or space separator.
func needsEscape(r rune) bool { return r == '\\' || !unicode.IsGraphic(r) }

// WriteJSON writes the summary as one indented JSON object and a newline.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText writes the summary for a person at a terminal.
func (s Summary) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "coordinator summary (schema %d) at %s\n", s.SchemaVersion, s.GeneratedAt.Format(time.RFC3339))

	fmt.Fprintf(&b, "tasks:      %d total", s.Tasks.Total)
	for _, st := range statuses {
		if n := s.Tasks.ByStatus[st]; n > 0 {
			fmt.Fprintf(&b, ", %d %s", n, st)
		}
	}
	for _, st := range sortedKeys(s.Tasks.ByStatus) {
		if !known(st) && s.Tasks.ByStatus[st] > 0 {
			fmt.Fprintf(&b, ", %d %s", s.Tasks.ByStatus[st], st)
		}
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "workers:    %d live", s.Workers.Total)
	for _, st := range sortedKeys(s.Workers.ByState) {
		fmt.Fprintf(&b, ", %d %s", s.Workers.ByState[st], st)
	}
	b.WriteString("\n")

	sc := s.Scheduler
	switch {
	case !sc.Ticked:
		fmt.Fprintf(&b, "scheduler:  %s, no tick recorded\n", running(sc.Running))
	default:
		fmt.Fprintf(&b, "scheduler:  %s, last tick %s ago, %d ticks, %d errors", running(sc.Running), secs(sc.LastTickAgeSeconds), sc.Ticks, sc.Errors)
		if sc.LastErrorAgeSeconds != nil {
			fmt.Fprintf(&b, ", last error %s ago (ttorch scheduler status shows it)", secs(sc.LastErrorAgeSeconds))
		}
		b.WriteString("\n")
	}
	if sc.Stalled {
		b.WriteString("            STALLED\n")
	}

	m := s.Manager
	if !m.Registered {
		b.WriteString("manager:    no manager session recorded\n")
	} else {
		window := "window present"
		if !m.WindowPresent {
			window = "no window"
		}
		fmt.Fprintf(&b, "manager:    %s, last action %s ago", window, secs(m.LastActionAgeSeconds))
		if m.AwaitingLead {
			b.WriteString(", awaiting the lead")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "repos:      %d\n", len(s.Repos))
	for _, r := range s.Repos {
		fmt.Fprintf(&b, "  %s  %s mode, %d free slots\n", r.Path, r.Mode, r.FreeSlots)
	}

	if e := s.Escalations; e.Open == 0 {
		b.WriteString("escalations: none open\n")
	} else {
		fmt.Fprintf(&b, "escalations: %d open, highest open #%d (ttorch decisions lists them)\n", e.Open, e.HighestOpenID)
	}

	d := s.Decisions
	fmt.Fprintf(&b, "decisions:  %d pending", d.Pending)
	if d.Pending > 0 {
		b.WriteString(" (their text: ttorch board, or ttorch tasks --timeline <task>)")
	}
	b.WriteString("\n")
	for _, it := range d.Items {
		fmt.Fprintf(&b, "  %-11s  %s (%s)", it.Kind, it.TaskID, it.Project)
		if it.EventID != 0 {
			fmt.Fprintf(&b, ", event %d, %s ago", it.EventID, secs(it.AgeSeconds))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func known(status string) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func running(ok bool) string {
	if ok {
		return "running"
	}
	return "not running"
}

func secs(s *int64) string {
	if s == nil {
		return "?"
	}
	return (time.Duration(*s) * time.Second).String()
}

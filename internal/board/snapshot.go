package board

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
)

// Event types the board appends as its own idempotency markers. They are lead-authored and
// non-actionable, so they never wake the watcher; they exist so a second click on a decision
// the board already acted on finds the first one and does nothing, even across a restart.
const (
	// eventBoardAnswer records that the lead answered one worker question from the board;
	// payload "question=<event id>".
	eventBoardAnswer = "board_answer"
	// eventBoardGatePrep records that the lead re-ran gate prep for one gate_blocked event;
	// payload "event=<event id>".
	eventBoardGatePrep = "board_gate_prep"
)

// completionEvents are the event types that mark a task's work as delivered.
var completionEvents = []string{db.EventMerged, db.EventDelivered, db.EventPRMerged}

// completionLimit caps the recent-completions list.
const completionLimit = 10

// Snapshot is everything the page shows, read in one pass from the DB and the live fleet.
type Snapshot struct {
	Generated   time.Time
	Gates       []GateDecision
	Questions   []Question
	Approvals   []Approval
	Workers     []Worker
	Completions []Completion
	Backlog     []BacklogItem
}

// Pending reports how many decisions wait on the lead.
func (s Snapshot) Pending() int { return len(s.Gates) + len(s.Questions) + len(s.Approvals) }

// GateDecision is a gate the daemon escalated: the latest gate_blocked event on a task that is
// still done and has had no verdict recorded since.
type GateDecision struct {
	TaskID   string
	Project  string
	EventID  int64
	At       time.Time
	Head     string   // the short sha the gate ran on
	Reason   string   // the escalation reason, reviewer text quoted
	Findings []string // the reviewers' findings when the reason is a review block
	Reprep   *time.Time
}

// Question is a worker waiting on the lead: a task in needs_input or blocked, with the text
// that came with that transition.
type Question struct {
	TaskID     string
	Project    string
	Status     string
	Actor      string
	EventID    int64 // the status_changed event that asked; 0 when none was recorded
	At         time.Time
	Text       string
	HasWindow  bool
	AnsweredAt *time.Time
}

// Approval is a done task that needs the lead's own approval before it can merge.
type Approval struct {
	TaskID  string
	Project string
	Mode    string
	Reason  string
	Command string
}

// Worker is one spawned, live task.
type Worker struct {
	TaskID       string
	Kind         string
	State        string
	Status       string
	Stage        string
	Owner        string
	Project      string
	Model        string
	Effort       string
	Footprint    []string
	LastProgress *time.Time
}

// Completion is one recently delivered task.
type Completion struct {
	TaskID string
	Type   string
	At     time.Time
	Detail string
}

// BacklogItem is one pending task.
type BacklogItem struct {
	TaskID    string
	Title     string
	Project   string
	Kind      string
	Footprint []string
	HasBrief  bool
	Created   time.Time
}

// liveStatuses are the task statuses the board looks at; terminal rows are history.
var liveStatuses = []string{db.StatusPending, db.StatusActive, db.StatusNeedsInput, db.StatusBlocked, db.StatusDone}

// Snapshot reads the board's state.
func (s *Server) Snapshot(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{Generated: s.now()}
	tasks, err := s.cfg.Store.ListTasks(ctx, db.TaskFilter{Status: liveStatuses})
	if err != nil {
		return snap, err
	}
	for _, t := range tasks {
		if t.Kind == db.KindCC {
			continue // ad-hoc lead sessions are not fleet work
		}
		if t.Status == db.StatusPending {
			snap.Backlog = append(snap.Backlog, BacklogItem{
				TaskID: t.ID, Title: t.Title, Project: project(t.Project), Kind: t.Kind,
				Footprint: t.Footprint, HasBrief: t.HasBrief, Created: t.Created,
			})
			continue
		}
		if t.Window != "" {
			snap.Workers = append(snap.Workers, Worker{
				TaskID: t.ID, Kind: t.Kind, State: s.cfg.Fleet.TaskState(t), Status: t.Status,
				Stage: t.Stage, Owner: t.Owner, Project: project(t.Project), Model: t.Model,
				Effort: t.Effort, Footprint: t.Footprint, LastProgress: t.LastProgressAt,
			})
		}
		switch t.Status {
		case db.StatusNeedsInput, db.StatusBlocked:
			tl, err := s.cfg.Store.Timeline(ctx, t.ID)
			if err != nil {
				return snap, err
			}
			snap.Questions = append(snap.Questions, questionFor(t, events(tl)))
		case db.StatusDone:
			tl, err := s.cfg.Store.Timeline(ctx, t.ID)
			if err != nil {
				return snap, err
			}
			evs := events(tl)
			if g, ok := gateFor(t, evs); ok {
				snap.Gates = append(snap.Gates, g)
			}
			a, ok, err := s.approvalFor(ctx, t, evs)
			if err != nil {
				return snap, err
			}
			if ok {
				snap.Approvals = append(snap.Approvals, a)
			}
		}
	}
	recent, err := s.cfg.Store.RecentTaskEvents(ctx, completionEvents, completionLimit*3)
	if err != nil {
		return snap, err
	}
	seen := map[string]bool{}
	for _, e := range recent {
		if seen[e.EntityID] || len(snap.Completions) >= completionLimit {
			continue
		}
		seen[e.EntityID] = true
		snap.Completions = append(snap.Completions, Completion{TaskID: e.EntityID, Type: e.Type, At: e.TS, Detail: e.Payload})
	}
	sort.SliceStable(snap.Gates, func(i, j int) bool { return snap.Gates[i].EventID < snap.Gates[j].EventID })
	sort.SliceStable(snap.Questions, func(i, j int) bool { return snap.Questions[i].At.Before(snap.Questions[j].At) })
	return snap, nil
}

// events keeps a timeline's events, in order, and drops its notes.
func events(tl []db.TimelineItem) []db.Event {
	var out []db.Event
	for _, it := range tl {
		if it.Event != nil {
			out = append(out, *it.Event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// latestQuestion is the status_changed event that moved the task into its current status.
func latestQuestion(t db.Task, evs []db.Event) (db.Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Type == db.EventStatusChanged && e.ToStatus != nil && *e.ToStatus == t.Status {
			return e, true
		}
	}
	return db.Event{}, false
}

// markerAt returns when the board recorded a marker of type typ for key, if it did.
func markerAt(evs []db.Event, typ, key string) (time.Time, bool) {
	for _, e := range evs {
		if e.Type == typ && e.Payload == key {
			return e.TS, true
		}
	}
	return time.Time{}, false
}

func questionKey(id int64) string { return "question=" + strconv.FormatInt(id, 10) }
func gateKey(id int64) string     { return "event=" + strconv.FormatInt(id, 10) }

func questionFor(t db.Task, evs []db.Event) Question {
	q := Question{TaskID: t.ID, Project: project(t.Project), Status: t.Status, HasWindow: t.Window != ""}
	e, ok := latestQuestion(t, evs)
	if !ok {
		return q
	}
	q.EventID, q.At, q.Text, q.Actor = e.ID, e.TS, e.Payload, e.Actor
	if at, ok := markerAt(evs, eventBoardAnswer, questionKey(e.ID)); ok {
		q.AnsweredAt = &at
	}
	return q
}

// latestGateBlocked is the task's newest gate_blocked event that no verdict has settled: a
// review_recorded event after it means the manager has adjudicated.
func latestGateBlocked(evs []db.Event) (db.Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		switch evs[i].Type {
		case db.EventReviewRecorded:
			return db.Event{}, false
		case db.EventGateBlocked:
			return evs[i], true
		}
	}
	return db.Event{}, false
}

// reviewBlockedPrefix starts the gate_blocked reason the daemon writes when reviewers block.
const reviewBlockedPrefix = "adversarial review blocked: "

func gateFor(t db.Task, evs []db.Event) (GateDecision, bool) {
	e, ok := latestGateBlocked(evs)
	if !ok {
		return GateDecision{}, false
	}
	g := GateDecision{TaskID: t.ID, Project: project(t.Project), EventID: e.ID, At: e.TS, Reason: e.Payload}
	if rest, found := strings.CutPrefix(e.Payload, "sha="); found {
		if sp := strings.IndexByte(rest, ' '); sp > 0 {
			g.Head, g.Reason = rest[:sp], rest[sp+1:]
		}
	}
	if list, found := strings.CutPrefix(g.Reason, reviewBlockedPrefix); found {
		g.Findings = splitFindings(list)
		g.Reason = strings.TrimSuffix(reviewBlockedPrefix, ": ")
	}
	if at, ok := markerAt(evs, eventBoardGatePrep, gateKey(e.ID)); ok {
		g.Reprep = &at
	}
	return g, true
}

// splitFindings splits the "; "-joined findings of a review block. Each finding's reviewer
// text is a quoted Go string (review.Describe), which may itself contain "; ", so a
// separator only counts outside quotes.
func splitFindings(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inQuote && c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == ';' && i+1 < len(s) && s[i+1] == ' ':
			if f := strings.TrimSpace(cur.String()); f != "" {
				out = append(out, f)
			}
			cur.Reset()
			i++ // skip the space
			continue
		}
		cur.WriteByte(c)
	}
	if f := strings.TrimSpace(cur.String()); f != "" {
		out = append(out, f)
	}
	return out
}

// approvalFor decides whether a done task waits on the lead's approval, and why. Approving
// is not something the board can do (see routes): it lists the task with the command the
// lead runs at their own terminal.
func (s *Server) approvalFor(ctx context.Context, t db.Task, evs []db.Event) (Approval, bool, error) {
	mode := "pr"
	if s.cfg.Mode != nil && t.Project != "" {
		mode = s.cfg.Mode(t.Project)
	}
	if mode == "pr" {
		return Approval{}, false, nil // the PR's own review is the gate; ttorch approve plays no part
	}
	approved := s.cfg.ApprovalValid != nil && s.cfg.ApprovalValid(t.ID)
	if approved {
		return Approval{}, false, nil
	}
	a := Approval{TaskID: t.ID, Project: project(t.Project), Mode: mode, Command: "ttorch approve " + t.ID}

	// The gate escalated to a human: an auto-approval lapsed (approval_required), and no
	// approval has been granted since.
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == db.EventApproved {
			break
		}
		if evs[i].Type == eventApprovalRequired {
			a.Reason = evs[i].Payload
			return a, true, nil
		}
	}
	v, ok, err := s.cfg.Store.GetVerdict(ctx, t.ID)
	if err != nil {
		return a, false, err
	}
	switch {
	case ok && v.Overall == review.Pass && v.ApprovedBy == "":
		// The review passed but nothing approved it: trusted mode declined to auto-approve
		// (the diff changes the gate, or no default-branch validate ran green), or the repo
		// is local/validated with --require-verdict. Either way the approval is the lead's.
		a.Reason = fmt.Sprintf("the review passed at %s, but no approval was granted", shortSHA(v.ReviewedSHA))
		return a, true, nil
	case mode == "local" || mode == "validated":
		a.Reason = fmt.Sprintf("this repo delivers in %s mode, which merges only on the lead's approval", mode)
		return a, true, nil
	}
	return a, false, nil
}

// eventApprovalRequired mirrors the orchestrator's unexported event type of the same name:
// the gate's escalation when a trusted auto-approval lapsed.
const eventApprovalRequired = "approval_required"

func project(repo string) string {
	if repo == "" {
		return ""
	}
	return filepath.Base(repo)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// safeText renders untrusted text as one printable line for the board's log.
func safeText(s string) string { return review.SafeLine(s) }

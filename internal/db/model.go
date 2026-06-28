package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Task statuses (§1.2). The actionable ones (needs_input/blocked/done) wake the
// manager only when a worker actor causes the transition (§1.3).
const (
	StatusPending    = "pending" // backlog, not yet spawned
	StatusActive     = "active"  // the canonical in-flight working status (§1.2)
	StatusNeedsInput = "needs_input"
	StatusBlocked    = "blocked"
	StatusDone       = "done" // work complete, awaiting manager
	StatusDelivered  = "delivered"
	StatusTornDown   = "torn_down"
	StatusAbandoned  = "abandoned"
)

// Event types (§1.3, extensible).
const (
	EventCreated          = "created"
	EventSpawned          = "spawned"
	EventStatusChanged    = "status_changed"
	EventStageChanged     = "stage_changed"
	EventNote             = "note"
	EventFollowOnCreated  = "follow_on_created"
	EventValidated        = "validated"
	EventReviewRecorded   = "review_recorded"
	EventSecurityRecorded = "security_recorded"
	EventQARecorded       = "qa_recorded"
	EventApproved         = "approved"
	EventMerged           = "merged"
	EventDelivered        = "delivered"
	EventTornDown         = "torn_down"
	EventPromoted         = "promoted"
	EventPRArmed          = "pr_armed"
	EventPRMerged         = "pr_merged"
	EventWindowGone       = "window_gone"
	EventIdleUnreported   = "idle_unreported"
	EventAutoResumed      = "auto_resumed"    // watcher nudged an API-stalled worker to continue (§4.4); non-actionable
	EventManagerStalled   = "manager_stalled" // external watchdog re-poke of a stalled manager (§4.7); actionable, entity_type=manager
)

// Task kinds (§1.1 CHECK; = state.Task.Kind).
const (
	KindShip  = "ship"
	KindScout = "scout"
	KindCC    = "cc"
)

// events.entity_type values (§1.1 CHECK).
const (
	EntityTypeProject = "project"
	EntityTypeEpic    = "epic"
	EntityTypePhase   = "phase"
	EntityTypeTask    = "task"
	EntityTypeManager = "manager"
	EntityTypeSystem  = "system"
)

// Common actors (events.actor / notes.author). worker:<id> is also valid.
const (
	ActorManager = "manager"
	ActorLead    = "lead"
	ActorSystem  = "system"
)

// EntityKind identifies a hierarchy table for SetEntityStatus. It maps to a fixed
// table name via a switch, never string-concatenated, so it cannot inject SQL.
type EntityKind string

const (
	EntityProject EntityKind = "project"
	EntityEpic    EntityKind = "epic"
	EntityPhase   EntityKind = "phase"
)

// Project mirrors a projects row (a repo the manager runs work in).
type Project struct {
	ID           int64
	RepoPath     string
	Name         string
	DeliveryMode string // DISPLAY CACHE ONLY — gates read AGENTS.md (§0.3)
	Status       string
	Owner        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Epic mirrors an epics row (a unit of work within a project).
type Epic struct {
	ID          int64
	ProjectID   int64
	Title       string
	Description string
	Status      string
	Owner       string
	Position    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Phase mirrors a phases row (a stage within an epic).
type Phase struct {
	ID          int64
	EpicID      int64
	Title       string
	Description string
	Status      string
	Owner       string
	Position    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Task is a superset of state.Task: it exposes the same field names the
// orchestrator already uses (ID, Window, Worktree, Project, Harness, Kind,
// Created, PR, SessionID, GatePassed, ApprovedBy, ReviewedSHA, Footprint) plus the
// new hierarchy/lifecycle fields. Project is the joined repo path (read-only;
// = state.Task.Project), populated by GetTask/ListTasks.
type Task struct {
	ID          string
	Window      string
	Worktree    string
	Project     string // joined projects.repo_path
	Harness     string
	Kind        string
	Created     time.Time // = tasks.created_at
	PR          string
	SessionID   string
	GatePassed  bool
	ApprovedBy  string
	ReviewedSHA string
	Footprint   []string

	ProjectID      int64
	EpicID         *int64
	PhaseID        *int64
	ParentTaskID   *string
	CreatedBy      string
	Title          string
	Status         string
	Stage          string
	Owner          string
	UpdatedAt      time.Time
	LastProgressAt *time.Time

	// liveness bookkeeping for the watcher's stale-detection (§4.4).
	LastPaneHash string
	IdleSweeps   int
}

// Event mirrors an events row: the append-only audit spine and the watcher's
// signal. id ordering equals commit ordering under IMMEDIATE writers (§1.4).
type Event struct {
	ID         int64
	TS         time.Time
	EntityType string
	EntityID   string
	Type       string
	Actor      string
	FromStatus *string
	ToStatus   *string
	Actionable bool
	Payload    string
}

// Note mirrors a notes row: freeform activity, append-only, never actionable.
type Note struct {
	ID     int64
	TS     time.Time
	TaskID *string
	Author string
	Body   string
}

// Manager mirrors the singleton manager row (replaces state.Manager).
// WatchWatermark and AwaitingLead are owned by SetWatermark/SetAwaitingLead;
// SetManager preserves them and writes only Dir/SessionID.
type Manager struct {
	Dir            string
	SessionID      string
	WatchWatermark int64
	AwaitingLead   bool
	UpdatedAt      time.Time
}

// TaskFilter narrows ListTasks. A zero/empty field means "no constraint".
type TaskFilter struct {
	Status      []string // status IN (?,…)
	ProjectID   int64    // 0 ⇒ any
	EpicID      int64    // 0 ⇒ any (a non-zero id excludes tasks with NULL epic_id)
	Owner       string   // "" ⇒ any
	ParentID    string   // "" ⇒ any
	ExcludeKind []string // kind NOT IN (?,…), e.g. exclude "cc"
}

// TaskFields is a partial update of a task's runtime/coupling fields (no event,
// §2.2). A nil pointer leaves that column unchanged; updated_at always advances.
//
// Kind is the scout↔ship lifecycle marker; Promote sets it here (a plain field
// write, no event) until the manager-authored 'promoted' event is layered on in
// increment 5 (§3.4). The CHECK constraint validates the value (ship|scout|cc).
type TaskFields struct {
	Window    *string
	Worktree  *string
	Harness   *string
	SessionID *string
	PR        *string
	Owner     *string
	Title     *string
	Kind      *string
	EpicID    *int64
	PhaseID   *int64
	Footprint *[]string
}

// Delivery carries the provenance RecordDelivery writes (gate/approval/sha) plus
// the manager-authored event it emits. EventType defaults to review_recorded and
// Actor to manager; the event is always actionable=0 (§1.3).
//
// Verdict, when non-nil, is upserted into the verdicts table in the SAME transaction
// as the task summary update and the event, so the flattened summary columns
// (gate_passed/approved_by/reviewed_sha) and the durable, authoritative verdict row
// can never drift apart.
type Delivery struct {
	GatePassed  bool
	ApprovedBy  string // "" | human | auto
	ReviewedSHA string
	EventType   string // review_recorded | security_recorded | qa_recorded | …
	Actor       string
	Payload     string
	Verdict     *Verdict // optional: upserted atomically with the summary
}

// Verdict mirrors a verdicts row: the durable, content-pinned trust-gate artifact for
// a task (the authoritative source the merge gate validates freshness from, replacing
// the short-lived TTL'd verdict file). It bundles the adversarial-review outcome
// (Overall + Findings), the commit pin (ReviewedSHA) and content pin (DiffID), and the
// approval token's durable record (ApprovedBy + ApprovalSHA). Findings is the opaque
// JSON the orchestrator marshals from []review.Finding; the db layer never interprets
// it, keeping internal/db free of any internal/review dependency.
type Verdict struct {
	TaskID      string
	Overall     string // pass | block
	ReviewedSHA string
	DiffID      string
	Findings    string // JSON array of review.Finding (opaque to db)
	ApprovedBy  string // "" | human | auto
	ApprovalSHA string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TimelineItem is one entry in a task's merged events∪notes history (§2.2).
type TimelineItem struct {
	TS    time.Time
	Kind  string // "event" | "note"
	Event *Event
	Note  *Note
}

// queryer is satisfied by both *sql.DB and *sql.Tx. Every data helper takes a
// queryer so a composite operation can pass its *sql.Tx and the helper can never
// accidentally reach for s.db inside an open transaction (§2.3).
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// formatTime renders a timestamp as sortable RFC3339Nano in UTC (§1.1).
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime parses a stored RFC3339Nano timestamp.
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// nullTime stores a *time.Time as NULL when nil.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// nullStr stores a *string as NULL when nil.
func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// nullInt stores a *int64 as NULL when nil.
func nullInt(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}

// marshalFootprint encodes a footprint as a JSON array. An empty/nil footprint
// becomes "[]" (the column default), preserving state.Task's "nil = undeclared".
func marshalFootprint(fp []string) (string, error) {
	if len(fp) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(fp)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalFootprint decodes a stored footprint, returning nil for "[]"/empty so
// an undeclared footprint round-trips to nil (state.Task semantics).
func unmarshalFootprint(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var fp []string
	if err := json.Unmarshal([]byte(s), &fp); err != nil {
		return nil, err
	}
	if len(fp) == 0 {
		return nil, nil
	}
	return fp, nil
}

// isActionableStatus reports whether a task status is one of the actionable
// transitions {needs_input, blocked, done} (§1.2).
func isActionableStatus(status string) bool {
	switch status {
	case StatusNeedsInput, StatusBlocked, StatusDone:
		return true
	}
	return false
}

// isWorkerActor reports whether an actor is a worker (worker:<id>). Only worker
// actors make a status transition actionable (§1.3).
func isWorkerActor(actor string) bool { return strings.HasPrefix(actor, "worker:") }

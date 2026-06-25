// Package state holds ttorch's pure, dependency-free task helpers: the
// footprint-overlap logic (footprint.go) and the legacy Task record shape. The JSON
// persistence that once lived here — the Store plus the task/manager record I/O —
// has moved to internal/db, the SQLite source of truth (§2.4). The migration of the
// old on-disk JSON is handled by internal/db.ImportLegacy, which reads it through its
// own private struct (§2.5), so the Task type below carries no persistence behavior.
package state

import "time"

// Task is the legacy durable record shape for one dispatched worker (or attached cc
// session). Persistence moved to internal/db.Task (a superset of these fields); this
// type is retained per the migration plan (§2.4).
type Task struct {
	ID        string    `json:"id"`
	Window    string    `json:"window"`
	Worktree  string    `json:"worktree"`
	Project   string    `json:"project"`
	Harness   string    `json:"harness"`
	Kind      string    `json:"kind"` // ship | scout | cc
	Created   time.Time `json:"created"`
	PR        string    `json:"pr,omitempty"`
	SessionID string    `json:"sessionId,omitempty"` // stable harness session id for resume
	// Delivery provenance, so a merge no human read can be reconstructed from state.
	GatePassed  bool   `json:"gatePassed,omitempty"`  // the adversarial-review verdict passed
	ApprovedBy  string `json:"approvedBy,omitempty"`  // human | auto (trusted-mode auto-approval)
	ReviewedSHA string `json:"reviewedSha,omitempty"` // the commit the verdict was recorded against
	// Footprint is the repo-relative file paths / directory prefixes this task
	// declares it will touch. Absent (nil) means undeclared: no overlap enforcement.
	Footprint []string `json:"footprint,omitempty"`
}

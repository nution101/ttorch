package orchestrator

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/state"
)

// Conflict reports that a proposed footprint overlaps a live worker's footprint.
// It names the worker and the exact path pairs that collide, so a refusal (or a
// `check-overlap` report) tells the manager precisely where to make the work
// disjoint.
type Conflict struct {
	TaskID   string
	Window   string
	Project  string
	Overlaps [][2]string // [proposedPath, existingPath] pairs that overlap
}

// computeConflicts is the pure overlap core: which of tasks a proposed footprint
// overlaps, and on which path pairs. A task with an empty footprint never
// conflicts (a worker that declared nothing is exempt). Deterministic — tasks are
// reported in input order — and free of tmux/liveness so it is unit-testable.
func computeConflicts(proposed []string, tasks []db.Task) []Conflict {
	var out []Conflict
	for _, t := range tasks {
		if ov := state.FootprintOverlap(proposed, t.Footprint); len(ov) > 0 {
			out = append(out, Conflict{
				TaskID:   t.ID,
				Window:   t.Window,
				Project:  t.Project,
				Overlaps: ov,
			})
		}
	}
	return out
}

// footprintCandidate reports whether a task is eligible to be considered for
// overlap before the (impure) liveness check: it declared a footprint, is not an
// ad-hoc cc/manager session, is not the excluded task, and — when repo is non-empty
// — belongs to that repo (footprints are repo-relative, so paths in different repos
// can't actually collide). Pure, so the scoping rules are unit-testable.
func footprintCandidate(t db.Task, repo, excludeID string) bool {
	if t.ID == excludeID || t.Kind == "cc" || len(t.Footprint) == 0 {
		return false
	}
	return repo == "" || t.Project == repo
}

// liveFootprintTasks returns the live worker tasks eligible for overlap, scoped to
// repo when non-empty. Liveness depends on tmux, so this impure half is kept out of
// footprintCandidate / computeConflicts. It propagates the liveTasks read error so a
// transient board-read failure cannot masquerade as "no live workers" (an empty set the
// caller would read as "no overlap" — a fail-open on the one invariant overlap protects).
func (m *Manager) liveFootprintTasks(repo, excludeID string) ([]db.Task, error) {
	live, err := m.liveTasks()
	if err != nil {
		return nil, err
	}
	var out []db.Task
	for _, t := range live {
		if footprintCandidate(t, repo, excludeID) && m.Live(t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// CheckOverlap reports which live workers a proposed footprint would conflict
// with, scoped to repo when non-empty. It is the single source of truth shared by
// the spawn guard and the `ttorch check-overlap` planning command.
//
// It returns an error — and the scheduler/spawn callers treat that as "cannot prove
// disjoint" and REFUSE — whenever the board cannot be read. Without this, a read failure
// would yield an empty conflict list indistinguishable from a genuine "no conflict", and
// the daemon would dispatch onto files a live worker may already hold. Fail closed: an
// overlap check that cannot read must never report safety it has not verified.
func (m *Manager) CheckOverlap(repo string, proposed []string) ([]Conflict, error) {
	if len(proposed) == 0 {
		return nil, nil
	}
	tasks, err := m.liveFootprintTasks(repo, "")
	if err != nil {
		return nil, err
	}
	return computeConflicts(proposed, tasks), nil
}

// OverlapString renders a conflict's path pairs as "proposed↔existing" entries,
// collapsing an equal pair to a single path. Shared by the spawn-refusal error and
// the `check-overlap` report so both name overlaps identically.
func (c Conflict) OverlapString() string {
	parts := make([]string, 0, len(c.Overlaps))
	for _, p := range c.Overlaps {
		if p[0] == p[1] {
			parts = append(parts, p[0])
		} else {
			parts = append(parts, p[0]+"↔"+p[1])
		}
	}
	return strings.Join(parts, ", ")
}

// ConflictError renders conflicts into the spawn-refusal error, naming every
// conflicting live worker and the overlapping paths, and pointing at the
// --force-overlap override.
func ConflictError(proposed []string, conflicts []Conflict) error {
	var b strings.Builder
	fmt.Fprintf(&b, "footprint %q overlaps %d live worker(s) — refusing to dispatch onto shared files:",
		strings.Join(proposed, ","), len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(&b, "\n  %s (window %s): %s", c.TaskID, c.Window, c.OverlapString())
	}
	b.WriteString("\n  dispatch to a disjoint set of files, or re-run with --force-overlap to override")
	return errors.New(b.String())
}

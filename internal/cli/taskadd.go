package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/paths"
)

// taskAdd is one backlog task to add: what `ttorch task add` reads from its flags, and what the
// peer control channel's task-add verb reads from a parent coordinator's request. Brief is the
// brief's text, already read; Touches is the comma-separated footprint as --touches takes it.
// EpicID and PhaseID are 0 when unset.
type taskAdd struct {
	ID                         string
	ProjectID, EpicID, PhaseID int64
	Title, Touches, Brief      string
	Effort, Model              string
	CitationsRef               string
	LintOffline, NoLint        bool
	// Actor is recorded as the task's creator: the manager for the CLI, the parent coordinator
	// for the control channel.
	Actor string
	// RequestID, when set, makes the add idempotent: a repeat returns the first result and
	// creates nothing (db.Store.AddTask). The CLI leaves it empty.
	RequestID string
}

// addBacklogTask is the core of `ttorch task add`, shared with the peer control channel so a task a
// parent hands over is checked exactly as a local one is. It refuses an invalid id, effort or
// model before any side effect, lints the brief against the project's repository (unless
// NoLint), resolves the epic and phase, and creates the pending row with its brief in one
// transaction, so a brief that cannot be written leaves no row. The lint report is written to
// out and its notes to errOut.
func addBacklogTask(ctx context.Context, store *db.Store, p paths.Paths, a taskAdd, out, errOut io.Writer) (db.TaskAddResult, error) {
	if err := db.ValidateTaskID(a.ID); err != nil {
		return db.TaskAddResult{}, fmt.Errorf("task add: %w", err)
	}
	if a.ProjectID == 0 {
		return db.TaskAddResult{}, fmt.Errorf("task add: a project is required")
	}
	// Reject an unknown effort/model before any side effect, so a typo fails loudly. A value
	// set here is persisted on the backlog row and ALWAYS wins over the scheduler's
	// dispatch-time tier classifier (explicit-wins); leaving them unset defers to it.
	if a.Effort != "" && !harness.ValidEffort(a.Effort) {
		return db.TaskAddResult{}, fmt.Errorf("task add: invalid --effort %q (want one of: %s)", a.Effort, strings.Join(harness.EffortLevels, "|"))
	}
	if a.Model != "" && !harness.ValidModel(a.Model) {
		return db.TaskAddResult{}, fmt.Errorf("task add: invalid --model %q (want an alias %s, or a full model id)", a.Model, strings.Join(harness.ModelAliases, "|"))
	}
	if a.NoLint && a.Brief == "" {
		return db.TaskAddResult{}, errNoBriefLintWithoutBrief
	}
	proj, ok, err := store.GetProject(ctx, a.ProjectID)
	if err != nil {
		return db.TaskAddResult{}, err
	} else if !ok {
		return db.TaskAddResult{}, fmt.Errorf("task add: no such project %d (see 'ttorch project ls')", a.ProjectID)
	}
	// Lint the brief before any side effect. The brief is a SNAPSHOT — it is copied into
	// the task here, so editing the file afterwards reaches nobody and a defect is only
	// discovered once a worker has acted on it. This is the last moment a fix is free. It
	// runs against the project's own repository (its remote, its refs, its AGENTS.md
	// configuration), and only when a brief was supplied: an add with no brief behaves
	// exactly as before.
	if !a.NoLint {
		if err := lintBriefTo(out, errOut, "task add", "added", a.Brief, proj.RepoPath, a.CitationsRef, a.LintOffline); err != nil {
			return db.TaskAddResult{}, err
		}
	}
	// Resolve and cross-validate the hierarchy refs so the row is coherent: an
	// --epic must live under --project, and a --phase must live under that epic
	// (a phase always has a parent epic). When only --phase is given we adopt its
	// parent epic, so a task can never end up with phase_id set and epic_id NULL —
	// which would otherwise render under an epic in --tree yet be invisible to
	// `tasks --epic` (the two read surfaces must agree).
	var epicID, phaseID *int64
	if a.EpicID != 0 {
		e, ok, err := store.GetEpic(ctx, a.EpicID)
		if err != nil {
			return db.TaskAddResult{}, err
		}
		if !ok {
			return db.TaskAddResult{}, fmt.Errorf("task add: no such epic %d (see 'ttorch epic ls')", a.EpicID)
		}
		if e.ProjectID != a.ProjectID {
			return db.TaskAddResult{}, fmt.Errorf("task add: epic %d belongs to project %d, not %d", a.EpicID, e.ProjectID, a.ProjectID)
		}
		ev := a.EpicID
		epicID = &ev
	}
	if a.PhaseID != 0 {
		ph, ok, err := store.GetPhase(ctx, a.PhaseID)
		if err != nil {
			return db.TaskAddResult{}, err
		}
		if !ok {
			return db.TaskAddResult{}, fmt.Errorf("task add: no such phase %d (see 'ttorch phase ls')", a.PhaseID)
		}
		if epicID != nil && *epicID != ph.EpicID {
			return db.TaskAddResult{}, fmt.Errorf("task add: phase %d belongs to epic %d, not the given epic %d", a.PhaseID, ph.EpicID, *epicID)
		}
		if epicID == nil {
			// Adopt the phase's parent epic and verify it belongs to the project.
			pe, ok, err := store.GetEpic(ctx, ph.EpicID)
			if err != nil {
				return db.TaskAddResult{}, err
			}
			if !ok {
				return db.TaskAddResult{}, fmt.Errorf("task add: phase %d references missing epic %d", a.PhaseID, ph.EpicID)
			}
			if pe.ProjectID != a.ProjectID {
				return db.TaskAddResult{}, fmt.Errorf("task add: phase %d belongs to project %d, not %d", a.PhaseID, pe.ProjectID, a.ProjectID)
			}
			ev := ph.EpicID
			epicID = &ev
		}
		pv := a.PhaseID
		phaseID = &pv
	}
	// The brief is written inside the transaction that inserts the row (db.Store.AddTask), so a
	// brief that cannot be stored rolls the row back: there is never a task the scheduler skips
	// for want of the brief it was added with, and nothing to clean up before a retry.
	res, err := store.AddTask(ctx, db.TaskAdd{
		Task: db.Task{
			ID: a.ID, ProjectID: a.ProjectID, EpicID: epicID, PhaseID: phaseID,
			Title: strings.TrimSpace(a.Title), Kind: db.KindShip, Status: db.StatusPending,
			Footprint: parseTouches(a.Touches),
			Effort:    strings.ToLower(strings.TrimSpace(a.Effort)), Model: harness.NormalizeModel(a.Model),
		},
		Actor:     a.Actor,
		RequestID: a.RequestID,
		Brief:     a.Brief,
		WriteBrief: func(brief string) error {
			if err := orchestrator.WriteBriefFile(p.BriefPath(a.ID), brief); err != nil {
				return fmt.Errorf("task add: could not store the brief for %s, so nothing was added: %w", a.ID, err)
			}
			return nil
		},
	})
	if errors.Is(err, db.ErrTaskExists) {
		return db.TaskAddResult{}, fmt.Errorf("task add: %w", err)
	}
	return res, err
}

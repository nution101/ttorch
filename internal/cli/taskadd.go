package cli

import (
	"context"
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
}

// addBacklogTask is the core of `ttorch task add`, shared with the peer control channel so a task a
// parent hands over is checked exactly as a local one is. It refuses an invalid id, effort or
// model before any side effect, lints the brief against the project's repository (unless
// NoLint), resolves the epic and phase, and creates the pending row with its brief. The lint
// report is written to out and its notes to errOut.
func addBacklogTask(ctx context.Context, store *db.Store, p paths.Paths, a taskAdd, out, errOut io.Writer) (db.Task, error) {
	if err := db.ValidateTaskID(a.ID); err != nil {
		return db.Task{}, fmt.Errorf("task add: %w", err)
	}
	if a.ProjectID == 0 {
		return db.Task{}, fmt.Errorf("task add: a project is required")
	}
	// Reject an unknown effort/model before any side effect, so a typo fails loudly. A value
	// set here is persisted on the backlog row and ALWAYS wins over the scheduler's
	// dispatch-time tier classifier (explicit-wins); leaving them unset defers to it.
	if a.Effort != "" && !harness.ValidEffort(a.Effort) {
		return db.Task{}, fmt.Errorf("task add: invalid --effort %q (want one of: %s)", a.Effort, strings.Join(harness.EffortLevels, "|"))
	}
	if a.Model != "" && !harness.ValidModel(a.Model) {
		return db.Task{}, fmt.Errorf("task add: invalid --model %q (want an alias %s, or a full model id)", a.Model, strings.Join(harness.ModelAliases, "|"))
	}
	if a.NoLint && a.Brief == "" {
		return db.Task{}, errNoBriefLintWithoutBrief
	}
	proj, ok, err := store.GetProject(ctx, a.ProjectID)
	if err != nil {
		return db.Task{}, err
	} else if !ok {
		return db.Task{}, fmt.Errorf("task add: no such project %d (see 'ttorch project ls')", a.ProjectID)
	}
	// Lint the brief before any side effect. The brief is a SNAPSHOT — it is copied into
	// the task here, so editing the file afterwards reaches nobody and a defect is only
	// discovered once a worker has acted on it. This is the last moment a fix is free. It
	// runs against the project's own repository (its remote, its refs, its AGENTS.md
	// configuration), and only when a brief was supplied: an add with no brief behaves
	// exactly as before.
	if !a.NoLint {
		if err := lintBriefTo(out, errOut, "task add", "added", a.Brief, proj.RepoPath, a.CitationsRef, a.LintOffline); err != nil {
			return db.Task{}, err
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
			return db.Task{}, err
		}
		if !ok {
			return db.Task{}, fmt.Errorf("task add: no such epic %d (see 'ttorch epic ls')", a.EpicID)
		}
		if e.ProjectID != a.ProjectID {
			return db.Task{}, fmt.Errorf("task add: epic %d belongs to project %d, not %d", a.EpicID, e.ProjectID, a.ProjectID)
		}
		ev := a.EpicID
		epicID = &ev
	}
	if a.PhaseID != 0 {
		ph, ok, err := store.GetPhase(ctx, a.PhaseID)
		if err != nil {
			return db.Task{}, err
		}
		if !ok {
			return db.Task{}, fmt.Errorf("task add: no such phase %d (see 'ttorch phase ls')", a.PhaseID)
		}
		if epicID != nil && *epicID != ph.EpicID {
			return db.Task{}, fmt.Errorf("task add: phase %d belongs to epic %d, not the given epic %d", a.PhaseID, ph.EpicID, *epicID)
		}
		if epicID == nil {
			// Adopt the phase's parent epic and verify it belongs to the project.
			pe, ok, err := store.GetEpic(ctx, ph.EpicID)
			if err != nil {
				return db.Task{}, err
			}
			if !ok {
				return db.Task{}, fmt.Errorf("task add: phase %d references missing epic %d", a.PhaseID, ph.EpicID)
			}
			if pe.ProjectID != a.ProjectID {
				return db.Task{}, fmt.Errorf("task add: phase %d belongs to project %d, not %d", a.PhaseID, pe.ProjectID, a.ProjectID)
			}
			ev := ph.EpicID
			epicID = &ev
		}
		pv := a.PhaseID
		phaseID = &pv
	}
	if _, exists, err := store.GetTask(ctx, a.ID); err != nil {
		return db.Task{}, err
	} else if exists {
		return db.Task{}, fmt.Errorf("task add: task %q already exists", a.ID)
	}
	t, err := store.CreateTask(ctx, db.Task{
		ID: a.ID, ProjectID: a.ProjectID, EpicID: epicID, PhaseID: phaseID,
		CreatedBy: a.Actor, Title: strings.TrimSpace(a.Title),
		Kind: db.KindShip, Status: db.StatusPending, Footprint: parseTouches(a.Touches),
		Effort: strings.ToLower(strings.TrimSpace(a.Effort)), Model: harness.NormalizeModel(a.Model),
	}, a.Actor)
	if err != nil {
		return db.Task{}, err
	}
	// Persist the brief (keyed by task id) so dispatch — by the scheduler daemon or a manual
	// spawn — launches the worker with it as the initial prompt. Done after the row exists, so a
	// failed CreateTask never strands an orphan brief; a failure here is loud (the task is added
	// but its brief is missing, which the lead should know to re-supply).
	if a.Brief != "" {
		err := orchestrator.WriteBriefFile(p.BriefPath(t.ID), a.Brief)
		if err == nil {
			err = store.SetBriefStored(ctx, t.ID)
		}
		if err != nil {
			// The row exists but has no brief, so dispatch would fall back to the stub. Fail
			// loudly with the recovery path rather than silently leave a brief-less task that the
			// scheduler then dispatches on the stub.
			return db.Task{}, fmt.Errorf("task add: created %s but could not store its brief (%w); set it before dispatch with 'ttorch spawn %s <repo> --brief-file <path>', or remove the task with 'ttorch teardown %s'", t.ID, err, t.ID, t.ID)
		}
		t.HasBrief = true
	}
	return t, nil
}

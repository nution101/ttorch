package board

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/scheduler"
)

// Result is what an action did. Done=false means it found nothing to do: the decision was
// already acted on, or the state moved on since the page was drawn. Message says which.
type Result struct {
	Done    bool
	Message string
}

// inputError marks a request the lead can fix (a missing field, an empty answer), as opposed
// to an action that ran and failed.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

func parseEventID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, inputError{"missing or malformed event id"}
	}
	return id, nil
}

// lockTask serializes the board's actions on one task, so two clicks that arrive together
// act one after the other and the second sees what the first did.
func (s *Server) lockTask(id string) func() {
	s.locksMu.Lock()
	mu, ok := s.locks[id]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// actedOn reports whether this process already acted on the decision a marker names.
func (s *Server) actedOn(typ, key string) bool {
	s.actedMu.Lock()
	defer s.actedMu.Unlock()
	return s.acted[typ+" "+key]
}

// recordActed marks a decision as acted on: first in memory, so a failed write below still
// cannot let this process act on it twice, then durably as a lead-authored, non-actionable
// marker event that a later process (or a restarted board) finds. The write ignores the
// request's context: the action has already happened, and a closed browser tab must not
// cancel the record of it.
func (s *Server) recordActed(taskID, typ, key string) error {
	s.actedMu.Lock()
	s.acted[typ+" "+key] = true
	s.actedMu.Unlock()
	_, err := s.cfg.Store.AppendEvent(context.Background(), db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: typ, Actor: db.ActorLead, Payload: key,
	})
	return err
}

// timeline reads a task and its events. ok=false means the task does not exist.
func (s *Server) timeline(ctx context.Context, id string) (db.Task, []db.Event, bool, error) {
	t, ok, err := s.cfg.Store.GetTask(ctx, id)
	if err != nil || !ok {
		return t, nil, ok, err
	}
	tl, err := s.cfg.Store.Timeline(ctx, id)
	if err != nil {
		return t, nil, true, err
	}
	return t, events(tl), true, nil
}

// normalizeAnswer turns a textarea's value into the message a worker receives: browser line
// endings become \n and trailing newlines go, the same trim `ttorch send` applies to a
// message read from a file or stdin.
func normalizeAnswer(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimRight(s, "\n")
}

// Answer sends the lead's answer to a worker waiting on input. questionID names the
// status_changed event the page showed; if the task has since moved on (it is no longer
// waiting, or it asked something newer), nothing is sent. Each question is answered at most
// once: the send is recorded as a board_answer marker, and a second submit finds it.
//
// The message goes through Fleet.Send, which is exactly what `ttorch send` calls.
func (s *Server) Answer(ctx context.Context, taskID string, questionID int64, text string) (Result, error) {
	if taskID == "" {
		return Result{}, inputError{"missing task id"}
	}
	msg := normalizeAnswer(text)
	// The same refusal `ttorch send` makes: an empty send would deliver a bare Enter.
	if strings.TrimSpace(msg) == "" {
		return Result{}, inputError{"empty answer: nothing to send"}
	}
	defer s.lockTask(taskID)()

	t, evs, ok, err := s.timeline(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, inputError{fmt.Sprintf("unknown task %q", taskID)}
	}
	if t.Status != db.StatusNeedsInput && t.Status != db.StatusBlocked {
		return Result{Message: fmt.Sprintf("%s is %s now, not waiting on an answer; nothing sent", t.ID, t.Status)}, nil
	}
	q, ok := latestQuestion(t, evs)
	if !ok || q.ID != questionID {
		return Result{Message: fmt.Sprintf("%s has moved on from the question you answered; nothing sent. Reload to see what it is asking now", t.ID)}, nil
	}
	if at, ok := markerAt(evs, eventBoardAnswer, questionKey(q.ID)); ok {
		return Result{Message: fmt.Sprintf("that question was already answered at %s; nothing sent", at.Local().Format("15:04:05"))}, nil
	}
	if s.actedOn(eventBoardAnswer, questionKey(q.ID)) {
		return Result{Message: "that question was already answered from this board; nothing sent"}, nil
	}

	if err := s.cfg.Fleet.Send(t.ID, msg); err != nil {
		return Result{}, fmt.Errorf("send to %s failed: %w", t.ID, err)
	}
	if err := s.recordActed(t.ID, eventBoardAnswer, questionKey(q.ID)); err != nil {
		return Result{Done: true, Message: fmt.Sprintf("sent to %s, but recording the answer failed (%v); do not send it again", t.ID, err)}, nil
	}
	return Result{Done: true, Message: fmt.Sprintf("sent to %s", t.ID)}, nil
}

// Dispatch starts a worker on a pending backlog task through the scheduler's own dispatch:
// the atomic ClaimTask, the dispatch-tier resolution, and Fleet.SpawnAutonomous. The claim is
// what makes it idempotent: a second click (or a scheduler tick that got there first) finds
// the task no longer pending and does nothing.
//
// Unlike the scheduler it does not require a footprint, because the lead clicking is the
// judgement the scheduler lacks for a task without one. It does require a stored brief: the
// autonomous spawn will not start a worker on the placeholder brief, which waits for a
// `ttorch send` that nothing would make.
func (s *Server) Dispatch(ctx context.Context, taskID string) (Result, error) {
	if taskID == "" {
		return Result{}, inputError{"missing task id"}
	}
	defer s.lockTask(taskID)()

	t, ok, err := s.cfg.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, inputError{fmt.Sprintf("unknown task %q", taskID)}
	}
	if t.Status != db.StatusPending {
		return Result{Message: fmt.Sprintf("%s is %s now, not pending; nothing dispatched", t.ID, t.Status)}, nil
	}
	switch {
	case t.Kind == db.KindCC:
		return Result{}, inputError{fmt.Sprintf("%s is an ad-hoc session, not backlog work", t.ID)}
	case t.Project == "":
		return Result{}, inputError{fmt.Sprintf("%s has no project repo to dispatch into", t.ID)}
	case !t.HasBrief:
		return Result{}, inputError{fmt.Sprintf("%s has no stored brief; store one with 'ttorch task add %s --brief-file <path>' or dispatch it with 'ttorch spawn'", t.ID, t.ID)}
	}

	owner := "worker:" + t.ID
	claimed, won, err := s.cfg.Store.ClaimTask(ctx, t.ID, owner)
	if err != nil {
		return Result{}, err
	}
	if !won {
		return Result{Message: fmt.Sprintf("%s was dispatched by something else first; nothing dispatched", t.ID)}, nil
	}
	model, effort, autoTiered := scheduler.ResolveDispatchTier(claimed)
	// Overlap follows the daemon's policy: a worker has its own worktree, so an overlapping
	// footprint runs in parallel and the land pass rebases the second one, unless
	// TTORCH_SERIALIZE_OVERLAP asked for the old refusal.
	worker, err := s.cfg.Fleet.SpawnAutonomous(claimed.ID, claimed.Project, claimed.Kind == db.KindScout, "",
		claimed.Footprint, !s.cfg.SerializeOverlap, effort, model)
	if err != nil {
		// Put it back so it can be dispatched again. The claim's lease is the backstop if
		// this release fails.
		if _, rerr := s.cfg.Store.ReleaseClaim(context.Background(), claimed.ID, owner); rerr != nil {
			return Result{}, fmt.Errorf("dispatch of %s failed (%v), and returning it to the backlog failed too (%v); its lease will return it", claimed.ID, err, rerr)
		}
		return Result{}, fmt.Errorf("dispatch of %s failed and it is back in the backlog: %w", claimed.ID, err)
	}
	_ = s.cfg.Store.SetTaskAutoTiered(context.Background(), claimed.ID, autoTiered)
	return Result{Done: true, Message: fmt.Sprintf("dispatched %s in window %s", worker.ID, worker.Window)}, nil
}

// GatePrep re-runs the trust prep for a task whose gate the daemon escalated. eventID names
// the gate_blocked event the page showed; if a newer escalation or a recorded verdict has
// superseded it, nothing runs. Each escalation is re-prepped at most once from the board,
// recorded as a board_gate_prep marker.
//
// It holds the gate claim the daemon's gate pass takes (ClaimForLand as "gater:<id>") while
// the prep runs, so it never stages review inputs over a daemon gate tick on the same task.
// It records no verdict: the reviewers still run and `ttorch trust record` still decides.
func (s *Server) GatePrep(ctx context.Context, taskID string, eventID int64) (Result, error) {
	if taskID == "" {
		return Result{}, inputError{"missing task id"}
	}
	defer s.lockTask(taskID)()

	t, evs, ok, err := s.timeline(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, inputError{fmt.Sprintf("unknown task %q", taskID)}
	}
	if t.Status != db.StatusDone {
		return Result{Message: fmt.Sprintf("%s is %s now, not done; nothing to prep", t.ID, t.Status)}, nil
	}
	g, ok := latestGateBlocked(evs)
	if !ok || g.ID != eventID {
		return Result{Message: fmt.Sprintf("that escalation on %s has been superseded; nothing run. Reload to see its current state", t.ID)}, nil
	}
	if at, ok := markerAt(evs, eventBoardGatePrep, gateKey(g.ID)); ok {
		return Result{Message: fmt.Sprintf("gate prep for that escalation already ran at %s; nothing run", at.Local().Format("15:04:05"))}, nil
	}
	if s.actedOn(eventBoardGatePrep, gateKey(g.ID)) {
		return Result{Message: "gate prep for that escalation already ran from this board; nothing run"}, nil
	}

	owner := "gater:" + t.ID
	won, err := s.cfg.Store.ClaimForLand(ctx, t.ID, owner)
	if err != nil {
		return Result{}, err
	}
	if !won {
		return Result{Message: fmt.Sprintf("the gate is working on %s right now; nothing run. Try again shortly", t.ID)}, nil
	}
	defer func() { _, _ = s.cfg.Store.ReleaseLandClaim(context.Background(), t.ID, owner) }()

	dir, err := s.cfg.Fleet.TrustPrep(t.ID)
	if err != nil {
		return Result{}, fmt.Errorf("gate prep for %s refused: %w", t.ID, err)
	}
	next := "then: ttorch trust record " + t.ID
	dims := s.cfg.Fleet.ReviewersFor(t.ID)
	if verr := review.ValidateDimensionSet(dims); verr != nil {
		next = "the prepared reviewer set is unusable (" + verr.Error() + "); fix it before running reviewers"
	} else {
		next = fmt.Sprintf("run the %d reviewer(s) (%s), %s", len(dims), strings.Join(dims, " | "), next)
	}
	msg := fmt.Sprintf("prepared review inputs for %s in %s; %s", t.ID, dir, next)
	if err := s.recordActed(t.ID, eventBoardGatePrep, gateKey(g.ID)); err != nil {
		return Result{Done: true, Message: msg + fmt.Sprintf(" (recording this failed: %v)", err)}, nil
	}
	return Result{Done: true, Message: msg}, nil
}

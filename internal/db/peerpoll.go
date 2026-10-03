package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// The root scheduler's poll of its peers (internal/scheduler/peerpass.go) keeps its state on the
// peers row: the escalation cursor, the failure streak and the steps of a down episode. Each
// change to that state is one transaction with the event it raises, so a crash between the two
// can neither lose an event nor raise it twice, and two schedulers polling one store raise each
// event once between them. The caller builds every payload, escaped and capped; nothing here
// reads peer text.

// Peer events. Each is recorded under entity system, entity id and actor peer:<name>
// (PeerEntityID), so the watcher shows one line per peer however many arrive at once.
const (
	// EventPeerEscalation is one escalation open on the peer, raised once. Actionable.
	EventPeerEscalation = "peer_escalation"
	// EventPeerUnreachable is the poll failing PeerUnreachableAfter times in a row. Actionable.
	EventPeerUnreachable = "peer_unreachable"
	// EventPeerRecovered is an unreachable peer answering a poll again. Not actionable.
	EventPeerRecovered = "peer_recovered"
	// EventPeerDown is a peer that still had no manager window or no running scheduler after
	// PeerEnsureUpAttempts ensure-up calls. Actionable.
	EventPeerDown = "peer_down"
)

// The payloads of the peer events, as JSON. The peer pass builds them and the watcher decodes
// them to print each event's own line, so the format is defined once, here. Every string field
// came from the peer, or is an error about reaching it, and was escaped and capped by the pass;
// a reader still prints each one quoted.

// PeerEscalationPayload is a peer_escalation event's payload: the escalation as the peer listed
// it, and how many the peer had open.
type PeerEscalationPayload struct {
	Peer         string `json:"peer"`
	EscalationID int64  `json:"escalation_id"`
	Kind         string `json:"kind"`
	TaskID       string `json:"task_id"`
	Open         int    `json:"open"`
	Body         string `json:"body"`
}

// PeerUnreachablePayload is a peer_unreachable event's payload: how many polls failed in a row,
// and the last one's error.
type PeerUnreachablePayload struct {
	Peer        string `json:"peer"`
	FailedPolls int    `json:"failed_polls"`
	Error       string `json:"error"`
}

// PeerRecoveredPayload is a peer_recovered event's payload.
type PeerRecoveredPayload struct {
	Peer string `json:"peer"`
}

// PeerDownPayload is a peer_down event's payload: how many ensure-up calls were made, and what
// the peer's summary said when the pass gave up.
type PeerDownPayload struct {
	Peer             string `json:"peer"`
	EnsureUpCalls    int    `json:"ensure_up_calls"`
	ManagerWindow    bool   `json:"manager_window"`
	SchedulerRunning bool   `json:"scheduler_running"`
	SchedulerStalled bool   `json:"scheduler_stalled"`
}

// PeerUnreachableAfter is how many polls in a row must fail before a peer is marked unreachable.
const PeerUnreachableAfter = 3

// PeerEnsureUpAttempts is how many ensure-up calls one down episode gets before peer_down.
const PeerEnsureUpAttempts = 3

// peerDownRaised is down_attempts once an episode's peer_down event is raised: one step past the
// last ensure-up call, so the episode stays spent until a healthy poll resets it to zero.
const peerDownRaised = PeerEnsureUpAttempts + 1

// ErrPeerNotPolled is a poll result for a peer that is neither live nor unreachable: retired, or
// sent back to provisioning, while the poll ran. It is dropped.
var ErrPeerNotPolled = errors.New("the peer is not polled")

// PeerEntityID is the entity id, and the actor, of peer name's events.
func PeerEntityID(name string) string { return "peer:" + name }

// PeerRaise is an escalation a poll found open on the peer: the peer's escalation id, and the
// payload of the event that raises it here.
type PeerRaise struct {
	ID      int64
	Payload string
}

// PeerPoll is a poll that reached the peer. Summary is cached on the row for display ("" keeps
// the cached one). Healthy says the summary showed a manager window and a scheduler that ticks,
// which ends a down episode. Raise is the escalations the peer listed above the cursor, in any
// order. RecoveredPayload is the payload of the peer_recovered event, if the poll raises one.
type PeerPoll struct {
	Summary          string
	Healthy          bool
	Raise            []PeerRaise
	RecoveredPayload string
}

// PeerPollResult is what recording a poll did: the escalation events it raised, in id order, the
// cursor it left, and whether the peer had been unreachable and is live again.
type PeerPollResult struct {
	Raised           []int64
	Cursor           int64
	Recovered        bool
	RecoveredEventID int64
}

// polledPeer reads name in tx and refuses it unless the poll still applies to it.
func polledPeer(ctx context.Context, tx *sql.Tx, name string) (Peer, error) {
	p, ok, err := getPeer(ctx, tx, name)
	if err != nil {
		return Peer{}, err
	}
	if !ok {
		return Peer{}, fmt.Errorf("peer %s: %w", name, ErrPeerNotFound)
	}
	if p.Status != PeerLive && p.Status != PeerUnreachable {
		return Peer{}, fmt.Errorf("peer %s is %s: %w", name, p.Status, ErrPeerNotPolled)
	}
	return p, nil
}

func peerEvent(name, typ, payload string, actionable bool) Event {
	id := PeerEntityID(name)
	return Event{EntityType: EntityTypeSystem, EntityID: id, Type: typ, Actor: id, Actionable: actionable, Payload: payload}
}

// RecordPeerPoll records a poll that reached the peer, in one transaction: the summary, the time
// as the last success, no error, no failure streak, and status live. An unreachable peer raises a
// peer_recovered event. A healthy poll ends any down episode. Each escalation in poll.Raise with
// an id above the cursor raises one actionable peer_escalation event, lowest id first, and the
// cursor moves to the highest raised. The cursor is read inside the transaction, so a repeat of
// the same poll, or another scheduler that recorded it first, raises nothing.
func (s *Store) RecordPeerPoll(ctx context.Context, name string, poll PeerPoll) (PeerPollResult, error) {
	raise := append([]PeerRaise(nil), poll.Raise...)
	sort.SliceStable(raise, func(i, j int) bool { return raise[i].ID < raise[j].ID })
	now := s.now()
	var out PeerPollResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, err := polledPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		summary, downAttempts := p.Summary, p.DownAttempts
		if poll.Summary != "" {
			summary = poll.Summary
		}
		if poll.Healthy {
			downAttempts = 0
		}
		if p.Status == PeerUnreachable {
			id, err := appendEvent(ctx, tx, now, peerEvent(name, EventPeerRecovered, poll.RecoveredPayload, false))
			if err != nil {
				return err
			}
			out.Recovered, out.RecoveredEventID = true, id
		}
		cursor := p.EscalationCursor
		for _, r := range raise {
			if r.ID <= cursor {
				continue
			}
			id, err := appendEvent(ctx, tx, now, peerEvent(name, EventPeerEscalation, r.Payload, true))
			if err != nil {
				return err
			}
			out.Raised = append(out.Raised, id)
			cursor = r.ID
		}
		out.Cursor = cursor
		ts := formatTime(now)
		_, err = tx.ExecContext(ctx,
			`UPDATE peers SET status = ?, summary = ?, escalation_cursor = ?, consecutive_failures = 0,
			 down_attempts = ?, last_ok_at = ?, last_error = '', updated_at = ? WHERE name = ?`,
			PeerLive, summary, cursor, downAttempts, ts, ts, name)
		return err
	})
	if err != nil {
		return PeerPollResult{}, err
	}
	return out, nil
}

// PeerFailureResult is what recording a failed poll did: the streak it left, and whether this
// failure marked the peer unreachable, with the event it raised.
type PeerFailureResult struct {
	Failures    int
	Unreachable bool
	EventID     int64
}

// RecordPeerFailure records a poll that failed, with why (escaped by the caller, capped here at
// MaxEscalationText), in one transaction. The failure that brings a live peer's streak to
// PeerUnreachableAfter marks it unreachable and raises one actionable peer_unreachable event
// whose payload is unreachablePayload. A peer already unreachable stays so, and raises nothing
// more until a poll succeeds.
func (s *Store) RecordPeerFailure(ctx context.Context, name, msg, unreachablePayload string) (PeerFailureResult, error) {
	msg, _ = CapText(msg, MaxEscalationText)
	now := s.now()
	var out PeerFailureResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, err := polledPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		out.Failures = p.ConsecutiveFailures + 1
		status := p.Status
		if status == PeerLive && out.Failures >= PeerUnreachableAfter {
			if out.EventID, err = appendEvent(ctx, tx, now, peerEvent(name, EventPeerUnreachable, unreachablePayload, true)); err != nil {
				return err
			}
			status, out.Unreachable = PeerUnreachable, true
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE peers SET status = ?, consecutive_failures = ?, last_error = ?, updated_at = ? WHERE name = ?`,
			status, out.Failures, msg, formatTime(now), name)
		return err
	})
	if err != nil {
		return PeerFailureResult{}, err
	}
	return out, nil
}

// PeerDownStep is the next step of a down episode: call ensure-up (EnsureUp, the episode's
// Attempt-th call), or raise peer_down (Down, with its event), or neither, once the episode's
// peer_down is raised.
type PeerDownStep struct {
	EnsureUp bool
	Attempt  int
	Down     bool
	EventID  int64
}

// StepPeerDown takes the next step of the peer's down episode, in one transaction. The first
// PeerEnsureUpAttempts steps each claim one ensure-up call. The step after them raises one
// actionable peer_down event whose payload is downPayload. Every step after that does nothing,
// until a healthy poll (RecordPeerPoll) ends the episode. Claiming the call before making it
// keeps the bound across a crash and across two schedulers: a claimed call is spent whether or
// not it ran.
func (s *Store) StepPeerDown(ctx context.Context, name, downPayload string) (PeerDownStep, error) {
	now := s.now()
	var out PeerDownStep
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, err := polledPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		attempts := p.DownAttempts
		switch {
		case attempts < PeerEnsureUpAttempts:
			attempts++
			out.EnsureUp, out.Attempt = true, attempts
		case attempts == PeerEnsureUpAttempts:
			if out.EventID, err = appendEvent(ctx, tx, now, peerEvent(name, EventPeerDown, downPayload, true)); err != nil {
				return err
			}
			attempts, out.Down = peerDownRaised, true
		default:
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE peers SET down_attempts = ?, updated_at = ? WHERE name = ?`,
			attempts, formatTime(now), name)
		return err
	})
	if err != nil {
		return PeerDownStep{}, err
	}
	return out, nil
}

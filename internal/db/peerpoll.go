package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
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
	// EventPeerDown is a peer that still had no manager window or no running scheduler once the
	// pass had spent its ensure-up calls for the window. Actionable.
	EventPeerDown = "peer_down"
	// EventPeerEnsureUp is one ensure-up call the pass made, recorded before it is made. Not
	// actionable; the window counts them.
	EventPeerEnsureUp = "peer_ensure_up"
	// EventPeerCursorReset is a peer whose escalation ids went back (a recreated or restored
	// store): the cursor was moved back to its current numbering. Actionable, once per episode.
	EventPeerCursorReset = "peer_cursor_reset"
	// EventPeerProtocolError is a peer that listed escalation ids out of range, which were not
	// raised. Actionable, once per episode.
	EventPeerProtocolError = "peer_protocol_error"
)

// The payloads of the peer events, as JSON. The peer pass builds them and the watcher decodes
// them to print each event's own line, so the format is defined once, here. Every string field
// came from the peer, or is an error about reaching it, and was escaped and capped by the pass;
// a reader still prints each one quoted.

// PeerEscalationPayload is a peer_escalation event's payload: the escalation as the peer listed
// it, with the peer's own creation stamp, and how many the peer had open.
type PeerEscalationPayload struct {
	Peer         string `json:"peer"`
	EscalationID int64  `json:"escalation_id"`
	Kind         string `json:"kind"`
	TaskID       string `json:"task_id"`
	CreatedAt    string `json:"created_at"`
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

// PeerDownPayload is a peer_down event's payload: how many ensure-up calls were made in what
// window, and what the peer's summary said when the pass gave up.
type PeerDownPayload struct {
	Peer             string `json:"peer"`
	EnsureUpCalls    int    `json:"ensure_up_calls"`
	Window           string `json:"window"`
	ManagerWindow    bool   `json:"manager_window"`
	SchedulerRunning bool   `json:"scheduler_running"`
	SchedulerStalled bool   `json:"scheduler_stalled"`
}

// PeerEnsureUpPayload is a peer_ensure_up event's payload: which call of the window it is.
type PeerEnsureUpPayload struct {
	Peer   string `json:"peer"`
	Call   int    `json:"call"`
	Window string `json:"window"`
}

// PeerUnreachableAfter is how many polls in a row must fail before a peer is marked unreachable.
const PeerUnreachableAfter = 3

// ErrPeerNotPolled is a poll result for a peer that is neither live nor unreachable: retired, or
// sent back to provisioning, while the poll ran. It is dropped.
var ErrPeerNotPolled = errors.New("the peer is not polled")

// PeerEntityID is the entity id, and the actor, of peer name's events.
func PeerEntityID(name string) string { return "peer:" + name }

// PeerOpenEscalation is one escalation a poll found open on the peer: the peer's id for it and the
// peer's own creation stamp, which together name it. An id alone does not: a recreated or
// restored store can give an id this coordinator already raised to a different escalation.
type PeerOpenEscalation struct {
	ID        int64
	CreatedAt string
}

// PeerPollBounds are the scheduler's limits on what one poll accepts. An id at or below zero is
// always refused. MaxID refuses a larger id (<= 0: no upper bound). MaxJump refuses a new id more
// than this above the cursor the poll started from, when that cursor is above zero (<= 0: no jump
// bound). MaxRaise is how many escalations one poll raises, lowest id first (<= 0: no cap); the
// rest stay new and are raised by the next polls.
type PeerPollBounds struct {
	MaxID    int64
	MaxJump  int64
	MaxRaise int
}

// PeerPoll is a poll that reached the peer. Summary is cached on the row for display ("" keeps
// the cached one). Healthy says the summary showed a manager window and a scheduler that ticks,
// which ends a down episode. Open is every escalation the peer listed as open, in any order, and
// PayloadFor(i) the payload of the event that raises Open[i] (called only for those raised).
// RecoveredPayload is the payload of the peer_recovered event, if the poll raises one.
type PeerPoll struct {
	Summary          string
	Healthy          bool
	Open             []PeerOpenEscalation
	PayloadFor       func(i int) string
	Bounds           PeerPollBounds
	RecoveredPayload string
}

// PeerPollResult is what recording a poll did. Raised is the peer_escalation events, in id order;
// Cursor the cursor it left. Recovered says the peer had been unreachable. Regressed says open
// escalations this coordinator never raised sat at or below the cursor, and CursorReset that this
// poll raised the episode's peer_cursor_reset for it. Refused is the ids refused as out of range,
// in the order the peer listed them, and ProtocolEventID the episode's peer_protocol_error if this
// poll raised it.
type PeerPollResult struct {
	Raised           []int64
	Cursor           int64
	Recovered        bool
	RecoveredEventID int64
	Regressed        bool
	CursorReset      bool
	ResetEventID     int64
	Refused          []int64
	ProtocolEventID  int64
}

// PeerCursorResetPayload is a peer_cursor_reset event's payload: the cursor the poll found,
// the one it left, how many open escalations it had not raised sat at or below the old cursor,
// and the lowest of them.
type PeerCursorResetPayload struct {
	Peer     string `json:"peer"`
	Cursor   int64  `json:"cursor"`
	Resynced int64  `json:"resynced"`
	Unseen   int    `json:"unseen"`
	Lowest   int64  `json:"lowest"`
}

// PeerProtocolErrorPayload is a peer_protocol_error event's payload: why ids were refused, how
// many, and the first few.
type PeerProtocolErrorPayload struct {
	Peer    string  `json:"peer"`
	Reason  string  `json:"reason"`
	Refused int     `json:"refused"`
	IDs     []int64 `json:"ids"`
}

// maxRefusedIDs is how many refused ids a peer_protocol_error payload lists.
const maxRefusedIDs = 8

func payloadJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
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
// peer_recovered event, and a healthy poll ends any down episode.
//
// Which escalations to raise is decided against peer_open_escalations, the ones this coordinator
// raised that are still open, read and written inside the transaction. Rows for escalations no
// longer in the peer's open list are dropped first. Each listed escalation with no row is new
// and raises one actionable peer_escalation event, lowest id first and at most Bounds.MaxRaise,
// and gets a row. So a repeat of the same poll, or another scheduler that recorded it first,
// raises nothing.
//
// A peer's ids only grow, so a new escalation is always above the cursor, the highest id raised.
// One at or below it means the peer's ids went back: its store was recreated or restored. That
// raises one actionable peer_cursor_reset per episode (cursor_reset_open), the new escalations are
// raised as usual, and the cursor moves to the highest id raised that is still open, in the peer's
// current numbering. An id at or below zero, over Bounds.MaxID, listed twice, or more than
// Bounds.MaxJump above a cursor above zero is refused: not raised, no row, and the cursor does not
// move for it. Refusals raise one actionable peer_protocol_error per episode (bad_ids_open).
func (s *Store) RecordPeerPoll(ctx context.Context, name string, poll PeerPoll) (PeerPollResult, error) {
	b := poll.Bounds
	now := s.now()
	var out PeerPollResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, err := polledPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		type cand struct {
			i     int
			id    int64
			stamp string
		}
		var (
			listed  = map[int64]string{}
			good    []cand
			reasons []string
		)
		refuse := func(id int64, why string) {
			out.Refused = append(out.Refused, id)
			for _, r := range reasons {
				if r == why {
					return
				}
			}
			reasons = append(reasons, why)
		}
		for i, e := range poll.Open {
			_, twice := listed[e.ID]
			switch {
			case e.ID <= 0:
				refuse(e.ID, "an id at or below zero")
			case b.MaxID > 0 && e.ID > b.MaxID:
				refuse(e.ID, fmt.Sprintf("an id above %d", b.MaxID))
			case twice:
				refuse(e.ID, "an id listed twice")
			default:
				listed[e.ID] = e.CreatedAt
				good = append(good, cand{i, e.ID, e.CreatedAt})
			}
		}
		sort.SliceStable(good, func(i, j int) bool { return good[i].id < good[j].id })

		// What was raised and is still open: drop what the peer no longer lists open, or now
		// lists under another stamp, which is another escalation.
		raised := map[int64]string{}
		rows, err := tx.QueryContext(ctx, `SELECT escalation_id, created_at FROM peer_open_escalations WHERE peer = ?`, name)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var stamp string
			if err := rows.Scan(&id, &stamp); err != nil {
				rows.Close()
				return err
			}
			raised[id] = stamp
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, stamp := range raised {
			if cur, ok := listed[id]; ok && cur == stamp {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM peer_open_escalations WHERE peer = ? AND escalation_id = ?`, name, id); err != nil {
				return err
			}
			delete(raised, id)
		}

		cursor := p.EscalationCursor
		var unseen []cand
		for _, c := range good {
			if _, ok := raised[c.id]; !ok {
				unseen = append(unseen, c)
			}
		}
		var lowest int64
		behind := 0
		for _, c := range unseen {
			if c.id <= cursor {
				if behind == 0 {
					lowest = c.id
				}
				behind++
			}
		}
		out.Regressed = behind > 0
		if !out.Regressed && cursor > 0 && b.MaxJump > 0 {
			kept := unseen[:0]
			for _, c := range unseen {
				if c.id-cursor > b.MaxJump {
					refuse(c.id, fmt.Sprintf("an id more than %d above the cursor %d", b.MaxJump, cursor))
					continue
				}
				kept = append(kept, c)
			}
			unseen = kept
		}
		if b.MaxRaise > 0 && len(unseen) > b.MaxRaise {
			unseen = unseen[:b.MaxRaise]
		}
		next := cursor
		if out.Regressed {
			next = 0
			for id := range raised {
				next = max(next, id)
			}
		}
		for _, c := range unseen {
			next = max(next, c.id)
		}
		out.Cursor = next

		if p.Status == PeerUnreachable {
			id, err := appendEvent(ctx, tx, now, peerEvent(name, EventPeerRecovered, poll.RecoveredPayload, false))
			if err != nil {
				return err
			}
			out.Recovered, out.RecoveredEventID = true, id
		}
		resetOpen := out.Regressed
		if out.Regressed && !p.CursorResetOpen {
			payload := payloadJSON(PeerCursorResetPayload{Peer: name, Cursor: cursor, Resynced: next, Unseen: behind, Lowest: lowest})
			if out.ResetEventID, err = appendEvent(ctx, tx, now, peerEvent(name, EventPeerCursorReset, payload, true)); err != nil {
				return err
			}
			out.CursorReset = true
		}
		badOpen := len(out.Refused) > 0
		if badOpen && !p.BadIDsOpen {
			ids := out.Refused
			if len(ids) > maxRefusedIDs {
				ids = ids[:maxRefusedIDs]
			}
			payload := payloadJSON(PeerProtocolErrorPayload{Peer: name, Reason: strings.Join(reasons, "; "), Refused: len(out.Refused), IDs: ids})
			if out.ProtocolEventID, err = appendEvent(ctx, tx, now, peerEvent(name, EventPeerProtocolError, payload, true)); err != nil {
				return err
			}
		}
		for _, c := range unseen {
			payload := ""
			if poll.PayloadFor != nil {
				payload = poll.PayloadFor(c.i)
			}
			id, err := appendEvent(ctx, tx, now, peerEvent(name, EventPeerEscalation, payload, true))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO peer_open_escalations (peer, escalation_id, created_at, event_id) VALUES (?, ?, ?, ?)`,
				name, c.id, c.stamp, id); err != nil {
				return err
			}
			out.Raised = append(out.Raised, id)
		}

		summary, downAttempts := p.Summary, p.DownAttempts
		if poll.Summary != "" {
			summary = poll.Summary
		}
		if poll.Healthy {
			downAttempts = 0
		}
		ts := formatTime(now)
		_, err = tx.ExecContext(ctx,
			`UPDATE peers SET status = ?, summary = ?, escalation_cursor = ?, consecutive_failures = 0,
			 down_attempts = ?, cursor_reset_open = ?, bad_ids_open = ?, last_ok_at = ?, last_error = '',
			 updated_at = ? WHERE name = ?`,
			PeerLive, summary, next, downAttempts, resetOpen, badOpen, ts, ts, name)
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

// PeerDownStep is the next step for a peer that answered but is down: call ensure-up (EnsureUp;
// Attempt is how many calls the window now holds, this one included), or raise peer_down (Down,
// with its event), or neither, once the window's calls are spent and its peer_down is raised.
type PeerDownStep struct {
	EnsureUp bool
	Attempt  int
	Down     bool
	EventID  int64
}

// PeerEnsureUpLimit bounds ensure-up calls to a peer: at most Calls whose time is within Window
// before At, which is now by the scheduler's clock and the time the call is recorded at.
type PeerEnsureUpLimit struct {
	Calls  int
	Window time.Duration
	At     time.Time
}

// StepPeerDown takes the next step for a down peer, in one transaction. While fewer than
// limit.Calls peer_ensure_up events of this peer are within limit.Window before limit.At, the
// step claims one more ensure-up call: it appends a non-actionable peer_ensure_up event at
// limit.At and counts it in down_attempts. Once the window is full, the step raises one actionable
// peer_down event whose payload is downPayload, unless one was raised since the last call, in
// which case it does nothing. A healthy poll resets down_attempts but not the window, so a peer
// whose summary flaps between healthy and down gets no more calls than the window allows, and
// one peer_down each time it uses them up. Claiming the call before it is made keeps the bound
// across a crash and across two schedulers: a claimed call is spent whether or not it ran.
func (s *Store) StepPeerDown(ctx context.Context, name, downPayload string, limit PeerEnsureUpLimit) (PeerDownStep, error) {
	if limit.Calls <= 0 || limit.Window <= 0 {
		return PeerDownStep{}, fmt.Errorf("an ensure-up limit needs calls and a window, got %d in %s", limit.Calls, limit.Window)
	}
	at := limit.At
	if at.IsZero() {
		at = s.now()
	}
	var out PeerDownStep
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		p, err := polledPeer(ctx, tx, name)
		if err != nil {
			return err
		}
		id := PeerEntityID(name)
		rows, err := tx.QueryContext(ctx,
			`SELECT ts FROM events WHERE entity_type = ? AND entity_id = ? AND type = ? ORDER BY id DESC LIMIT ?`,
			EntityTypeSystem, id, EventPeerEnsureUp, limit.Calls)
		if err != nil {
			return err
		}
		inWindow := 0
		for rows.Next() {
			var ts string
			if err := rows.Scan(&ts); err != nil {
				rows.Close()
				return err
			}
			t, err := parseTime(ts)
			if err != nil {
				rows.Close()
				return err
			}
			if at.Sub(t) < limit.Window {
				inWindow++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if inWindow < limit.Calls {
			if _, err := appendEvent(ctx, tx, at, Event{EntityType: EntityTypeSystem, EntityID: id, Type: EventPeerEnsureUp,
				Actor: id, TS: at, Payload: payloadJSON(PeerEnsureUpPayload{Peer: name, Call: inWindow + 1, Window: limit.Window.String()})}); err != nil {
				return err
			}
			out.EnsureUp, out.Attempt = true, inWindow+1
			_, err = tx.ExecContext(ctx, `UPDATE peers SET down_attempts = ?, updated_at = ? WHERE name = ?`,
				p.DownAttempts+1, formatTime(at), name)
			return err
		}
		var last string
		err = tx.QueryRowContext(ctx,
			`SELECT type FROM events WHERE entity_type = ? AND entity_id = ? AND type IN (?, ?) ORDER BY id DESC LIMIT 1`,
			EntityTypeSystem, id, EventPeerEnsureUp, EventPeerDown).Scan(&last)
		if err != nil {
			return err
		}
		if last == EventPeerDown {
			return nil
		}
		ev := peerEvent(name, EventPeerDown, downPayload, true)
		ev.TS = at
		out.EventID, err = appendEvent(ctx, tx, at, ev)
		out.Down = err == nil
		return err
	})
	if err != nil {
		return PeerDownStep{}, err
	}
	return out, nil
}

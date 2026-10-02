package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Escalations are the decisions waiting on the lead, kept as rows so "what is open now"
// survives a restart. One is raised by the manager (`ttorch escalate`), or opened for a done
// task whose approval_required event has had no human approval since
// (SyncApprovalEscalations). The lead's answer comes back through `ttorch answer`, which marks
// the escalation answered and appends one actionable event for the manager.
//
// Nothing here can tell who typed an answer. The CLI refuses a worker context, but a process
// running as the lead's uid can step around that or write this database directly, so an
// answer is recorded as relayed by the manager (answered_by, and the event's actor), never as
// the lead. An answer only carries text: it mints no approval and passes no gate.
//
// Body and answer text can come from a worker by way of the manager, and will come from
// another machine once peers exist, so the store caps it (CapText) and keeps it otherwise as
// written. Every renderer escapes it.

// Escalation kinds (migration 0011 CHECK).
const (
	EscalationApproval   = "approval"
	EscalationQuestion   = "question"
	EscalationGateChange = "gate_change"
)

// Escalation statuses (migration 0011 CHECK).
const (
	EscalationOpen      = "open"
	EscalationAnswered  = "answered"
	EscalationResolved  = "resolved"  // closed by the state it waited on changing, not by an answer
	EscalationWithdrawn = "withdrawn" // closed by whoever raised it
)

// MaxEscalationText is the cap, in bytes, on every text field an escalation stores: the
// body, the answer, and the payload of the event an answer appends.
const MaxEscalationText = 2048

// MaxRequestIDLen bounds a request id. Ids are minted by a client, never by a person.
const MaxRequestIDLen = 128

// EventApprovalRequired is the event the gate appends when an auto-approval lapses
// (orchestrator.eventApprovalRequired, which board also mirrors as a literal). It is named
// here because SyncApprovalEscalations reads it; the string must stay in step with the gate's.
const EventApprovalRequired = "approval_required"

// EventEscalationAnswered is what an answer appends: entity manager, actor who recorded the
// answer (the manager, for the CLI), actionable, payload naming the escalation, its task, who
// relayed the answer, and the answer, capped.
const EventEscalationAnswered = "escalation_answered"

// requestVerbAnswer is the peer_requests verb an answer is stored under.
const requestVerbAnswer = "answer"

// Escalation is one row of the escalations table. TaskID is "" when the escalation names no
// task (its task was deleted); SourceEventID is 0 unless an approval_required event is behind
// it. EpisodeEventID is set only on an approval the sync opened: the status event that put the
// task in done, 0 when it got there without one. AnsweredBy is the actor that recorded the
// answer.
type Escalation struct {
	ID              int64     `json:"id"`
	TaskID          string    `json:"task_id"`
	Kind            string    `json:"kind"`
	Body            string    `json:"body"`
	Status          string    `json:"status"`
	SourceEventID   int64     `json:"source_event_id"`
	EpisodeEventID  *int64    `json:"episode_event_id"`
	Answer          string    `json:"answer"`
	AnsweredBy      string    `json:"answered_by"`
	AnswerRequestID string    `json:"answer_request_id"`
	CreatedAt       time.Time `json:"created_at"`
	ResolvedAt      time.Time `json:"resolved_at"` // zero while open
}

const escalationColumns = `id, task_id, kind, body, status, source_event_id, episode_event_id, answer, answered_by, answer_request_id, created_at, resolved_at`

func scanEscalation(sc rowScanner) (Escalation, error) {
	var (
		e               Escalation
		task, requestID sql.NullString
		source, episode sql.NullInt64
		created         string
		resolved        sql.NullString
	)
	if err := sc.Scan(&e.ID, &task, &e.Kind, &e.Body, &e.Status, &source, &episode, &e.Answer, &e.AnsweredBy, &requestID, &created, &resolved); err != nil {
		return Escalation{}, err
	}
	e.TaskID, e.SourceEventID, e.AnswerRequestID = task.String, source.Int64, requestID.String
	if episode.Valid {
		e.EpisodeEventID = &episode.Int64
	}
	var err error
	if e.CreatedAt, err = parseTime(created); err != nil {
		return Escalation{}, err
	}
	if resolved.Valid {
		if e.ResolvedAt, err = parseTime(resolved.String); err != nil {
			return Escalation{}, err
		}
	}
	return e, nil
}

// CapText returns s as valid UTF-8 (invalid bytes become U+FFFD) cut to at most n bytes on a
// rune boundary, and whether anything was cut.
func CapText(s string, n int) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// ValidRequestID reports whether id may name a request: 1 to MaxRequestIDLen bytes of ASCII
// letters, digits and . _ : -, so it prints on one line and needs no quoting.
func ValidRequestID(id string) error {
	if id == "" || len(id) > MaxRequestIDLen {
		return fmt.Errorf("request id must be 1 to %d bytes, got %d", MaxRequestIDLen, len(id))
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == ':', r == '-':
		default:
			return fmt.Errorf("request id may hold only letters, digits and . _ : -, got %q", id)
		}
	}
	return nil
}

func validEscalationKind(kind string) bool {
	return kind == EscalationApproval || kind == EscalationQuestion || kind == EscalationGateChange
}

// OpenEscalation raises an open escalation of kind against an existing task. body is capped
// at MaxEscalationText; it must not be blank. Raising one appends no event: it is the manager
// asking the lead, so there is no one on this side to wake.
func (s *Store) OpenEscalation(ctx context.Context, taskID, kind, body string) (Escalation, error) {
	if !validEscalationKind(kind) {
		return Escalation{}, fmt.Errorf("escalation kind %q is not one of approval, question, gate_change", kind)
	}
	if strings.TrimSpace(body) == "" {
		return Escalation{}, errors.New("escalation body is empty")
	}
	body, _ = CapText(body, MaxEscalationText)
	now := s.now()
	var out Escalation
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id = ?`, taskID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("task %s not found", taskID)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO escalations (task_id, kind, body, status, created_at) VALUES (?, ?, ?, 'open', ?)`,
			taskID, kind, body, formatTime(now))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out, err = getEscalation(ctx, tx, id)
		return err
	})
	if err != nil {
		return Escalation{}, err
	}
	return out, nil
}

func getEscalation(ctx context.Context, q queryer, id int64) (Escalation, error) {
	return scanEscalation(q.QueryRowContext(ctx, `SELECT `+escalationColumns+` FROM escalations WHERE id = ?`, id))
}

// GetEscalation returns one escalation and whether it exists.
func (s *Store) GetEscalation(ctx context.Context, id int64) (Escalation, bool, error) {
	e, err := getEscalation(ctx, s.db, id)
	if err == sql.ErrNoRows {
		return Escalation{}, false, nil
	}
	if err != nil {
		return Escalation{}, false, err
	}
	return e, true, nil
}

// ListEscalations returns the escalations with status, or every one when status is "", in id
// order.
func (s *Store) ListEscalations(ctx context.Context, status string) ([]Escalation, error) {
	query := `SELECT ` + escalationColumns + ` FROM escalations`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Escalation
	for rows.Next() {
		e, err := scanEscalation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EscalationOpenCounts returns how many escalations are open and the highest open id, 0 when
// none is.
func (s *Store) EscalationOpenCounts(ctx context.Context) (open int, highestOpen int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(MAX(id), 0) FROM escalations WHERE status = 'open'`).Scan(&open, &highestOpen)
	return open, highestOpen, err
}

// AnswerResult is what an answer did. Replayed is set when the request id had already been
// answered and the stored result was returned without changing anything. EventID is the
// manager event the original answer appended.
type AnswerResult struct {
	Escalation Escalation `json:"escalation"`
	EventID    int64      `json:"event_id"`
	Replayed   bool       `json:"-"`
}

// AnswerEscalation answers an open escalation, once per request id. In one transaction it
// marks the escalation answered, appends one actionable event addressed to the manager, and
// stores the result under requestID in peer_requests. A repeat of requestID returns that
// stored result and writes nothing, whatever the answer text, so a retry after a timeout can
// never append a second event. A request id already used for another escalation, or a new
// request id for an escalation that is no longer open, is refused.
//
// actor is who records the answer, stored as answered_by and as the event's actor. It may not
// be the lead: the store cannot tell who typed the text, so it never claims the lead did. The
// CLI passes the manager, which relays the lead's words.
//
// The answer is capped at MaxEscalationText, and so is the event's payload, which carries the
// escalation id, its task, who relayed the answer, and the answer.
func (s *Store) AnswerEscalation(ctx context.Context, id int64, requestID, answer, actor string) (AnswerResult, error) {
	if err := ValidRequestID(requestID); err != nil {
		return AnswerResult{}, err
	}
	if actor == "" || actor == ActorLead {
		return AnswerResult{}, fmt.Errorf("an answer is recorded as relayed by its caller, not as %q: nothing verifies who typed it", actor)
	}
	if strings.TrimSpace(answer) == "" {
		return AnswerResult{}, errors.New("answer is empty")
	}
	answer, _ = CapText(answer, MaxEscalationText)
	now := s.now()
	var out AnswerResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var verb, stored string
		err := tx.QueryRowContext(ctx,
			`SELECT verb, result FROM peer_requests WHERE request_id = ?`, requestID).Scan(&verb, &stored)
		switch {
		case err == nil:
			if verb != requestVerbAnswer {
				return fmt.Errorf("request id %s was already used for %s", requestID, verb)
			}
			if err := json.Unmarshal([]byte(stored), &out); err != nil {
				return fmt.Errorf("request id %s: stored result unreadable: %w", requestID, err)
			}
			if out.Escalation.ID != id {
				return fmt.Errorf("request id %s already answered escalation %d", requestID, out.Escalation.ID)
			}
			out.Replayed = true
			return nil
		case err != sql.ErrNoRows:
			return err
		}

		esc, err := getEscalation(ctx, tx, id)
		if err == sql.ErrNoRows {
			return fmt.Errorf("escalation %d not found", id)
		}
		if err != nil {
			return err
		}
		if esc.Status != EscalationOpen {
			return fmt.Errorf("escalation %d is %s, not open", id, esc.Status)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE escalations SET status = 'answered', answer = ?, answered_by = ?, answer_request_id = ?, resolved_at = ?
			WHERE id = ? AND status = 'open'`,
			answer, actor, requestID, formatTime(now), id); err != nil {
			return err
		}
		payload := fmt.Sprintf("escalation %d", id)
		if esc.TaskID != "" {
			payload += " (task " + esc.TaskID + ")"
		}
		relayer := actor
		if actor == ActorManager {
			relayer = "the manager"
		}
		payload, _ = CapText(payload+" answer relayed by "+relayer+": "+answer, MaxEscalationText)
		eventID, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeManager, EntityID: "manager", Type: EventEscalationAnswered,
			Actor: actor, Actionable: true, Payload: payload,
		})
		if err != nil {
			return err
		}
		if esc, err = getEscalation(ctx, tx, id); err != nil {
			return err
		}
		out = AnswerResult{Escalation: esc, EventID: eventID}
		result, err := json.Marshal(out)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO peer_requests (request_id, verb, result, created_at) VALUES (?, ?, ?, ?)`,
			requestID, requestVerbAnswer, string(result), formatTime(now))
		return err
	})
	if err != nil {
		return AnswerResult{}, err
	}
	return out, nil
}

// SyncResult counts what one SyncApprovalEscalations call changed.
type SyncResult struct {
	Opened   int
	Resolved int
}

// SyncApprovalEscalations makes the approval escalations follow the tasks, in one
// transaction. It first resolves every open approval escalation whose task is no longer done
// or no longer exists, and every one opened for an approval_required event that a human
// approval (an `approved` event) has since answered. Then, for every task that is done, has
// an approval_required event with no `approved` event after it, and has no open approval
// escalation, it opens one for the task's current done episode, unless that episode already
// had one.
//
// The episode is the newest status event that put the task in done, 0 when it got there with
// none. The gate appends approval_required once per task (it checks HasEventType first), so
// keying on the event alone would let a task that goes done -> blocked -> done sit done and
// unapproved with no open escalation. Keying on the episode reopens it once per return to
// done, and the partial UNIQUE index on (task, kind, episode) keeps that exactly-once. An
// escalation answered within an episode stays answered for that episode.
//
// The escalation's created_at is when the task started waiting: the later of the
// approval_required event and the episode's status event. Its body is the event's payload,
// capped. It runs on read (`ttorch decisions`, `ttorch summary`) rather than inside the gate,
// so the gate's own code is untouched; an approval nobody has listed yet is still raised as an
// actionable event, which wakes the manager as it always has.
func (s *Store) SyncApprovalEscalations(ctx context.Context) (SyncResult, error) {
	now := formatTime(s.now())
	var out SyncResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE escalations SET status = 'resolved', resolved_at = ?
			WHERE kind = 'approval' AND status = 'open'
			  AND (task_id IS NULL
			       OR task_id NOT IN (SELECT id FROM tasks WHERE status = ?)
			       OR (source_event_id IS NOT NULL AND EXISTS (
			           SELECT 1 FROM events a WHERE a.entity_type = 'task' AND a.entity_id = escalations.task_id
			             AND a.type = ? AND a.id > escalations.source_event_id)))`,
			now, StatusDone, EventApproved)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		out.Resolved = int(n)

		type candidate struct {
			taskID           string
			eventID, episode int64
			eventTS, payload string
			episodeTS        sql.NullString
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, r.id, r.ts, r.payload,
			       COALESCE(d.id, 0), d.ts
			FROM tasks t
			JOIN events r ON r.id = (
			     SELECT MAX(id) FROM events WHERE entity_type = 'task' AND entity_id = t.id AND type = ?)
			LEFT JOIN events d ON d.id = (
			     SELECT MAX(id) FROM events WHERE entity_type = 'task' AND entity_id = t.id AND to_status = ?)
			WHERE t.status = ?
			  AND NOT EXISTS (SELECT 1 FROM events a WHERE a.entity_type = 'task' AND a.entity_id = t.id
			                  AND a.type = ? AND a.id > r.id)
			  AND NOT EXISTS (SELECT 1 FROM escalations x WHERE x.task_id = t.id AND x.kind = 'approval'
			                  AND (x.status = 'open' OR x.episode_event_id = COALESCE(d.id, 0)))
			ORDER BY t.id`,
			EventApprovalRequired, StatusDone, StatusDone, EventApproved)
		if err != nil {
			return err
		}
		var todo []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.taskID, &c.eventID, &c.eventTS, &c.payload, &c.episode, &c.episodeTS); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range todo {
			body, _ := CapText(c.payload, MaxEscalationText)
			if strings.TrimSpace(body) == "" {
				body = "approval required"
			}
			created := c.eventTS
			if c.episode > c.eventID && c.episodeTS.Valid {
				created = c.episodeTS.String
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO escalations (task_id, kind, body, status, source_event_id, episode_event_id, created_at)
				VALUES (?, 'approval', ?, 'open', ?, ?, ?)`,
				c.taskID, body, c.eventID, c.episode, created); err != nil {
				return err
			}
			out.Opened++
		}
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	return out, nil
}

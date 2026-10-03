package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Requests a parent coordinator sends are made idempotent by the peer_requests ledger
// (migration 0011): a mutating request carries an id the parent minted, its result is stored
// under that id in the transaction that acts on it, and a repeat of the id returns the stored
// result and writes nothing. A retry after a timeout, whose first attempt did succeed, can then
// never add a second task, append a second goal or record a second answer.
//
// Every lookup runs inside the acting transaction. Transactions begin IMMEDIATE (see dsn), so
// the write lock is held before the ledger is read, and two processes racing one request id
// are serialized: the second finds the first one's result.

// The peer_requests verbs a task add and a goal are stored under. An answer is stored under
// requestVerbAnswer.
const (
	RequestVerbTaskAdd = "task-add"
	RequestVerbGoal    = "goal"
)

// ErrRequestReused is a request id that is already stored for a different request: another
// verb, or the same verb acting on another task or escalation.
var ErrRequestReused = errors.New("request id already used for a different request")

// ErrTaskExists is an add for a task id that is already taken.
var ErrTaskExists = errors.New("task already exists")

// storedRequest reads the verb and result stored under requestID, and whether there is one.
func storedRequest(ctx context.Context, q queryer, requestID string) (verb, result string, ok bool, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT verb, result FROM peer_requests WHERE request_id = ?`, requestID).Scan(&verb, &result)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return verb, result, true, nil
}

// storeRequest records result under requestID for verb.
func storeRequest(ctx context.Context, tx *sql.Tx, now time.Time, requestID, verb string, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO peer_requests (request_id, verb, result, created_at) VALUES (?, ?, ?, ?)`,
		requestID, verb, string(b), formatTime(now))
	return err
}

// TaskAdd is one backlog task for AddTask to create.
type TaskAdd struct {
	// Task is the row to insert. HasBrief is set from Brief, whatever it holds here.
	Task Task
	// Actor is recorded as the task's creator and as the created event's actor.
	Actor string
	// RequestID, when set, makes the add idempotent through the peer_requests ledger. Empty
	// adds the task with no ledger entry, and a second add of the id is refused.
	RequestID string
	// Brief is the task's brief, "" for none. When set, WriteBrief must store it.
	Brief string
	// WriteBrief stores Brief where dispatch reads it. It runs inside the transaction, after
	// the row and its created event are written, and an error rolls both back, so a task never
	// exists without the brief it was added with. It must not use the store: the transaction
	// holds the store's only connection.
	WriteBrief func(brief string) error
}

// TaskAddResult is what an add created, and what is stored under its request id. BriefSHA256
// is the hex SHA-256 of the brief, "" when there was none; EventID is the task's created event.
// Replayed is set when the request id had already been used and this is the stored result.
type TaskAddResult struct {
	TaskID      string    `json:"task_id"`
	ProjectID   int64     `json:"project_id"`
	Status      string    `json:"status"`
	HasBrief    bool      `json:"has_brief"`
	BriefSHA256 string    `json:"brief_sha256"`
	EventID     int64     `json:"event_id"`
	CreatedAt   time.Time `json:"created_at"`
	Replayed    bool      `json:"-"`
}

// StoredTaskAdd returns the result stored under requestID by an earlier AddTask, and whether
// there is one. A caller checks it before the work that precedes an add (the brief lint), so a
// repeat is answered with the first result even when that work would now fail; AddTask checks
// again inside its transaction, so a race between the two still replays. An id stored for
// another verb is refused with ErrRequestReused.
func (s *Store) StoredTaskAdd(ctx context.Context, requestID string) (TaskAddResult, bool, error) {
	if err := ValidRequestID(requestID); err != nil {
		return TaskAddResult{}, false, err
	}
	verb, stored, ok, err := storedRequest(ctx, s.db, requestID)
	if err != nil || !ok {
		return TaskAddResult{}, false, err
	}
	if verb != RequestVerbTaskAdd {
		return TaskAddResult{}, false, fmt.Errorf("request id %s is stored for %s: %w", requestID, verb, ErrRequestReused)
	}
	var out TaskAddResult
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return TaskAddResult{}, false, fmt.Errorf("request id %s: stored result unreadable: %w", requestID, err)
	}
	out.Replayed = true
	return out, true, nil
}

// AddTask creates a backlog task, its created event and its brief in one transaction, and,
// when a.RequestID is set, stores the result under it. A repeat of the request id for the same
// task returns the stored result and writes nothing, whatever else the repeat carries; one for a
// different task, or an id stored for another verb, is refused with ErrRequestReused. A task id
// that is already taken is refused with ErrTaskExists and stores nothing.
//
// The brief is written before the commit. If the commit itself then fails, the brief file is
// left behind for a task that does not exist; the next add of that id overwrites it.
func (s *Store) AddTask(ctx context.Context, a TaskAdd) (TaskAddResult, error) {
	if a.RequestID != "" {
		if err := ValidRequestID(a.RequestID); err != nil {
			return TaskAddResult{}, err
		}
	}
	if a.Brief != "" && a.WriteBrief == nil {
		return TaskAddResult{}, errors.New("a task add with a brief needs somewhere to write it")
	}
	t := a.Task
	if a.Actor != "" {
		t.CreatedBy = a.Actor
	}
	s.applyTaskDefaults(&t)
	t.HasBrief = a.Brief != ""
	briefSum := ""
	if t.HasBrief {
		sum := sha256.Sum256([]byte(a.Brief))
		briefSum = hex.EncodeToString(sum[:])
	}
	now := s.now()
	var out TaskAddResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if a.RequestID != "" {
			verb, stored, ok, err := storedRequest(ctx, tx, a.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if verb != RequestVerbTaskAdd {
					return fmt.Errorf("request id %s is stored for %s: %w", a.RequestID, verb, ErrRequestReused)
				}
				if err := json.Unmarshal([]byte(stored), &out); err != nil {
					return fmt.Errorf("request id %s: stored result unreadable: %w", a.RequestID, err)
				}
				if out.TaskID != t.ID {
					return fmt.Errorf("request id %s already added task %s: %w", a.RequestID, out.TaskID, ErrRequestReused)
				}
				out.Replayed = true
				return nil
			}
		}
		if _, exists, err := getTask(ctx, tx, t.ID); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("task %s: %w", t.ID, ErrTaskExists)
		}
		if err := s.insertTaskTx(ctx, tx, t); err != nil {
			return err
		}
		to := t.Status
		eventID, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeTask, EntityID: t.ID, Type: EventCreated, Actor: t.CreatedBy, ToStatus: &to,
		})
		if err != nil {
			return err
		}
		if t.HasBrief {
			if err := a.WriteBrief(a.Brief); err != nil {
				return err
			}
		}
		row, ok, err := getTask(ctx, tx, t.ID)
		if err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("task %s vanished after insert", t.ID)
		}
		out = TaskAddResult{
			TaskID: row.ID, ProjectID: row.ProjectID, Status: row.Status, HasBrief: row.HasBrief,
			BriefSHA256: briefSum, EventID: eventID, CreatedAt: row.Created,
		}
		if a.RequestID == "" {
			return nil
		}
		return storeRequest(ctx, tx, now, a.RequestID, RequestVerbTaskAdd, out)
	})
	if err != nil {
		return TaskAddResult{}, err
	}
	return out, nil
}

// EventGoal is what a goal from the parent coordinator appends: entity manager, actor parent,
// actionable, payload the goal's text, capped.
const EventGoal = "goal"

// GoalResult is what a goal appended: the manager event that carries it. Replayed is set when
// the request id had already been used and this is the stored result.
type GoalResult struct {
	EventID  int64 `json:"event_id"`
	Replayed bool  `json:"-"`
}

// RecordGoal hands the manager plain-language work from the parent coordinator, once per
// request id. In one transaction it appends one actionable event addressed to the manager,
// recorded as the parent's (ActorParent), and stores the result under requestID. A repeat of
// requestID returns the stored event and appends nothing, whatever text it carries; an id
// stored for another verb is refused with ErrRequestReused.
//
// The text is capped at MaxEscalationText, like an answer. The event is the only record of the
// goal: the manager reads it and turns it into tasks.
func (s *Store) RecordGoal(ctx context.Context, requestID, text string) (GoalResult, error) {
	if err := ValidRequestID(requestID); err != nil {
		return GoalResult{}, err
	}
	if strings.TrimSpace(text) == "" {
		return GoalResult{}, errors.New("goal is empty")
	}
	text, _ = CapText(text, MaxEscalationText)
	now := s.now()
	var out GoalResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		verb, stored, seen, err := storedRequest(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if seen {
			if verb != RequestVerbGoal {
				return fmt.Errorf("request id %s was already used for %s: %w", requestID, verb, ErrRequestReused)
			}
			if err := json.Unmarshal([]byte(stored), &out); err != nil {
				return fmt.Errorf("request id %s: stored result unreadable: %w", requestID, err)
			}
			out.Replayed = true
			return nil
		}
		id, err := appendEvent(ctx, tx, now, Event{
			EntityType: EntityTypeManager, EntityID: "manager", Type: EventGoal,
			Actor: ActorParent, Actionable: true, Payload: text,
		})
		if err != nil {
			return err
		}
		out = GoalResult{EventID: id}
		return storeRequest(ctx, tx, now, requestID, RequestVerbGoal, out)
	})
	if err != nil {
		return GoalResult{}, err
	}
	return out, nil
}

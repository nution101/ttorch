package peer

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

// DecisionsSchemaVersion is the schema of `ttorch decisions --json`. It moves on any change to
// a field's name, type or meaning.
const DecisionsSchemaVersion = 1

// MaxText is the cap, in bytes, on every text field printed from an escalation, after
// escaping. It is the store's cap (db.MaxEscalationText); escaping can grow a stored value
// several times over, so the printed form is capped again.
const MaxText = db.MaxEscalationText

// truncated marks a printed field the cap cut short.
const truncated = "…"

// DecisionList is the open escalations as `ttorch decisions` prints them. Every string field
// is escaped (see escape) and capped at MaxText, so either rendering is safe to print to a
// terminal and no field can grow without bound.
type DecisionList struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Open          int              `json:"open"`
	HighestOpenID int64            `json:"highest_open_id"`
	Escalations   []EscalationItem `json:"escalations"`
}

// EscalationItem is one open escalation. TaskID is "" when the escalation names no task;
// SourceEventID is the approval_required event behind it, 0 for one raised by hand.
type EscalationItem struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	TaskID        string    `json:"task_id"`
	Body          string    `json:"body"`
	SourceEventID int64     `json:"source_event_id"`
	CreatedAt     time.Time `json:"created_at"`
	AgeSeconds    *int64    `json:"age_seconds"`
}

// NewDecisionList builds the list from open escalations, as of now.
func NewDecisionList(open []db.Escalation, now time.Time) DecisionList {
	d := DecisionList{SchemaVersion: DecisionsSchemaVersion, GeneratedAt: now.UTC(), Escalations: []EscalationItem{}}
	for _, e := range open {
		d.Escalations = append(d.Escalations, EscalationItem{
			ID:            e.ID,
			Kind:          SafeText(e.Kind),
			TaskID:        SafeText(e.TaskID),
			Body:          SafeText(e.Body),
			SourceEventID: e.SourceEventID,
			CreatedAt:     e.CreatedAt.UTC(),
			AgeSeconds:    age(now, e.CreatedAt),
		})
		d.Open++
		d.HighestOpenID = max(d.HighestOpenID, e.ID)
	}
	return d
}

// WriteJSON writes the list as one indented JSON object and a newline.
func (d DecisionList) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// WriteText writes the list for a person at a terminal: a head line per escalation built from
// its id, kind, task and age, and its body on the line below.
func (d DecisionList) WriteText(w io.Writer) error {
	var b strings.Builder
	switch d.Open {
	case 0:
		b.WriteString("no open escalations\n")
	case 1:
		b.WriteString("1 open escalation\n")
	default:
		fmt.Fprintf(&b, "%d open escalations\n", d.Open)
	}
	if d.Open > 0 {
		b.WriteString("answer with: ttorch answer <id> -m \"...\" (answers are recorded as relayed by the manager)\n")
	}
	for _, e := range d.Escalations {
		task := e.TaskID
		if task == "" {
			task = "-"
		}
		fmt.Fprintf(&b, "#%d  %s  task %s  raised %s ago", e.ID, e.Kind, task, secs(e.AgeSeconds))
		if e.SourceEventID != 0 {
			fmt.Fprintf(&b, " (event %d)", e.SourceEventID)
		}
		fmt.Fprintf(&b, "\n      %s\n", e.Body)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// SafeText is escalation text made safe to print: escaped (see escape) and capped at MaxText
// bytes. Commands that echo an escalation's task id or text use it.
func SafeText(s string) string { return escapeCap(s, MaxText) }

// escapeCap escapes s and cuts the result to at most n bytes, never inside one rune's escape,
// ending a cut value with truncated.
func escapeCap(s string, n int) string {
	e := escape(s)
	if len(e) <= n {
		return e
	}
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		unit := escape(string(r))
		if b.Len()+len(unit)+len(truncated) > n {
			break
		}
		b.WriteString(unit)
	}
	b.WriteString(truncated)
	return b.String()
}

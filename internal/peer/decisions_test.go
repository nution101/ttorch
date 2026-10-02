package peer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

func TestEscapeCap(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"abcdefghijkl", 10, "abcdefg…"},
		// Escaping happens before the cap, and the cut never splits an escape.
		{"\x1b\x1b\x1b\x1b", 10, `\x1b…`},
		{"\x1b\x1b", 8, `\x1b\x1b`},
		{"日本語日本語", 10, "日本…"},
		{`\\\\\\`, 9, `\\\\\\…`},
	}
	for _, c := range cases {
		got := escapeCap(c.in, c.n)
		if got != c.want || len(got) > c.n {
			t.Errorf("escapeCap(%q, %d) = %q (%d bytes), want %q", c.in, c.n, got, len(got), c.want)
		}
	}
}

// TestDecisionListSchema pins the decisions JSON: its version and every field name.
func TestDecisionListSchema(t *testing.T) {
	if DecisionsSchemaVersion != 1 {
		t.Fatalf("DecisionsSchemaVersion = %d; a schema change must update this test", DecisionsSchemaVersion)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	d := NewDecisionList([]db.Escalation{{
		ID: 4, TaskID: "t\u202e1", Kind: db.EscalationApproval, Body: "b\x1b", Status: db.EscalationOpen,
		SourceEventID: 9, CreatedAt: now.Add(-90 * time.Second),
	}}, now)
	var buf bytes.Buffer
	if err := d.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "top", top, "schema_version", "generated_at", "open", "highest_open_id", "escalations")
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(top["escalations"], &items); err != nil || len(items) != 1 {
		t.Fatalf("escalations = %s err=%v", top["escalations"], err)
	}
	assertKeys(t, "escalation", items[0], "id", "kind", "task_id", "body", "source_event_id", "created_at", "age_seconds")
	if d.Open != 1 || d.HighestOpenID != 4 || *d.Escalations[0].AgeSeconds != 90 ||
		d.Escalations[0].TaskID != `t\u202e1` || d.Escalations[0].Body != `b\x1b` {
		t.Errorf("decision list = %+v", d)
	}
	buf.Reset()
	if err := d.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 open escalation", `#4  approval  task t\u202e1  raised 1m30s ago (event 9)`, `      b\x1b`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, buf.String())
		}
	}
}

func assertKeys(t *testing.T, what string, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(m) != len(want) {
		t.Errorf("%s has %d keys, want %d: %v", what, len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q", what, k)
		}
	}
}

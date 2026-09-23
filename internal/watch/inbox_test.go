package watch

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestReadInbox_PrintsUnreadAndAdvances: the inbox prints every unread update (one line per
// entity, latest wins), advances the watermark past them, and a second read with nothing
// new prints an empty inbox without moving the watermark.
func TestReadInbox_PrintsUnreadAndAdvances(t *testing.T) {
	_, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "alpha", "wk-alpha")
	seedActiveTask(t, s, "beta", "wk-beta")
	report(t, s, "alpha", db.StatusBlocked, "needs a decision")
	report(t, s, "alpha", db.StatusActive, "")
	done := report(t, s, "alpha", db.StatusDone, "")
	ask := report(t, s, "beta", db.StatusNeedsInput, "which schema?")

	var out bytes.Buffer
	res, err := ReadInbox(ctx, s, &out)
	if err != nil {
		t.Fatalf("ReadInbox: %v", err)
	}
	if len(res.Batch) != 2 || res.Batch[0].ID != done.ID || res.Batch[1].ID != ask.ID {
		t.Fatalf("batch = %+v, want alpha→done #%d and beta→needs_input #%d", res.Batch, done.ID, ask.ID)
	}
	text := out.String()
	for _, want := range []string{"2 unread update(s)", "task=alpha", "→ done", "which schema?", fmt.Sprintf("INBOX_WATERMARK=%d", ask.ID)} {
		if !strings.Contains(text, want) {
			t.Errorf("inbox output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "needs a decision") {
		t.Errorf("a superseded transition was printed alongside the latest one:\n%s", text)
	}
	m, _, _ := s.GetManager(ctx)
	if m.WatchWatermark != ask.ID {
		t.Fatalf("watermark = %d, want %d", m.WatchWatermark, ask.ID)
	}

	out.Reset()
	again, err := ReadInbox(ctx, s, &out)
	if err != nil {
		t.Fatalf("second ReadInbox: %v", err)
	}
	if len(again.Batch) != 0 || again.Watermark != ask.ID {
		t.Fatalf("second read = %+v, want an empty inbox at #%d", again, ask.ID)
	}
	if !strings.Contains(out.String(), "no unread updates") {
		t.Fatalf("second read should say the inbox is empty:\n%s", out.String())
	}
}

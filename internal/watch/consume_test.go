package watch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

// TestRun_WatermarkModeFiresAndPersists: armed the way cmdWatch arms it (no --since), the
// watcher starts from the stored watermark, surfaces the new update, and leaves the
// watermark on it.
func TestRun_WatermarkModeFiresAndPersists(t *testing.T) {
	w, s, buf, _ := newWatcher(t)
	w.Since = -1
	seedActiveTask(t, s, "alpha", "wk-alpha")
	old := report(t, s, "alpha", db.StatusBlocked, "earlier")
	if err := s.SetWatermark(context.Background(), old.ID); err != nil {
		t.Fatal(err)
	}
	done := report(t, s, "alpha", db.StatusDone, "")

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired || res.Watermark != done.ID || len(res.Batch) != 1 || res.Batch[0].ID != done.ID {
		t.Fatalf("Run = %+v, want a single-row batch for #%d", res, done.ID)
	}
	if strings.Contains(buf.String(), "earlier") {
		t.Fatalf("an update below the stored watermark was surfaced again:\n%s", buf.String())
	}
	m, _, _ := s.GetManager(context.Background())
	if m.WatchWatermark != done.ID {
		t.Fatalf("stored watermark = %d, want %d", m.WatchWatermark, done.ID)
	}
}

// TestRun_NeverResurfacesAnUpdateTheInboxTook: an armed watcher sees an update, and while
// it is absorbing the burst `ttorch inbox` consumes that same update. The watcher must not
// then surface it a second time; with nothing else arriving it times out quietly.
func TestRun_NeverResurfacesAnUpdateTheInboxTook(t *testing.T) {
	w, s, buf, _ := newWatcher(t)
	w.Since = -1
	w.Timeout = time.Second
	seedActiveTask(t, s, "alpha", "wk-alpha")
	report(t, s, "alpha", db.StatusDone, "")

	inner := w.wait
	inboxRan := false
	w.wait = func(ctx context.Context, d time.Duration) error {
		if !inboxRan {
			inboxRan = true
			if _, err := s.ConsumeActionable(ctx, 0); err != nil {
				t.Fatalf("inbox consume: %v", err)
			}
		}
		return inner(ctx, d)
	}

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !inboxRan {
		t.Fatal("setup: the watcher never reached its coalesce wait")
	}
	if res.Fired {
		t.Fatalf("watcher surfaced an update the inbox already consumed: %+v\n%s", res, buf.String())
	}
	if !res.TimedOut {
		t.Fatalf("want a quiet timeout after the inbox took the only update, got %+v", res)
	}
}

// TestRun_SurfacesOnlyWhatTheInboxLeft: the inbox took the first update during the
// watcher's coalesce window and a second one arrives afterwards. The watcher surfaces the
// second one only.
func TestRun_SurfacesOnlyWhatTheInboxLeft(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	w.Since = -1
	w.Timeout = time.Minute
	seedActiveTask(t, s, "alpha", "wk-alpha")
	seedActiveTask(t, s, "beta", "wk-beta")
	report(t, s, "alpha", db.StatusDone, "")

	inner := w.wait
	var second db.Event
	w.wait = func(ctx context.Context, d time.Duration) error {
		if second.ID == 0 {
			if _, err := s.ConsumeActionable(ctx, 0); err != nil {
				t.Fatalf("inbox consume: %v", err)
			}
			second = report(t, s, "beta", db.StatusNeedsInput, "which schema?")
		}
		return inner(ctx, d)
	}

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Fired || len(res.Batch) != 1 || res.Batch[0].ID != second.ID {
		t.Fatalf("Run = %+v, want only beta's update #%d", res, second.ID)
	}
}

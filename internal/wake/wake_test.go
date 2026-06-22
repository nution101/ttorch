package wake

import (
	"path/filepath"
	"testing"
)

func TestQueue_AppendDrainDedupe(t *testing.T) {
	q := Queue{Path: filepath.Join(t.TempDir(), "wake-queue")}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.Append("signal", "a", "a.turn-ended"))
	must(q.Append("signal", "b", "b.status"))
	must(q.Append("heartbeat", "", ""))
	must(q.Append("signal", "a", "a.status")) // duplicate kind+key -> collapsed
	must(q.Append("heartbeat", "", ""))       // duplicate heartbeat -> collapsed

	wakes, err := q.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(wakes) != 3 {
		t.Fatalf("got %d wakes, want 3: %+v", len(wakes), wakes)
	}
	// First-seen order preserved: signal a, signal b, heartbeat.
	if wakes[0].Key != "a" || wakes[1].Key != "b" || wakes[2].Kind != "heartbeat" {
		t.Fatalf("unexpected order: %+v", wakes)
	}

	// Draining again yields nothing.
	again, err := q.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second drain returned %d, want 0", len(again))
	}
}

func TestQueue_DrainEmpty(t *testing.T) {
	q := Queue{Path: filepath.Join(t.TempDir(), "wake-queue")}
	w, err := q.Drain()
	if err != nil || w != nil {
		t.Fatalf("empty drain: w=%v err=%v", w, err)
	}
}

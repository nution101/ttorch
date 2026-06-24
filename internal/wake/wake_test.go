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

func TestFilter(t *testing.T) {
	ws := []Wake{
		{Kind: "signal", Key: "t1", Payload: "t1.turn-ended"},
		{Kind: "stale", Key: "t2", Payload: "wk-t2"},
		{Kind: "heartbeat", Key: "", Payload: ""},
		{Kind: "signal", Key: "t1", Payload: "t1.status"},
	}

	// A task-scoped waiter returns only its task's wakes; every other wake (incl.
	// other tasks and the keyless heartbeat) lands in rest so it can be re-queued.
	matched, rest := Filter("t1", ws)
	if len(matched) != 2 {
		t.Fatalf("matched = %d, want 2: %+v", len(matched), matched)
	}
	for _, w := range matched {
		if w.Key != "t1" {
			t.Fatalf("matched contains a non-t1 wake: %+v", w)
		}
	}
	if len(rest) != 2 {
		t.Fatalf("rest = %d, want 2 (stale t2 + heartbeat): %+v", len(rest), rest)
	}
	for _, w := range rest {
		if w.Key == "t1" {
			t.Fatalf("rest leaked a t1 wake: %+v", w)
		}
	}

	// An empty task (plain `ttorch wait`) matches everything and re-queues nothing.
	all, none := Filter("", ws)
	if len(all) != len(ws) || none != nil {
		t.Fatalf("empty filter: matched=%d rest=%v, want %d/nil", len(all), none, len(ws))
	}
}

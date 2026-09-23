package board

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestDispatchFailsClosedWhenTheFleetCannotBeRead: a dispatch must refuse, before claiming,
// when the live fleet cannot be read, rather than treat the unreadable fleet as empty. The
// daemon's tick aborts on the same failure. The undeclared footprint matters: the spawn's own
// overlap check never runs for one, so only the board's read stands between it and a dispatch.
func TestDispatchFailsClosedWhenTheFleetCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.addTask(db.Task{ID: "disjoint", HasBrief: true, Footprint: []string{"b.go"}})
	h.addTask(db.Task{ID: "undeclared", HasBrief: true})
	h.fleet.readErr = errors.New("database is locked")
	for _, id := range []string{"disjoint", "undeclared"} {
		if res := decode(t, h.action("/api/dispatch", url.Values{"task": {id}})); res.OK {
			t.Fatalf("dispatch of %s against an unreadable fleet succeeded: %+v", id, res)
		}
		got, _, _ := h.store.GetTask(context.Background(), id)
		if got.Status != db.StatusPending {
			t.Fatalf("%s is %s after a refused dispatch, want pending", id, got.Status)
		}
		tl, err := h.store.Timeline(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range tl {
			if it.Event != nil && it.Event.Type == db.EventStatusChanged {
				t.Fatalf("%s was claimed (%s) although the fleet could not be read", id, it.Event.Payload)
			}
		}
	}
	if len(h.fleet.spawns) != 0 {
		t.Fatalf("spawned %v against an unreadable fleet", h.fleet.spawns)
	}
}

// liveWorker puts a live, windowed worker holding footprint into the fake fleet.
func (h *harness) liveWorker(id string, footprint ...string) {
	h.fleet.live = append(h.fleet.live, db.Task{
		ID: id, Project: h.proj.RepoPath, Status: db.StatusActive, Window: "wk-" + id, Footprint: footprint,
	})
	h.fleet.liveWindows = append(h.fleet.liveWindows, "wk-"+id)
}

// TestDispatchForcesOverlapOnlyForARealConflict: like the daemon, the board overrides the
// spawn's overlap gate only for a footprint that conflicts with a live worker, so a disjoint
// dispatch keeps the gate's fail-closed check.
func TestDispatchForcesOverlapOnlyForARealConflict(t *testing.T) {
	h := newHarness(t)
	h.liveWorker("w1", "internal/a.go")
	h.addTask(db.Task{ID: "disjoint", HasBrief: true, Footprint: []string{"internal/b.go"}})
	h.addTask(db.Task{ID: "overlapping", HasBrief: true, Footprint: []string{"internal/a.go"}})
	h.addTask(db.Task{ID: "undeclared", HasBrief: true})
	for _, id := range []string{"disjoint", "overlapping", "undeclared"} {
		if res := decode(t, h.action("/api/dispatch", url.Values{"task": {id}})); !res.Done {
			t.Fatalf("dispatch %s: %+v", id, res)
		}
	}
	want := []bool{false, true, false}
	for i, got := range h.fleet.forced {
		if got != want[i] {
			t.Fatalf("forceOverlap per dispatch = %v, want %v (disjoint, overlapping, undeclared)", h.fleet.forced, want)
		}
	}
}

// TestDispatchRefusesAnOverlapWhenSerialized: with TTORCH_SERIALIZE_OVERLAP set the daemon
// leaves an overlapping task pending, and so does the board.
func TestDispatchRefusesAnOverlapWhenSerialized(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.SerializeOverlap = true
	h.liveWorker("w1", "internal/a.go")
	h.addTask(db.Task{ID: "overlapping", HasBrief: true, Footprint: []string{"internal/a.go"}})
	if res := decode(t, h.action("/api/dispatch", url.Values{"task": {"overlapping"}})); res.OK {
		t.Fatalf("serialized overlapping dispatch succeeded: %+v", res)
	}
	if len(h.fleet.spawns) != 0 {
		t.Fatalf("spawned %v", h.fleet.spawns)
	}
	if got, _, _ := h.store.GetTask(context.Background(), "overlapping"); got.Status != db.StatusPending {
		t.Fatalf("task is %s, want pending", got.Status)
	}
}

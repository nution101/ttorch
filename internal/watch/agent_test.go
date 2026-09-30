package watch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/proc"
)

// A present, readable window whose recorded agent has exited surfaces one actionable
// agent_exited event naming the window and the reason, and is not re-flagged next sweep.
func TestPollLiveness_AgentExited(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) {
		return proc.AgentExited, "agent process 4242 has exited"
	}

	for i := 0; i < 3; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	rows, err := s.EventsSince(ctx, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var exited []db.Event
	for _, e := range rows {
		if e.EntityID != "fp" {
			continue
		}
		if e.Type == db.EventIdleUnreported || e.Type == db.EventWindowGone {
			t.Fatalf("an exited agent was reported as %s", e.Type)
		}
		if e.Type == db.EventAgentExited {
			exited = append(exited, e)
		}
	}
	if len(exited) != 1 {
		t.Fatalf("agent_exited events = %d, want exactly 1", len(exited))
	}
	var entry bytes.Buffer
	writeUpdateEntry(&entry, exited[0])
	for _, want := range []string{`agent-exited task="fp"`, "peek it, then respawn or tear down", `window: "wk-fp (agent process 4242 has exited)"`} {
		if !strings.Contains(entry.String(), want) {
			t.Errorf("update entry %q does not contain %q", entry.String(), want)
		}
	}
}

// A check that could not complete (a ps failure) is neither exited nor alive. It raises no
// event of its own, and it does not stop the sweep: the pane path still runs, so an idle
// worker is flagged idle_unreported as if it had no fingerprint.
func TestPollLiveness_AgentUnknownTakesPanePath(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentUnknown, "ps: exit status 2" }

	for i := 0; i < idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	if idle, _ := s.HasEventType(ctx, "fp", db.EventIdleUnreported); !idle {
		t.Fatal("an unknown agent state skipped the idle check")
	}
	if exited, _ := s.HasEventType(ctx, "fp", db.EventAgentExited); exited {
		t.Fatal("an unknown agent state produced an agent_exited event")
	}
}

// A corrupt fingerprint file (here only a pid) cannot be loaded, which reads as unknown. It
// must not hide an idle worker: the production seam still lets idle_unreported fire.
func TestPollLiveness_CorruptFingerprintStillFlagsIdle(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "fp", "wk-fp")
	path := w.P.AgentFingerprintPath("fp")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pid=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, _, _ := s.GetTask(ctx, "fp")
	if st, _ := w.agent(task); st != proc.AgentUnknown {
		t.Fatalf("production seam with a corrupt fingerprint = %v, want unknown", st)
	}

	for i := 0; i < idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	if idle, _ := s.HasEventType(ctx, "fp", db.EventIdleUnreported); !idle {
		t.Fatal("a corrupt fingerprint file hid an idle worker from idle_unreported")
	}
	if exited, _ := s.HasEventType(ctx, "fp", db.EventAgentExited); exited {
		t.Fatal("a corrupt fingerprint file produced an agent_exited event")
	}
}

// With no stored fingerprint the watcher behaves as before: an idle, stable pane is
// flagged idle_unreported, and no agent_exited is emitted. This uses the production agent
// seam, which reads an absent fingerprint file as untracked without touching tmux.
func TestPollLiveness_NoFingerprintKeepsWindowPresence(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "legacy", "wk-legacy")
	task, _, _ := s.GetTask(ctx, "legacy")
	if st, _ := w.agent(task); st != proc.AgentUntracked {
		t.Fatalf("production seam with no fingerprint = %v, want untracked", st)
	}

	for i := 0; i < idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	idle, err := s.HasEventType(ctx, "legacy", db.EventIdleUnreported)
	if err != nil || !idle {
		t.Fatalf("expected idle_unreported for an untracked idle worker (has=%v err=%v)", idle, err)
	}
	exited, err := s.HasEventType(ctx, "legacy", db.EventAgentExited)
	if err != nil || exited {
		t.Fatalf("untracked worker got an agent_exited event (has=%v err=%v)", exited, err)
	}
}

// A live agent takes the normal pane path.
func TestPollLiveness_AgentAliveTakesPanePath(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentAlive, "" }

	for i := 0; i < idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	if idle, _ := s.HasEventType(ctx, "fp", db.EventIdleUnreported); !idle {
		t.Fatal("a live, idle agent should still be flagged idle_unreported")
	}
	if exited, _ := s.HasEventType(ctx, "fp", db.EventAgentExited); exited {
		t.Fatal("a live agent got an agent_exited event")
	}
}

// agent_exited does not mask the task's later liveness events. The fingerprint file has no
// integrity check, so one that always reads as exited must not be able to hide a window that
// then disappears: window_gone still fires after an agent_exited for the same task.
func TestPollLiveness_AgentExitedDoesNotMaskWindowGone(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentExited, "forged" }

	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	if has, _ := s.HasEventType(ctx, "fp", db.EventAgentExited); !has {
		t.Fatal("first sweep did not raise agent_exited")
	}
	w.capture = func(string) paneObservation { return paneObservation{} }
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	if has, _ := s.HasEventType(ctx, "fp", db.EventWindowGone); !has {
		t.Fatal("window_gone was suppressed by an earlier agent_exited")
	}
}

// The same holds for idle_unreported: once agent_exited has been raised, the idle pane path
// still runs and flags the worker, and agent_exited itself is raised only once.
func TestPollLiveness_AgentExitedDoesNotMaskIdleUnreported(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentExited, "forged" }

	for i := 0; i < idleStaleSweeps+2; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	if has, _ := s.HasEventType(ctx, "fp", db.EventIdleUnreported); !has {
		t.Fatal("idle_unreported was suppressed by an earlier agent_exited")
	}
	rows, err := s.EventsSince(ctx, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	exited := 0
	for _, e := range rows {
		if e.EntityID == "fp" && e.Type == db.EventAgentExited {
			exited++
		}
	}
	if exited != 1 {
		t.Fatalf("agent_exited events = %d, want exactly 1", exited)
	}
}

// Another actionable event still suppresses the sweep as before: a task already flagged
// idle_unreported is not re-flagged, and gets no agent_exited on top while it stays surfaced.
func TestPollLiveness_OtherSurfacedEventStillSuppresses(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	seedActiveTask(t, s, "fp", "wk-fp")
	if _, err := s.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: "fp", Type: db.EventIdleUnreported,
		Actor: db.ActorSystem, Actionable: true, Payload: "wk-fp",
	}); err != nil {
		t.Fatal(err)
	}
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentExited, "gone" }
	w.capture = func(string) paneObservation { return paneObservation{} }
	if err := w.pollLiveness(ctx); err != nil {
		t.Fatalf("pollLiveness: %v", err)
	}
	for _, typ := range []string{db.EventAgentExited, db.EventWindowGone} {
		if has, _ := s.HasEventType(ctx, "fp", typ); has {
			t.Fatalf("an already-surfaced task got a %s event", typ)
		}
	}
}

// In the surfaced batch, agent_exited is deduplicated on its own: it never displaces another
// event for the same task, and a later event never displaces it.
func TestDedupeByEntity_AgentExitedKeptBesideOtherEvents(t *testing.T) {
	rows := []db.Event{
		{ID: 1, EntityID: "a", Type: db.EventIdleUnreported},
		{ID: 2, EntityID: "a", Type: db.EventAgentExited},
		{ID: 3, EntityID: "a", Type: db.EventAgentExited},
		{ID: 4, EntityID: "b", Type: db.EventAgentExited},
		{ID: 5, EntityID: "b", Type: db.EventWindowGone},
		{ID: 6, EntityID: "c", Type: db.EventWindowGone},
		{ID: 7, EntityID: "c", Type: db.EventIdleUnreported},
	}
	var got []int64
	for _, e := range dedupeByEntity(rows) {
		got = append(got, e.ID)
	}
	want := []int64{1, 3, 4, 5, 7}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dedupeByEntity ids = %v, want %v", got, want)
	}
}

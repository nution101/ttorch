package watch

import (
	"context"
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
	line := formatEventLine(exited[0])
	for _, want := range []string{"agent-exited", "task=fp", "wk-fp", "agent process 4242 has exited"} {
		if !strings.Contains(line, want) {
			t.Errorf("event line %q does not contain %q", line, want)
		}
	}
}

// A check that could not complete (a ps failure) is neither exited nor alive: no event, and
// the idle bookkeeping is left alone rather than advanced on a pane it could not vouch for.
func TestPollLiveness_AgentUnknownIsSkipped(t *testing.T) {
	w, s, _, _ := newWatcher(t)
	ctx := context.Background()
	w.Dwell = -1
	seedActiveTask(t, s, "fp", "wk-fp")
	w.agent = func(db.Task) (proc.AgentState, string) { return proc.AgentUnknown, "ps: exit status 2" }

	for i := 0; i < 2*idleStaleSweeps+1; i++ {
		if err := w.pollLiveness(ctx); err != nil {
			t.Fatalf("pollLiveness: %v", err)
		}
	}
	rows, err := s.EventsSince(ctx, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.EntityID == "fp" && e.Type != db.EventCreated {
			t.Fatalf("unknown agent state produced a %s event", e.Type)
		}
	}
	task, _, err := s.GetTask(ctx, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if task.IdleSweeps != 0 || task.LastPaneHash != "" {
		t.Fatalf("idle bookkeeping advanced on an unknown agent: sweeps=%d hash=%q", task.IdleSweeps, task.LastPaneHash)
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

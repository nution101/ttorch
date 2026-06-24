package supervisor

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/wake"
)

func TestTick_SignalsHeartbeatAndDedup(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(p)
	q := wake.Queue{Path: p.WakeQueue()}

	// A worker reports a turn boundary.
	if err := os.WriteFile(p.TurnEndMarker("t1"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	s.tick()
	ws, _ := q.Drain()
	if !hasWake(ws, "signal", "t1") {
		t.Fatalf("expected a signal wake for t1, got %+v", ws)
	}

	// Unchanged file -> no repeat signal.
	s.tick()
	ws, _ = q.Drain()
	if hasWake(ws, "signal", "t1") {
		t.Fatal("signal re-fired for an unchanged marker")
	}

	// Heartbeat fires when due.
	s.lastHeartbeat = time.Now().Add(-time.Hour)
	s.tick()
	ws, _ = q.Drain()
	if !hasWake(ws, "heartbeat", "") {
		t.Fatalf("expected a heartbeat wake, got %+v", ws)
	}
}

func hasWake(ws []wake.Wake, kind, key string) bool {
	for _, w := range ws {
		if w.Kind == kind && w.Key == key {
			return true
		}
	}
	return false
}

func TestRunningFalseWhenNoPID(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	if _, ok := Running(paths.Default()); ok {
		t.Fatal("Running should be false with no pid file")
	}
}

func TestStart_NoOpWhenRunning(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	p := paths.Default()
	if err := os.MkdirAll(p.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live supervisor is represented by a pid file naming a running process; our
	// own pid is alive, so Start must treat the singleton as held and launch nothing.
	if err := os.WriteFile(p.PIDFile(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, started, err := Start(p)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if started {
		t.Fatal("Start launched a second supervisor while one was running")
	}
	if pid != os.Getpid() {
		t.Fatalf("Start returned pid %d, want the running pid %d", pid, os.Getpid())
	}
}

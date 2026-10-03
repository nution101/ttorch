package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/gittest"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/proc"
	"github.com/nution101/ttorch/internal/tmux"
)

func TestDeriveLiveState(t *testing.T) {
	tests := []struct {
		agent   proc.AgentState
		present string // what the hook record and pane say (liveState)
		want    string
	}{
		{proc.AgentAlive, "idle", "idle"},
		{proc.AgentAlive, "working", "working"},
		{proc.AgentExited, "working", StateAgentExited}, // a stale busy frame or hook record does not revive a dead agent
		{proc.AgentExited, "idle", StateAgentExited},
		{proc.AgentUnknown, "idle", StateUnknown},
		{proc.AgentUnknown, "working", StateUnknown},
		{proc.AgentUntracked, "idle", "idle"}, // no fingerprint: window presence, as before
		{proc.AgentUntracked, "working", "working"},
	}
	for _, tc := range tests {
		if got := deriveLiveState(tc.agent, tc.present); got != tc.want {
			t.Errorf("deriveLiveState(%v, %q) = %q, want %q", tc.agent, tc.present, got, tc.want)
		}
	}
}

// fingerprintTestRepo makes a one-commit git repo to spawn a worker against.
func fingerprintTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := gittest.Command(repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

// waitTaskState polls TaskState until it reads want, returning the last state seen.
func waitTaskState(m *Manager, id, want string) string {
	var got string
	for i := 0; i < 50; i++ {
		task, _, _ := m.Store.GetTask(context.Background(), id)
		if got = m.TaskState(task); got == want {
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
	return got
}

// TestTaskState_AgentExitedLeavesShell spawns a real worker window, kills its agent so only
// the pane's shell is left, and checks `ttorch status` reads agent-exited rather than idle.
// A new process started in the same pane is not the agent ttorch launched either. With the
// fingerprint removed, the task falls back to window presence.
func TestTaskState_AgentExitedLeavesShell(t *testing.T) {
	skipIfShort(t)
	if !tmux.Available() {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := fingerprintTestRepo(t)
	session := fmt.Sprintf("ttorch-fp-test-%d", os.Getpid())
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_DB", filepath.Join(t.TempDir(), "state.db"))
	t.Setenv("TTORCH_TMUX_SESSION", session)
	t.Setenv("TTORCH_NO_AUTOINIT", "1")
	t.Cleanup(func() { testTmux.Command("kill-session", "-t", session).Run() })

	m, err := New(paths.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	task, err := m.Spawn("fp1", repo, false, "sleep 300")
	if err != nil {
		t.Fatal(err)
	}
	fpPath := m.P.AgentFingerprintPath("fp1")
	fp, ok, err := proc.LoadFingerprint(fpPath)
	if err != nil || !ok {
		t.Fatalf("spawn recorded no agent fingerprint: ok %v err %v", ok, err)
	}
	pane, err := tmux.PanePIDErr(m.Session, task.Window)
	if err != nil {
		t.Fatal(err)
	}
	if fp.PID == pane {
		t.Fatalf("fingerprinted the pane shell (pid %d), want its child", pane)
	}
	if got := m.TaskState(task); got != "idle" {
		t.Fatalf("live agent: TaskState = %q, want idle", got)
	}

	// Kill the agent the way a crash or a stray Ctrl-C does: the shell survives and the
	// window stays.
	if err := tmux.SendKey(m.Session, task.Window, "C-c"); err != nil {
		t.Fatal(err)
	}
	if got := waitTaskState(m, "fp1", StateAgentExited); got != StateAgentExited {
		t.Fatalf("agent killed, shell left: TaskState = %q, want %q", got, StateAgentExited)
	}
	if !m.Live(task) {
		t.Fatal("the window should still be present; this case is about a present window")
	}

	// Something else started in the same pane is not the launched agent.
	if err := tmux.SendLine(m.Session, task.Window, "sleep 301"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if got := m.TaskState(task); got != StateAgentExited {
		t.Fatalf("another process in the pane: TaskState = %q, want %q", got, StateAgentExited)
	}

	// No stored fingerprint (a task spawned before this change): window presence decides.
	if err := proc.RemoveFingerprint(fpPath); err != nil {
		t.Fatal(err)
	}
	if got := m.TaskState(task); got != "idle" {
		t.Fatalf("no fingerprint: TaskState = %q, want idle (window presence)", got)
	}
}

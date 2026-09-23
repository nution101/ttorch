package orchestrator

import (
	"fmt"
	"os"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/proc"
)

// The agent fingerprint lives in the task's data dir (paths.AgentFingerprintPath, beside its
// brief) rather than in a tasks column. It describes one incarnation of one process on this
// host, and every launch replaces or clears it, so it is runtime state of the window in the
// same way the brief is launch input for it; nothing queries it across tasks. Keeping it out
// of the schema also means no migration: migrations are keyed by version number, and two
// changes adding the next number in parallel would collide on land.

// StateAgentExited is the `ttorch status` state of a task whose window is present but whose
// launched agent is not the process in it any more (the agent died and left its shell, or
// the pid now belongs to something else). StateUnknown is a window whose agent could not be
// checked: neither alive nor exited.
const (
	StateAgentExited = "agent-exited"
	StateUnknown     = "unknown"
)

// fingerprintSettle is the gap between the two reads recordAgentFingerprint compares, so a
// launcher caught mid-exec is not recorded as the agent. A var so tests can shrink it.
var fingerprintSettle = 200 * time.Millisecond

// clearAgentFingerprint drops any fingerprint recorded for taskID, so a launch whose agent
// is not (yet) fingerprinted is judged by window presence and never against the process a
// previous incarnation recorded.
func (m *Manager) clearAgentFingerprint(taskID string) {
	if err := proc.RemoveFingerprint(m.P.AgentFingerprintPath(taskID)); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not clear the agent fingerprint for %s: %v\n", taskID, err)
	}
}

// recordAgentFingerprint records the identity of the agent now running in window, once the
// launch has taken over the pane. It reads twice, fingerprintSettle apart, and records only
// a fingerprint that is the same both times. It is best-effort: a task without a
// fingerprint keeps window-presence liveness, so a failed read is reported and never fails
// the spawn.
func (m *Manager) recordAgentFingerprint(taskID, window string) {
	fp, err := m.stableAgentFingerprint(window)
	if err == nil {
		err = proc.SaveFingerprint(m.P.AgentFingerprintPath(taskID), fp)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: no agent fingerprint for %s, its liveness falls back to window presence: %v\n", taskID, err)
	}
}

func (m *Manager) stableAgentFingerprint(window string) (proc.Fingerprint, error) {
	read := func() (proc.Fingerprint, error) {
		pane, err := m.backend().PanePIDErr(m.Session, window)
		if err != nil {
			return proc.Fingerprint{}, err
		}
		return proc.AgentOf(pane)
	}
	first, err := read()
	if err != nil {
		return proc.Fingerprint{}, err
	}
	time.Sleep(fingerprintSettle)
	second, err := read()
	if err != nil {
		return proc.Fingerprint{}, err
	}
	if first != second {
		return proc.Fingerprint{}, fmt.Errorf("the agent process changed while it was read (pid %d, then pid %d)", first.PID, second.PID)
	}
	return first, nil
}

// agentState checks task t's recorded agent against the process in its window.
func (m *Manager) agentState(t db.Task) (proc.AgentState, string) {
	return proc.StoredAgentState(m.P.AgentFingerprintPath(t.ID), func() (int, error) {
		return m.backend().PanePIDErr(m.Session, t.Window)
	})
}

// deriveLiveState combines the agent check with present, the state a present window reads
// from its hook record and pane (liveState). An exited or unchecked agent overrides it: a
// stale hook record or busy frame does not revive a dead agent. A task with no fingerprint
// (AgentUntracked) keeps present, the window-presence answer.
func deriveLiveState(agent proc.AgentState, present string) string {
	switch agent {
	case proc.AgentExited:
		return StateAgentExited
	case proc.AgentUnknown:
		return StateUnknown
	}
	return present
}

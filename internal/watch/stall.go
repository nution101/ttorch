package watch

// The stall ladder keeps saying that a worker has gone silent. idle_unreported (§4.4)
// flags an idle worker once per episode and then goes quiet, and the only backstop after
// that is the two-hour task lease. The ladder closes the gap: a per-worker "no progress
// observed" clock, and an actionable `stalled` update each time it crosses a threshold —
// the first after TTORCH_STALL_AFTER, then again every TTORCH_STALL_REPEAT. Once
// TTORCH_STALL_RERAISES re-raises have gone unanswered, the updates carry the
// needs-inspection level instead. The ladder only reports; lease expiry and recovery are
// untouched.
//
// Progress is any of: the pane changed (a busy pane is always changing), the worker
// reported or staged (which extends its lease), its status changed or it was
// re-dispatched, or a new commit appeared in its worktree. Any of them restarts the clock
// and puts the worker back at the bottom of the ladder. What the ladder flags is a worker
// sitting at an idle, unchanging prompt while its task is still active.
//
// The clock lives on the event spine (db.EventStallClock / db.EventStalled), not in the
// Watcher, because the watcher exits every time it surfaces a batch; an in-memory timer
// would restart on every arm and a busy fleet would never let it reach the threshold.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/livestate"
)

// Ladder defaults, each overridable by the environment variable named beside it.
const (
	defaultStallAfter    = 10 * time.Minute // TTORCH_STALL_AFTER: quiet this long ⇒ first `stalled` update; 0/off disables the ladder
	defaultStallRepeat   = 15 * time.Minute // TTORCH_STALL_REPEAT: re-raise cadence while the worker stays silent
	defaultStallReraises = 3                // TTORCH_STALL_RERAISES: re-raises before the level becomes needs-inspection
)

// stallClockGap is the minimum spacing between two stall_clock writes for one task. It
// bounds the audit rows a pane that flickers every sweep can produce; a restart it defers
// is written on a later sweep, and no update is raised in the meantime because the pane
// does not match the clock.
const stallClockGap = time.Minute

// minStallInterval is the floor for TTORCH_STALL_AFTER and TTORCH_STALL_REPEAT. A smaller
// value is raised to it: without a floor, REPEAT=1ns writes a stalled row and wakes the
// manager on every poll.
const minStallInterval = time.Minute

// Ladder levels, carried in the stalled payload.
const (
	stallLevelStalled = "stalled"
	stallLevelInspect = "needs-inspection"
)

// stallPolicy is the ladder's thresholds. After <= 0 turns the ladder off.
type stallPolicy struct {
	After    time.Duration
	Repeat   time.Duration
	Reraises int
}

// stallTracker holds the ladder's policy and its one external seam.
type stallTracker struct {
	policy stallPolicy
	// headCommitTime reports the committer time of HEAD in a worktree (ok=false when it
	// cannot be read). Tests swap it out.
	headCommitTime func(dir string) (time.Time, bool)
}

func newStallTracker() stallTracker {
	return stallTracker{policy: stallPolicyFromEnv(), headCommitTime: gitHeadCommitTime}
}

// stallPolicyFromEnv reads the ladder thresholds. A missing or invalid value falls back
// to its default, a positive duration under minStallInterval is raised to it, and
// TTORCH_STALL_AFTER set to 0/off/false/no disables the ladder.
func stallPolicyFromEnv() stallPolicy {
	p := stallPolicy{After: defaultStallAfter, Repeat: defaultStallRepeat, Reraises: defaultStallReraises}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_STALL_AFTER"))); v != "" {
		switch v {
		case "0", "off", "false", "no":
			p.After = 0
		default:
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				p.After = max(d, minStallInterval)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("TTORCH_STALL_REPEAT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.Repeat = max(d, minStallInterval)
		}
	}
	if v := strings.TrimSpace(os.Getenv("TTORCH_STALL_RERAISES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.Reraises = n
		}
	}
	return p
}

// gitHeadCommitTime is the production headCommitTime. The worktree belongs to the worker,
// so its git config is worker-controlled, and a porcelain read such as `git log` honours
// settings that run programs (log.showSignature hands a signed commit to gpg.program). So
// it reads the raw commit object with `cat-file`, which formats nothing and verifies
// nothing, and parses the committer line itself. core.fsmonitor is forced off as well, so
// no index refresh can launch a monitor hook.
func gitHeadCommitTime(dir string) (time.Time, bool) {
	if dir == "" {
		return time.Time{}, false
	}
	out, err := exec.Command("git", "-C", dir, "-c", "core.fsmonitor=false", "cat-file", "commit", "HEAD").Output()
	if err != nil {
		return time.Time{}, false
	}
	return committerTime(string(out))
}

// committerTime parses the committer timestamp from a raw commit object: the header line
// "committer <name> <email> <unix-seconds> <tz>", before the first blank line.
func committerTime(obj string) (time.Time, bool) {
	for _, line := range strings.Split(obj, "\n") {
		if line == "" {
			break // end of the header; the message follows
		}
		if !strings.HasPrefix(line, "committer ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return time.Time{}, false
		}
		sec, err := strconv.ParseInt(f[len(f)-2], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(sec, 0), true
	}
	return time.Time{}, false
}

// stallPayload is the JSON payload of a stalled event. Raise counts from 1 within one
// silent stretch; Idle is how long since progress was last observed.
type stallPayload struct {
	Level  string `json:"level"`
	Raise  int    `json:"raise"`
	Idle   string `json:"idle"`
	Window string `json:"window"`
}

// trackStall advances one task's stall ladder from this sweep's pane observation. It is
// called from pollLiveness for every active worker past the quiet-hold gate, BEFORE the
// already-surfaced gate: the ladder's job is to keep re-raising a worker the manager has
// already been told about, and its own actionable updates would otherwise mask it. A gone
// or unreadable window is left to the rest of the liveness poll.
func (w *Watcher) trackStall(ctx context.Context, now time.Time, t db.Task, obs paneObservation) error {
	p := w.stall.policy
	if p.After <= 0 || !obs.present || !obs.captured {
		return nil
	}
	st, err := w.Store.StallInfo(ctx, t.ID)
	if err != nil {
		return err
	}
	busy := livestate.Busy(obs.pane)
	pane := ""
	if !busy {
		pane = hashPane(obs.pane)
	}
	if !st.HasClock || st.ClockPane != pane || progressSinceClock(t, st) {
		return w.restartStallClock(ctx, now, t.ID, st, pane)
	}
	if busy {
		return nil // mid-turn: the clock stays stopped
	}

	n := len(st.Raises)
	due := st.ClockAt.Add(p.After)
	if n > 0 {
		due = st.Raises[n-1].Add(p.Repeat)
	}
	if now.Before(due) {
		return nil
	}
	// A commit is only checked once an update is due, so the git call runs rarely.
	if at, ok := w.stall.headCommitTime(t.Worktree); ok && at.After(st.ClockAt) {
		return w.restartStallClock(ctx, now, t.ID, st, pane)
	}

	level := stallLevelStalled
	if n > p.Reraises {
		level = stallLevelInspect
	}
	payload, err := json.Marshal(stallPayload{
		Level:  level,
		Raise:  n + 1,
		Idle:   now.Sub(st.ClockAt).Round(time.Second).String(),
		Window: t.Window,
	})
	if err != nil {
		return err
	}
	_, err = w.Store.AppendEvent(ctx, db.Event{
		TS: now, EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventStalled,
		Actor: db.ActorSystem, Actionable: true, Payload: string(payload),
	})
	return err
}

// progressSinceClock reports progress the task's DB row or event spine shows since the
// clock last restarted: a sign of life (worker event, status change, re-dispatch), a
// report/stage (last_progress_at), or a lease grant or extension.
func progressSinceClock(t db.Task, st db.StallState) bool {
	if st.SignOfLifeID > st.ClockID {
		return true
	}
	if t.LastProgressAt != nil && t.LastProgressAt.After(st.ClockAt) {
		return true
	}
	if t.LeaseExpiresAt != nil && t.LeaseExpiresAt.Add(-db.DefaultLeaseDuration).After(st.ClockAt) {
		return true
	}
	return false
}

// restartStallClock records progress: a stall_clock at now carrying pane (the idle pane's
// hash, or "" while busy). Within stallClockGap of the previous clock the write is
// deferred to a later sweep.
func (w *Watcher) restartStallClock(ctx context.Context, now time.Time, taskID string, st db.StallState, pane string) error {
	if st.HasClock {
		if d := now.Sub(st.ClockAt); d >= 0 && d < stallClockGap {
			return nil
		}
	}
	_, err := w.Store.AppendEvent(ctx, db.Event{
		TS: now, EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventStallClock,
		Actor: db.ActorSystem, Payload: pane,
	})
	return err
}

// formatStallLine renders a stalled event for the watch batch.
func formatStallLine(e db.Event, id string) string {
	var p stallPayload
	_ = json.Unmarshal([]byte(e.Payload), &p) // a malformed payload still renders, with blanks
	level := p.Level
	if level == "" {
		level = stallLevelStalled
	}
	return fmt.Sprintf("%-21s task=%-18s window=%s idle=%s raise=%d%s", level, e.EntityID, p.Window, p.Idle, p.Raise, id)
}

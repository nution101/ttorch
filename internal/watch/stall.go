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
// re-dispatched, or HEAD moved in its worktree. Any of them restarts the clock
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
	// headIdentity reports the commit id HEAD resolves to in a worktree (ok=false when
	// it cannot be read). It reads files only; see headref.go. Tests swap it out.
	headIdentity func(dir string) (string, bool)
}

func newStallTracker() stallTracker {
	return stallTracker{policy: stallPolicyFromEnv(), headIdentity: headIdentity}
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

// stallClock is the JSON payload of a stall_clock event: what the watcher saw when it
// last observed progress. Pane is the idle pane's hash ("" while busy); Head is the
// worktree's HEAD id ("" if it has never been readable).
type stallClock struct {
	Pane string `json:"pane"`
	Head string `json:"head,omitempty"`
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
	var clock stallClock
	decoded := json.Unmarshal([]byte(st.ClockPayload), &clock) == nil
	if !st.HasClock || !decoded || clock.Pane != pane || progressSinceClock(t, st) {
		return w.restartStallClock(ctx, now, t, st, clock, pane)
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
	// HEAD is only read once an update is due. A HEAD that cannot be read now, or was
	// never recorded, is not progress: unknown neither restarts the clock nor raises.
	if head, ok := w.stall.headIdentity(t.Worktree); ok && clock.Head != "" && head != clock.Head {
		return w.restartStallClock(ctx, now, t, st, clock, pane)
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
// hash, or "" while busy) and the worktree's current HEAD id. When HEAD cannot be read the
// previous clock's HEAD is carried forward, so a later readable HEAD is still compared
// against the last one seen. Within stallClockGap of the previous clock the write is
// deferred to a later sweep.
func (w *Watcher) restartStallClock(ctx context.Context, now time.Time, t db.Task, st db.StallState, prev stallClock, pane string) error {
	if st.HasClock {
		if d := now.Sub(st.ClockAt); d >= 0 && d < stallClockGap {
			return nil
		}
	}
	next := stallClock{Pane: pane, Head: prev.Head}
	if head, ok := w.stall.headIdentity(t.Worktree); ok {
		next.Head = head
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = w.Store.AppendEvent(ctx, db.Event{
		TS: now, EntityType: db.EntityTypeTask, EntityID: t.ID, Type: db.EventStallClock,
		Actor: db.ActorSystem, Payload: string(payload),
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

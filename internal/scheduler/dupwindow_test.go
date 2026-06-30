package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// These tests pin the recovery side of the duplicate-window fix. The window-creation half —
// that a (re-)spawn never STACKS a second window of the same name on a stale one — is enforced
// and tested at the tmux layer (NewWindow dedupes via KillWindows; see internal/tmux). Here we
// guard the scheduler's recovery DECISIONS: an ambiguous/duplicate-name capture must never be
// what drives a re-spawn (which would feed the duplicate loop), while a genuinely window-gone
// worker must still be recovered exactly as before.

// ambiguousCaptureErr stands in for what tmux returns when capture/send target a name shared by
// two windows: the target cannot be resolved to one window. With the dedup fix this should not
// occur, but a recovery pass must be robust if it ever does — it is "unobservable", never "gone".
var ambiguousCaptureErr = errors.New("can't find window: wk-w1 (multiple windows match)")

// TestRecovery_AmbiguousCaptureDoesNotRespawn proves the loop-breaker: a live, leased, active
// worker whose pane capture fails with a duplicate-name ambiguity is left untouched by every
// recovery pass — it is NOT nudged, NOT reclaimed, and so NOT re-dispatched. The scheduler's
// reclaim is driven by durable window_gone events (recorded by the watcher only on a VERIFIED
// absent window), never by a live capture; the pane-observing passes (idle-nudge, stall-recovery)
// treat a capture error as "unobservable this tick" and skip. So an ambiguous capture can never
// escalate into the re-spawn that would stack another duplicate window.
func TestRecovery_AmbiguousCaptureDoesNotRespawn(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	// An alive worker: active, valid (future) lease, a progress anchor — a genuine idle/stall
	// nudge candidate, so the ONLY reason a pass skips it is the ambiguous capture.
	addIdleWorker(t, s, repo, "w1", time.Now().Add(-10*time.Minute))

	f := &fakeFleet{peekErr: map[string]error{"w1": ambiguousCaptureErr}}
	sc := &Scheduler{
		Store: s, Fleet: f, Pool: worktree.Pool{Max: 100},
		IdleNudgeGrace: time.Minute, MaxIdleNudges: 2,
		StallNudgeGrace: 15 * time.Second, MaxStallNudges: 2,
	}

	// The full recovery sequence a tick runs, plus dispatch — none of which may act on w1.
	if n, err := sc.RunSuperviseOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunSuperviseOnce = (%d,%v), want (0,nil) — a live, leased worker with no window_gone is not reclaimed", n, err)
	}
	if n, err := sc.RunNudgeIdleOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunNudgeIdleOnce = (%d,%v), want (0,nil) — an unreadable (ambiguous) pane is skipped, not nudged", n, err)
	}
	if n, err := sc.RunStallRecoveryOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunStallRecoveryOnce = (%d,%v), want (0,nil) — an unreadable (ambiguous) pane is skipped, not nudged", n, err)
	}
	if n, err := sc.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunOnce = (%d,%v), want (0,nil) — nothing pending, so nothing is (re-)spawned", n, err)
	}

	if got := f.dispatched(); len(got) != 0 {
		t.Errorf("dispatched = %v, want none — an ambiguous capture must never trigger a re-spawn", got)
	}
	if sc := f.sendCount("w1"); sc != 0 {
		t.Errorf("sends to w1 = %d, want 0 — an unreadable pane is never injected into", sc)
	}
	if got := status(t, s, "w1"); got != db.StatusActive {
		t.Errorf("status = %q, want active — the worker is untouched (not reclaimed) on an ambiguous capture", got)
	}
	if rc := retryCount(t, s, "w1"); rc != 0 {
		t.Errorf("retry_count = %d, want 0 — no reclaim happened", rc)
	}
}

// TestRecovery_WindowGoneStillRecovered proves the genuine-death path is preserved: a worker
// whose window the watcher confirmed gone is reclaimed to pending AND re-dispatched exactly once
// (no duplicate spawn). This is the behavior the dedup fix must not regress.
func TestRecovery_WindowGoneStillRecovered(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := "/repo"
	addActiveWorker(t, s, repo, "g1", []string{"pkg/g1"})
	markWindowGone(t, s, "g1")

	f := &fakeFleet{} // empty live set: the reclaimed-pending task dispatches without contention
	sc := &Scheduler{Store: s, Fleet: f, Pool: worktree.Pool{Max: 100}}

	// Supervise reclaims the confirmed-gone worker to pending (re-dispatchable).
	if n, err := sc.RunSuperviseOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunSuperviseOnce = (%d,%v), want (1,nil)", n, err)
	}
	if got := status(t, s, "g1"); got != db.StatusPending {
		t.Fatalf("status after reclaim = %q, want pending", got)
	}

	// Dispatch re-spawns it — exactly once.
	if n, err := sc.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = (%d,%v), want (1,nil)", n, err)
	}
	if got := f.dispatched(); len(got) != 1 || got[0] != "g1" {
		t.Errorf("dispatched = %v, want exactly [g1] (recovered, no duplicate spawn)", got)
	}
}

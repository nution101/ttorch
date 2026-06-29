package scheduler

import (
	"context"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/review"
)

// RunGateOnce performs one daemon GATE pass and returns how many tasks it RECORDED a verdict
// for this tick (the headline hands-off-gated count). It is the gate analogue of RunLandOnce
// (roadmap A1): it takes the mechanical prep→dispatch→aggregate→record choreography off the LLM
// manager so a stalled or absent manager no longer halts the steady-state land path.
//
// Each tick it re-derives the gate candidates from the DB — done tasks (excluding ad-hoc cc)
// in a TRUSTED repo (Fleet.Gateable) that do NOT already carry a PASSING verdict — and, for
// each, atomically claims it and drives one tick of the gate via Fleet.GateOnce. Two
// independent guards keep it from racing or weakening anything:
//
//   - Candidate filter: a task that already carries a passing verdict is SKIPPED here — the
//     land pass owns it (carry-forward or land), so the gate never re-runs reviewers on
//     already-gated work. Non-trusted repos are skipped too (a daemon-recorded verdict there
//     would not advance delivery — a human still approves).
//   - Atomic claim (Store.ClaimForLand with a distinct "gater:" owner): exactly one actor gates
//     a given task per tick, so two concurrent ticks/instances can never both prep/dispatch it.
//     The claim is the SAME within-tick mechanism the land pass uses (a done task's lease is
//     dead weight); the owner namespace differs ("gater:" vs "lander:") so the gate and land
//     claims are never confused. Like the land pass, the claims are HELD across the whole tick
//     (claim every candidate, then gate them, then release) — not taken-and-released per task —
//     so a second instance backs off every task this one holds rather than re-winning it after a
//     premature release. Cross-tick idempotency does NOT rest on a held lease (it expires); it
//     rests on the review dir + reviewer windows (GateOnce's source of truth), so the lease is
//     released at the end of the tick and a fresh one is taken next tick.
//
// FAIL CLOSED is enforced inside GateOnce: it records ONLY an all-pass verdict (through the
// unchanged trust-record path); a blocking finding, a prep refusal, a missing/mismatched
// report, or a stalled reviewer is never recorded and instead surfaces an actionable
// gate_blocked event for the manager. The merge/land authority is untouched — the gate produces
// exactly the verdict state the land pass already expects, and the land pass's own gate (a
// fresh, commit-pinned passing verdict + a single-use approval consumed at the fast-forward)
// remains the authority over whether anything merges.
//
// It is a no-op (returns 0) when nothing is done, nothing is gateable, or every done task is
// either already gated or still has reviewers in flight.
func (sc *Scheduler) RunGateOnce(ctx context.Context) (int, error) {
	done, err := sc.Store.ListTasks(ctx, db.TaskFilter{
		Status:      []string{db.StatusDone},
		ExcludeKind: []string{db.KindCC}, // cc sessions are ad-hoc, lead-driven — never auto-gated
	})
	if err != nil {
		return 0, err
	}
	if len(done) == 0 {
		return 0, nil // nothing awaiting a gate — the common idle-tick fast path
	}

	// Claim every candidate up front and HOLD the claims for the whole tick — the loser of each
	// atomic claim backs off rather than re-winning a task after a premature release, so two
	// concurrent instances partition the work and never double-gate. owners records the claim
	// owner per task so a release targets exactly the claim it took; release uses
	// context.Background() so a claim is freed even when the tick's own ctx is the reason we
	// unwind (a SIGTERM cancel), never stranding a done task on the lease until it lapses.
	var claimed []string
	owners := map[string]string{}
	release := func(id string) {
		if owner := owners[id]; owner != "" {
			if _, rerr := sc.Store.ReleaseLandClaim(context.Background(), id, owner); rerr != nil {
				sc.logf("could not release gate claim on %s: %v", id, rerr)
			}
		}
	}
	releaseAll := func() {
		for _, id := range claimed {
			release(id)
		}
	}
	for _, t := range done {
		select {
		case <-ctx.Done():
			releaseAll() // free this tick's claims so a partial batch never strands on cancel
			return 0, ctx.Err()
		default:
		}
		if t.Project == "" {
			continue // no repo on the task — nothing to gate against
		}
		// Only TRUSTED repos gate hands-off: there a recorded pass auto-mints the approval token
		// and the land pass merges without a human. Skip the rest before taking any claim.
		if !sc.Fleet.Gateable(t.Project) {
			continue
		}
		// A task that already carries a PASSING verdict is the LAND pass's to land (it may even
		// carry the verdict forward over a clean rebase) — the gate never re-runs reviewers on
		// it. A missing or blocking verdict falls through to gating. (GateOnce re-checks, under
		// the claim, whether the verdict already covers the current head — the precise re-gate
		// decision lives there, since it needs the worktree HEAD.)
		v, ok, err := sc.Store.GetVerdict(ctx, t.ID)
		if err != nil {
			releaseAll()
			return 0, err
		}
		if ok && v.Overall == review.Pass {
			continue
		}
		owner := "gater:" + t.ID
		won, err := sc.Store.ClaimForLand(ctx, t.ID, owner)
		if err != nil {
			releaseAll()
			return 0, err
		}
		if !won {
			continue // another tick/instance holds this gate claim, or it moved off done — skip
		}
		claimed = append(claimed, t.ID)
		owners[t.ID] = owner
	}
	if len(claimed) == 0 {
		return 0, nil
	}
	defer releaseAll() // release the whole tick's claims once gating is done

	recorded := 0
	for _, id := range claimed {
		select {
		case <-ctx.Done():
			return recorded, ctx.Err() // deferred releaseAll frees the held claims
		default:
		}
		outcome, gerr := sc.Fleet.GateOnce(id)
		if gerr != nil {
			// A per-task gate error (a board-read hiccup inside GateOnce) is logged and the task
			// left for a later tick; it never aborts the whole pass (the next tick re-derives).
			sc.logf("gate of %s did not complete: %v", id, gerr)
			continue
		}
		switch outcome {
		case orchestrator.GateRecorded:
			recorded++
			sc.logf("gated %s (verdict recorded; ready to land)", id)
		case orchestrator.GateDispatched:
			sc.logf("gate dispatched reviewers for %s", id)
		case orchestrator.GateBlocked:
			sc.logf("gate blocked %s — surfaced for the manager to adjudicate", id)
		case orchestrator.GateWaiting, orchestrator.GateSkipped:
			// waiting (reviewers still running) / skipped (already gated, or not a candidate
			// under the claim): no log line — these are the quiet steady states.
		}
	}
	return recorded, nil
}

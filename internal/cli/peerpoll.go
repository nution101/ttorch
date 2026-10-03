package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
	"github.com/nution101/ttorch/internal/scheduler"
)

// The scheduler's peer pass (internal/scheduler/peerpass.go) reaches each peer through the same
// control client the parent's peer commands use: one ssh process per call, the control key and
// nothing else, killed at the client's deadline (peerCallTimeout). This file only adapts that
// client to the pass's PeerChannel; what to poll, and what to do with the answers, is the pass's.

// dialPeer is the control channel to p, for the scheduler's peer pass.
func dialPeer(p db.Peer) scheduler.PeerChannel {
	return controlChannel{c: peer.Client{SSH: peerSSH, Dest: p.ControlDest, Key: p.ControlKey, Timeout: peerCallTimeout}}
}

type controlChannel struct{ c peer.Client }

// Summary reads the peer's summary. A summary in a schema this binary does not read is refused
// rather than guessed at, so it fails the poll and, repeated, marks the peer unreachable with the
// reason.
func (ch controlChannel) Summary(ctx context.Context) (scheduler.PeerSummary, error) {
	var sum peer.Summary
	if err := ch.c.Call(ctx, peer.VerbSummary, nil, &sum); err != nil {
		return scheduler.PeerSummary{}, err
	}
	if sum.SchemaVersion != peer.SchemaVersion {
		return scheduler.PeerSummary{}, fmt.Errorf("summary on %s: the peer sent schema %d and this coordinator reads %d; upgrade the older one", ch.c.Dest, sum.SchemaVersion, peer.SchemaVersion)
	}
	var cache bytes.Buffer
	if err := sum.WriteJSON(&cache); err != nil {
		return scheduler.PeerSummary{}, err
	}
	return scheduler.PeerSummary{
		ManagerWindow:    sum.Manager.WindowPresent,
		SchedulerRunning: sum.Scheduler.Running,
		SchedulerStalled: sum.Scheduler.Stalled,
		JSON:             strings.TrimSpace(cache.String()),
	}, nil
}

// Decisions reads the peer's open escalations above since.
func (ch controlChannel) Decisions(ctx context.Context, since int64) (scheduler.PeerDecisions, error) {
	var d peer.DecisionsResult
	if err := ch.c.Call(ctx, peer.VerbDecisions, peer.DecisionsRequest{Since: since}, &d); err != nil {
		return scheduler.PeerDecisions{}, err
	}
	out := scheduler.PeerDecisions{Open: d.Open}
	for _, e := range d.Escalations {
		out.Escalations = append(out.Escalations, scheduler.PeerEscalation{ID: e.ID, Kind: e.Kind, TaskID: e.TaskID, Body: e.Body, CreatedAt: e.CreatedAt})
	}
	return out, nil
}

// EnsureUp asks the peer to restore its manager and workers and start its scheduler, and says
// what it did.
func (ch controlChannel) EnsureUp(ctx context.Context) (string, error) {
	var res peer.EnsureUpResult
	if err := ch.c.Call(ctx, peer.VerbEnsureUp, nil, &res); err != nil {
		return "", err
	}
	restored := "nothing to restore"
	if len(res.Restored) > 0 {
		restored = "restored " + strings.Join(res.Restored, "; ")
	}
	return restored + "; scheduler: " + res.Scheduler, nil
}

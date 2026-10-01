package watch

// The stall ladder asks whether a worker's HEAD moved, without starting a git process.
// The read itself is worktree.ObserveHead, which follows files only, because any git command
// run in the worker's tree can be steered into running a program the worker configured.
// This file bounds that read.
//
// Only the identity (the commit id HEAD resolves to) is read, never a timestamp: the ladder
// asks whether HEAD changed since the clock last restarted, which a committer date forged
// into the future cannot fake. An unknown HEAD neither restarts the clock nor counts against
// the worker. The read runs under a short deadline (boundedHead), so a path that never
// answers cannot hold up the sweep.

import (
	"context"
	"sync"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// headReadTimeout bounds one HEAD read. The files are the worker's, and a gitdir: line or
// commondir can name an automount or FUSE path whose open never returns; past the deadline
// the read is unknown and the sweep moves on to the next task.
const headReadTimeout = 2 * time.Second

// A read that takes headReadSlow or longer, or times out, backs that task's HEAD reads off
// for headReadBackoff: each sweep would otherwise spend up to the full deadline on it.
const (
	headReadSlow    = 500 * time.Millisecond
	headReadBackoff = 5 * time.Minute
)

// boundedHead runs a HEAD read under a deadline, at most one per task at a time. A read
// that outlives its deadline keeps its slot until it returns, so a hung path costs one
// goroutine per task rather than one per sweep; until then the task reads as unknown.
type boundedHead struct {
	timeout time.Duration
	slow    time.Duration // a read this long or longer starts a backoff
	backoff time.Duration
	read    func(worktree, project string) (string, bool)
	now     func() time.Time

	mu       sync.Mutex
	inflight map[string]bool      // task id → a read is still running
	until    map[string]time.Time // task id → no read before this instant
}

// headReader is shared by every Watcher in the process, so a read left hanging by one
// arm still holds its task's slot, and its backoff, in the next.
var headReader = newBoundedHead(headReadTimeout, worktree.ObserveHead)

func newBoundedHead(timeout time.Duration, read func(worktree, project string) (string, bool)) *boundedHead {
	return &boundedHead{
		timeout: timeout, slow: headReadSlow, backoff: headReadBackoff, read: read, now: time.Now,
		inflight: map[string]bool{}, until: map[string]time.Time{},
	}
}

// identity reads the HEAD id of t's worktree. ok is false when the read fails, times out,
// or ctx ends first; and, without reading, when ctx has already ended, an earlier read for
// t has not returned yet, or t is backing off after a slow read.
func (b *boundedHead) identity(ctx context.Context, t db.Task) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	start := b.now()
	b.mu.Lock()
	if b.inflight[t.ID] {
		b.mu.Unlock()
		return "", false
	}
	if until, ok := b.until[t.ID]; ok {
		if start.Before(until) {
			b.mu.Unlock()
			return "", false
		}
		delete(b.until, t.ID)
	}
	b.inflight[t.ID] = true
	b.mu.Unlock()

	type result struct {
		id string
		ok bool
	}
	done := make(chan result, 1) // buffered: a read that outlives its deadline never blocks on send
	go func() {
		id, ok := b.read(t.Worktree, t.Project)
		end := b.now()
		b.mu.Lock()
		delete(b.inflight, t.ID)
		if end.Sub(start) >= b.slow {
			b.holdOffLocked(t.ID, end)
		}
		b.mu.Unlock()
		done <- result{id, ok}
	}()
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.id, r.ok
	case <-timer.C:
		b.mu.Lock()
		b.holdOffLocked(t.ID, b.now())
		b.mu.Unlock()
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// holdOffLocked backs t's reads off until b.backoff after from, never shortening a
// backoff already set. b.mu must be held.
func (b *boundedHead) holdOffLocked(id string, from time.Time) {
	if until := from.Add(b.backoff); until.After(b.until[id]) {
		b.until[id] = until
	}
}

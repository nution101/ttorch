package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/worktree"
)

// fakeWatchLoop stands in for watch.Daemon: it reports when Run starts and when it returns.
type fakeWatchLoop struct {
	started chan struct{}
	stopped chan struct{}
}

func (f *fakeWatchLoop) Run(ctx context.Context) error {
	close(f.started)
	<-ctx.Done()
	close(f.stopped)
	return ctx.Err()
}

// TestRunDrivesTheWatchLoop: a Scheduler with a Watch loop starts it when Run starts, even
// with an hour between ticks, and Run does not return until the loop has stopped.
func TestRunDrivesTheWatchLoop(t *testing.T) {
	loop := &fakeWatchLoop{started: make(chan struct{}), stopped: make(chan struct{})}
	sc := &Scheduler{Store: newStore(t), Fleet: &fakeFleet{}, Pool: worktree.Pool{Max: 100}, Interval: time.Hour, Watch: loop}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()
	select {
	case <-loop.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never started the watch loop")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	select {
	case <-loop.stopped:
	default:
		t.Fatal("Run returned before the watch loop stopped")
	}
}

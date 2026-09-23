package board

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

// twoBoards starts two boards, each with its own store, on one DB file: two `ttorch board`
// processes as far as the DB can tell.
func twoBoards(t *testing.T) (*harness, *harness) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() *db.Store {
		s, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	delay := 50 * time.Millisecond
	a := newHarnessOn(t, open(), &fakeFleet{sendDelay: delay})
	b := newHarnessOn(t, open(), &fakeFleet{sendDelay: delay})
	return a, b
}

// race runs fn from n goroutines on each board at once and waits for all of them.
func race(n int, a, b *harness, fn func(*harness)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, h := range []*harness{a, b} {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(h *harness) {
				defer wg.Done()
				<-start
				fn(h)
			}(h)
		}
	}
	close(start)
	wg.Wait()
}

func TestTwoBoardsOnOneDBAnswerOnce(t *testing.T) {
	a, b := twoBoards(t)
	a.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := a.ask("t1", "which base?")
	race(4, a, b, func(h *harness) { _, _ = h.srv.Answer(context.Background(), "t1", q, "main") })
	if n := a.fleet.sendCount() + b.fleet.sendCount(); n != 1 {
		t.Fatalf("two boards on one DB sent the answer %d times (a=%d, b=%d), want 1",
			n, a.fleet.sendCount(), b.fleet.sendCount())
	}
}

func TestTwoBoardsOnOneDBGatePrepOnce(t *testing.T) {
	a, b := twoBoards(t)
	a.addTask(taskDone("g1"))
	ev := a.escalate("g1", "sha=abc123 reviewer stalled")
	race(4, a, b, func(h *harness) { _, _ = h.srv.GatePrep(context.Background(), "g1", ev) })
	if n := a.fleet.prepCount() + b.fleet.prepCount(); n != 1 {
		t.Fatalf("two boards on one DB ran gate prep %d times, want 1", n)
	}
}

// TestFailedSendReleasesTheClaimAcrossBoards: a send that fails gives the question back, so the
// other board (or a retry) can answer it.
func TestFailedSendReleasesTheClaimAcrossBoards(t *testing.T) {
	a, b := twoBoards(t)
	a.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := a.ask("t1", "which base?")
	a.fleet.sendErr = errors.New("pane is in copy-mode")
	if _, err := a.srv.Answer(context.Background(), "t1", q, "main"); err == nil {
		t.Fatal("a failed send reported success")
	}
	if res, err := b.srv.Answer(context.Background(), "t1", q, "main"); err != nil || !res.Done {
		t.Fatalf("the other board could not answer after a failed send: %+v %v", res, err)
	}
	if res, _ := a.srv.Answer(context.Background(), "t1", q, "main"); res.Done {
		t.Fatalf("answered again after the other board's send: %+v", res)
	}
	if n := a.fleet.sendCount() + b.fleet.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
	if body := a.sections().body; !containsAll(body, "Answered from the board") {
		t.Fatalf("answered question not shown as answered")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

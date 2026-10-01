package watch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// writeTree writes files (relative path → contents) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBoundedHead_SlowOpenTimesOutWithoutPileUp: a HEAD read that never returns (an open
// hung on an automount or FUSE path behind gitdir:) must read as unknown once the deadline
// passes, and must not start a second read for the task while the first is still hanging.
func TestBoundedHead_SlowOpenTimesOutWithoutPileUp(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var opens atomic.Int32
	t.Cleanup(unblock) // let any hung read finish
	hung := func(dir, project string) (string, bool) {
		opens.Add(1)
		<-release
		return worktree.ObserveHead(dir, project)
	}

	wt := t.TempDir()
	writeTree(t, wt, map[string]string{".git": "gitdir: meta/git\n", "meta/git/HEAD": idA + "\n"})
	b := newBoundedHead(50*time.Millisecond, hung)
	clk := &fakeNow{t: time.Unix(1_800_000_000, 0)}
	b.now = clk.now
	task := db.Task{ID: "slow", Worktree: wt}

	read := func(stage string) (string, bool) {
		t.Helper()
		type res struct {
			id string
			ok bool
		}
		done := make(chan res, 1)
		go func() { id, ok := b.identity(context.Background(), task); done <- res{id, ok} }()
		select {
		case r := <-done:
			return r.id, r.ok
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the HEAD read blocked past its deadline", stage)
			return "", false
		}
	}

	if id, ok := read("hung open"); ok {
		t.Fatalf("hung open: got %q, want unknown", id)
	}
	if id, ok := read("second sweep"); ok {
		t.Fatalf("second sweep: got %q, want unknown", id)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("started %d reads while the first still hung, want 1", n)
	}

	unblock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		busy := b.inflight[task.ID]
		b.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task's slot was never freed after the hung read returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The timeout backed the task off; once that passes it is read again.
	clk.add(b.backoff)
	if id, ok := read("after the backoff"); !ok || id != idA {
		t.Fatalf("after the backoff: got %q, %v; want %q", id, ok, idA)
	}
	if n := opens.Load(); n != 2 {
		t.Fatalf("%d reads in all, want 2", n)
	}
}

// fakeNow is a settable clock for boundedHead.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeNow) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// TestBoundedHead_SlowReadBacksOff: a read that is slow but inside the deadline, or one
// that times out, backs that task's reads off. Sweeps within the backoff do not read and
// report unknown; once it passes, the task is read again. Other tasks are unaffected.
func TestBoundedHead_SlowReadBacksOff(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_800_000_000, 0)}
	var reads atomic.Int32
	delay := map[string]time.Duration{"slow": 0, "fast": 0}
	var dmu sync.Mutex
	read := func(worktree, _ string) (string, bool) {
		reads.Add(1)
		dmu.Lock()
		d := delay[worktree]
		dmu.Unlock()
		clk.add(d) // the read takes d on the watcher's clock
		return idA, true
	}
	b := newBoundedHead(time.Hour, read)
	b.now = clk.now
	slow, fast := db.Task{ID: "s", Worktree: "slow"}, db.Task{ID: "f", Worktree: "fast"}
	ctx := context.Background()

	dmu.Lock()
	delay["slow"] = b.slow + time.Millisecond
	dmu.Unlock()
	if _, ok := b.identity(ctx, slow); !ok {
		t.Fatal("a slow read inside the deadline still returns its id")
	}
	dmu.Lock()
	delay["slow"] = 0
	dmu.Unlock()
	clk.add(time.Minute)
	if id, ok := b.identity(ctx, slow); ok {
		t.Fatalf("second sweep inside the backoff read %q, want unknown", id)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads, want 1: the sweep inside the backoff read again", n)
	}
	if _, ok := b.identity(ctx, fast); !ok || reads.Load() != 2 {
		t.Fatal("another task must not be held by this task's backoff")
	}
	clk.add(b.backoff)
	if _, ok := b.identity(ctx, slow); !ok || reads.Load() != 3 {
		t.Fatalf("after the backoff the task must be read again (reads=%d)", reads.Load())
	}
	if _, ok := b.identity(ctx, slow); !ok || reads.Load() != 4 {
		t.Fatal("a fast read must not start a backoff")
	}
}

// TestBoundedHead_TimeoutBacksOff: a read that times out backs the task off too, so after
// the hung read returns, the next sweep inside the backoff still does not read.
func TestBoundedHead_TimeoutBacksOff(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_800_000_000, 0)}
	release := make(chan struct{})
	var reads atomic.Int32
	first := true
	read := func(string, string) (string, bool) {
		reads.Add(1)
		if first {
			first = false
			<-release
		}
		return idA, true
	}
	b := newBoundedHead(20*time.Millisecond, read)
	b.now = clk.now
	task := db.Task{ID: "t", Worktree: "wt"}
	if _, ok := b.identity(context.Background(), task); ok {
		t.Fatal("a hung read must time out as unknown")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		busy := b.inflight[task.ID]
		b.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hung read never released its slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	clk.add(time.Minute)
	if _, ok := b.identity(context.Background(), task); ok || reads.Load() != 1 {
		t.Fatalf("a sweep inside the timeout's backoff must not read (reads=%d)", reads.Load())
	}
}

// TestBoundedHead_HonoursContext: a cancelled context returns unknown without starting a
// read, and cancelling during a hung read returns before the deadline.
func TestBoundedHead_HonoursContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var reads atomic.Int32
	b := newBoundedHead(time.Hour, func(string, string) (string, bool) {
		reads.Add(1)
		<-release
		return idA, true
	})
	// call runs identity and fails the test if it is still waiting after 5s.
	call := func(ctx context.Context, id string, cancelAfter func()) bool {
		t.Helper()
		done := make(chan bool, 1)
		go func() { _, ok := b.identity(ctx, db.Task{ID: id}); done <- ok }()
		if cancelAfter != nil {
			time.Sleep(20 * time.Millisecond)
			cancelAfter()
		}
		select {
		case ok := <-done:
			return ok
		case <-time.After(5 * time.Second):
			t.Fatalf("task %s: identity ignored its cancelled context and kept waiting", id)
			return false
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if call(cancelled, "a", nil) || reads.Load() != 0 {
		t.Fatalf("a cancelled context must not start a read (reads=%d)", reads.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	if call(ctx, "b", cancel) {
		t.Fatal("a cancelled read must report unknown")
	}
}

// TestHeadReader_ReadsThroughObserveHead: the watcher's shared reader resolves a real
// repository's HEAD through worktree.ObserveHead, so the stall ladder sees what git sees.
func TestHeadReader_ReadsThroughObserveHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	want := gitIn(t, repo, "rev-parse", "HEAD")
	got, ok := headReader.identity(context.Background(), db.Task{ID: "reads-through-observehead", Worktree: repo, Project: repo})
	if !ok || got != want {
		t.Fatalf("headReader.identity = %q, %v; git says %q", got, ok, want)
	}
}

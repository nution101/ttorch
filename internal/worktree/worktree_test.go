package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return repo
}

func TestPool_ReuseAndDistinctWhenBusy(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}

	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a is in use -> a distinct new slot.
	b, err := p.Acquire(repo, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("expected a distinct slot when the first is in use")
	}
	// a is now idle and clean -> reused.
	c, err := p.Acquire(repo, []string{b})
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected reuse of %q, got %q", a, c)
	}
}

func TestPool_RespectsMax(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 1}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(repo, []string{a}); err == nil {
		t.Fatal("expected pool-full error at Max=1")
	}
}

func TestPool_SkipsSlotWithTrackedChanges(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Orphaned tracked change in slot a (not in use).
	if err := os.WriteFile(filepath.Join(a, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b == a {
		t.Fatal("must not reuse a slot with uncommitted tracked changes")
	}
}

func TestPool_KeepsUntrackedCachesOnReuse(t *testing.T) {
	repo := makeRepo(t)
	p := Pool{Root: t.TempDir(), Max: 4}
	a, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a build cache (untracked).
	cache := filepath.Join(a, "node_modules_marker")
	if err := os.WriteFile(cache, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reuse a (idle, no tracked changes) -> cache preserved.
	c, err := p.Acquire(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected reuse of %q, got %q", a, c)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("untracked cache should survive reuse")
	}
}

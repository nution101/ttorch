package clonepool

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestObserveTipReadsFilesOnly checks the file reader against the layouts a clone can be
// in, and that every unexpected shape reads as unknown rather than being followed.
func TestObserveTipReadsFilesOnly(t *testing.T) {
	isolateGit(t)
	mk := func(t *testing.T) (string, string) {
		t.Helper()
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		run(t, dir, "init", "-q", "-b", "main")
		sha := commit(t, dir, "f", "x\n", "one")
		return dir, sha
	}

	t.Run("loose branch", func(t *testing.T) {
		dir, sha := mk(t)
		if got, ok := observeTip(dir); !ok || got != sha {
			t.Fatalf("got %q %v, want %s", got, ok, sha)
		}
	})
	t.Run("detached", func(t *testing.T) {
		dir, sha := mk(t)
		run(t, dir, "checkout", "-q", "--detach")
		if got, ok := observeTip(dir); !ok || got != sha {
			t.Fatalf("got %q %v, want %s", got, ok, sha)
		}
	})
	t.Run("packed branch", func(t *testing.T) {
		dir, sha := mk(t)
		run(t, dir, "pack-refs", "--all")
		if exists(filepath.Join(dir, ".git", "refs", "heads", "main")) {
			t.Fatal("pack-refs left the loose ref")
		}
		if got, ok := observeTip(dir); !ok || got != sha {
			t.Fatalf("got %q %v, want %s", got, ok, sha)
		}
	})
	t.Run("gitfile", func(t *testing.T) {
		dir, _ := mk(t)
		other := filepath.Join(t.TempDir(), "gd")
		if err := os.Rename(filepath.Join(dir, ".git"), other); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: "+other+"\n")
		if got, ok := observeTip(dir); ok {
			t.Fatalf("a gitfile was followed to %q", got)
		}
	})
	t.Run("symlinked .git", func(t *testing.T) {
		dir, _ := mk(t)
		other := filepath.Join(t.TempDir(), "gd")
		if err := os.Rename(filepath.Join(dir, ".git"), other); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		if got, ok := observeTip(dir); ok {
			t.Fatalf("a symlinked .git was followed to %q", got)
		}
	})
	t.Run("symlinked ref", func(t *testing.T) {
		dir, sha := mk(t)
		elsewhere := filepath.Join(t.TempDir(), "ref")
		writeFile(t, elsewhere, sha+"\n")
		ref := filepath.Join(dir, ".git", "refs", "heads", "main")
		if err := os.Remove(ref); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, ref); err != nil {
			t.Fatal(err)
		}
		if got, ok := observeTip(dir); ok {
			t.Fatalf("a symlinked ref was followed to %q", got)
		}
	})
	t.Run("ref outside refs", func(t *testing.T) {
		dir, _ := mk(t)
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/../../../etc/passwd\n")
		if got, ok := observeTip(dir); ok {
			t.Fatalf("an escaping ref read as %q", got)
		}
	})
	t.Run("second symref level", func(t *testing.T) {
		dir, _ := mk(t)
		writeFile(t, filepath.Join(dir, ".git", "refs", "heads", "main"), "ref: refs/heads/other\n")
		if got, ok := observeTip(dir); ok {
			t.Fatalf("a nested symref read as %q", got)
		}
	})
	t.Run("reftable", func(t *testing.T) {
		dir, _ := mk(t)
		run(t, dir, "config", "extensions.refStorage", "reftable")
		if got, ok := observeTip(dir); ok {
			t.Fatalf("a reftable repository read as %q", got)
		}
	})
	t.Run("fifo HEAD", func(t *testing.T) {
		dir, _ := mk(t)
		head := filepath.Join(dir, ".git", "HEAD")
		if err := os.Remove(head); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(head, 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		done := make(chan bool, 1)
		go func() { _, ok := observeTip(dir); done <- ok }()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("a FIFO HEAD read as known")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a FIFO HEAD blocked the read")
		}
	})
	t.Run("oversized HEAD", func(t *testing.T) {
		dir, _ := mk(t)
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), strings.Repeat("a", maxSmallFile+1))
		if got, ok := observeTip(dir); ok {
			t.Fatalf("an oversized HEAD read as %q", got)
		}
	})
	t.Run("symlinked slot", func(t *testing.T) {
		dir, _ := mk(t)
		link := filepath.Join(t.TempDir(), "slot")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if got, ok := observeTip(link); ok {
			t.Fatalf("a symlinked slot was followed to %q", got)
		}
	})
}

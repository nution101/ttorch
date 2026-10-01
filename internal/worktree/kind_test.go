package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestKindOf: a task's kind is read from its working directory, so a clone (.git is a
// directory) and a linked worktree (.git is a file) are told apart with no record of their
// own, and anything else, symlinks included, is unknown with a reason.
func TestKindOf(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	main := makeRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitT(t, main, "worktree", "add", "-q", "--detach", linked)

	cases := []struct {
		name  string
		setup func(t *testing.T) string
		want  Kind
	}{
		{"a repository of its own", func(*testing.T) string { return main }, KindClone},
		{"a linked worktree", func(*testing.T) string { return linked }, KindWorktree},
		{"path spelled with a trailing slash", func(*testing.T) string { return linked + "/" }, KindWorktree},
		{"no path", func(*testing.T) string { return "" }, KindUnknown},
		{"missing directory", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, KindUnknown},
		{"no .git", func(t *testing.T) string { return t.TempDir() }, KindUnknown},
		{"a file, not a directory", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "f")
			writeTree(t, filepath.Dir(p), map[string]string{"f": "x"})
			return p
		}, KindUnknown},
		{".git a symlink to a directory", func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Symlink(filepath.Join(main, ".git"), filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, KindUnknown},
		{".git a symlink to a file", func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Symlink(filepath.Join(linked, ".git"), filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, KindUnknown},
		{"the directory itself a symlink to a repository", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(main, p); err != nil {
				t.Fatal(err)
			}
			return p
		}, KindUnknown},
		{".git a FIFO", func(t *testing.T) string {
			dir := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(dir, ".git"), 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			return dir
		}, KindUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.setup(t)
			got, err := KindOf(dir)
			if got != c.want {
				t.Fatalf("KindOf(%q) = %v (%v), want %v", dir, got, err, c.want)
			}
			if (err != nil) != (c.want == KindUnknown) {
				t.Fatalf("KindOf(%q) = %v with error %v: an error must come with KindUnknown and only then", dir, got, err)
			}
		})
	}
}

package paths

import (
	"path/filepath"
	"testing"
)

// TestClonesSitsBesideWorktrees pins the clone pool root under Home and apart from the
// worktree pool, so a clone slot can never be mistaken for a worktree slot by location.
func TestClonesSitsBesideWorktrees(t *testing.T) {
	p := Paths{Home: filepath.Join("/x", ".ttorch")}
	if got, want := p.Clones(), filepath.Join("/x", ".ttorch", "clones"); got != want {
		t.Fatalf("Clones() = %q, want %q", got, want)
	}
	if p.Clones() == p.Worktrees() {
		t.Fatalf("Clones() and Worktrees() are the same directory %q", p.Clones())
	}
}

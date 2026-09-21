package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChangedFiles_UnquotedAndComplete is the gate-config guard's input contract.
//
// orchestrator.diffTouchesGateConfig matches these names against literal paths and prefixes
// (".ttorch/validate.sh", "content/skills/"), so a name that comes back C-quoted —
// "content/skills/caf\303\251.md", which is what plain `git diff --name-only` emits for a
// non-ASCII path under git's default core.quotePath — matches nothing and the guard lets a
// gate-instruction change merge unflagged. The mirror-image failure is a path containing a
// literal newline once core.quotePath is off: splitting on "\n" turns one path into two
// fragments. `--name-only -z` avoids both, and this test pins it with both hostile names
// present at once, under both quotePath settings.
func TestChangedFiles_UnquotedAndComplete(t *testing.T) {
	repo := makeRepo(t)
	base := gitT(t, repo, "rev-parse", "HEAD")

	skills := filepath.Join(repo, "content", "skills", "ttorch-review")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-ASCII name (git quotes it by default) and a plain one, both under a covered
	// prefix, plus an ordinary file so the listing has more than one record to mis-split.
	want := []string{
		"content/skills/ttorch-review/SKILL.md",
		"content/skills/ttorch-review/référence.md",
		"f.txt",
	}
	for _, rel := range want[:2] {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "add a non-ASCII path under a covered prefix")
	rev := gitT(t, repo, "rev-parse", "HEAD")

	for _, quotePath := range []string{"true", "false"} {
		gitT(t, repo, "config", "core.quotePath", quotePath)
		got, err := ChangedFiles(repo, base, rev)
		if err != nil {
			t.Fatalf("core.quotePath=%s: %v", quotePath, err)
		}
		if len(got) != len(want) {
			t.Fatalf("core.quotePath=%s: got %d paths %q, want %d %q", quotePath, len(got), got, len(want), want)
		}
		set := map[string]bool{}
		for _, g := range got {
			if g == "" {
				t.Errorf("core.quotePath=%s: an empty path entry means the listing was split wrong", quotePath)
			}
			set[g] = true
		}
		for _, w := range want {
			if !set[w] {
				t.Errorf("core.quotePath=%s: %q missing from %q — a quoted or mis-split name matches no gate-config prefix, so the guard would not see it", quotePath, w, got)
			}
		}
	}
}

// TestChangedFiles_NoChanges keeps the empty case explicit: no changed files is an empty
// list, never a one-element list holding "".
func TestChangedFiles_NoChanges(t *testing.T) {
	repo := makeRepo(t)
	head := gitT(t, repo, "rev-parse", "HEAD")
	got, err := ChangedFiles(repo, head, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ChangedFiles for an empty diff = %q, want none", got)
	}
}

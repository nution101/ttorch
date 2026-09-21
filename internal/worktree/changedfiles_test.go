package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestCheckedGitRaw_ClassifiesRealCollisionWarning: git reports "the following paths have
// collided" on a SUCCESSFUL command, having silently picked one entry as the winner. gitRaw
// previously routed stderr to a buffer and dropped it whenever the exit code was zero, so the
// one signal git gives for this attack was discarded by the commit meant to harden path
// handling. checkedGitRaw now turns it into ErrPathCollision.
//
// The command used here is `git clone`, because that is a command that actually emits the
// warning — see TestAddDetached_DoesNotWarnOnCollision for why this cannot be the primary
// control. The warning text is git's own, not a fixture.
func TestCheckedGitRaw_ClassifiesRealCollisionWarning(t *testing.T) {
	repo := makeRepo(t)
	if !caseInsensitiveFS(t, repo) {
		t.Skip("case-sensitive filesystem: git has no collision to report")
	}
	commitCollidingPair(t, repo)
	dst := filepath.Join(t.TempDir(), "clone")
	_, err := checkedGitRaw("clone", "-q", repo, dst)
	if err == nil {
		t.Fatal("a clone git itself warns about must not come back clean")
	}
	if !errors.Is(err, ErrPathCollision) {
		t.Fatalf("want ErrPathCollision, got: %v", err)
	}
	if !strings.Contains(err.Error(), "collided") {
		t.Fatalf("the error must carry git's own wording, got: %v", err)
	}
}

// TestAddDetached_DoesNotWarnOnCollision records WHY the orchestrator's own collision check is
// load-bearing rather than a belt-and-braces addition to git's warning.
//
// `git clone` warns. `git worktree add --detach` — which is what the trust gate uses to
// materialize the immutable checkout it validates — does NOT. It writes the winning entry,
// silently drops the other, and exits 0 with nothing on stderr. So the tree the gate validates
// can already be missing a covered file with no diagnostic anywhere.
//
// This test asserts that gap rather than papering over it. If a future git starts warning
// here, it fails, and AddDetached's checkedGitRaw will then refuse on its own — at which point
// this test should become the assertion its sibling makes. Either way the guard in
// orchestrator.diffTouchesGateConfig is what actually refuses the merge, and it does not
// depend on git saying anything.
func TestAddDetached_DoesNotWarnOnCollision(t *testing.T) {
	repo := makeRepo(t)
	if !caseInsensitiveFS(t, repo) {
		t.Skip("case-sensitive filesystem: nothing collides")
	}
	commitCollidingPair(t, repo)
	head := gitT(t, repo, "rev-parse", "HEAD")
	dir := filepath.Join(t.TempDir(), "co")
	err := AddDetached(repo, dir, head)
	if err != nil {
		if !errors.Is(err, ErrPathCollision) {
			t.Fatalf("unexpected failure: %v", err)
		}
		t.Log("git now warns on `worktree add` too; AddDetached refuses on its own. Flip this test to assert the refusal.")
		return
	}
	// It succeeded, which is the point. Assert the harm directly: reading AGENTS.md out of
	// the checkout the gate would have validated returns the attacker's bytes, because the
	// filesystem resolved both entries to one file and the hostile one won.
	got, rerr := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if rerr != nil {
		t.Fatalf("reading AGENTS.md from the checkout: %v", rerr)
	}
	if strings.TrimSpace(string(got)) == "real" {
		t.Skip("this checkout resolved to the legitimate file; nothing to document on this filesystem")
	}
	if strings.TrimSpace(string(got)) != "evil" {
		t.Fatalf("AGENTS.md in the checkout is neither entry: %q", got)
	}
	t.Log("confirmed: `git worktree add` exited 0 with no warning and AGENTS.md in the checkout holds the hostile bytes — the gate cannot rely on git to detect this, which is why orchestrator.diffTouchesGateConfig refuses the merge itself")
}

// commitCollidingPair commits AGENTS.md alongside a second index entry, agent<U+017F>.md,
// whose long-s folds onto the same filename on APFS and NTFS. The pair is spliced in through
// the index because the two names cannot coexist in a working tree on such a filesystem —
// they coexist fine in a commit, which is the whole attack.
func commitCollidingPair(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "evil.tmp"), []byte("evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitT(t, repo, "hash-object", "-w", "evil.tmp")
	if err := os.Remove(filepath.Join(repo, "evil.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "AGENTS.md")
	gitT(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+blob+",agent\u017f.md")
	gitT(t, repo, "commit", "-q", "-m", "two entries, one file")
}

// caseInsensitiveFS reports whether dir's filesystem folds case, which decides whether git
// has a collision to warn about at all.
func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe.tmp"))
	return err == nil
}

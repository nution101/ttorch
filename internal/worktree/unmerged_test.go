package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// commitOn writes name=body in repo's current checkout and commits it, returning the
// new HEAD sha.
func commitOn(t *testing.T, repo, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", name)
	return gitT(t, repo, "rev-parse", "HEAD")
}

// TestUnmergedCommits_ListsUntilMerged is the core teardown-guard primitive: a task
// branch's own commits are reported until they become reachable from the default branch,
// after which the branch is "merged" and lists nothing.
func TestUnmergedCommits_ListsUntilMerged(t *testing.T) {
	repo := makeRepo(t)
	def := gitT(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	gitT(t, repo, "checkout", "-q", "-b", "ttorch/x")
	commitOn(t, repo, "feature.txt", "work\n")

	got, err := UnmergedCommits(repo, "ttorch/x", def, "origin/"+def)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an unmerged commit must be listed, got %v", got)
	}

	// Land it onto the default branch; now the branch holds nothing unique.
	tip := gitT(t, repo, "rev-parse", "ttorch/x")
	gitT(t, repo, "checkout", "-q", def)
	gitT(t, repo, "merge", "--ff-only", tip)
	got, err = UnmergedCommits(repo, "ttorch/x", def, "origin/"+def)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a merged branch must list nothing, got %v", got)
	}
}

// TestUnmergedCommits_OriginAheadOfLocal is the correctness case that forces comparing
// against origin/<default> too: a freshly-spawned worker branch is cut from
// origin/<default> (StartBranch), so when origin is ahead of the local default branch,
// comparing only against the local default would wrongly flag an untouched worker as
// holding unmerged work. Excluding origin/<default> as well reports nothing.
func TestUnmergedCommits_OriginAheadOfLocal(t *testing.T) {
	repo := makeRepo(t)
	def := gitT(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	head0 := gitT(t, repo, "rev-parse", "HEAD")

	bare := t.TempDir()
	gitT(t, bare, "init", "--bare", "-q")
	gitT(t, repo, "remote", "add", "origin", bare)

	// Advance the default branch one commit, publish it as origin/<default>, then move
	// the LOCAL default back so origin is one commit ahead of local.
	commitOn(t, repo, "ahead.txt", "ahead\n")
	if err := Push(repo, "origin", def+":refs/heads/"+def); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "fetch", "-q", "origin")
	gitT(t, repo, "reset", "--hard", "-q", head0)

	// A fresh worker branch cut from the (ahead) remote tip, with NO worker commits.
	gitT(t, repo, "branch", "ttorch/y", "origin/"+def)

	// Comparing only against the local default falsely flags origin's extra commit.
	if got, err := UnmergedCommits(repo, "ttorch/y", def); err != nil || len(got) == 0 {
		t.Fatalf("baseline: comparing only against local default should flag origin's commit (got %v err %v)", got, err)
	}
	// Including origin/<default> correctly reports nothing — the worker wrote nothing.
	if got, err := UnmergedCommits(repo, "ttorch/y", def, "origin/"+def); err != nil || len(got) != 0 {
		t.Fatalf("a fresh worker branch off origin must list nothing, got %v err %v", got, err)
	}
}

// TestUnmergedCommits_NoBaseErrors confirms it refuses to list a branch's entire history
// when no base ref resolves, rather than reporting everything as unmerged.
func TestUnmergedCommits_NoBaseErrors(t *testing.T) {
	repo := makeRepo(t)
	def := gitT(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if _, err := UnmergedCommits(repo, def, "refs/heads/nope", "origin/nope"); err == nil {
		t.Fatal("UnmergedCommits with no resolvable base must error")
	}
}

// TestSetRef_CreatesRecoverableRef confirms SetRef points a fully-qualified ref at a
// commit, so teardown's recovery ref keeps discarded work reachable.
func TestSetRef_CreatesRecoverableRef(t *testing.T) {
	repo := makeRepo(t)
	head := gitT(t, repo, "rev-parse", "HEAD")
	ref := "refs/ttorch/discarded/td1-" + head[:12]
	if err := SetRef(repo, ref, head); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRef(repo, ref)
	if err != nil || got != head {
		t.Fatalf("ResolveRef(%s) = %q err %v, want %q", ref, got, err, head)
	}
}

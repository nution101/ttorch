package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
)

// gateClone runs a clone task at head through prep and record with a clean report for every
// required reviewer, so in trusted mode it carries a passing, auto-approved verdict.
func gateClone(t *testing.T, m *Manager, id, head string) {
	t.Helper()
	if _, err := m.TrustPrep(id); err != nil {
		t.Fatalf("TrustPrep: %v", err)
	}
	for _, dim := range m.ReviewersFor(id) {
		writeCleanReport(t, m.P.ReviewInputsDir(id), dim, head)
	}
	v, err := m.TrustRecord(id, "", time.Minute)
	if err != nil {
		t.Fatalf("TrustRecord: %v", err)
	}
	if v.Overall != review.Pass || !approval.Valid(m.P.ApprovalFile(id)) {
		t.Fatalf("a clean review of a green commit should pass and auto-approve; verdict %q", v.Overall)
	}
}

// advanceMain commits name=content on the project's main and pushes it to origin, standing in
// for another task landing first, and returns the new tip.
func advanceMain(t *testing.T, repo, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", name)
	gitIn(t, repo, "commit", "-q", "-m", "landed first")
	gitIn(t, repo, "push", "-q", "origin", "main")
	return gitIn(t, repo, "rev-parse", "HEAD")
}

// snapshotTree records every entry under dir, .git included: its type, its permission bits,
// and the bytes of a file or the target of a link. Two equal snapshots mean nothing under dir
// was created, removed or rewritten.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		entry := fi.Mode().String()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			entry += " " + hex.EncodeToString(sum[:])
		}
		snap[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// assertSameTree fails for every entry that differs between two snapshots of one directory.
func assertSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, b := range before {
		if a, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		} else if a != b {
			t.Errorf("%s changed: %s -> %s", p, b, a)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}

// assertNoScratchLeft fails when the project still lists a worktree outside the two the
// harness made: the project itself and the harness's own worker worktree.
func assertNoScratchLeft(t *testing.T, repo string) {
	t.Helper()
	var listed []string
	for _, line := range strings.Split(gitIn(t, repo, "worktree", "list", "--porcelain"), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			listed = append(listed, p)
		}
	}
	if len(listed) != 2 {
		t.Errorf("the project lists %d worktrees, want the project and the harness worktree only: %v", len(listed), listed)
	}
}

// TestCloneLand_RebaseConflictLeavesTheCloneByteIdentical lands a clone task whose commit
// conflicts with one main gained after it was reviewed. The land must report the conflict as
// one, name the base the worker has to rebase onto, and leave the clone exactly as it was: not
// one byte under it, .git included, may change, and no git may run there. The project keeps
// no scratch worktree and no rebased ref, and the verdict survives for after the worker
// resolves the overlap.
func TestCloneLand_RebaseConflictLeavesTheCloneByteIdentical(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "f.txt", "from the clone\n")
	sentinel := armClone(t, clone)
	calls := recordGitCalls(t)
	gateClone(t, m, "cl1", head)
	base := advanceMain(t, repo, "f.txt", "from main\n")
	before := snapshotTree(t, clone)

	_, err := m.Land("cl1", false)
	if !errors.Is(err, ErrLandRebaseConflict) {
		t.Fatalf("Land = %v, want a rebase conflict", err)
	}
	if !strings.Contains(err.Error(), "rebase its branch onto "+base) {
		t.Errorf("the conflict does not tell the worker which commit to rebase onto (%s): %v", short(base), err)
	}

	assertSameTree(t, before, snapshotTree(t, clone))
	assertNoGitInClone(t, calls(), clone)
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("a program from the clone's config ran during the land")
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != base {
		t.Errorf("main moved to %s on a conflicted land, want it left at %s", short(got), short(base))
	}
	if out := gitOutOrFail(t, repo, "for-each-ref", cloneRebasedRef("cl1")); out != "" {
		t.Errorf("a conflicted land left a rebased ref: %s", out)
	}
	assertNoScratchLeft(t, repo)
	if v, ok := m.TrustShow("cl1"); !ok || v.ReviewedSHA != head {
		t.Errorf("a conflicted land must leave the verdict for %s in place, got %+v ok=%v", short(head), v, ok)
	}
}

// TestCloneLand_CleanRebaseLandsTheRebasedCommitWithTheCarriedVerdict lands a reviewed clone
// task after main gained a disjoint commit. The land must rebase in the project, carry the
// verdict to the rebased commit because the reviewed diff is unchanged, and fast-forward main to
// that commit, not to the clone's head and not to a commit made in the clone. The clone is left
// as it was and no git runs there apart from prep's import.
func TestCloneLand_CleanRebaseLandsTheRebasedCommitWithTheCarriedVerdict(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "from the clone\n")
	sentinel := armClone(t, clone)
	calls := recordGitCalls(t)
	gateClone(t, m, "cl1", head)
	base := advanceMain(t, repo, "other.txt", "from main\n")
	before := snapshotTree(t, clone)

	out, err := m.Land("cl1", false)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	landed := gitIn(t, repo, "rev-parse", "HEAD")
	if landed == head || landed == base {
		t.Fatalf("main is at %s; want a rebase of the clone's %s onto %s", short(landed), short(head), short(base))
	}
	if parent := gitIn(t, repo, "rev-parse", landed+"^"); parent != base {
		t.Fatalf("the landed commit's parent is %s, want the base %s", short(parent), short(base))
	}
	if got := gitIn(t, repo, "rev-parse", cloneRebasedRef("cl1")); got != landed {
		t.Errorf("%s names %s, want the landed commit %s", cloneRebasedRef("cl1"), short(got), short(landed))
	}
	for _, f := range []string{"feature.txt", "other.txt"} {
		gitIn(t, repo, "cat-file", "-e", landed+":"+f)
	}
	if !strings.Contains(out, "rebased "+short(head)+"→"+short(landed)) {
		t.Errorf("the land summary does not report the rebase: %q", out)
	}

	auditLog, err := os.ReadFile(m.P.AuditLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fast-land task=cl1 carried verdict " + short(head) + "->" + short(landed),
		"main -> " + short(landed) + " gate=verdict approver=auto",
	} {
		if !strings.Contains(string(auditLog), want) {
			t.Errorf("the audit log lacks %q:\n%s", want, auditLog)
		}
	}
	if n := strings.Count(string(auditLog), "trust-record task=cl1"); n != 1 {
		t.Errorf("the land must carry the verdict, not re-record it; trust-record count = %d", n)
	}
	if _, ok := m.TrustShow("cl1"); ok {
		t.Error("the carried verdict must be consumed by the merge")
	}

	assertSameTree(t, before, snapshotTree(t, clone))
	assertNoGitInClone(t, calls(), clone)
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("a program from the clone's config ran during the land")
	}
	assertNoScratchLeft(t, repo)
}

// TestCloneLand_MergeRefusedWhenTheCloneAdvanced prepares a land of a clone task, then has
// the worker commit again before the merge. The merge must refuse, because the clone is no
// longer at the head the land rebased and validated, and refuse without moving main or
// spending the verdict and approval a re-run of the land needs.
func TestCloneLand_MergeRefusedWhenTheCloneAdvanced(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "first\n")
	gateClone(t, m, "cl1", head)
	base := advanceMain(t, repo, "other.txt", "from main\n")

	task, _, err := m.Store.GetTask(context.Background(), "cl1")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := m.resolveLandSpec(task, false)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := m.landPrep(task, spec, &sync.Mutex{})
	if err != nil {
		t.Fatalf("landPrep: %v", err)
	}
	if prep.rebasedHead == head {
		t.Fatal("main advanced, so the land must have rebased; the merge below would prove nothing")
	}
	commitInClone(t, clone, "feature.txt", "second\n")

	if _, err := m.landCommit(task, spec, prep); err == nil || !strings.Contains(err.Error(), "advanced") {
		t.Fatalf("landCommit = %v, want a refusal because the clone advanced", err)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != base {
		t.Errorf("main moved to %s, want it left at %s", short(got), short(base))
	}
	if v, ok := m.TrustShow("cl1"); !ok || v.ReviewedSHA != prep.rebasedHead {
		t.Errorf("the refusal must leave the carried verdict in place, got %+v ok=%v", v, ok)
	}
	if !approval.Valid(m.P.ApprovalFile("cl1")) {
		t.Error("the refusal must leave the approval in place")
	}
}

// TestCloneLand_PublishRefusedWhenTheCloneAdvanced is the pr-mode half: integratePR must not
// push a commit once the clone has moved past the head the land validated, and it must find
// that out from the clone's files, not by running git there.
func TestCloneLand_PublishRefusedWhenTheCloneAdvanced(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "first\n")
	calls := recordGitCalls(t)
	if _, err := workHead(db.Task{ID: "cl1", Project: repo, Worktree: clone}); err != nil {
		t.Fatalf("importing the clone's head: %v", err)
	}
	imported := calls()
	task, _, err := m.Store.GetTask(context.Background(), "cl1")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := m.resolveLandSpec(task, false)
	if err != nil {
		t.Fatal(err)
	}
	spec.mode = "pr"
	base := gitIn(t, repo, "rev-parse", "HEAD")
	prep := landPrepResult{base: "refs/heads/main", baseSha: base, preRebase: head, rebasedHead: head}
	commitInClone(t, clone, "feature.txt", "second\n")
	fixture := len(calls()) // the commit above is the worker's own git, not the land's

	if _, err := m.landCommit(task, spec, prep); err == nil || !strings.Contains(err.Error(), "advanced") {
		t.Fatalf("landCommit = %v, want a refusal because the clone advanced", err)
	}
	if out := gitIn(t, repo, "ls-remote", "origin", "refs/heads/ttorch/cl1"); out != "" {
		t.Errorf("the refusal still pushed the branch: %s", out)
	}
	assertNoGitInClone(t, append(imported, calls()[fixture:]...), clone)
}

// TestCloneLand_UngatedLandBehindTheDefaultAsksTheWorkerToRebase lands a clone task in local
// mode with no verdict after main moved on. Its approval pins the clone's head, which a rebase
// in the project cannot move, so no approval could cover a rebased commit. The land must say so
// and name the commit to rebase onto, before rebasing anything, and once the worker has
// rebased and the lead has approved that commit the land goes through.
func TestCloneLand_UngatedLandBehindTheDefaultAsksTheWorkerToRebase(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "local", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "from the clone\n")
	if _, err := m.Approve("cl1", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	base := advanceMain(t, repo, "other.txt", "from main\n")
	before := snapshotTree(t, clone)

	_, err := m.Land("cl1", false)
	if err == nil || !strings.Contains(err.Error(), "have the worker rebase its branch onto "+base) {
		t.Fatalf("Land = %v, want it to ask for the worker's rebase onto %s", err, short(base))
	}
	if errors.Is(err, ErrLandRebaseConflict) {
		t.Error("nothing conflicted, so the refusal must not read as a rebase conflict")
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != base {
		t.Errorf("main moved to %s, want it left at %s", short(got), short(base))
	}
	if out := gitOutOrFail(t, repo, "for-each-ref", cloneRebasedRef("cl1")); out != "" {
		t.Errorf("the refusal came after a rebase: %s", out)
	}
	assertSameTree(t, before, snapshotTree(t, clone))

	gitIn(t, clone, "rebase", "-q", base)
	rebased := gitIn(t, clone, "rev-parse", "HEAD")
	if rebased == head {
		t.Fatal("the worker's rebase did not move its branch")
	}
	if _, err := m.Approve("cl1", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Land("cl1", false); err != nil {
		t.Fatalf("Land after the worker rebased: %v", err)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != rebased {
		t.Errorf("main is at %s, want the worker's rebased commit %s", short(got), short(rebased))
	}
}

// TestCloneLand_LandSetLandsTwoClonesThatTouchDifferentFiles lands two reviewed clone tasks in
// one LandSet after main moved on. Each preps its own scratch at once and the fast-forwards
// serialize, so the second re-preps over the first; both must land, each clone left as it was.
func TestCloneLand_LandSetLandsTwoClonesThatTouchDifferentFiles(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	ids := []string{"cl1", "cl2"}
	clones := map[string]string{}
	for _, id := range ids {
		clones[id] = cloneTask(t, m, repo, id)
		gateClone(t, m, id, commitInClone(t, clones[id], id+".txt", id+"\n"))
	}
	advanceMain(t, repo, "other.txt", "from main\n")
	before := map[string]map[string]string{}
	for _, id := range ids {
		before[id] = snapshotTree(t, clones[id])
	}

	for _, r := range m.LandSet(context.Background(), ids, false) {
		if r.Err != nil {
			t.Errorf("%s did not land: %v", r.TaskID, r.Err)
		}
	}
	tip := gitIn(t, repo, "rev-parse", "HEAD")
	for _, f := range []string{"cl1.txt", "cl2.txt", "other.txt"} {
		gitIn(t, repo, "cat-file", "-e", tip+":"+f)
	}
	for _, id := range ids {
		assertSameTree(t, before[id], snapshotTree(t, clones[id]))
	}
	assertNoScratchLeft(t, repo)
}

// TestRebaseInScratch_RunsNoRebaseHookAndLeavesNoScratch drives the primitive directly. The
// project's pre-rebase and post-rewrite hooks must not run over the worker's commits, the
// rebased commit must sit on the base and be held by the task's rebased ref, and the scratch
// must be gone afterwards. A conflict reports itself as one and leaves no ref and no scratch.
// A malformed task id or sha never reaches git.
func TestRebaseInScratch_RunsNoRebaseHookAndLeavesNoScratch(t *testing.T) {
	repo := newRepoMain(t)
	gitIn(t, repo, "checkout", "-q", "-b", "side")
	work := commitFeature(t, repo, "feature.txt", "work\n")
	conflicting := commitFeature(t, repo, "f.txt", "from the side\n")
	gitIn(t, repo, "checkout", "-q", "main")
	gitIn(t, repo, "update-ref", "refs/test/conflicting", conflicting)
	gitIn(t, repo, "branch", "-q", "-D", "side")
	base := commitFeature(t, repo, "f.txt", "from main\n")

	sentinel := filepath.Join(t.TempDir(), "hook-ran")
	for _, hook := range []string{"pre-rebase", "post-rewrite"} {
		p := filepath.Join(repo, ".git", "hooks", hook)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+hook+" >> '"+sentinel+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	listWorktrees := func() string { return gitIn(t, repo, "worktree", "list", "--porcelain") }
	worktreesBefore := listWorktrees()

	rebased, err := rebaseInScratch(repo, "t1", work, base)
	if err != nil {
		t.Fatalf("rebaseInScratch: %v", err)
	}
	if b, err := os.ReadFile(sentinel); err == nil {
		t.Errorf("the project's hooks ran during the scratch rebase: %s", b)
	}
	if parent := gitIn(t, repo, "rev-parse", rebased+"^"); parent != base {
		t.Errorf("the rebased commit's parent is %s, want the base %s", short(parent), short(base))
	}
	if got := gitIn(t, repo, "rev-parse", cloneRebasedRef("t1")); got != rebased {
		t.Errorf("%s names %s, want %s", cloneRebasedRef("t1"), short(got), short(rebased))
	}
	if got := listWorktrees(); got != worktreesBefore {
		t.Errorf("the scratch worktree was left behind:\n%s", got)
	}

	if _, err := rebaseInScratch(repo, "t2", conflicting, base); !errors.Is(err, ErrLandRebaseConflict) {
		t.Errorf("a conflicting rebase = %v, want ErrLandRebaseConflict", err)
	}
	if out := gitOutOrFail(t, repo, "for-each-ref", cloneRebasedRef("t2")); out != "" {
		t.Errorf("a conflicting rebase left a ref: %s", out)
	}
	if got := listWorktrees(); got != worktreesBefore {
		t.Errorf("a conflicting rebase left its scratch worktree behind:\n%s", got)
	}

	calls := recordGitCalls(t)
	for _, c := range []struct{ task, sha, onto string }{
		{"../t3", work, base},
		{"-t3", work, base},
		{"t3", "HEAD", base},
		{"t3", work, "main"},
	} {
		if _, err := rebaseInScratch(repo, c.task, c.sha, c.onto); err == nil {
			t.Errorf("rebaseInScratch(%q, %q, %q) was accepted", c.task, c.sha, c.onto)
		}
	}
	if n := len(calls()); n != 0 {
		t.Errorf("a malformed task id or sha reached git %d times", n)
	}
}

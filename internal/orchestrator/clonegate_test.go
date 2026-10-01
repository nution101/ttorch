package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
)

// cloneTask provisions a clone for task id the way the per-worker-clones design provisions one
// and records a done task for it in m's store. The clone is a fresh repository whose object
// store borrows repo's through an alternates file, with no remote that points at repo, an
// origin/main snapshot at repo's origin/main, and ttorch/<id> checked out there. It stands in
// for the clone pool, which is built separately; the gate only needs a directory of that shape.
func cloneTask(t *testing.T, m *Manager, repo, id string) string {
	t.Helper()
	base := gitIn(t, repo, "rev-parse", "refs/remotes/origin/main")
	clone := filepath.Join(t.TempDir(), "clones", "pool", "1")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "init", "-q", "--template="+t.TempDir(), "--initial-branch=main")
	common := gitIn(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	info := filepath.Join(clone, ".git", "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(common, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "update-ref", "refs/remotes/origin/main", base)
	gitIn(t, clone, "checkout", "-q", "-B", "ttorch/"+id, base)

	ctx := context.Background()
	proj, err := m.Store.UpsertProject(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.CreateTask(ctx, db.Task{
		ID: id, ProjectID: proj.ID, Worktree: clone, Status: db.StatusDone, Title: "work",
	}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	return clone
}

// armClone gives the clone the config a hostile worker would: a core.fsmonitor that `git
// status` runs, a diff.external that `git diff` runs, and a -diff attribute on .go files. Each
// program touches the returned sentinel, so its existence proves git ran in the clone with
// the clone's own config. Call it after the fixture's own commits in the clone.
func armClone(t *testing.T, clone string) string {
	t.Helper()
	sentinel := filepath.Join(t.TempDir(), "clone-program-ran")
	prog := filepath.Join(t.TempDir(), "hostile.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "config", "core.fsmonitor", prog)
	gitIn(t, clone, "config", "diff.external", prog)
	if err := os.MkdirAll(filepath.Join(clone, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".git", "info", "attributes"), []byte("*.go -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sentinel
}

// gitCall is one recorded git invocation: the directory it ran in and its arguments.
type gitCall struct {
	dir  string
	args []string
}

// recordGitCalls puts a git on PATH that appends each invocation's working directory and
// arguments to a log and then runs the real git, and returns a reader for the log. Every git
// the code under test starts by name goes through it.
func recordGitCalls(t *testing.T) func() []gitCall {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf '%s' \"$PWD\"; for a in \"$@\"; do printf '\\t%s' \"$a\"; done; printf '\\n'; } >> '" + log + "'\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []gitCall {
		b, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var calls []gitCall
		for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			f := strings.Split(line, "\t")
			calls = append(calls, gitCall{dir: f[0], args: f[1:]})
		}
		return calls
	}
}

// assertNoGitInClone fails for every recorded call that ran inside the clone or named it,
// except the import: a fetch that names the clone once, as the argument after "--", bare or as
// a file:// URL. That fetch
// starts upload-pack in the clone, which the design accepts as the one process of ours that
// reads a clone's repository. It also fails when no import was recorded, since a recorder that
// saw nothing would pass every check below.
func assertNoGitInClone(t *testing.T, calls []gitCall, clone string) {
	t.Helper()
	imported := false
	defer func() {
		if !imported {
			t.Errorf("no import fetch was recorded among %d git calls; the recorder is not seeing the code under test", len(calls))
		}
	}()
	spellings := []string{clone}
	if r, err := filepath.EvalSymlinks(clone); err == nil && r != clone {
		spellings = append(spellings, r)
	}
	inClone := func(p string) bool {
		p = strings.TrimPrefix(p, "file://") // the import names its source as a file:// URL
		for _, c := range spellings {
			if p == c || strings.HasPrefix(p, c+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	for _, c := range calls {
		if inClone(c.dir) {
			t.Errorf("git ran with its working directory in the clone: git %s", strings.Join(c.args, " "))
			continue
		}
		fetch := false
		for _, a := range c.args {
			fetch = fetch || a == "fetch"
		}
		for i, a := range c.args {
			if !inClone(a) {
				continue
			}
			if fetch && i > 0 && c.args[i-1] == "--" {
				imported = true
				continue // the import
			}
			t.Errorf("git named the clone outside the import: git %s", strings.Join(c.args, " "))
		}
	}
}

// commitInClone commits name=content on the clone's branch with the fixture's own git and
// returns the new head.
func commitInClone(t *testing.T, clone, name, content string) string {
	t.Helper()
	return commitFeature(t, clone, name, content)
}

// TestCloneTask_GateLandsTheImportedCommitWithoutRunningGitInTheClone drives a clone task
// through the whole trusted gate: prep, record with its auto-approval, and the merge. The
// clone's config would run a program on any git status or diff there, and the clone holds an
// untracked file that a dirty check would refuse. The commit must land on main, and no git
// the gate starts may run in the clone or name it, apart from the one import fetch.
func TestCloneTask_GateLandsTheImportedCommitWithoutRunningGitInTheClone(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "from the clone\n")
	sentinel := armClone(t, clone)
	if err := os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := recordGitCalls(t)

	if _, err := m.TrustPrep("cl1"); err != nil {
		t.Fatalf("TrustPrep: %v", err)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/ttorch/clones/cl1/"+head); got != head {
		t.Fatalf("the import ref names %s, want the clone's head %s", got, head)
	}
	dir := m.P.ReviewInputsDir("cl1")
	for _, dim := range m.ReviewersFor("cl1") {
		writeCleanReport(t, dir, dim, head)
	}
	v, err := m.TrustRecord("cl1", "", time.Minute)
	if err != nil {
		t.Fatalf("TrustRecord: %v", err)
	}
	if v.Overall != review.Pass || !approval.Valid(m.P.ApprovalFile("cl1")) {
		t.Fatalf("a clean review of a green clone commit should pass and auto-approve; verdict %q, approval valid %v", v.Overall, approval.Valid(m.P.ApprovalFile("cl1")))
	}
	if _, err := m.MergeLocal("cl1", false); err != nil {
		t.Fatalf("MergeLocal: %v", err)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("main is at %s, want the clone's commit %s", got, head)
	}

	assertNoGitInClone(t, calls(), clone)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("a program from the clone's config ran during the gate")
	}
}

// TestCloneTask_PrepMatchesAWorktreeAtTheSameCommit preps a worktree task and a clone task at
// one commit and requires the same staged inputs from both. The clone carries a -diff
// attribute and an external diff, which change the patch and the line count when git runs
// there, and a replace ref that swaps the reviewed commit for a docs-only one, which changes
// the file list and so the reviewer set. Only a prep that reads the commit in the project
// repository gives the worktree's answer.
func TestCloneTask_PrepMatchesAWorktreeAtTheSameCommit(t *testing.T) {
	m, repo, wt := trustHarness(t, "wt1", "trusted", "exit 0")
	head := commitCodeFiles(t, wt)
	clone := cloneTask(t, m, repo, "cl1")
	docsOnly := commitInClone(t, clone, "NOTES.md", "notes\n")
	gitIn(t, clone, "checkout", "-q", "-B", "ttorch/cl1", head)
	gitIn(t, clone, "update-ref", "refs/replace/"+head, docsOnly)
	armClone(t, clone)

	for _, id := range []string{"wt1", "cl1"} {
		if _, err := m.TrustPrep(id); err != nil {
			t.Fatalf("TrustPrep %s: %v", id, err)
		}
	}
	for _, name := range []string{"diff.patch", reviewersFileName, "head.txt", review.StagedValidateFile} {
		want, err := os.ReadFile(filepath.Join(m.P.ReviewInputsDir("wt1"), name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(m.P.ReviewInputsDir("cl1"), name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between the worktree task and the clone task at %s:\nworktree:\n%s\nclone:\n%s", name, short(head), want, got)
		}
	}
}

// TestCloneTask_ReviewDiffAndApproveReadTheImportedCommit checks the lead's two direct reads of
// a clone task. review-diff shows the committed change and not an edit the worker left
// uncommitted, and approve pins the token to the clone's head after importing it.
func TestCloneTask_ReviewDiffAndApproveReadTheImportedCommit(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "local", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	head := commitInClone(t, clone, "feature.txt", "committed line\n")
	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("committed line\nuncommitted line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := armClone(t, clone)
	calls := recordGitCalls(t)

	diff, err := m.ReviewDiff("cl1", false)
	if err != nil {
		t.Fatalf("ReviewDiff: %v", err)
	}
	if !strings.Contains(diff, "+committed line") || strings.Contains(diff, "uncommitted line") {
		t.Fatalf("review-diff of a clone must show the committed change only:\n%s", diff)
	}
	if _, err := m.Approve("cl1", time.Minute, false); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	data, _ := approval.Data(m.P.ApprovalFile("cl1"))
	if _, sha, _ := splitApprovalPayload(data); sha != head {
		t.Fatalf("the approval is pinned to %q, want the clone's head %s", sha, head)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/ttorch/clones/cl1/"+head); got != head {
		t.Fatalf("approve did not import the clone's head: the ref names %s", got)
	}
	assertNoGitInClone(t, calls(), clone)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("a program from the clone's config ran during review-diff or approve")
	}
}

// TestCloneTask_PatchIgnoresDiffDriversInTheGlobalConfig stages a clone task's patch with a
// global config that a worker can write through `git config --global`: an external diff that
// would replace the patch, a textconv driver that would rewrite a file's lines, and a -diff
// attribute that would print a source file as binary. The reviewers must still get the real
// lines and no driver may run.
func TestCloneTask_PatchIgnoresDiffDriversInTheGlobalConfig(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	commitInClone(t, clone, "a.go", "package a\n")
	commitInClone(t, clone, "n.txt", "real text\n")

	scratch := t.TempDir()
	sentinel := filepath.Join(scratch, "driver-ran")
	prog := filepath.Join(scratch, "driver.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+sentinel+"'\necho FAKE\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	attrs := filepath.Join(scratch, "attributes")
	if err := os.WriteFile(attrs, []byte("*.go -diff\n*.txt diff=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(scratch, "gitconfig")
	cfg := "[diff]\n\texternal = " + prog + "\n[diff \"evil\"]\n\ttextconv = " + prog + "\n[core]\n\tattributesFile = " + attrs + "\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := m.TrustPrep("cl1"); err != nil {
		t.Fatalf("TrustPrep: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join(m.P.ReviewInputsDir("cl1"), "diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+package a", "+real text"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("the staged patch lacks %q:\n%s", want, patch)
		}
	}
	if strings.Contains(string(patch), "FAKE") || strings.Contains(string(patch), "Binary files") {
		t.Errorf("a driver or attribute from the global config shaped the staged patch:\n%s", patch)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("a diff driver from the global config ran while the patch was staged")
	}
}

// TestCloneTask_ImportFailureBlocksTheGate points a clone's branch at a commit the clone does
// not have. The daemon gate observes that head from files, prep's import is refused, and the
// refusal is surfaced as a block for the manager rather than retried every tick.
func TestCloneTask_ImportFailureBlocksTheGate(t *testing.T) {
	m, repo, _ := trustHarness(t, "wt1", "trusted", "exit 0")
	clone := cloneTask(t, m, repo, "cl1")
	missing := strings.Repeat("cd", 20)
	if err := os.WriteFile(filepath.Join(clone, ".git", "refs", "heads", "ttorch", "cl1"), []byte(missing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordingReviewer(t, false)

	out, err := m.gateOnceAt("cl1", time.Minute, 2, time.Hour, time.Now())
	if err != nil || out != GateBlocked {
		t.Fatalf("gateOnceAt = %q, %v; want %q", out, err, GateBlocked)
	}
	if !gateBlockedEventMentions(t, m, "cl1", "could not import") {
		t.Fatalf("the gate_blocked event does not say the import failed: %s", gateBlockedPayload(t, m, "cl1"))
	}
	if gitOutOrFail(t, repo, "for-each-ref", "refs/ttorch/clones/") != "" {
		t.Fatal("a refused import left a ref in the project repository")
	}
}

// TestCloneTask_WorkdirKind covers how the seam reads a workdir's kind: a gitfile is a
// worktree, a git directory is a clone, a workdir that is the project itself is a worktree
// whatever its .git looks like, and anything else, a symlink at the workdir included, is
// refused rather than handed to git.
func TestCloneTask_WorkdirKind(t *testing.T) {
	repo := newRepoMain(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "task/k", wt)
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, filepath.Dir(clone), "init", "-q", clone)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(t.TempDir(), "pointer")
	if err := os.Symlink(clone, pointer); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, workdir string
		clone, err    bool
	}{
		{name: "linked worktree", workdir: wt},
		{name: "clone", workdir: clone, clone: true},
		{name: "the project itself", workdir: repo},
		{name: "symlinked .git", workdir: linked, err: true},
		{name: "symlinked workdir", workdir: pointer, err: true},
		{name: "no .git", workdir: t.TempDir(), err: true},
		{name: "no workdir", workdir: "", err: true},
	} {
		w, err := openWork(db.Task{ID: "k", Project: repo, Worktree: c.workdir})
		if (err != nil) != c.err {
			t.Errorf("%s: openWork error = %v, want error %v", c.name, err, c.err)
			continue
		}
		if err == nil && w.clone != c.clone {
			t.Errorf("%s: read as clone=%v, want %v", c.name, w.clone, c.clone)
		}
	}
}

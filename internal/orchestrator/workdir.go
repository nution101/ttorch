package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// A task's working directory is one of two kinds. A linked worktree of the project repository is
// what every spawn has produced so far. A private clone is what a spawn provisions when
// TTORCH_WORKER_CLONES is set. They hold their git state in different places, and this file is
// the one place the gate asks which kind it has.
//
// A linked worktree shares the project's refs and object store, so a commit made there is an
// object in the project the moment it exists, and the gate has always read the worker's HEAD,
// its dirty state and its diffs by running git in the worktree. For a worktree every function
// here does exactly that, so a fleet with no clone in it gates the way it did before this file.
//
// A clone's config, hooks, attributes and refs are the worker's own, and git run there can be
// made to run a program the worker chose: status runs core.fsmonitor, diff runs diff.external,
// and a replace ref changes what a diff reports. So the gate runs no git in a clone. It reads
// the clone's HEAD from files, fetches that one commit into the project repository under
// refs/ttorch/clones/<task>/<sha>, and runs every diff and check in the project repository
// against that sha. The fetch is the only process of ours that reads the clone's repository.
//
// The kind is read from the workdir, not from the flag, so a task keeps the kind it was spawned
// with when the flag changes.

// work is a task's working directory as one gate step resolved it. The step resolves it once
// and takes every read through the result, so a worker that rewrites its .git between two reads
// of the same step cannot send the second read down the other path.
type work struct {
	t     db.Task
	clone bool
}

// openWork resolves t's working directory for one step.
//
// A workdir that is neither kind is refused, with worktree.KindOf's reason. A missing or
// symlinked .git, or a workdir that is itself a symlink or not a directory, is not something
// either kind has, and running git there anyway lets git discover whatever repository sits
// above the directory, which for a clone pool is a directory any worker can write to.
//
// A workdir that IS the project, which is what a cc session opened in place records, reads as a
// worktree: git there runs in the lead's own repository, as it always has.
func openWork(t db.Task) (work, error) {
	if t.Worktree == "" {
		return work{}, fmt.Errorf("task %q has no working directory", t.ID)
	}
	if filepath.Clean(t.Worktree) == filepath.Clean(t.Project) {
		return work{t: t}, nil
	}
	k, err := worktree.KindOf(t.Worktree)
	switch k {
	case worktree.KindWorktree:
		return work{t: t}, nil
	case worktree.KindClone:
		return work{t: t, clone: true}, nil
	}
	return work{}, fmt.Errorf("the working directory for %q is neither a linked worktree nor a clone: %w", t.ID, err)
}

// workHead returns the commit the gate pins t to, the sha every later step validates, diffs,
// records and merges, for a step that reads nothing else from the workdir. See work.head.
func workHead(t db.Task) (string, error) {
	w, err := openWork(t)
	if err != nil {
		return "", err
	}
	return w.head()
}

// observedHead returns the commit t's workdir is at without importing it, for a step that only
// compares heads. See work.observe.
func observedHead(t db.Task) (string, error) {
	w, err := openWork(t)
	if err != nil {
		return "", err
	}
	return w.observe()
}

// head returns the commit the step pins to. For a clone it makes that commit an object in the
// project repository first, because the gate-config guard, validate, the stale-base check and
// the fast-forward all run there and need it as one.
//
// The observed sha is the worker's claim, since the worker writes its own HEAD. That is enough:
// everything downstream pins to the sha and judges the content it names, so the most a lie buys
// is choosing which of the worker's own commits gets reviewed.
func (w work) head() (string, error) {
	if !w.clone {
		return worktree.Head(w.t.Worktree)
	}
	sha, err := w.observe()
	if err != nil {
		return "", err
	}
	if _, err := worktree.ImportCommit(context.Background(), w.t.Project, filepath.Clean(w.t.Worktree), w.t.ID, sha); err != nil {
		return "", fmt.Errorf("could not import %s from the clone for %q into %s: %w", short(sha), w.t.ID, w.t.Project, err)
	}
	return sha, nil
}

// observe returns the commit the workdir is at. For a clone it reads files and starts no
// process, which is why the per-tick gate check and the freshness bracket before a merge use it.
// worktree.ObserveHead takes no deadline, so a workdir on a path that never answers holds the
// step the way `git rev-parse` in a worktree always could.
func (w work) observe() (string, error) {
	if !w.clone {
		return worktree.Head(w.t.Worktree)
	}
	sha, ok := worktree.ObserveHead(w.t.Worktree, w.t.Project)
	if !ok {
		return "", fmt.Errorf("could not read the HEAD of the clone for %q from its files", w.t.ID)
	}
	return sha, nil
}

// clean reports whether the workdir has no uncommitted change, for the steps that refuse a
// worker mid-edit. A clone always reads as clean. The check is `git status` in the workdir,
// which runs the clone's core.fsmonitor, and the gate's correctness does not rest on it: it
// reviews, validates and merges the imported commit, so an edit the worker never committed
// cannot reach any of them.
func (w work) clean() (bool, error) {
	if w.clone {
		return true, nil
	}
	return worktree.IsClean(w.t.Worktree)
}

// gitDir is the directory a step runs its reads of committed objects in: the worktree, as it
// always has been, or the project repository for a clone.
func (w work) gitDir() string {
	if w.clone {
		return w.t.Project
	}
	return w.t.Worktree
}

// patch is the committed three-dot diff base...rev that the reviewers read and the verdict's
// diff identity is taken over (see mergeBaseDiff).
//
// For a clone it runs in the project repository with --text, --no-ext-diff and --no-textconv,
// so no diff driver and no attribute in any config the gate reads can change the bytes the
// reviewers are shown. A -diff attribute otherwise prints a changed source file as "Binary
// files differ", and a diff.external in the global config, which a worker can set with `git
// config --global`, would write the patch itself. A worktree keeps the command it has always
// had, so its patches and the diff identities already recorded over them do not move.
func (w work) patch(base, rev string) (string, error) {
	if !w.clone {
		return mergeBaseDiff(w.t.Worktree, base, rev)
	}
	return gitOut(w.t.Project, "diff", "--text", "--no-ext-diff", "--no-textconv", base+"..."+rev)
}

// reviewDiff is the diff ReviewDiff shows the lead. A worktree's is its working tree against
// base, as it has always been. A clone's is the imported head against base, in the project
// repository with the same flags as patch: what the lead approves is a commit, and the clone's
// working tree belongs to the worker.
func (w work) reviewDiff(base string, stat bool) (string, error) {
	if !w.clone {
		return worktree.Diff(w.t.Worktree, base, stat)
	}
	head, err := w.head()
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--text", "--no-ext-diff", "--no-textconv"}
	if stat {
		args = append(args, "--stat")
	}
	out, err := gitOut(w.t.Project, append(args, base, head)...)
	return strings.TrimSpace(out), err
}

// mirrorSource is where prepareReviewWorkspace may fetch the reviewed commit from when its
// mirror of the project lacks it: the worktree, as before, or nowhere for a clone. A clone's
// commit is in the mirror already, because the import wrote it under refs/ttorch/clones/ and a
// mirror copies every ref. Fetching from the clone would run upload-pack in the worker's
// repository for whatever its HEAD has become.
func (w work) mirrorSource() string {
	if w.clone {
		return ""
	}
	return w.t.Worktree
}

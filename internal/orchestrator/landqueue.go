package orchestrator

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/worktree"
)

// LandResult is the outcome of attempting to land one task within a LandSet. Exactly one of
// Output (the land summary) or Err is meaningful: Err == nil means the task landed and Output
// holds the same summary the single `ttorch land` prints; Err != nil means it did not land and
// Output is empty. Attempts records how many prep→fast-forward cycles it took — 1 in the common
// case, >1 when this task lost a fast-forward race to a sibling in the same repo and re-prepped
// against the advanced default before landing.
type LandResult struct {
	TaskID   string
	Repo     string
	Output   string
	Err      error
	Attempts int
}

// LandSet lands every task in taskIDs concurrently, each as soon as it is individually ready,
// with NO global barrier — the synchronous gate→merge→next loop is replaced by an async,
// pipelined queue. Each task flows through its own landing pipeline: fetch, rebase onto the
// current default, validate the rebased tree, carry its review verdict forward over a clean
// rebase (or re-gate when the content changed), and finally fast-forward the default branch.
// Everything BEFORE the fast-forward runs per task without blocking the others — the expensive
// validate in particular overlaps across tasks — because landPrep touches only the task's own
// worktree and an immutable detached checkout, never the shared default.
//
// Only the fast-forward itself serializes, and only per repo: the default branch is a single
// git ref that one process advances at a time (a hard git requirement, not a ttorch choice).
// LandSet holds a per-repo lock across just that fast-forward — the cheap step — so the
// throughput bound is file-disjointness, not task count:
//
//   - Tasks in DIFFERENT repos are fully independent: separate default branches, separate
//     locks, no contention at any stage.
//   - Tasks in the SAME repo with DISJOINT footprints overlap all of their prep and serialize
//     only the fast-forward. When an earlier sibling advances the default, a later one finds
//     its prep stale, re-rebases onto the new tip (clean, because the files are disjoint) and
//     carries its verdict forward with no re-review — then lands. The validate re-runs, but in
//     concurrent prep, off the lock.
//   - Tasks in the same repo touching the SAME files (same package) cannot both fast-forward a
//     linear history: the later one's re-rebase onto the earlier one's change conflicts (or
//     changes its reviewed diff), so it falls back to the existing re-gate path. Same-package
//     tasks therefore serialize the actual landing — exactly as a hand-run sequence would, and
//     by design: there is no safe way to fast-forward two overlapping changes independently.
//
// Results are returned in taskIDs order. LandSet never merges anything Land would not: it reuses
// resolveLandSpec/landPrep/landCommit unchanged, so every approval, verdict, validate, and
// post-merge verify gate applies identically — the queue only changes WHEN each task lands, not
// WHETHER it is allowed to.
func (m *Manager) LandSet(ctx context.Context, taskIDs []string, requireVerdict bool) []LandResult {
	results := make([]LandResult, len(taskIDs))

	// Resolve each task up front and tally per-repo membership: a task can lose the
	// fast-forward race at most once per OTHER landing in its repo, so the per-repo count
	// bounds the retry budget (plus slack for genuine external origin advances).
	type job struct {
		idx  int
		id   string
		task db.Task
	}
	var jobs []job
	repoCount := map[string]int{}
	for i, id := range taskIDs {
		t, ok, err := m.Store.GetTask(ctx, id)
		if err != nil || !ok {
			results[i] = LandResult{TaskID: id, Err: fmt.Errorf("unknown task %q", id)}
			continue
		}
		results[i] = LandResult{TaskID: id, Repo: t.Project}
		jobs = append(jobs, job{idx: i, id: id, task: t})
		repoCount[t.Project]++
	}
	if len(jobs) == 0 {
		return results
	}

	// One fast-forward lock and one fetch lock per repo, created on demand and shared by every
	// task in that repo. The fast-forward lock serializes the default-branch advance; the fetch
	// lock serializes the shared remote-tracking-ref update. They are distinct so a fetch and a
	// fast-forward (different refs) never block each other — only same-kind operations serialize.
	var mapMu sync.Mutex
	ffLocks := map[string]*sync.Mutex{}
	fetchLocks := map[string]*sync.Mutex{}
	lockFor := func(locks map[string]*sync.Mutex, repo string) *sync.Mutex {
		mapMu.Lock()
		defer mapMu.Unlock()
		l := locks[repo]
		if l == nil {
			l = &sync.Mutex{}
			locks[repo] = l
		}
		return l
	}

	// Bound the number of in-flight prep pipelines: each runs a full validate suite, so an
	// unbounded fan-out over a large done set could thrash the machine. The fast-forward bound
	// is still file-disjointness; this only caps how many validates run at once.
	limit := runtime.NumCPU()
	if limit < 1 {
		limit = 1
	}
	if limit > len(jobs) {
		limit = len(jobs)
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ffMu := lockFor(ffLocks, j.task.Project)
			fetchMu := lockFor(fetchLocks, j.task.Project)
			maxAttempts := repoCount[j.task.Project] + 4
			// results[j.idx] is a distinct slice element per goroutine (no aliasing); wg.Wait
			// establishes the happens-before for the post-loop read below.
			results[j.idx] = m.landOne(ctx, j.task, requireVerdict, ffMu, fetchMu, maxAttempts)
		}(j)
	}
	wg.Wait()
	return results
}

// landOne runs one task's async landing pipeline to completion: it preps (outside any
// fast-forward lock, so it overlaps every sibling), then attempts the fast-forward under the
// repo lock. If a sibling advanced the default since this prep — making the rebased commit no
// longer a fast-forward of it — the attempt reports stale and landOne re-preps against the new
// tip and retries. A terminal prep or commit error (rebase conflict, red validate, a verdict
// that cannot be carried, a refused merge) ends the task immediately. The retry budget is
// bounded so external churn cannot livelock it.
func (m *Manager) landOne(ctx context.Context, t db.Task, requireVerdict bool, ffMu, fetchMu *sync.Mutex, maxAttempts int) LandResult {
	res := LandResult{TaskID: t.ID, Repo: t.Project}
	spec, err := m.resolveLandSpec(t, requireVerdict)
	if err != nil {
		res.Err = err
		return res
	}
	for attempt := 1; ; attempt++ {
		res.Attempts = attempt
		if err := ctx.Err(); err != nil {
			res.Err = fmt.Errorf("land: %q canceled before it landed: %w", t.ID, err)
			return res
		}
		prep, err := m.landPrep(t, spec, fetchMu)
		if err != nil {
			res.Err = err
			return res
		}
		out, stale, err := m.landCommitLocked(spec, t, prep, ffMu)
		if stale {
			if attempt >= maxAttempts {
				res.Err = fmt.Errorf("land: %q kept losing the fast-forward race after %d attempts; the default branch %s is advancing faster than it can land (external churn?) — retry, or land it on its own", t.ID, attempt, spec.def)
				return res
			}
			continue // a sibling advanced the default under us; re-prep against the new tip
		}
		res.Output, res.Err = out, err
		return res
	}
}

// landCommitLocked holds the repo's fast-forward lock across exactly the default-branch advance.
// Under the lock it first confirms the prep is still landable: the LOCAL default must be an
// ancestor of the rebased commit, or a concurrent landing advanced it off this prep's rebase
// base and the rebased commit no longer descends from it — reported as stale (the caller
// re-preps) rather than left for MergeLocal to refuse as a non-fast-forward. When still
// landable it runs landCommit, whose own gates (approval, verdict, fresh/ reused validate,
// post-merge verify) are unchanged. The expensive validate already ran in landPrep, outside
// this lock, so the lock is held only for the cheap, inherently-serial fast-forward.
func (m *Manager) landCommitLocked(spec landSpec, t db.Task, prep landPrepResult, ffMu *sync.Mutex) (out string, stale bool, err error) {
	ffMu.Lock()
	defer ffMu.Unlock()
	defSha, rerr := worktree.ResolveRef(spec.repo, spec.def)
	if rerr != nil {
		return "", false, fmt.Errorf("land: could not resolve the local default %s for %q: %w", spec.def, spec.taskID, rerr)
	}
	if !worktree.IsAncestor(spec.repo, defSha, prep.rebasedHead) {
		return "", true, nil
	}
	out, err = m.landCommit(t, spec, prep)
	return out, false, err
}

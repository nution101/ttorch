package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/worktree"
)

// Validate runs the worktree's detected checks for a task. It returns nil results
// when no checks are detected (the caller reports that distinctly from a pass).
func (m *Manager) Validate(taskID string) ([]validate.Result, error) {
	ctx := context.Background()
	t, ok, err := m.Store.GetTask(ctx, taskID)
	if err != nil || !ok {
		return nil, fmt.Errorf("unknown task %q", taskID)
	}
	steps := validate.Detect(t.Worktree)
	if len(steps) == 0 {
		return nil, nil
	}
	results := validate.Run(t.Worktree, steps)
	// Record a typed, manager-authored, non-actionable 'validated' event carrying the
	// pass/fail tally (§3.4). Best-effort: the results are already produced for the
	// caller, so a failed audit append must not mask them.
	passed := len(results) - len(validate.Failures(results))
	if _, err := m.Store.AppendEvent(ctx, db.Event{
		EntityType: db.EntityTypeTask, EntityID: taskID, Type: db.EventValidated, Actor: db.ActorManager,
		Payload: fmt.Sprintf("%d/%d checks passed", passed, len(results)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ttorch: could not record the validated event for %s: %v\n", taskID, err)
	}
	return results, nil
}

// gateConfigFiles define the trust gate itself: the validation script and the repo's
// delivery-mode/gate config. A trusted AUTO-merge must never change them — altering the
// gate requires an explicit human approval.
var gateConfigFiles = []string{".ttorch/validate.sh", "AGENTS.md"}

// gateValidate runs the trust gate's fresh validation against the worker's worktree, but
// using the validation DEFINITION resolved from the repo's DEFAULT BRANCH: the
// .ttorch/validate.sh as it exists on the default branch (run from a temp copy so a
// worker cannot weaken its own gate by editing the script on its branch), or, when the
// default branch defines none, the built-in ecosystem steps (fixed ttorch commands the
// worker cannot redefine). It returns whether the worktree is green, the results for
// reporting, and any error. No detected checks => NOT green (a hard block): an empty
// Failures() must never be read as a pass.
func gateValidate(worktreeDir, repo string) (bool, []validate.Result, error) {
	def := worktree.DefaultBranch(repo)
	var steps []validate.Step
	if script, ok := worktree.ShowFile(repo, def, ".ttorch/validate.sh"); ok {
		tmp, err := os.CreateTemp("", "ttorch-gate-validate-*.sh")
		if err != nil {
			return false, nil, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(script); err != nil {
			tmp.Close()
			return false, nil, err
		}
		if err := tmp.Close(); err != nil {
			return false, nil, err
		}
		steps = []validate.Step{{Name: "gate", Cmd: []string{"sh", tmp.Name()}}}
	} else {
		steps = validate.DetectDefaults(worktreeDir)
	}
	if len(steps) == 0 {
		return false, nil, nil
	}
	results := validate.Run(worktreeDir, steps)
	return gateGreen(steps, results), results, nil
}

// gateGreen reports whether every detected step produced a passing result. A step that
// failed even to start is reported by validate.Run as a non-passing result, so it is
// treated as a failure (block), never conflated with a pass; a missing result for any
// step is likewise not green.
func gateGreen(steps []validate.Step, results []validate.Result) bool {
	return len(steps) > 0 && len(results) == len(steps) && len(validate.Failures(results)) == 0
}

// validateCommitted runs the trust gate's validation against an IMMUTABLE detached
// checkout of the committed sha — never the live worktree, which a running worker could
// mutate mid-run (race good bytes in, validate, revert) — using the validation
// definition from the default branch. This makes the thing validated exactly the thing
// that fast-forwards.
func validateCommitted(repo, sha string) (bool, []validate.Result, error) {
	parent, err := os.MkdirTemp("", "ttorch-gate-*")
	if err != nil {
		return false, nil, err
	}
	defer os.RemoveAll(parent)
	// A unique basename (the temp dir's own name) avoids linked-worktree id collisions
	// when gates run concurrently.
	co := filepath.Join(parent, filepath.Base(parent))
	if err := worktree.AddDetached(repo, co, sha); err != nil {
		return false, nil, err
	}
	defer worktree.RemoveWorktree(repo, co)
	return gateValidate(co, repo)
}

// validateForMerge resolves the trust gate's green/results decision for the committed sha,
// reusing the commit-pinned validate trust prep already staged (validate.json, pinned by
// head.txt) when it pins to EXACTLY this sha — so the identical commit is not run through
// the full suite twice (once at prep, once at the merge). It falls back to a fresh
// validateCommitted whenever no staged result pins to this sha: the worker advanced HEAD
// since prep, or prep never ran. reused reports which path was taken. The green semantics
// are identical either way (validateCommitted persisted the same []validate.Result prep
// staged), so the merged commit is always backed by a green, commit-pinned validate — the
// gate just stops re-running it.
func (m *Manager) validateForMerge(repo, taskID, sha string) (green bool, results []validate.Result, reused bool, err error) {
	if staged, ok := m.reusablePrepValidate(taskID, sha); ok {
		return stagedGreen(staged), staged, true, nil
	}
	green, results, err = validateCommitted(repo, sha)
	return green, results, false, err
}

// reusablePrepValidate returns the validate results trust prep staged for taskID IF AND
// ONLY IF they are pinned (via head.txt) to exactly sha — i.e. the staged run validated
// the very commit now being merged. A missing/mismatched head.txt, or an unreadable or
// malformed validate.json, yields ok=false so the caller re-validates rather than trusts a
// stale or absent result. It reads committed objects' recorded outcome only; the immutable
// sha guarantees the tree behind the result is unchanged.
func (m *Manager) reusablePrepValidate(taskID, sha string) ([]validate.Result, bool) {
	dir := m.P.ReviewInputsDir(taskID)
	pinned, err := os.ReadFile(filepath.Join(dir, "head.txt"))
	if err != nil || strings.TrimSpace(string(pinned)) != sha {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "validate.json"))
	if err != nil {
		return nil, false
	}
	var results []validate.Result
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, false
	}
	return results, true
}

// stagedGreen mirrors gateGreen's pass semantics for a persisted result set, where the
// originating step list is no longer available: at least one check ran and none failed. An
// empty or null result set is a no-checks-detected hard BLOCK, never a pass (an empty
// Failures() must not read as green) — exactly as the merge gate treats it.
func stagedGreen(results []validate.Result) bool {
	return len(results) > 0 && len(validate.Failures(results)) == 0
}

// hasDefaultBranchGateScript reports whether the repo's default branch defines the gate
// script (.ttorch/validate.sh). The trusted AUTO path requires it: without it,
// gateValidate falls back to ecosystem detection (go.mod / package.json) on the worker's
// own checkout, whose presence and scripts the worker controls — so "green" would be
// worker-influenced. A human `ttorch approve` is still allowed to use the detection
// fallback (a human is then in the loop).
func hasDefaultBranchGateScript(repo string) bool {
	_, ok := worktree.ShowFile(repo, worktree.DefaultBranch(repo), ".ttorch/validate.sh")
	return ok
}

// diffTouchesGateConfig reports whether the COMMITTED diff base..rev modifies any
// gate-definition file (and which one), so a trusted auto-merge of such a change can be
// refused in favor of an explicit human approval. It reads committed objects, not the
// working tree, so the check cannot be evaded by reverting the bytes in the worktree.
func diffTouchesGateConfig(repo, base, rev string) (bool, string, error) {
	names, err := worktree.ChangedFiles(repo, base, rev)
	if err != nil {
		return false, "", err
	}
	for _, n := range names {
		for _, g := range gateConfigFiles {
			if n == g {
				return true, n, nil
			}
		}
	}
	return false, "", nil
}

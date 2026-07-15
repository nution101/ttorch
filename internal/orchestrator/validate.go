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

// gateDefinition is the resolved trust-gate validation DEFINITION for a repo: the
// .ttorch/validate.sh text as it exists on the DEFAULT BRANCH (hasScript true), or, when the
// default branch defines none, the ecosystem-detection fallback (hasScript false). Only the
// deterministic default-branch script is a stable, content-hashable gate authority; the
// detection fallback's checks depend on the worker's own checkout, so a result under it is
// NEVER cached (see validateCommitted). Resolving the definition once — from committed
// objects, never the worktree — lets validateCommitted key its cache on the script text
// without re-reading it, and lets runGate reuse the already-read script.
type gateDefinition struct {
	script    string // the default-branch .ttorch/validate.sh text (meaningful iff hasScript)
	hasScript bool   // whether the default branch defines the gate script (⇒ cacheable)
}

// resolveGateDefinition reads the trust gate's validation DEFINITION from the repo's DEFAULT
// BRANCH: the .ttorch/validate.sh as it exists there (via worktree.ShowFile — committed
// objects, never the worker-controlled worktree copy, so a worker cannot weaken its own gate
// by editing the script on its branch). hasScript is false when the default branch defines
// none; runGate then falls back to the built-in ecosystem steps and validateCommitted does
// NOT cache the result.
func resolveGateDefinition(repo string) gateDefinition {
	if script, ok := worktree.ShowFile(repo, worktree.DefaultBranch(repo), ".ttorch/validate.sh"); ok {
		return gateDefinition{script: script, hasScript: true}
	}
	return gateDefinition{}
}

// runGate runs the resolved gate definition against worktreeDir and returns whether it is
// green, the results for reporting, and any error. With a default-branch script it runs that
// exact script from a temp copy (so the worker's own worktree copy is irrelevant); otherwise
// it runs the built-in ecosystem steps (fixed ttorch commands the worker cannot redefine). No
// detected checks => NOT green (a hard block): an empty Failures() must never be read as a
// pass.
func runGate(worktreeDir string, def gateDefinition) (bool, []validate.Result, error) {
	var steps []validate.Step
	if def.hasScript {
		tmp, err := os.CreateTemp("", "ttorch-gate-validate-*.sh")
		if err != nil {
			return false, nil, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(def.script); err != nil {
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

// runGateOnCommitted validates the trust gate against an IMMUTABLE detached checkout of the
// committed sha — never the live worktree, which a running worker could mutate mid-run (race
// good bytes in, validate, revert) — using the definition resolved from the default branch.
// This makes the thing validated exactly the thing that fast-forwards. It is the single "run
// the real suite" step, and a package-level seam (like landRebase/landIntegrate) so a test can
// COUNT how many times the suite actually runs and prove the content-addressed cache collapses
// re-validations of an identical tree+gate into one run.
var runGateOnCommitted = func(repo, sha string, def gateDefinition) (bool, []validate.Result, error) {
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
	return runGate(co, def)
}

// validateCommitted resolves the trust gate's green/results decision for the committed sha,
// backed by a content-addressed cache so an identical tree validated under an identical gate
// re-runs the (often minutes-long) suite exactly ONCE — collapsing the re-validations a
// worker's iteration, trust-prep, and land otherwise trigger (a rebase mints a new commit sha
// but the SAME tree, so the old commit-keyed reuse missed on every rebase). It is the single
// choke point every caller (landPrep, validateForMerge, gate.go's auto-mint green check and
// scoring) goes through, so caching here benefits all of them uniformly.
//
// Flow: resolve the gate definition from the DEFAULT BRANCH. When the default branch defines
// NO .ttorch/validate.sh, the gate falls back to ecosystem detection on the checkout — whose
// checks depend on the worker's own tree and are not a stable, content-hashable authority — so
// it is NEVER cached (run fresh every time, exactly as before). Otherwise key the cache on
// (tree hash, gate script): on a HIT return the cached GREEN results; on a MISS run the real
// suite and, only when green, Store it (a red tree is being iterated on, and caching a red
// risks pinning a flaky failure).
//
// SAFETY INVARIANT (proven in validatecache_test.go, stated at validateCacheKey): a hit is
// served ONLY for a BYTE-IDENTICAL tree AND identical gate definition — the git tree hash is a
// cryptographic content identity — so it can never serve a result for a different tree or
// different checks. Caching is strictly UNDER this function: it changes only WHETHER the suite
// re-runs, never the (green, results, err) decision. A hit reproduces the identical decision
// (stagedGreen faithfully mirrors gateGreen for a persisted GREEN set, exactly as
// reusablePrepValidate does), and the "no checks detected => NOT green (hard block)" rule is
// preserved: a no-checks run is never green, so is never cached, so a hit is always green.
func validateCommitted(repo, sha string) (bool, []validate.Result, error) {
	def := resolveGateDefinition(repo)
	// No default-branch script ⇒ the ecosystem-detection fallback is not a cacheable gate
	// authority: run fresh, with no cache read or write.
	if !def.hasScript {
		return runGateOnCommitted(repo, sha, def)
	}
	tree, err := worktree.TreeHash(repo, sha)
	if err != nil {
		// Cannot compute the content key ⇒ fail closed to a fresh, uncached run rather than
		// risk keying on a bad value; validation still proceeds correctly.
		return runGateOnCommitted(repo, sha, def)
	}
	key := validateCacheKey(tree, def.script)
	if results, ok := loadValidateCache(key); ok {
		return stagedGreen(results), results, nil
	}
	green, results, err := runGateOnCommitted(repo, sha, def)
	if err != nil {
		return green, results, err
	}
	if green {
		storeValidateCache(key, results)
	}
	return green, results, nil
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
// script (.ttorch/validate.sh). The trusted AUTO path requires it: without it, the gate
// falls back to ecosystem detection (go.mod / package.json) on the worker's own checkout,
// whose presence and scripts the worker controls — so "green" would be worker-influenced. A
// human `ttorch approve` is still allowed to use the detection fallback (a human is then in
// the loop). It mirrors resolveGateDefinition's hasScript, which validateCommitted uses to
// decide whether the result is cacheable.
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

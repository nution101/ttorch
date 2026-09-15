package orchestrator

import (
	"fmt"
	"strings"

	"github.com/nution101/ttorch/internal/worktree"
)

// The trust gate's DEFINITION is the set of files whose contents decide whether a change is
// allowed to merge. A trusted AUTO-merge must never change them: altering the gate requires
// an explicit human approval, because a passing verdict cannot be evidence about the very
// machinery that produced it. The single class of change that most needs a human reading the
// diff is a change to the gate.
//
// THE BREADTH LINE. A file is gate definition if and only if THE GATE ITSELF reads, executes
// or dispatches it, or it is the code that decides. That rule is narrow enough that ordinary
// work on the tool still auto-merges (a guard that refuses everything gets switched off) and
// wide enough to cover every input to the pass/fail decision:
//
//   - .ttorch/validate.sh — the script the gate EXECUTES as its green authority.
//   - AGENTS.md — the delivery-mode block the gate READS to decide the trusted path applies.
//   - content/agents/ttorch-reviewer-* — the adversarial reviewers the gate DISPATCHES. Their
//     prompts are the review; the gate has no other notion of what gets checked.
//   - content/skills/ttorch-review/ — the review protocol the gate RUNS.
//   - the Go source that decides: verdict aggregation and reviewer scaling (internal/review),
//     the approval token (internal/approval), what "green" means (internal/validate), the
//     durable verdict row (internal/db/verdict.go), the delivery-mode reader
//     (internal/projectinit/projectinit.go), and the gate's own orchestrator files — the
//     auto-mint path, the merge-time re-checks, the freshness and pin checks, the
//     content-addressed validate cache (a cache hit IS a green), and this guard.
//
// DELIBERATELY OUTSIDE the line, with the reasoning, because each was considered:
//
//   - The rest of content/agents/ and content/skills/. The other agents and skills are a
//     library the gate never consults; content/agents/ttorch-worker.md is the worker's
//     contract, not the reviewer's. content/assets/AGENTS.global.md states the no-merge-
//     without-approval policy in prose, but the gate does not read it — the Go gate enforces
//     the same rule regardless, so rewriting the prose cannot authorize a merge.
//   - internal/worktree. The gate leans on it (ShowFile, ChangedFilesNoRenames, TreeHash,
//     IsAncestor), and a primitive that stopped reporting changed files would blind this
//     guard. It is still excluded: it is a mechanical wrapper over git commands carrying no
//     gate policy, and it is used by spawn, land, teardown and status, so guarding it would
//     make a large share of ordinary work ineligible. The mitigation is that gateguard_test.go
//     drives this guard end to end over a real git repo, so a change that broke the primitive
//     turns the gate's own validate RED, and a trusted auto-merge requires a fresh green.
//   - internal/scheduler. It chooses WHICH tasks to gate and land, but every authorization it
//     relies on is re-checked here and in MergeLocal (token provenance, a fresh passing
//     verdict, a fresh validate), so it cannot manufacture a pass the gate would refuse.
//   - internal/orchestrator/landqueue.go. Concurrency and fast-forward serialization only; the
//     land's gate decisions live in merge.go.
//
// HARDCODED, NOT CONFIGURABLE. The guard exists to constrain the project, so the project must
// not be able to edit it: a repo that could widen the list could also narrow it, and a
// per-project setting would be exactly the file a self-approving change would edit first.
// Deriving the list structurally (say, from every file the gate imports) was ruled out too —
// it would pull in the standard library and half of internal/db through transitive imports,
// which is the "refuses everything" failure. The list is therefore explicit, and
// TestGateDefinitionPathsAnchored keeps it honest: it fails if any entry stops naming real
// content, so a rename that orphans an entry turns validate red instead of silently opening a
// hole.
var (
	// gateDefinitionFiles are matched exactly, for gate files that live among non-gate
	// siblings in the same directory.
	gateDefinitionFiles = []string{
		".ttorch/validate.sh",
		"AGENTS.md",
		"internal/db/verdict.go",
		"internal/orchestrator/gate.go",
		"internal/orchestrator/gateguard.go",
		"internal/orchestrator/gateguard_test.go",
		"internal/orchestrator/merge.go",
		"internal/orchestrator/validate.go",
		"internal/orchestrator/validatecache.go",
		"internal/projectinit/projectinit.go",
	}

	// gateDefinitionPrefixes are matched as path prefixes, so a file ADDED to the gate later
	// (a fifth reviewer, another file in the review skill or the review package) is covered
	// the moment it exists. Exact filenames would not scale here, and a gap in the guard is
	// worse than an occasional needless refusal. Each prefix is either a directory that
	// belongs wholly to the gate or, for content/agents/, the reviewer filename stem — the
	// directory itself also holds the non-gate agent library.
	gateDefinitionPrefixes = []string{
		"content/agents/ttorch-reviewer-",
		"content/skills/ttorch-review/",
		"internal/approval/",
		"internal/review/",
		"internal/validate/",
	}
)

// isGateDefinition reports whether an unquoted repo-relative path names a gate-definition
// file.
func isGateDefinition(name string) bool {
	for _, f := range gateDefinitionFiles {
		if name == f {
			return true
		}
	}
	for _, p := range gateDefinitionPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// couldBeGateDefinition reports whether prefix — a VERBATIM leading run of a path git could
// not report in full — is still consistent with some gate-definition path. It is the
// fail-closed half of the guard: an undecidable name is only permitted when its readable part
// already PROVES it is not the gate.
func couldBeGateDefinition(prefix string) bool {
	for _, f := range gateDefinitionFiles {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	for _, p := range gateDefinitionPrefixes {
		// Either the prefix already reaches into a guarded directory, or it is still short
		// enough that the unread remainder could take it there.
		if strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// verbatimPrefix reports whether name arrived C-QUOTED — `git diff --name-only` wraps a path
// containing non-ASCII or control bytes in double quotes and backslash-escapes those bytes —
// and, when it did, returns the leading run git emitted byte for byte. Everything from the
// first backslash on is an escape sequence rather than the path, so that leading run is all
// the guard can compare. A quoted name with no readable run at all yields "", which
// couldBeGateDefinition treats as undecidable.
func verbatimPrefix(name string) (prefix string, quoted bool) {
	body, ok := strings.CutPrefix(name, `"`)
	if !ok {
		return "", false
	}
	if i := strings.IndexByte(body, '\\'); i >= 0 {
		return body[:i], true
	}
	return strings.TrimSuffix(body, `"`), true
}

// diffTouchesGateConfig reports whether the COMMITTED diff base..rev modifies any
// gate-definition file (and which one), so a trusted auto-merge of such a change can be
// refused in favor of an explicit human approval. It reads committed objects, not the working
// tree, so the check cannot be evaded by reverting the bytes in the worktree, and it lists
// changed files with rename detection OFF so moving a gate file away reports the path it was
// moved FROM.
//
// It FAILS CLOSED. Both outcomes it can return other than a clean "no" refuse the auto-merge:
// touched==true refuses by name, and a non-nil error refuses too — MergeLocal propagates it,
// and TrustRecord's auto-mint requires a nil error before it mints. So a diff the guard cannot
// evaluate is never permitted. There is no path on which an unreadable or unlistable diff
// returns (false, "", nil).
func diffTouchesGateConfig(repo, base, rev string) (bool, string, error) {
	names, err := worktree.ChangedFilesNoRenames(repo, base, rev)
	if err != nil {
		return false, "", fmt.Errorf("could not list the files changed between %s and %s, so whether this change alters the gate definition cannot be decided; refusing a trusted auto-merge: %w", base, rev, err)
	}
	for _, n := range names {
		if prefix, quoted := verbatimPrefix(n); quoted {
			if couldBeGateDefinition(prefix) {
				return false, "", fmt.Errorf("cannot evaluate the changed path %s against the gate definition (git could not report it verbatim) and its readable prefix %q does not rule out a gate-definition file; refusing a trusted auto-merge", n, prefix)
			}
			continue
		}
		if isGateDefinition(n) {
			return true, n, nil
		}
	}
	return false, "", nil
}

// gateDecisionDeclarations names the declarations that actually make the gate's decisions, by
// the source text that declares each one. A path list alone is fragile for Go source: a
// declaration can be MOVED to a new file and slip out from under the guard, and the gate's
// reviewer instructions are not confined to content/ — reviewerBrief in gate.go is reviewer
// prompt text embedded directly in Go, including the rule that high/critical findings block
// the merge. TestGateDecisionSourceStaysGuarded asserts each of these still lives in a file
// the list above covers, so relocating one turns the gate's own validate RED rather than
// quietly opening a hole. It is the structural anchor over the hardcoded list: the list says
// WHERE the gate is, this says WHAT the gate is and checks the two still agree.
var gateDecisionDeclarations = []string{
	// What a reviewer is told, and which reviewers run at all.
	"func reviewerBrief(",
	"func (m *Manager) dispatchReviewer(",
	"func (m *Manager) ReviewersFor(",
	// The auto-mint path and this guard.
	"func (m *Manager) TrustRecord(",
	"func diffTouchesGateConfig(",
	"func isGateDefinition(",
	"func couldBeGateDefinition(",
	// What "green" means, and the cache that can stand in for a run.
	"func gateGreen(",
	"func stagedGreen(",
	"func resolveGateDefinition(",
	"func hasDefaultBranchGateScript(",
	"func validateCommitted(",
	"var runGateOnCommitted = func(",
	"func loadValidateCache(",
	"func validateCacheKey(",
	// The merge-time re-checks: freshness, the commit pin, and verdict portability.
	"func (m *Manager) MergeLocal(",
	"func (m *Manager) validateForMerge(",
	"func (m *Manager) reusablePrepValidate(",
	"func (m *Manager) carryVerdictForward(",
	"func (m *Manager) gateCoversRebased(",
	"func (m *Manager) remintFromVerdict(",
}

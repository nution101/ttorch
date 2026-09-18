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
//   - .ttorch/validate.sh - the script the gate EXECUTES as its green authority.
//   - Makefile - what that script actually runs. validate.sh is read from the DEFAULT branch
//     so a worker cannot weaken it, but it runs `make lint` and `make test-fast` inside a
//     checkout of the WORKER'S sha, so the worker's Makefile supplies the commands. A branch
//     defining `test-fast: @true` has a green gate.
//   - AGENTS.md - the delivery-mode block the gate READS to decide the trusted path applies.
//   - content/agents/ttorch-reviewer-* - the adversarial reviewers the gate DISPATCHES. Their
//     prompts are the review; the gate has no other notion of what gets checked.
//   - content/skills/ttorch-review/ - the review protocol the gate RUNS.
//   - the Go PACKAGES that decide (see below): internal/orchestrator (the auto-mint, the
//     merge-time re-checks, the freshness and pin checks, the validate cache where a hit IS a
//     green, and this guard), internal/review (verdict aggregation, reviewer scaling),
//     internal/approval (the token), internal/validate (what "green" means), internal/db (the
//     verdict row, read AND written), internal/projectinit (the delivery-mode reader) and
//     internal/worktree (the git primitives the guard feeds itself from).
//
// FOR GO SOURCE THE UNIT IS THE PACKAGE, NEVER THE FILE. This is the correction that matters,
// and it was found the hard way: an earlier version of this list named six files inside the
// eleven-source-file package orchestrator. Go package-level vars are writable from ANY file in
// the same package, and the gate's decisions hang off five of them - runGateOnCommitted
// (validate.go), requiredReviewers and reviewerDispatcher (gate.go), nowFunc and landRebase /
// landIntegrate (merge.go). A worker adding internal/orchestrator/anything.go with an init()
// that rebinds runGateOnCommitted to return green, or editing spawn.go, which the old guard
// explicitly permitted, changes what the gate decides without touching one guarded file. The
// guard said not touched, the validate passed, the approval auto-minted, and every gate after
// that was green. A partly guarded Go package is not a boundary at all, so the guard's
// granularity has to equal the language's encapsulation boundary.
// TestGateSeamsAreRebindableFromAnyFileInPackage performs that rebind and
// TestGuardedGoPackagesAreWholePackages enforces the rule, so this cannot regress to file
// granularity. Non-Go entries stay file-granular: a markdown file cannot rewrite another
// markdown file's state.
//
// DELIBERATELY OUTSIDE the line, with the reasoning, because each was considered:
//
//   - The rest of content/agents/ and content/skills/. The other agents and skills are a
//     library the gate never consults; content/agents/ttorch-worker.md is the worker's
//     contract, not the reviewer's. content/assets/AGENTS.global.md states the no-merge-
//     without-approval policy in prose, but the gate does not read it - the Go gate enforces
//     the same rule regardless, so rewriting the prose cannot authorize a merge.
//   - internal/scheduler. It chooses WHICH tasks to gate and land, but every authorization it
//     relies on is re-checked in TrustRecord and MergeLocal (token provenance, a fresh passing
//     verdict, a fresh validate), so it cannot manufacture a pass the gate would refuse. It is
//     a separate package, so it cannot rebind the gate's seams either.
//   - .github/workflows/. CI is the default branch's required check, not ttorch's gate; a
//     trusted auto-merge is a local fast-forward and does not consult it.
//   - Every other package: internal/cli, internal/tmux, internal/harness, internal/installer,
//     internal/manifest, internal/watch, internal/termtab, internal/selfupdate, internal/state,
//     internal/livestate, internal/skills, internal/learnings, internal/ciparity,
//     internal/profile, internal/singleton, internal/doctor, internal/paths, internal/buildinfo,
//     cmd/ttorch. None declares state the gate reads, and none can reach into a guarded
//     package's vars.
//
// RULED OUT: moving the gate into its own package so the trust boundary IS a package boundary.
// That is the more correct fix and it stays the right long-term shape, but it means relocating
// TrustRecord, MergeLocal, the land pipeline and the validate cache (about 2,300 lines of
// gate.go plus merge.go) out of package orchestrator, which requires exporting or re-interfacing
// most of Manager's internals. Rewriting the gate's own plumbing while trying to secure it is
// the wrong order: the refactor would need exactly the careful human review the guard exists to
// force, and a mistake in it is a fail-open. Widening the list is cheap, reversible and
// verifiable today. The narrower alternative - deleting the five mutable seams so file
// granularity becomes sound again - is recorded as the follow-up that would let this guard
// shrink back, but those seams exist so the tests can count validate runs and fake rebase
// conflicts, so removing them is itself a change to the gate's testability.
//
// HARDCODED, NOT CONFIGURABLE. The guard exists to constrain the project, so the project must
// not be able to edit it: a repo that could widen the list could also narrow it, and a
// per-project setting would be exactly the file a self-approving change would edit first.
// Deriving the list structurally (say, from every file the gate imports) was ruled out too -
// it would pull in the standard library through transitive imports, which is the
// "refuses everything" failure. The list is therefore explicit, and three tests keep it honest:
// TestGateDefinitionPathsAnchored (every entry still names real content),
// TestGuardedGoPackagesAreWholePackages (no partly guarded Go package) and
// TestGateDecisionSourceStaysGuarded (every gate-deciding declaration still lives inside it).
var (
	// gateDefinitionFiles are matched exactly. Non-Go only: a Go file belongs to a package,
	// and packages are guarded whole.
	gateDefinitionFiles = []string{
		".ttorch/validate.sh",
		"AGENTS.md",
		"Makefile",
	}

	// gateDefinitionPrefixes are matched as path prefixes. For Go this is the package
	// directory, which is the trust boundary. For content/agents/ it is the reviewer filename
	// stem, because that directory also holds the non-gate agent library; a file ADDED to the
	// gate later (a fifth reviewer, another file in the review skill or the review package) is
	// then covered the moment it exists.
	gateDefinitionPrefixes = []string{
		"content/agents/ttorch-reviewer-",
		"content/skills/ttorch-review/",
		"internal/approval/",
		"internal/db/",
		"internal/orchestrator/",
		"internal/projectinit/",
		"internal/review/",
		"internal/validate/",
		"internal/worktree/",
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
// DECLARED IDENTITY: a bare name for a package-level func or var, and "(recv).Name" for a
// method. TestGateDecisionSourceStaysGuarded resolves each one against the parsed AST of every
// .go file in the repo and asserts it is still declared, and still declared only in files the
// list above covers.
//
// A path list alone is fragile for Go source: a declaration can be MOVED to a new file and
// slip out from under the guard, and the gate's reviewer instructions are not confined to
// content/ — reviewerBrief in gate.go is reviewer prompt text embedded directly in Go,
// including the rule that high/critical findings block the merge. This is the structural
// anchor over the hardcoded list: the list says WHERE the gate is, this says WHAT the gate is
// and checks the two still agree, so relocating or renaming one turns the gate's own validate
// RED rather than quietly opening a hole.
//
// The identities are matched against PARSED DECLARATIONS, never against source text. A text
// scan cannot work here: this file quotes every identity it looks for, so its own bytes would
// satisfy every search, the "is it still declared anywhere" half would be unreachable, and a
// rename-and-relocate would evade the check entirely (the moved code no longer carries the old
// name, and the only remaining match is this list quoting itself). Parsing ignores string
// literals, so this file matches only the declarations it genuinely makes.
var gateDecisionDeclarations = []string{
	// What a reviewer is told, and which reviewers run at all.
	"reviewerBrief",
	"(*Manager).spawnReviewer",
	"reviewerDispatcher",
	"requiredReviewers",
	"(*Manager).ReviewersFor",
	"(*Manager).clearStaleReviewerReports",
	// The auto-mint path, the daemon gate pass, and this guard.
	"(*Manager).TrustRecord",
	"(*Manager).GateOnce",
	"(*Manager).gateOnceAt",
	"diffTouchesGateConfig",
	"isGateDefinition",
	"couldBeGateDefinition",
	"verbatimPrefix",
	// What "green" means, and the cache that can stand in for a run.
	"gateGreen",
	"stagedGreen",
	"resolveGateDefinition",
	"hasDefaultBranchGateScript",
	"validateCommitted",
	"runGateOnCommitted",
	"loadValidateCache",
	"validateCacheKey",
	// The merge-time re-checks: freshness, the commit pin, and verdict portability.
	"(*Manager).MergeLocal",
	"(*Manager).validateForMerge",
	"(*Manager).reusablePrepValidate",
	"(*Manager).carryVerdictForward",
	"(*Manager).gateCoversRebased",
	"(*Manager).remintFromVerdict",
	"nowFunc",
	"landRebase",
	"landIntegrate",
	// The verdict row, written as well as read.
	"(*Store).RecordDelivery",
	"(*Store).GetVerdict",
	// The delivery-mode reader, and the git primitives the guard feeds itself from.
	"ReadMode",
	"ChangedFilesNoRenames",
	"DefaultBranch",
	"ShowFile",
	"TreeHash",
}

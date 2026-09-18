package orchestrator

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

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
// THE GO SET IS DERIVED, NOT CURATED. Per-package triage of "could this one manufacture a
// pass" is the reasoning that failed twice: round 1 explicitly permitted spawn.go, and round 2
// cleared internal/harness BY NAME as settled while harness composes the reviewer's command
// line, model and effort and writes its settings file. So the rule is mechanical instead: every
// first-party package in the transitive import closure of gateClosureRoots is guarded, and
// TestGuardCoversEveryFirstPartyGateDependency re-derives that closure with `go list -deps` and
// fails if a member is uncovered. The closure covers the gate itself and the path that delivers
// the reviewer definitions it dispatches, because guarding a reviewer's prompt buys nothing if
// what installs it is open.
//
// Concrete attacks that motivated the widening, beyond the seam rebinding above: internal/paths
// locates the approval token, the review inputs and the validate cache, so redirecting it makes
// the gate read an attacker's file; internal/harness decides what binary runs as the reviewer
// and with what settings; internal/tmux and internal/termtab are how that command is actually
// run, so either can substitute a command that writes a clean findings report; the ROOT package
// declares `var Content embed.FS`, the payload carrying the reviewer definitions, and
// internal/installer with internal/manifest write it into ~/.claude/agents; go.mod and go.sum
// choose what every guarded package compiles against. For internal/ciparity, internal/doctor,
// internal/livestate, internal/profile, internal/singleton and internal/state I could not
// construct a concrete attack; they are guarded because they are in the closure and because
// deciding case by case is what went wrong twice.
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
//     outside the import closure, so it cannot rebind the gate's seams either.
//   - .github/workflows/. CI is the default branch's required check, not ttorch's gate; a
//     trusted auto-merge is a local fast-forward and does not consult it.
//   - The packages outside the closure: internal/cli, internal/scheduler, internal/watch,
//     internal/selfupdate, internal/skills, internal/learnings, internal/buildinfo, cmd/ttorch.
//     The gate does not import them, so they cannot reach a guarded package's state.
//
// WHAT THIS GUARD CANNOT DO, stated here because the list above would otherwise read as
// complete. The gate does not read a test RESULT; it EXECUTES worker-controlled code
// (`make lint`, `make test-fast`) unsandboxed, as the lead's own user, in a checkout of the
// worker's sha. Code in any package - guarded or not, test file or not - can therefore write
// the approval token, the verdict row in state.db, ~/.claude/agents/ttorch-reviewer-*.md or
// ~/.ttorch/bin/ttorch while the gate is running, which defeats every path in this list at
// once. That is not a path-guarding problem and no entry here fixes it; whether to sandbox the
// validate run is a separate decision. What the guard does is make a change to the gate's own
// definition require a human to read it.
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
		// The dependency versions every guarded package compiles against.
		"go.mod",
		"go.sum",
		// The ROOT package, which declares `var Content embed.FS` - the embedded payload
		// carrying the reviewer definitions the gate dispatches, and a mutable package var
		// like any other, so any file in that package can rebind it and replace every
		// reviewer's instructions. Named file by file rather than by a "any .go at the root"
		// rule: this guard runs against EVERY managed repo, and such a rule would refuse
		// auto-merge for an ordinary root main.go in any project that keeps one. A root .go
		// file ADDED here is caught instead by TestRootPackageIsFullyGuarded, which turns this
		// repo's own validate red, and a trusted auto-merge needs a fresh green. Content only
		// matters for this repo, so that is where the protection belongs.
		"content.go",
		"content_test.go",
	}

	// gateDefinitionPreemptiveFiles are guarded precisely because they must NEVER appear.
	// GNU Make reads GNUmakefile in preference to Makefile, and lowercase makefile ahead of it
	// too, so guarding only Makefile left the hole intact: a worker commits a GNUmakefile whose
	// lint and test-fast targets are @true and the gate is green having run nothing. They are
	// kept apart from the list above because TestGateDefinitionPathsAnchored requires those to
	// EXIST and requires these not to.
	gateDefinitionPreemptiveFiles = []string{
		"GNUmakefile",
		"makefile",
	}

	// gateDefinitionPrefixes are matched as path prefixes. For Go this is the package
	// directory, which is the trust boundary. For content/agents/ it is the reviewer filename
	// stem, because that directory also holds the non-gate agent library; a file ADDED to the
	// gate later (a fifth reviewer, another file in the review skill or the review package) is
	// then covered the moment it exists.
	gateDefinitionPrefixes = []string{
		"content/agents/ttorch-reviewer-",
		"content/skills/ttorch-review/",
		// Derived, not curated: every first-party package in the transitive import closure of
		// gateClosureRoots. TestGuardCoversEveryFirstPartyGateDependency re-derives it.
		"internal/approval/",
		"internal/ciparity/",
		"internal/db/",
		"internal/doctor/",
		"internal/harness/",
		"internal/installer/",
		"internal/livestate/",
		"internal/manifest/",
		"internal/orchestrator/",
		"internal/paths/",
		"internal/profile/",
		"internal/projectinit/",
		"internal/review/",
		"internal/singleton/",
		"internal/state/",
		"internal/termtab/",
		"internal/tmux/",
		"internal/validate/",
		"internal/worktree/",
	}
)

// isGateDefinition reports whether an unquoted repo-relative path names a gate-definition
// file. Comparison FOLDS CASE, because an exact match did not hold: a tree entry spelled
// Internal/orchestrator/rebind.go defeated the case-sensitive version outright, and
// `git update-index --cacheinfo` puts such an entry in a tree with no filesystem trick needed.
// On a case-folding filesystem (macOS, where both the gate's detached checkout and the local
// fast-forward run) that file lands in internal/orchestrator/ and compiles into the package,
// so its init() can rebind the gate's seams. Folding is Unicode simple folding, not ASCII
// lowering, so U+212A KELVIN SIGN and U+017F LATIN SMALL LETTER LONG S fold too.
//
// No guarded path differs from another only by case, and none contains a non-ASCII rune
// (TestGateDefinitionPathsAnchored enforces the ASCII half), so folding cannot collide two
// guarded entries. It can only widen what matches, which is the safe direction.
func isGateDefinition(name string) bool {
	for _, f := range gateDefinitionFiles {
		if strings.EqualFold(name, f) {
			return true
		}
	}
	for _, f := range gateDefinitionPreemptiveFiles {
		if strings.EqualFold(name, f) {
			return true
		}
	}
	for _, p := range gateDefinitionPrefixes {
		if foldHasPrefix(name, p) {
			return true
		}
	}
	return false
}

// foldHasPrefix reports whether s begins with prefix under Unicode simple case folding. A
// byte-length prefix compare on lowered strings is not enough: some runes fold across byte
// lengths (U+212A folds to the 1-byte "k"), so the comparison walks runes.
func foldHasPrefix(s, prefix string) bool {
	for _, want := range prefix {
		got, size := utf8.DecodeRuneInString(s)
		if size == 0 || !foldEqualRune(got, want) {
			return false
		}
		s = s[size:]
	}
	return true
}

// foldEqualRune reports whether a and b are the same rune under Unicode simple folding, by
// walking the fold orbit unicode.SimpleFold exposes.
func foldEqualRune(a, b rune) bool {
	if a == b {
		return true
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
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
	// No slash yet, so the unread remainder could still make this a Go file in the root
	// package - or put a slash in and land anywhere. Undecidable.
	if !strings.Contains(prefix, "/") {
		return true
	}
	for _, f := range append(append([]string{}, gateDefinitionFiles...), gateDefinitionPreemptiveFiles...) {
		if foldHasPrefix(f, prefix) {
			return true
		}
	}
	for _, p := range gateDefinitionPrefixes {
		// Either the prefix already reaches into a guarded directory, or it is still short
		// enough that the unread remainder could take it there.
		if foldHasPrefix(prefix, p) || foldHasPrefix(p, prefix) {
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

// gateClosureRoots are the packages whose transitive first-party imports must all be guarded:
// the gate itself, and the path that delivers the reviewer definitions it dispatches (the root
// package embeds them; installer and manifest write them into ~/.claude).
// TestGuardCoversEveryFirstPartyGateDependency re-derives that closure with `go list -deps`.
var gateClosureRoots = []string{
	".",
	"./internal/approval",
	"./internal/db",
	"./internal/installer",
	"./internal/manifest",
	"./internal/orchestrator",
	"./internal/projectinit",
	"./internal/review",
	"./internal/validate",
	"./internal/worktree",
}

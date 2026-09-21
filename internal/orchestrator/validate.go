package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
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

// gateConfigFiles define the trust gate itself by exact path. Two kinds of file are here.
//
// The repo-local gate config — ".ttorch/validate.sh" (what green means) and "AGENTS.md"
// (whether the gate runs at all) — exists in every ttorch-managed repo and takes effect on
// the very next gate run.
//
// The rest is ttorch's own DECIDING CODE: the four files in this package that resolve,
// enforce and cache the gate's decision. They exist only in this repo, and a change to them
// takes effect one step later than a config change — it alters the NEXT binary, after a
// build and an install, not the running one. That extra step is a real difference but a thin
// one, because the maintainer self-updates routinely; a landed weakening of gate.go reaches
// every repo on the machine the first time they do.
//
// The orchestrator entries are listed by exact FILE and not as "internal/orchestrator/",
// because the package is 39% of this repo's commits and covering all of it would put more
// than half of every change behind --allow-gate-change (measured: see docs/ARCHITECTURE.md).
// audit.go is here because MergeLocal refuses to merge when writeAudit fails, so a one-line
// change there strips the record from every trusted merge; it costs 0 additional commits,
// since nothing has ever touched it alone.
//
// A file-granular list over a package that gets refactored decays silently, so two tests hold
// it: TestGateConfigCoversTheDecidingCode catches a MOVE of a listed deciding function into an
// unnamed file, and TestOrchestratorFilesAreClassified catches a NEW file that nobody has
// classified either way. Neither can recognise a new deciding function inside an already-
// classified non-deciding file; that remains a review responsibility and is stated as such.
//
// Makefile and content.go sit outside both packages and are here on the same delayed-diff
// argument. .ttorch/validate.sh — the gate's validation authority, already covered — does
// nothing but run `make lint` and `make test-fast`, so redefining those targets redefines
// what green means without touching a covered script. content.go is the //go:embed that
// decides which repo file becomes which installed reviewer definition; it costs 0 additional
// commits, as does Makefile.
var gateConfigFiles = []string{
	".ttorch/validate.sh",
	"AGENTS.md",
	"internal/orchestrator/gate.go",
	"internal/orchestrator/merge.go",
	"internal/orchestrator/validate.go",
	"internal/orchestrator/validatecache.go",
	"internal/orchestrator/audit.go",
	"Makefile",
	"content.go",
}

// gateConfigPrefixes define the gate by path prefix, where the covered unit is a directory
// (or a filename family) rather than a named file.
//
// content/skills/ and content/agents/ttorch-reviewer- are the gate's own INSTRUCTIONS — not
// documentation about the gate, but the text the gate executes on. content.go embeds the
// whole content/ tree and installer.desiredFiles lays it down under ~/.claude: content/skills/
// becomes ~/.claude/skills (the ttorch-review procedure the manager follows, the ttorch-manager
// instructions, the ttorch-validate procedure) and content/agents/ttorch-reviewer-*.md becomes
// ~/.claude/agents (the adversarial reviewers' own definitions, including the security
// reviewer). A landed change to any of them alters what the gate does on the NEXT run, for
// every repo on the machine, with no further review — the same delayed diff-channel effect
// AGENTS.md has, which is why they belong alongside it. "content/agents/ttorch-reviewer-" is a
// FILENAME prefix and deliberately does not cover the other agent definitions in that
// directory, which the gate does not dispatch.
//
// internal/review/ is the verdict itself: the findings contract, the severity-to-block rule,
// and the diff-size classifier that decides WHICH reviewers run at all. internal/approval/ is
// the token the whole --allow-gate-change scope rides on. internal/validate/ decides what a
// passing check is (gateGreen is a thin wrapper over validate.Failures). internal/projectinit/
// parses AGENTS.md into the delivery mode and the auto-mint staleness bound, so it decides
// whether the gate runs at all and for how long an auto-approval stays good — covering the
// config file but not the code that reads it would be a gap of exactly the kind this guard
// exists to close. All four are whole packages because each is small and single-purpose, so
// the prefix is the honest unit and carries none of the refactor brittleness the orchestrator
// file list does.
//
// internal/installer/ maps the embedded content/ tree onto ~/.claude — it decides WHICH
// embedded file becomes ~/.claude/agents/ttorch-reviewer-security.md. Covering the reviewer
// definitions while leaving the mapping open would let an uncovered file (content/agents/
// ttorch-worker.md is deliberately excluded) be installed as a reviewer instead, through the
// same delayed diff channel. 3 commits.
//
// .github/workflows/ is here because .ttorch/validate.sh on this repo runs only the FAST lane
// and says so in its own header: the full suite, including the orchestrator e2e tests, runs in
// CI as the required check. CI is therefore half of what "validated" means for this repo, and
// a landed weakening of ci.yml weakens every later change's validation through exactly the
// delayed diff channel that put the skills on this list. The trusted gate does not itself
// consult CI, which is the argument against including it; it loses to the fact that the gate
// script defers to CI by name. It costs 5 commits in 196, so the blast-radius argument that
// keeps internal/orchestrator/ off the list does not apply.
var gateConfigPrefixes = []string{
	"content/skills/",
	"content/agents/ttorch-reviewer-",
	"internal/review/",
	"internal/approval/",
	"internal/validate/",
	"internal/projectinit/",
	"internal/installer/",
	".github/workflows/",
}

// fsIdentityKey is the key under which two repository paths are THE SAME FILE on a
// case-insensitive, normalizing filesystem. It is the only path comparison in this guard.
//
// It must model what the filesystem does, not what Go's string package makes convenient, and
// the two are not the same. Two earlier versions of this comparison were exploitable:
//
//	strings.ToLower   simple LOWERCASING. Missed U+017F (ſ), which folds with 's' but
//	                  lowercases to itself: "agentſ.md" evaded the guard and overwrote
//	                  AGENTS.md on checkout.
//	unicode.SimpleFold single-rune folding. Missed the MULTI-RUNE full folds, which are the
//	                  ones that reach an ASCII target: U+FB01 (ﬁ) folds to "fi", so "Makeﬁle"
//	                  evaded the guard and substituted the Makefile the gate itself executes.
//	                  U+FB06 (ﬆ) → "st" and U+00DF (ß) → "ss" reach internal/inﬆaller/ and
//	                  any covered path containing a double s the same way.
//
// So: NFD-normalize, apply Unicode FULL case folding, normalize again — the canonical
// caseless match, which is what APFS compares under. TestFSIdentityKeyMatchesTheFilesystem
// checks this against the real filesystem by creating both files and seeing whether they
// collide, rather than against anybody's reading of the tables.
//
// cases.Caser is documented as possibly stateful and not safe to share, so a Caser is built
// per call rather than cached in a package var.
func fsIdentityKey(p string) string {
	return norm.NFD.String(cases.Fold().String(norm.NFD.String(p)))
}

// matchesGateConfig reports whether a repository path names a gate-definition file. Matching
// is exact against gateConfigFiles and by prefix against gateConfigPrefixes, with BOTH SIDES
// reduced to foldKey first.
//
// Folding both sides matters because "AGENTS.md" is the one entry that is not already
// lowercase; folding only the incoming path would leave it unmatchable, which is the
// half-folded comparison this repo has shipped before. Callers must supply a slash-separated,
// repo-relative path; worktree.ChangedFiles produces exactly that, unquoted.
//
// Folding makes the guard deliberately OVER-match on a case-sensitive filesystem, where
// "Agents.md" is a genuinely different file that configures nothing. Over-matching costs an
// --allow-gate-change on a merge that did not need one; under-matching merges a gate change
// unflagged. The guard takes the first. It also means the guard and resolveGateDefinition can
// disagree — the resolver asks git for ".ttorch/validate.sh" by its exact name — but only in
// that same safe direction.
//
// Spelling-based matching is NECESSARY AND NOT SUFFICIENT, and the limit is structural rather
// than a gap to be closed by folding harder: the attacker picks the spelling, and the guard
// must anticipate every spelling a filesystem might collapse onto a covered name. That is a
// race the guard loses eventually. diffTouchesGateConfig therefore also refuses any changed
// path that COLLIDES with another path in the resulting tree, whatever either is called.
func matchesGateConfig(name string) bool {
	folded := fsIdentityKey(name)
	for _, g := range gateConfigFiles {
		if folded == fsIdentityKey(g) {
			return true
		}
	}
	for _, p := range gateConfigPrefixes {
		if strings.HasPrefix(folded, fsIdentityKey(p)) {
			return true
		}
	}
	return false
}

// hostilePath reports whether a repo path carries a byte that has no business in a filename
// and that downstream consumers render verbatim.
//
// A C0 control character or DEL is the marker. The audit log is newline-delimited
// (audit.go's "%s %s\n") and the merge record interpolates the matched path into it, so a
// path containing a literal newline writes a second, well-formed, entirely fabricated
// trusted-merge line. git's C-quoting used to make that unreachable; reading the file list
// with -z removed the quoting along with the quoting bug, so the refusal has to be explicit.
// The audit sink escapes control characters as well — this is the half that stops the path
// from reaching a merge at all, that one is the half that protects every other call site.
func hostilePath(name string) bool {
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

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
	raw, err := os.ReadFile(filepath.Join(dir, review.StagedValidateFile))
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
// Failures() must not read as green) — exactly as the merge gate treats it. The rule itself
// lives in review.StagedGreen, which the verdict fold applies to the validate a review was
// staged over, so the merge gate and the gate verdict cannot drift on what "green" means.
func stagedGreen(results []validate.Result) bool {
	return review.StagedGreen(results)
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

// gateConfigHit describes why a committed diff counts as changing the gate's own definition.
// Path is the changed path that tripped the guard — the value the merge's audit line records
// as gate-change= — and Reason is the sentence the refusal shows the lead.
type gateConfigHit struct {
	Path   string
	Reason string
	// Blocking marks a hit that NO approval clears, --allow-gate-change included. A gate
	// CHANGE is a legitimate act that needs naming; a path collision or a control character
	// in a filename is not a change to anything, it is a diff that cannot be checked out
	// deterministically. There is no spelling of "I meant to add two index entries that
	// resolve to one file", so offering a flag for it would only turn an attack into a
	// formality the lead can wave through.
	Blocking bool
}

// diffTouchesGateConfig reports whether the COMMITTED diff base..rev changes the gate's own
// definition, so a merge of such a change can be refused in favour of an explicit human
// approval. It reads committed objects, not the working tree, so the check cannot be evaded
// by reverting the bytes in the worktree. A nil hit means the diff is clean.
//
// Three refusals, in increasing order of how little they trust the path's spelling:
//
// The first two are BLOCKING: no approval clears them, --allow-gate-change included. They are
// not gate changes a lead might legitimately authorize, they are diffs that do not have one
// well-defined checkout.
//
//  1. A changed path carrying a C0 control character or DEL. It cannot be rendered into a
//     line-delimited audit record safely and no legitimate change introduces one.
//
//  2. A changed path that COLLIDES with a different path in rev's resulting tree — two index
//     entries the target filesystem resolves to one file. This is the control that does not
//     depend on guessing spellings. The attack adds "agent\u017f.md" (U+017F folds to 's' on
//     APFS) next to an UNCHANGED "AGENTS.md"; checkout writes the attacker's bytes into
//     AGENTS.md and the repo is in trusted mode with attacker-chosen content. Spelling-based
//     matching can be beaten by picking a spelling the guard did not anticipate, but the
//     attack ALWAYS needs two entries landing on one file, and that is observable regardless
//     of what either is called. It fires whether or not either path matches the gate set,
//     because the guard cannot know which entry wins the checkout.
//
//     The comparison is against the WHOLE tree, not just the changed list: in the attack only
//     the new entry is in the diff, so pairwise comparison within the diff sees nothing. It
//     is keyed on fsIdentityKey, which models the filesystem (full folding + NFD) rather than
//     mirroring matchesGateConfig — keying the backstop on the same relation as the matcher
//     would make it blind to exactly what the matcher is blind to, which is what an earlier
//     version of this function did while its comment claimed otherwise.
//
//  3. A changed path that matches the gate set (matchesGateConfig).
//
// A collision or a hostile path is reported with the changed path as Path, so the refusal and
// the audit both name the entry the worker actually added.
func diffTouchesGateConfig(repo, base, rev string) (*gateConfigHit, error) {
	changed, err := worktree.ChangedFiles(repo, base, rev)
	if err != nil {
		return nil, err
	}
	for _, n := range changed {
		if hostilePath(n) {
			return &gateConfigHit{
				Path:     sanitizePathForMessage(n),
				Reason:   fmt.Sprintf("adds a path containing a control character (%s); such a path cannot be recorded in the audit log unambiguously", sanitizePathForMessage(n)),
				Blocking: true,
			}, nil
		}
	}
	if hit, err := collidesInTree(repo, rev, changed); err != nil || hit != nil {
		return hit, err
	}
	for _, n := range changed {
		if matchesGateConfig(n) {
			return &gateConfigHit{Path: n, Reason: fmt.Sprintf("changes a gate-definition file (%s)", n)}, nil
		}
	}
	return nil, nil
}

// collidesInTree reports the first changed path that shares a fold key with a DIFFERENT path
// in rev's tree. Pairs are reported deterministically (the tree list is sorted before the
// partner is chosen) so the same diff always produces the same refusal and the same audit
// line, which matters for a record a human is meant to compare across runs.
//
// A pre-existing collision between two paths the diff does not touch is NOT reported: it
// would block every unrelated merge in the repo forever with no way to land the fix. The
// attack always introduces its own entry, so it always appears in the changed list.
func collidesInTree(repo, rev string, changed []string) (*gateConfigHit, error) {
	if len(changed) == 0 {
		return nil, nil
	}
	tree, err := worktree.TreeFiles(repo, rev)
	if err != nil {
		return nil, err
	}
	sort.Strings(tree)
	byKey := make(map[string][]string, len(tree))
	for _, p := range tree {
		k := fsIdentityKey(p)
		byKey[k] = append(byKey[k], p)
	}
	for _, n := range changed {
		for _, other := range byKey[fsIdentityKey(n)] {
			if other == n {
				continue
			}
			return &gateConfigHit{
				Path: n,
				Reason: fmt.Sprintf("adds %q, which resolves to the same file as %q on a case-insensitive or normalizing filesystem; a checkout writes one over the other and the guard cannot tell which wins",
					sanitizePathForMessage(n), sanitizePathForMessage(other)),
				Blocking: true,
			}, nil
		}
	}
	return nil, nil
}

// sanitizePathForMessage renders a path safely inside a one-line refusal or audit record,
// escaping the control characters hostilePath refuses. It mirrors the audit sink's escaping
// so a path never breaks the line it is written on, wherever it is rendered.
func sanitizePathForMessage(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

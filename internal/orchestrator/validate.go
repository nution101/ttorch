package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
// The repo-local gate config — ".ttorch/validate.sh" (what green means), "AGENTS.md"
// (whether the gate runs at all) and "CLAUDE.md" — exists in every ttorch-managed repo and
// takes effect on the very next gate run.
//
// ".ttorch/validate.sh" is ALSO matched by the ".ttorch/" prefix, and the redundancy is
// deliberate for the same reason AGENTS.md is named twice: it is the gate's validation
// authority, and narrowing the prefix later must not silently drop it.
//
// "content.go" is a separate exact entry because the "content/" prefix does NOT match it —
// no trailing slash means "content/" and "content.go" share no prefix relationship. It is
// cited throughout as the reason the content/ tree is covered at all, so it must not be left
// to look as though the prefix already covers it.
//
// CLAUDE.md is here because in a managed repo it is a SYMLINK to AGENTS.md, and the guard
// matches the LINK's own path, not what it resolves to. A commit that deletes the symlink and
// writes a real CLAUDE.md reports the changed path "CLAUDE.md", which is not "AGENTS.md" and
// matched nothing. It is the agent-instruction file every Claude Code session in the repo
// loads — the manager session that adjudicates gates included — which is the same rationale
// that covers all of content/; unlike content/ it needs no rebuild and no install, so it
// takes effect on the merge. projectinit.ensureSymlink also refuses to clobber a real file
// ("already exists as a real file; left it"), so `ttorch init` never undoes the swap.
//
// The collision check does NOT back this up, and TestGateGuard_SymlinkSwapNeedsAllowGateChange
// asserts that rather than leaving it to be assumed: the swap changes the blob and the mode
// but not the path, so the tree gains no entry and there is no pair to collide. This entry is
// the only thing standing there. TestTreeHasNoUncoveredSymlinks fails if any OTHER symlink
// appears outside the covered set, since every one of them is the same trick.
//
// The rest is ttorch's own DECIDING CODE: the four files in this package that resolve,
// enforce and cache the gate's decision. They exist only in this repo, and a change to them
// takes effect one step later than a config change — it alters the NEXT binary, after a
// build and an install, not the running one. That extra step is a real difference but a thin
// one, because the maintainer self-updates routinely; a landed weakening of gate.go reaches
// every repo on the machine the first time they do.
//
// The orchestrator entries are listed by exact FILE and not as "internal/orchestrator/".
// Covering the whole package is what the last row of the generated table in
// docs/ARCHITECTURE.md (gate-cost block) measures, and it is declined on that basis.
// audit.go is here because MergeLocal refuses to merge when writeAudit fails, so a one-line
// change there strips the record from every trusted merge.
//
// A file-granular list over a package that gets refactored decays silently, so two tests hold
// it: TestGateConfigCoversTheDecidingCode catches a MOVE of a listed deciding function into an
// unnamed file, and TestOrchestratorFilesAreClassified catches a NEW file that nobody has
// classified either way. Neither can recognise a new deciding function inside an already-
// classified non-deciding file; that remains a review responsibility and is stated as such.
//
// Makefile, go.work, go.work.sum and content.go sit outside both packages and are here on the
// same delayed-diff argument. .ttorch/validate.sh — the gate's validation authority, already
// covered — does nothing but run `make lint` and `make test-fast`, so anything that redefines
// what those two commands compile or run redefines what green means without touching a
// covered script:
//
//	Makefile      redefines the targets themselves
//	go.work       the toolchain AUTO-DISCOVERS it at the repo root via GOWORK, and its
//	              `replace` directives OVERRIDE go.mod, so a committed one silently redirects
//	              what `go test` compiles (verified: a go.work replace swapped a dependency's
//	              implementation while go.mod and the real source were untouched)
//	go.work.sum   inert without go.work, covered alongside it so the pair cannot drift
//	go.mod        the authoritative version of the same trick: a `replace` redirects a
//	              dependency, and a `toolchain` directive changes the compiler. Unlike
//	              go.work it actually exists here — and since fsIdentityKey now depends on
//	              golang.org/x/text, a replace on x/text redirects the guard's OWN Unicode
//	              folding.
//	go.sum        the hashes that make go.mod's choices verifiable
//
// content.go is the //go:embed that decides which repo file becomes which installed reviewer
// definition. For what each entry here costs, see the generated table in
// docs/ARCHITECTURE.md (gate-cost block).
var gateConfigFiles = []string{
	".ttorch/validate.sh",
	"AGENTS.md",
	"CLAUDE.md",
	"Makefile",
	"go.work",
	"go.work.sum",
	"go.mod",
	"go.sum",
	".mcp.json",
	// .gitattributes changes how git REPORTS every other file. A committed
	// `content.go -diff` made `git grep <rev>` print "Binary file ... matches" for a commit
	// whose bytes never changed, which un-scoped the whole ttorch-source tier. The read is
	// on raw blobs now and no attribute reaches it, so this entry is defence in depth: a
	// file that can change what git says about other files belongs in the set whether or
	// not a current mechanism depends on it.
	".gitattributes",
}

// ttorchSourceFiles are covered ONLY in the repository that is ttorch's own source. They are
// paths in THIS codebase: the Go that decides a merge, and the installers README publishes.
// In any other repository these names mean something else entirely, and gating on them would
// demand --allow-gate-change for an unrelated change. See gateScope.
var ttorchSourceFiles = []string{
	"content.go",
	"docs/install.sh",
	"docs/install.ps1",
	"internal/orchestrator/gate.go",
	"internal/orchestrator/merge.go",
	"internal/orchestrator/validate.go",
	"internal/orchestrator/validatecache.go",
	"internal/orchestrator/audit.go",
	// The gate's own proofs. Without these, the backstop can be deleted in the same merge
	// as the attack it would catch: TestTtorchRepoIsScopedIn goes red on a .gitattributes
	// that un-scopes the repo, but a diff that adds the attribute AND deletes the test
	// touched nothing covered and auto-merged.
	//
	// Why these two and not every _test.go tree-wide: these are the files that carry the
	// guard's assertions, and tree-wide coverage would put every test change behind the
	// flag for no gain, since a test elsewhere cannot weaken this guard.
	//
	// Three checks hold the boundary, each covering what the one before it cannot see.
	//
	// TestOrchestratorFilesAreClassified marks a test file a proof when it uses a symbol
	// from the guard's transitive closure, and fails both ways: a proof file that is not
	// covered, and a covered file that proves nothing. The closure skipped METHODS for
	// several revisions of this comment, so gateConfigFiles, ttorchSourceFiles,
	// gateConfigPrefixes and ttorchSourcePrefixes were reachable only through
	// gateScope.files() and .prefixes() and therefore not reachable at all. A test file
	// asserting on nothing but the four lists that define the covered set read as
	// ordinary. It now walks a type's methods, and fatals if those four are unreachable.
	//
	// TestOnTopicTestsLiveInAProofFile covers what the symbol signal structurally cannot:
	// a test that drives mergeLocal with --allow-gate-change proves the gate while
	// touching none of its identifiers. That sweep matches on the test NAME and refuses
	// one that lives in a file no proof derivation reaches.
	//
	// TestGateTestsSelectorCoversTheProofs then GENERATES the Makefile's GATE_TESTS from
	// the proof files and compares it byte for byte, so a proof that is covered against
	// deletion also runs in the lane that gates a merge rather than only one of the two.
	//
	// What none of them catch, stated rather than left implied: a test that is about the
	// gate, uses none of its identifiers, AND is named for none of it. Once the symbol
	// signal is gone the name is the only handle left, and nothing mechanical sees past
	// it. The place such a test would most likely be written is orchestrator_test.go,
	// which is deliberately left uncovered. That gap is accepted, not overlooked.
	//
	// The gate-config tests used to live in orchestrator_test.go too. Rather than cover
	// that file, they moved into these two, which exist only to hold the gate's proofs.
	"internal/orchestrator/gateattacks_test.go",
	"internal/orchestrator/gateconfig_test.go",
}

// gateConfigPrefixes define the gate by path prefix, where the covered unit is a directory
// (or a filename family) rather than a named file.
//
// content/ is the gate's own INSTRUCTIONS — not documentation about the gate, but the text
// the gate executes on. content.go embeds the whole tree and installer.desiredFiles lays it
// down under ~/.claude, so a landed change to any installed file alters what the gate does on
// the NEXT run, for every repo on the machine, with no further review. That is the same
// delayed diff-channel effect AGENTS.md has, which is why these belong alongside it.
//
// This used to be two narrower prefixes, "content/skills/" and the FILENAME prefix
// "content/agents/ttorch-reviewer-", on the argument that the gate dispatches the reviewers
// and nothing else under content/agents/. That criterion was wrong, and it is the THIRD time
// an enumerated subset of this tree has missed installed files. The right criterion is what
// installer.desiredFiles INSTALLS, because anything it writes into ~/.claude is loaded by the
// sessions that run and adjudicate the gate:
//
//	assets/AGENTS.global.md    merged into the GLOBAL ~/.claude/AGENTS.md managed block, which
//	                           every Claude Code session on the machine reads — including the
//	                           manager session that adjudicates gates
//	commands/ttorch.md         installs as the /ttorch entry point
//	hooks/prompt-reminders.sh  installs to ~/.claude/hooks and runs on EVERY PROMPT, which is
//	                           the most direct execution channel in the tree
//	agents/*.md                ALL of them, not just ttorch-reviewer-*: desiredFiles walks the
//	                           directory, so ttorch-worker.md and the 31 curated profiles are
//	                           installed the same way the reviewers are
//
// With the two narrow prefixes, 35 of the 42 installed files were uncovered and 7 covered
// (measured through the real matcher). The prefix "content/" is a superset of everything
// desiredFiles can install, so it closes all 35 and every subtree added later.
//
// It is a PREFIX rather than a list derived from desiredFiles at runtime. Deriving it would
// couple the guard to installer internals — a change to desiredFiles would silently change
// what the guard covers, and internal/installer/ is covered precisely because that mapping is
// attackable. The prefix is a superset, so it cannot be narrowed by editing installer code.
// TestEveryInstalledContentFileIsCovered walks the real embedded tree and asserts every file
// under content/ matches, which gives derivation's safety property without the coupling.
//
// Cost: see the generated table in docs/ARCHITECTURE.md (gate-cost block).
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
// embedded file becomes ~/.claude/agents/ttorch-reviewer-security.md. Covering the content
// while leaving the mapping open would let a file from an uncovered subtree be installed as a
// reviewer instead, through the same delayed diff channel; it is also what makes the "content/"
// prefix a safe superset, since desiredFiles cannot install from outside content/ without a
// change here.
//
// internal/skills/ is an install channel in its own right, and the one the covered set
// missed for longest. Recommended() returns third-party skill refs, InstallCmd turns each
// into `npx skills add <ref>`, and EnsureInstalled runs it before EVERY team launch and
// EVERY worker spawn. So one line there fetches and installs arbitrary third-party code
// into ~/.claude/skills — the same directory content/skills/ is covered to protect, reached
// by a shorter route, since npx fetches at spawn time with no ttorch build or install in
// between. The limits list used to name ~/.claude/skills only as an out-of-repo exposure no
// diff-channel guard could see; that was an understatement once this route existed.
//
// .ttorch/ is a PREFIX rather than the single ".ttorch/validate.sh" it used to be, and this
// inversion closes a class rather than a file. The channel that forced it:
// .ttorch/learnings.jsonl is the per-repo lessons ledger, learnings.Apply renders it into
// AGENTS.md between markers (learnings.go's writeBlock), Promoted admits any entry that is
// pinned or has been seen twice, Render emits "- " + e.Text VERBATIM up to 20 entries, and
// content/skills/ttorch-manager/SKILL.md has the manager run `ttorch learn` at every
// delivery. So a committed ledger transplants attacker-chosen text into AGENTS.md — the one
// file this guard treats as the gate's own configuration — on the merge alone, with no build
// and no install. The ledger is not gitignored.
//
// Adding "learnings.jsonl" as a second exact path would have left the NEXT .ttorch/ file in
// exactly the same position. An enumerated subset of content/ had already missed installed
// files twice, and an enumerated subset of .ttorch/ then missed the ledger; covering the tree
// covers everything under it, including files that do not exist yet. .ttorch/task, the one file
// besides validate.sh that shows up locally, is gitignored so it cannot appear in a diff, and
// that needed FIXING as part of this change: it was excluded only through .git/info/exclude,
// which is local to a clone and does not travel. The manager writes that file into every worker
// worktree, so in a fresh clone a worker's `git add -A` would have staged it and this prefix
// would have refused the merge on every task. TestTtorchRuntimeFileIsIgnored holds the
// .gitignore line so that false positive cannot come back quietly.
//
// .claude/ and .mcp.json are PROJECT-level agent configuration, and this is the strongest
// version of the content/agents argument rather than a weaker one. A landed
// .claude/agents/ttorch-reviewer-security.md REPLACES the security reviewer for every later
// gate run in the repo: Claude Code loads project-level .claude/agents/, and project-level
// takes precedence over ~/.claude/agents/ on a name collision. content/agents/ttorch-reviewer-*
// is already covered for the same effect, but that route needs a build and an install first;
// this one takes effect on the merge. .mcp.json adds tools to those same sessions. Neither
// exists in this repo, so their appearance in a diff is itself the event.
//
// vendor/ is the third way to change what `go test` compiles without touching a covered
// script: with a consistent vendor/modules.txt the toolchain builds from vendor/ rather than
// the module cache, so committed bytes there replace a dependency's implementation (verified
// the same way as go.work). It is a whole directory, so a prefix is the honest unit. 0
// commits in 196.
//
// docs/install.sh and docs/install.ps1 are in on a DIFFERENT argument from everything above,
// named as such so the flag keeps one meaning. They do not decide how a change is reviewed or
// validated. README publishes them by raw URL — `curl … /main/docs/install.sh | sh` and
// `irm … /main/docs/install.ps1 | iex` — so a merge changes bytes that users pipe straight
// into a shell, with no build, no install and no release step in between. That is the same
// delayed out-of-band effect that justifies the rest of the set, with the most severe
// consequence of any file in the repo, and covering one platform's installer but not the
// other would be an obvious gap.
//
// So the covered set answers two questions, not one: what decides how a change is REVIEWED or
// VALIDATED, and what a merge PUBLISHES DIRECTLY to users. Anything outside both is not
// covered however alarming it looks, which is what keeps the flag legible.
//
// .github/workflows/ is here because .ttorch/validate.sh on this repo runs only the FAST lane
// and says so in its own header: the full suite, including the orchestrator e2e tests, runs in
// CI as the required check. CI is therefore half of what "validated" means for this repo, and
// a landed weakening of ci.yml weakens every later change's validation through exactly the
// delayed diff channel that put the skills on this list. The trusted gate does not itself
// consult CI, which is the argument against including it; it loses to the fact that the gate
// script defers to CI by name. Its cost is in the generated table in docs/ARCHITECTURE.md
// (gate-cost block).
var gateConfigPrefixes = []string{
	"vendor/",
	".claude/",
	".ttorch/",
	".github/workflows/",
}

// ttorchSourcePrefixes are covered ONLY in ttorch's own source repository, for the same
// reason as ttorchSourceFiles.
//
// "content/" is the one that forced this split. It is ttorch's embedded payload here, but it
// is also the conventional content directory for Hugo, Next and most CMS layouts, so an
// unscoped rule demanded --allow-gate-change from every user whose repo happens to have one.
// The internal/ entries are the same mistake one step quieter: internal/validate/ and
// internal/review/ are ordinary names for ordinary Go packages.
var ttorchSourcePrefixes = []string{
	"content/",
	"internal/review/",
	"internal/approval/",
	"internal/validate/",
	"internal/projectinit/",
	"internal/installer/",
	"internal/skills/",
	// internal/worktree/ holds the guard's EVIDENCE layer: ChangedFiles and ChangedLinks are
	// its entire input, TreeFiles is what the collision check keys, and CatBlobs is the raw
	// read the scope resolves from. A one-line change to CatBlobs's record framing
	// reproduces the .gitattributes critical, and it sat outside the covered set while
	// internal/review/ and internal/validate/ were inside it on a thinner dependency.
	// Whole package rather than the one file, because a file list over a package decays
	// silently when the package is re-split, and a prefix does not.
	"internal/worktree/",
}

// fsIdentityKey is the key under which two repository paths are THE SAME FILE on a
// case-insensitive, normalizing filesystem. It is the only path comparison in this guard.
//
// It must model what the filesystem does, not what a plausible reading of the Unicode tables
// suggests, and each previous attempt got that wrong in a way that was exploitable:
//
//	strings.ToLower    simple LOWERCASING. Missed U+017F (ſ), which folds with 's' but
//	                   lowercases to itself: "agentſ.md" evaded the guard and overwrote
//	                   AGENTS.md on checkout.
//	unicode.SimpleFold single-rune folding. Missed the MULTI-RUNE full folds: U+FB01 (ﬁ)
//	                   folds to "fi", so "Makeﬁle" substituted the Makefile the gate runs.
//	NFD+fold+NFD       missed the 172 Cherokee runes, where cases.Fold is an INVOLUTION
//	                   rather than a canonicalisation — fold(U+13A0)=U+AB70 and
//	                   fold(U+AB70)=U+13A0, so the two spellings swap and never meet.
//
// The pipeline is NFD, full case folding, then SimpleFold orbit-minimum. The orbit pass runs
// LAST and is what settles Cherokee: it maps every rune to the lowest code point in its fold
// orbit, so an involution's two endpoints land on the same one. Running it first instead
// misses 33 pairs, and folding twice does not help at all — fold(fold(x)) == x is precisely
// the problem.
//
// There is deliberately NO trailing NFD. It was in an earlier version justified as insurance
// against "folding producing characters that themselves decompose", which is not true of the
// fold output; and once the orbit pass exists it stops being a no-op and becomes wrong. It is
// wrong on exactly two unordered pairs, named here rather than counted, because a bare count
// in this comment has twice been the thing that turned out to be unverifiable:
//
//	U+1F9C  ᾜ            vs  U+1F28 U+038A  ἨΊ
//	U+1F94  ᾔ            vs  U+1F28 U+038A  ἨΊ
//
// A trailing NFD merges each pair; the shipped pipeline separates them; and APFS keeps the
// files apart, so merging them is a false positive. The effect needs TWO runes, which is why
// an exhaustive single-rune search finds nothing — that search is reproduced as case (a) of
// TestTrailingNFDWouldBeAFalsePositive, which also pins the pairs above against the real
// filesystem so the claim cannot rot back into an unchecked number.
//
// Measurements behind those claims, all against real APFS by creating both files and reading
// one back — 16,304 single-rune pairs covering the lower/upper/title/SimpleFold-orbit/NFC/NFD
// /fold image of every printable rune:
//
//	NFD,fold,orbit  (this)    0 misses   0 false positives
//	NFD,fold,NFD              172 misses (all Cherokee)
//	fold,NFD,fold             172 misses (the involution cancels)
//	orbit,NFD,fold,NFD        33 misses
//	SimpleFold orbit alone    13,328 misses
//
// TestFSIdentityKeySweep keeps a bounded version of that sweep (2,367 pairs) in the suite.
// It calls skipIfShort, so `go test -short` skips it, but .ttorch/validate.sh runs
// `make test-gate` as well as `make test-fast` and test-gate is not -short. The sweep
// therefore DOES execute in the lane that gates, measured at 7.80s. This comment has now
// been wrong in both directions: it first cited the seven-pair
// TestFSIdentityKeyMatchesTheFilesystem as the sweep, then said the sweep does not run in
// the gate lane, which stopped being true when test-gate was added in the same wave.
//
// cases.Caser is documented as possibly stateful and not safe to share, so a Caser is built
// per call rather than cached in a package var.
func fsIdentityKey(p string) string {
	return orbitMin(cases.Fold().String(norm.NFD.String(p)))
}

// orbitMin maps every rune to the lowest code point in its Unicode simple-case-folding orbit.
// It is a canonicalisation, which full folding alone is not: cases.Fold swaps the two Cherokee
// cases instead of picking one, and walking the orbit to its minimum picks one.
func orbitMin(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < lowest {
				lowest = f
			}
		}
		b.WriteRune(lowest)
	}
	return b.String()
}

// gateConfigBasenames define the gate by FILENAME, at any depth. Claude Code loads a
// CLAUDE.md or AGENTS.md from a subdirectory on demand when a session reads files in that
// directory, so internal/orchestrator/CLAUDE.md is the root file's rationale applied one level
// down: agent instructions that need no build and no install and take effect on the merge.
//
// Covering them by name rather than by enumerating the two that could plausibly exist today
// is deliberate — the set of directories is open, and a list would go stale the first time
// someone adds a package. It is why docs/AGENTS.md, which an earlier version of
// TestMatchesGateConfig pinned as a near-miss that must stay OUT, is now correctly IN: a
// session reading files under docs/ loads it, so it is an instruction file like any other.
//
// This OVERLAPS gateConfigFiles, which also names AGENTS.md and CLAUDE.md, and the redundancy
// is deliberate. The root AGENTS.md is the delivery-mode config — the single most important
// entry in the whole set — and the root CLAUDE.md is the symlink to it. Leaving them to be
// covered only as a side effect of a filename rule would mean that narrowing this rule later
// (deciding nested instruction files are too broad, say) silently drops the gate's own mode
// config. They stay named explicitly so that cannot happen.
var gateConfigBasenames = []string{"AGENTS.md", "CLAUDE.md"}

// gateScope is the covered set resolved for ONE repository. The set is not universal: some
// of it is ttorch's own source.
//
// gateConfigPrefixes used to be a package-level list consulted with no idea which repo it was
// in, and "content/" was in it. In this repository content/ is the embedded payload, so
// covering it is right. In a Hugo, Next or any CMS-shaped repository content/ is where the
// articles live, so the same rule demanded --allow-gate-change for every ordinary change.
// internal/review/ and internal/validate/ are the same error in quieter clothes.
type gateScope struct {
	// TtorchSource says this repository is ttorch's own source, so the paths that decide a
	// merge HERE are the paths ttorch's Go code lives at.
	TtorchSource bool
}

func (sc gateScope) files() []string {
	if !sc.TtorchSource {
		return gateConfigFiles
	}
	return append(append([]string{}, gateConfigFiles...), ttorchSourceFiles...)
}

func (sc gateScope) prefixes() []string {
	if !sc.TtorchSource {
		return gateConfigPrefixes
	}
	return append(append([]string{}, gateConfigPrefixes...), ttorchSourcePrefixes...)
}

// resolveGateScope decides whether repo is ttorch's own source, reading BASE and never rev.
//
// The signal is a Go //go:embed directive whose pattern is rooted at "content". That is the
// property that makes content/ load-bearing in the first place: installer.apply walks
// embedRoot ("content") in the embedded FS, so a repo embedding content/ as a Go payload is a
// repo where content/ IS the thing the installer lays into ~/.claude. A repo that merely has
// a content/ directory of articles has no such directive and is not scoped in. Detected from
// the repository rather than from its name or remote URL, both of which are cosmetic.
//
// WHY A WORKER'S DIFF CANNOT SPOOF IT, which is the question worth answering rather than
// assuming: the directive is read from BASE, the default-branch tree, never from rev. This is
// the same rule resolveGateDefinition already applies to the gate script, and for the same
// reason. A worker cannot widen the scope, and more importantly cannot NARROW it: deleting
// content.go on its branch does not change what base says, so content/ stays covered for that
// merge. Reading rev instead would have been the hole, since a diff could drop the directive
// and un-cover content/ in the same commit that rewrites it.
//
// Two backstops if the signal is ever wrong. Scoping in wrongly costs an --allow-gate-change
// on a merge that did not need one, the same over-match the fold already accepts. Scoping out
// wrongly in THIS repo is caught by TestTtorchRepoIsScopedIn, which resolves the scope against
// this very tree.
//
// Fails CLOSED: a git error returns TtorchSource=true, because the expensive mistake is
// leaving ttorch's own deciding code uncovered.
func resolveGateScope(repo, base string) gateScope {
	goFiles, err := worktree.TreeFiles(repo, base)
	if err != nil {
		return gateScope{TtorchSource: true}
	}
	var candidates []string
	for _, f := range goFiles {
		if strings.HasSuffix(f, ".go") {
			candidates = append(candidates, f)
		}
	}
	blobs, err := worktree.CatBlobs(repo, base, candidates)
	if err != nil {
		return gateScope{TtorchSource: true}
	}
	for _, body := range blobs {
		// Cheap byte filter first, on bytes already in hand, so only the handful of files
		// that mention a directive get parsed.
		if !bytes.Contains(body, []byte("go:embed")) {
			continue
		}
		if embedsContentRoot(body) {
			return gateScope{TtorchSource: true}
		}
	}
	return gateScope{}
}

// embedsContentRoot reports whether Go source carries a //go:embed directive that selects a
// top-level entry named "content".
//
// Parsed with go/parser rather than scanned for a prefix. Two hand-rolled versions of this
// shipped before and both were evaded: //go:embed all:cont* compiles, produces an FS with
// content at top level, and reads as the literal element "cont*" to a string scanner. The
// answer is not a longer list of spellings, it is to implement the grammar the directive
// actually has.
//
//   - the directive is a COMMENT immediately preceding a declaration, which go/ast gives us
//     positionally rather than by guessing at line prefixes
//   - a pattern may be a Go string literal, so strconv.Unquote is the right reader
//   - a pattern is a path.Match GLOB, per the go:embed spec, so path.Match against the name
//     is the right test. That is what catches cont* without knowing cont* exists.
//
// Syntax errors are reported as MATCHING. A file this cannot parse is a file whose payload
// cannot be ruled out, and the expensive mistake is un-covering the deciding code.
func embedsContentRoot(src []byte) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return true
	}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			rest, ok := strings.CutPrefix(c.Text, "//go:embed ")
			if !ok {
				continue
			}
			for _, pat := range splitEmbedPatterns(rest) {
				if after, found := strings.CutPrefix(pat, "all:"); found {
					pat = after
				}
				first, _, _ := strings.Cut(pat, "/")
				if ok, err := path.Match(first, embedRootName); err == nil && ok {
					return true
				}
			}
		}
	}
	return false
}

// embedRootName is the directory installer.apply walks in the embedded FS. It is the
// installer's embedRoot constant; the gate has to agree with it, and a mismatch would mean
// the guard scopes on one name while the installer installs from another.
const embedRootName = "content"

// splitEmbedPatterns splits a directive's arguments, honouring Go string literals so a
// quoted pattern is read as one argument with its quotes removed.
func splitEmbedPatterns(rest string) []string {
	var out []string
	for _, tok := range strings.Fields(rest) {
		if unq, err := strconv.Unquote(tok); err == nil {
			tok = unq
		}
		out = append(out, tok)
	}
	return out
}

// matchesGateConfig reports whether a repository path names a gate-definition file. Matching
// is exact against gateConfigFiles, by prefix against gateConfigPrefixes, and by filename at
// any depth against gateConfigBasenames — with BOTH SIDES reduced to fsIdentityKey first.
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
func matchesGateConfig(name string, sc gateScope) bool {
	folded := fsIdentityKey(name)
	for _, g := range sc.files() {
		if folded == fsIdentityKey(g) {
			return true
		}
	}
	for _, p := range sc.prefixes() {
		fp := fsIdentityKey(p)
		if strings.HasPrefix(folded, fp) {
			return true
		}
		// The DIRECTORY'S OWN PATH, with no trailing slash. Every entry here ends in "/", so
		// the prefix test alone matched ".claude/agents/x" and missed ".claude" -- and a
		// changed path that IS the covered directory is exactly what a symlink standing in
		// its place reports. Compared as a whole path rather than by trimming the slash off
		// the prefix: trimming would turn "content/" into the prefix "content" and pull in
		// "contentious/", which TestMatchesGateConfig pins as a near-miss.
		//
		// It does not flag ordinary commits, and not by luck: git names a directory in a
		// diff only when that path IS an entry, which for a directory means a symlink or a
		// gitlink, and ordinary commits report the files inside it.
		if dir, ok := strings.CutSuffix(fp, "/"); ok && folded == dir {
			return true
		}
	}
	// Folding never touches '/', so the basename of the folded path is the folded basename.
	base := folded
	if i := strings.LastIndexByte(folded, '/'); i >= 0 {
		base = folded[i+1:]
	}
	for _, b := range gateConfigBasenames {
		if base == fsIdentityKey(b) {
			return true
		}
	}
	return false
}

// hostilePath reports whether a repo path carries a byte that has no business in a filename
// and that downstream consumers render verbatim.
//
// A C0 control character, DEL or a C1 control is the marker. C1 is in because U+0085 NEL is
// itself a line break and U+009B is CSI, the single-character "ESC [" several terminals
// accept; no legitimate path carries either. The audit log is newline-delimited
// (audit.go's "%s %s\n") and the merge record interpolates the matched path into it, so a
// path containing a literal newline writes a second, well-formed, entirely fabricated
// trusted-merge line. git's C-quoting used to make that unreachable; reading the file list
// with -z removed the quoting along with the quoting bug, so the refusal has to be explicit.
// The audit sink escapes control characters as well — this is the half that stops the path
// from reaching a merge at all, that one is the half that protects every other call site.
func hostilePath(name string) bool {
	for _, r := range name {
		if isAuditControl(r) {
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
//     The comparison is over the WHOLE tree, and over DIRECTORIES as well as blobs, because a
//     blob can collide with a covered directory and erase it. It is keyed on fsIdentityKey,
//     which models the filesystem, rather than mirroring matchesGateConfig — keying the
//     backstop on the same relation as the matcher makes it blind to exactly what the matcher
//     is blind to, which an earlier version did while claiming otherwise.
//
//     What this is NOT: it is not a guarantee that any substitution is observable. It catches
//     the case where two entries in ONE tree resolve to one path. It cannot see a substitution
//     that never produces two entries — and an earlier version of this comment claimed it
//     could, which was wrong twice over, since it was also blind to directories at the time.
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
	sc := resolveGateScope(repo, base)
	if hit, err := linksOverGateConfig(repo, base, rev, sc); err != nil || hit != nil {
		return hit, err
	}
	if hit, err := collidesInTree(repo, base, rev); err != nil || hit != nil {
		return hit, err
	}
	for _, n := range changed {
		if matchesGateConfig(n, sc) {
			return &gateConfigHit{Path: n, Reason: fmt.Sprintf("changes a gate-definition file (%s)", n)}, nil
		}
	}
	return nil, nil
}

// linksOverGateConfig refuses a diff that introduces a symlink or gitlink standing at, or
// above, covered ground.
//
// A symlink AT a covered path is refusable on its own, and Blocking rather than flaggable.
// The reason is that --allow-gate-change authorizes a change a human has read in the diff,
// and a link's diff does not show what changes. Committing
//
//	.claude -> docs/payload
//	docs/payload/agents/ttorch-reviewer-security.md
//
// reports two paths. Before this check neither matched anything, so the merge was not even
// flagged; with the directory's own path now matching, ".claude" would be flaggable, and a
// lead reading "changes a gate-definition file (.claude)" would be approving the contents of
// docs/payload without being shown them. The bytes that take effect live at a path the
// refusal never names, and repointing the link later moves them again with no diff at
// .claude at all. No flag can make that reviewable, so there is no flag.
//
// "Above" covers the ancestor case: docs/install.sh is covered, so a symlink at "docs"
// redirects it while reporting only "docs". A path-based guard cannot see through either.
//
// Only links the diff INTRODUCES are reported (see worktree.ChangedLinks). This repo's own
// CLAUDE.md is a committed symlink; refusing every changed link would refuse every commit
// that touches it, with no flag to clear it, and repointing an existing link is already
// flaggable through the name match.
//
// Left open deliberately: REPOINTING a pre-existing symlink that stands at covered ground is
// flaggable rather than blocking. Closing it would mean refusing every commit that touches
// CLAUDE.md outright, which is a real cost for a case that cannot arise here without first
// landing a link at covered ground, and that first merge is blocking. If a repo ever
// legitimately carries such a link, revisit this rather than discovering it during an
// incident.
//
// Limit, stated rather than implied: a symlink at a path that shadows NOTHING covered is not
// refused, even though any directory symlink can introduce files git never lists by path.
// Refusing all of them would fire on ordinary repository layout, and the basename rule makes
// "could a nested AGENTS.md appear under it" true of every directory in the tree.
func linksOverGateConfig(repo, base, rev string, sc gateScope) (*gateConfigHit, error) {
	links, err := worktree.ChangedLinks(repo, base, rev)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if !linkShadowsGateConfig(l.Path, sc) {
			continue
		}
		kind := "a symlink"
		if l.Mode == "160000" {
			// A gitlink checks out as an empty directory until `submodule update --init`,
			// so the covered ground is hidden rather than replaced. Same refusal: the tree
			// the gate validates is not the tree the path describes.
			kind = "a submodule pointer (gitlink)"
		}
		return &gateConfigHit{
			Path: sanitizePathForMessage(l.Path),
			Reason: fmt.Sprintf("introduces %s at %s, which stands at or above gate-definition ground; "+
				"the bytes that take effect are not the bytes at the path this diff reports, so no "+
				"approval scope can authorize it", kind, sanitizePathForMessage(l.Path)),
			Blocking: true,
		}, nil
	}
	return nil, nil
}

// linkShadowsGateConfig reports whether a link at p stands at, or above, covered ground.
func linkShadowsGateConfig(p string, sc gateScope) bool {
	if matchesGateConfig(p, sc) {
		return true
	}
	under := fsIdentityKey(p) + "/"
	for _, f := range sc.files() {
		if strings.HasPrefix(fsIdentityKey(f), under) {
			return true
		}
	}
	for _, pre := range sc.prefixes() {
		if strings.HasPrefix(fsIdentityKey(pre), under) {
			return true
		}
	}
	return false
}

// collidesInTree reports the first NEWLY INTRODUCED filesystem entry in rev's tree that
// shares an fsIdentityKey with a DIFFERENT entry in the same tree.
//
// "Entry" means blob OR DIRECTORY, and the directories are the whole point. An earlier
// version keyed only the blob list from `git ls-tree -r`, where a directory is never an
// entry, so a blob that collides with a covered DIRECTORY was invisible:
//
//	tree:     .github/workflows/ci.yml   .github/workflowſ   main.go
//	guard:    nil — the blob matches no covered path and no other BLOB
//	checkout: .github/workflowſ is a 7-byte file; .github/workflows/ is GONE
//
// The whole covered directory vanishes from the bytes the gate validates, git warns about
// nothing, and because a missing .github/workflows/ breaks no Go build the validate then goes
// green against a checkout that is not the tree — which is the exact outcome running this
// guard before the validate exists to prevent. content/ and .claude/ go the same way. The
// spelling has to sort after the directory's entries for the blob to win, which plain ASCII
// cannot do ('/' sorts low) but a non-ASCII spelling does for free.
//
// So the keyed set is every blob plus every ancestor directory those blobs imply. Git stores
// no empty trees in a commit, so the implied set is exactly what `ls-tree -r -t` would list;
// deriving it here rather than asking git keeps the decision inside a covered file.
//
// Only entries ABSENT FROM BASE are reported. A repository that already contains a colliding
// pair would otherwise be unmergeable forever with no flag to clear it and no way to land the
// rename that fixes it. Restricting to new entries means the guard fires on the diff that
// INTRODUCES the collision, which is both the attack and the thing a human can act on.
//
// Pairs are reported deterministically — entries are sorted before a partner is chosen — so
// the same diff always yields the same refusal and the same audit line.
func collidesInTree(repo, base, rev string) (*gateConfigHit, error) {
	revBlobs, err := worktree.TreeFiles(repo, rev)
	if err != nil {
		return nil, err
	}
	if len(revBlobs) == 0 {
		return nil, nil
	}
	baseBlobs, err := worktree.TreeFiles(repo, base)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(baseBlobs)*2)
	for _, e := range entriesWithDirs(baseBlobs) {
		existing[e] = true
	}
	entries := entriesWithDirs(revBlobs)
	sort.Strings(entries)
	byKey := make(map[string][]string, len(entries))
	for _, e := range entries {
		k := fsIdentityKey(e)
		byKey[k] = append(byKey[k], e)
	}
	for _, e := range entries {
		if existing[e] {
			continue // already on the default branch; this diff did not introduce it
		}
		for _, other := range byKey[fsIdentityKey(e)] {
			if other == e {
				continue
			}
			return &gateConfigHit{
				Path: e,
				Reason: fmt.Sprintf("adds %q, which resolves to the same file as %q on a case-insensitive or normalizing filesystem; a checkout writes one over the other and the guard cannot tell which wins",
					sanitizePathForMessage(e), sanitizePathForMessage(other)),
				Blocking: true,
			}, nil
		}
	}
	return nil, nil
}

// entriesWithDirs expands a blob list into every filesystem entry the tree materializes: the
// blobs themselves plus every ancestor directory. Deduplicated; order is not meaningful and
// callers that need determinism sort it.
func entriesWithDirs(blobs []string) []string {
	seen := make(map[string]bool, len(blobs)*2)
	out := make([]string, 0, len(blobs)*2)
	for _, b := range blobs {
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
		for i := strings.LastIndexByte(b, '/'); i > 0; i = strings.LastIndexByte(b, '/') {
			b = b[:i]
			if seen[b] {
				break // this ancestor and all of its own are already recorded
			}
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
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
		case isAuditControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

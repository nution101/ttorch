package orchestrator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	ttorchembed "github.com/nution101/ttorch"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
)

// ttorchScope is the scope for ttorch's own source repository, where content/ is the
// embedded payload and internal/ holds the code that decides a merge. Tests that assert
// against this repo's paths pass it explicitly, so the dependency on the repo's identity is
// visible in the call rather than assumed. TestGateScope_ContentOnlyGatesTheRepoThatEmbedsIt
// covers the other scope.
var ttorchScope = gateScope{TtorchSource: true}

// decidingFunctions names every function in this package that participates in deciding
// whether a change may merge: resolving what the gate is, running it, caching its result,
// minting and reading the approval that authorizes a merge, and the gate-config guard
// itself. gateConfigFiles must name whichever file each one lives in, so that a diff
// rewriting the gate's behaviour cannot merge without --allow-gate-change.
//
// This list is the SPEC. TestGateConfigCoversTheDecidingCode checks the code against it,
// not the other way round, so adding a new gate-deciding function is a deliberate edit
// here rather than something a test infers and silently blesses.
var decidingFunctions = []string{
	// the gate's definition, its execution, and its content-addressed cache
	"resolveGateDefinition", "runGate", "gateGreen", "runGateOnCommitted",
	"validateCommitted", "stagedGreen", "hasDefaultBranchGateScript",
	"validateForMerge", "reusablePrepValidate",
	"validateCacheKey", "loadValidateCache", "storeValidateCache",
	// the gate-config guard
	"matchesGateConfig", "diffTouchesGateConfig",
	// the approval token: what it grants, how it is read, and when it is re-minted
	"Approve", "approvalPayload", "splitApprovalPayload", "remintFromVerdict",
	// the merge and review decisions themselves
	"MergeLocal", "TrustPrep", "TrustRecord", "carryVerdictForward", "gateCoversRebased",
	// the audit record a trusted merge refuses to proceed without
	"writeAudit", "sanitizeAuditLine",
	// the path-spelling and collision controls the guard rests on
	"fsIdentityKey", "orbitMin", "hostilePath", "collidesInTree", "entriesWithDirs",
	"sanitizePathForMessage", "isAuditControl",
}

// nonDecidingFiles are the files in this package that have been LOOKED AT and judged not to
// decide whether a change may merge. Together with gateConfigFiles they must account for every
// non-test file in the package; TestOrchestratorFilesAreClassified enforces that, so a new
// file forces one explicit decision instead of defaulting to uncovered.
var nonDecidingFiles = map[string]string{
	"internal/orchestrator/autostart.go":    "starts the scheduler daemon; every gate check still runs inside MergeLocal",
	"internal/orchestrator/landqueue.go":    "serializes WHEN tasks land, not WHETHER; each land re-enters MergeLocal",
	"internal/orchestrator/orchestrator.go": "Manager construction, task CRUD and status",
	"internal/orchestrator/overlap.go":      "plans dispatch order from file footprints",
	"internal/orchestrator/spawn.go":        "launches worker sessions",
}

// TestGateConfigCoversTheDecidingCode is the anchor for the file-granular half of
// gateConfigFiles.
//
// gateConfigPrefixes can name whole directories because content/, internal/review/,
// internal/approval/ and internal/validate/ are each small and single-purpose. This package
// is not, so covering "internal/orchestrator/" wholesale would put most of the repo's
// commits behind --allow-gate-change and turn the flag into a formality. gateConfigFiles
// therefore names five files instead.
//
// The figures are in docs/ARCHITECTURE.md and only there. This comment used to restate them
// and drifted twice; the second time it said 54% where the measurement was 66.3%, which
// understated the cost in the direction that made the boundary look better than it is.
// TestGateCostFiguresMatchTheDoc measures the doc, so a figure that lives only there cannot
// go stale unnoticed, and one restated here can.
//
// A file list over a package that gets refactored decays silently — this package has already
// been re-split once (140d2b91, "split god-file into focused single-responsibility files"),
// and a later split that moved MergeLocal into mergegate.go would drop it out of coverage
// with nothing failing. This test is what makes that loud: move a deciding function to a file
// gateConfigFiles does not name and it fails here, at `make test`, before the guard has
// quietly stopped guarding.
//
// Fixing a failure means either moving the function back, or adding its new file to
// gateConfigFiles and re-measuring the blast radius — not deleting the entry.
//
// WHAT THIS TEST DOES NOT DO: it asks one direction only. Each listed function must live in a
// covered file. It cannot notice a NEW deciding function, and inside a file already classified
// as non-deciding it cannot notice anything at all. TestOrchestratorFilesAreClassified covers
// the new-FILE case; a new deciding function extracted into, say, landqueue.go is caught by
// review or not at all. Do not describe either test as "fails if any deciding function lands
// in an uncovered file" — that was claimed here before and it was not true.
func TestGateConfigCoversTheDecidingCode(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the orchestrator package: %v", err)
	}
	// Where each top-level func (or method) is declared, by bare function name.
	declaredIn := map[string]string{}
	for _, p := range pkg {
		for name, f := range p.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				declaredIn[fn.Name.Name] = filepath.ToSlash(filepath.Join("internal", "orchestrator", filepath.Base(name)))
			}
		}
	}
	// runGateOnCommitted is a package-level var holding a func literal (a test seam), not a
	// FuncDecl, so record the var declarations too.
	for _, p := range pkg {
		for name, f := range p.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						if i < len(vs.Values) {
							if _, isFunc := vs.Values[i].(*ast.FuncLit); isFunc {
								declaredIn[id.Name] = filepath.ToSlash(filepath.Join("internal", "orchestrator", filepath.Base(name)))
							}
						}
					}
				}
			}
		}
	}

	// Asks the resolved scope rather than gateConfigFiles alone. The deciding orchestrator
	// files are ttorch-source tier, covered only in this repository, so reading one list
	// would report them uncovered and read as a hole that is not there.
	covered := map[string]bool{}
	for _, f := range ttorchScope.files() {
		covered[f] = true
	}
	for _, fn := range decidingFunctions {
		file, ok := declaredIn[fn]
		if !ok {
			t.Errorf("%s is listed as gate-deciding but no longer exists in this package; if it was renamed, rename it in decidingFunctions too — do not just drop it", fn)
			continue
		}
		if !covered[file] {
			t.Errorf("%s now lives in %s, which the covered set does not name: a diff rewriting it would merge without --allow-gate-change. Move it back, or add %s to the covered set and re-measure the blast radius.", fn, file, file)
		}
	}
}

// absentByDesign are covered entries that do NOT exist in this repo and are not expected to.
// Each is exempt from the dead-coverage check below for a stated reason, so the exemption is
// a decision on the record rather than a quiet hole in it.
//
// It used to also carry .ttorch/validate.sh and AGENTS.md on the grounds that they "describe
// every managed repo, not this one". Both are tracked HERE, so those exemptions were dead and
// only weakened the check; they are gone and the dead-coverage test is stricter for it.
var absentByDesign = map[string]string{
	// These three are covered PRECISELY BECAUSE they should not exist. Each silently changes
	// what `go test` compiles, which is what .ttorch/validate.sh runs via `make test-fast`,
	// so a worker introducing one substitutes the thing the gate validates against. Their
	// appearance in a diff is the event being guarded, so requiring them to exist would
	// invert the test.
	"go.work":     "auto-discovered via GOWORK; its replace directives override go.mod",
	"go.work.sum": "inert without go.work, covered alongside it so the pair cannot drift",
	"vendor/":     "a consistent vendor/ makes the toolchain build from it instead of the module cache",
	".claude/":    "project-level agent config; a .claude/agents/ttorch-reviewer-*.md would REPLACE a gate reviewer",
	".mcp.json":   "project-level MCP servers for those same sessions",
}

// TestGateConfigFilesAreRealPaths guards the other direction: an entry that names nothing is
// dead coverage that reads as protection. Entries in absentByDesign are exempt, and the test
// also fails if one of THOSE turns up — an entry justified as "should not exist" that now
// exists needs its reason rechecked, not silently kept.
//
// It asks git for the TRACKED path list rather than stat-ing the working directory, because
// that is the universe the guard actually inspects: diffTouchesGateConfig reads committed
// objects. The distinction is not academic here — this worktree carries an untracked,
// un-gitignored .claude/settings.local.json, so a stat-based check reported .claude/ as
// "exists" and failed, while no .claude/ path has ever been committed. (That the directory is
// committable and not ignored is itself part of why it is covered.)
func TestGateConfigFilesAreRealPaths(t *testing.T) {
	root := repoRootForGateConfig(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}
	var tracked []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			tracked = append(tracked, n)
		}
	}
	if len(tracked) == 0 {
		t.Fatal("git reported no tracked files; the check would pass vacuously")
	}
	// Each set is checked under ITS OWN matching rule rather than a guessed one, so this
	// cannot drift from what matchesGateConfig does.
	exactCovers := func(entry string) bool {
		for _, f := range tracked {
			if f == entry {
				return true
			}
		}
		return false
	}
	prefixCovers := func(entry string) bool {
		for _, f := range tracked {
			if strings.HasPrefix(f, entry) {
				return true
			}
		}
		return false
	}
	baseCovers := func(entry string) bool {
		for _, f := range tracked {
			if path.Base(f) == entry {
				return true
			}
		}
		return false
	}
	covers := func(entry string) bool {
		return exactCovers(entry) || prefixCovers(entry) || baseCovers(entry)
	}
	for _, entry := range gateConfigFiles {
		if _, exempt := absentByDesign[entry]; exempt {
			continue
		}
		if !exactCovers(entry) {
			t.Errorf("no tracked path equals the covered file %q. Either it is dead coverage, or it belongs in absentByDesign with a reason.", entry)
		}
	}
	for _, entry := range gateConfigPrefixes {
		if _, exempt := absentByDesign[entry]; exempt {
			continue
		}
		if !prefixCovers(entry) {
			t.Errorf("no tracked path starts with the covered prefix %q. Either it is dead coverage, or it belongs in absentByDesign with a reason.", entry)
		}
	}
	for _, entry := range gateConfigBasenames {
		if !baseCovers(entry) {
			t.Errorf("no tracked path is named %q. Either it is dead coverage, or it belongs in absentByDesign with a reason.", entry)
		}
	}
	// Every exemption must still be a covered entry, and the should-not-exist ones must still
	// match nothing tracked.
	// Asks the resolved scope rather than gateConfigFiles alone. The deciding orchestrator
	// files are ttorch-source tier, covered only in this repository, so reading one list
	// would report them uncovered and read as a hole that is not there.
	covered := map[string]bool{}
	for _, f := range ttorchScope.files() {
		covered[f] = true
	}
	for _, p := range gateConfigPrefixes {
		covered[p] = true
	}
	for _, b := range gateConfigBasenames {
		covered[b] = true
	}
	for entry, why := range absentByDesign {
		if !covered[entry] {
			t.Errorf("absentByDesign exempts %q, which is not in the covered set at all", entry)
			continue
		}
		if covers(entry) {
			t.Errorf("%q is now tracked in this repo, but it is covered on the grounds that it should not be (%s). Recheck the reason before keeping the exemption.", entry, why)
		}
	}
}

func path0Dir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	return "./"
}

// repoRootForGateConfig walks up from this package to the module root.
func repoRootForGateConfig(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root above internal/orchestrator")
	return ""
}

// TestMergeLocal_DecidingCodeChangeNeedsAllowGateChange walks the newly covered paths
// end-to-end, through the real merge gate rather than through matchesGateConfig alone: the
// deciding Go code and the CI workflow that runs the full suite. Each is refused on a plain
// human approval, merges once the lead approves it with --allow-gate-change, and lands an
// audit line naming the file.
//
// The last case is the control. An ordinary source file still merges with no flag, which is
// the property that keeps the guard a signal rather than a blanket — the widening is only
// worth having if most changes still pass through it untouched.
func TestMergeLocal_DecidingCodeChangeNeedsAllowGateChange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		needFlag bool
	}{
		{"the gate implementation", "internal/orchestrator/gate.go", true},
		{"the findings contract", "internal/review/review.go", true},
		{"the approval token", "internal/approval/approval.go", true},
		{"the check runner", "internal/validate/validate.go", true},
		{"the delivery-mode parser", "internal/projectinit/projectinit.go", true},
		{"the full-suite CI authority", ".github/workflows/ci.yml", true},
		{"a workspace file", "go.work", true},
		{"the workspace checksums", "go.work.sum", true},
		{"a vendored dependency", "vendor/example.com/dep/d.go", true},
		// One line in Recommended() installs arbitrary third-party code into
		// ~/.claude/skills via `npx skills add`, before every team launch and every worker
		// spawn. No ttorch build and no ttorch install in between.
		{"the recommended external skills", "internal/skills/skills.go", true},
		// Still the control, and now deliberately so: internal/cli/ costs 32 of 196 commits,
		// which would take the covered set to 116/196 = 59% — past the more-than-half line
		// that is the stated reason internal/orchestrator/ is not covered wholesale. The
		// bypass that ran through it is closed by installer.ApplyEmbedded instead.
		{"CONTROL: ordinary source", "internal/cli/cli.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, repo := deliveryHarness(t, "decide")
			commitGateScript(t, repo, "exit 0")
			if _, err := projectinit.Init(repo, "trusted"); err != nil {
				t.Fatal(err)
			}
			id := "dc1"
			task, err := m.Spawn(id, repo, false, "sleep 60")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = m.Teardown(id, true) }()
			wt := task.Worktree
			if err := os.MkdirAll(filepath.Join(wt, filepath.Dir(filepath.FromSlash(tc.path))), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(tc.path)), []byte("// edited\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "-A")
			gitIn(t, wt, "commit", "-q", "-m", "edit "+tc.path)
			head := gitIn(t, wt, "rev-parse", "HEAD")
			writeReviewReports(t, m.P.ReviewInputsDir(id), head, nil)
			if _, err := m.TrustRecord(id, "", time.Minute); err != nil {
				t.Fatal(err)
			}

			if tc.needFlag {
				// Trusted mode must not auto-approve a change to what decides the merge.
				if approval.Valid(m.P.ApprovalFile(id)) {
					t.Fatalf("%s must not auto-approve in trusted mode", tc.path)
				}
				if err := m.Approve(id, time.Minute, false); err != nil {
					t.Fatal(err)
				}
				defHead := gitIn(t, repo, "rev-parse", "HEAD")
				_, err := m.MergeLocal(id, false)
				if err == nil {
					t.Fatalf("a plain approval must not authorize a change to %s", tc.path)
				}
				if !strings.Contains(err.Error(), tc.path) {
					t.Fatalf("the refusal must name %s, got: %v", tc.path, err)
				}
				if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
					t.Fatalf("%s must not have merged", tc.path)
				}
				if err := m.Approve(id, time.Minute, true); err != nil {
					t.Fatal(err)
				}
				if _, err := m.MergeLocal(id, false); err != nil {
					t.Fatalf("--allow-gate-change should let %s merge: %v", tc.path, err)
				}
				if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "gate-change="+tc.path) {
					t.Fatalf("the merge audit line must name %s: %s", tc.path, b)
				}
				return
			}

			// The control: an ordinary file auto-approves and merges with no flag at all.
			if !approval.Valid(m.P.ApprovalFile(id)) {
				t.Fatalf("an ordinary change to %s should still auto-approve in trusted mode", tc.path)
			}
			if _, err := m.MergeLocal(id, false); err != nil {
				t.Fatalf("an ordinary change to %s should merge with no flag: %v", tc.path, err)
			}
			if b, _ := os.ReadFile(m.P.AuditLog()); strings.Contains(string(b), "gate-change=") {
				t.Fatalf("an ordinary change must not be recorded as a gate change: %s", b)
			}
		})
	}
}

// TestOrchestratorFilesAreClassified answers the reverse of the question
// TestGateConfigCoversTheDecidingCode asks. That test walks a hand-written list of functions
// and checks where they live, so a brand-new deciding function in a brand-new file passes it
// in silence — which is how audit.go, holding the writeAudit that MergeLocal refuses to merge
// without, sat uncovered and unlisted.
//
// This test walks the FILES instead. Every non-test file in the package must appear either in
// gateConfigFiles (it decides, so changing it needs --allow-gate-change) or in
// nonDecidingFiles with a reason (it was read and judged not to). A new file belongs to
// neither and fails, which turns "nobody thought about it" into one deliberate line of
// classification. It cannot judge the classification, only force it to exist.
func TestOrchestratorFilesAreClassified(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Asks the resolved scope rather than gateConfigFiles alone. The deciding orchestrator
	// files are ttorch-source tier, covered only in this repository, so reading one list
	// would report them uncovered and read as a hole that is not there.
	covered := map[string]bool{}
	for _, f := range ttorchScope.files() {
		covered[f] = true
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		rel := "internal/orchestrator/" + n
		_, classified := nonDecidingFiles[rel]
		if covered[rel] == classified {
			if classified {
				t.Errorf("%s is in BOTH gateConfigFiles and nonDecidingFiles; it cannot be both", rel)
				continue
			}
			t.Errorf("%s is new and unclassified. Decide: if a change to it can alter whether or how a merge is gated, add it to gateConfigFiles (and re-measure the blast radius in docs/ARCHITECTURE.md); otherwise add it to nonDecidingFiles with the reason.", rel)
		}
	}
	// A stale entry is dead documentation that reads as a decision.
	for rel := range nonDecidingFiles {
		if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(rel))); err != nil {
			t.Errorf("nonDecidingFiles names %s, which no longer exists", rel)
		}
	}
}

// collidingPairs are path pairs that a case-insensitive, normalizing filesystem resolves to
// one file. Each was verified on APFS by creating both files and reading one back, not by
// reading a Unicode table — the last two rounds of this work were both lost to reasoning
// about folding instead of measuring it.
var collidingPairs = []struct {
	a, b, why string
}{
	{"gate.go", "gate.GO", "plain case"},
	{"AGENTS.md", "agent\u017f.md", "U+017F long s: folds with 's', lowercases to itself"},
	{"k.txt", "\u212a.txt", "U+212A kelvin sign: folds with 'k'"},
	{"Makefile", "Make\ufb01le", "U+FB01 fi ligature: FULL fold to \"fi\", missed by single-rune folding"},
	{"installer.txt", "in\ufb06aller.txt", "U+FB06 st ligature: full fold to \"st\""},
	{"ass.txt", "a\u00df.txt", "U+00DF sharp s: full fold to \"ss\""},
	{"cafe\u0301.md", "caf\u00e9.md", "NFD vs NFC of the same name"},
	{"\u13a0.txt", "\uab70.txt", "U+13A0/U+AB70 Cherokee: cases.Fold SWAPS these rather than canonicalising, so folding alone never makes them meet"},
}

// distinctPairs must NOT share a key. Folding and normalizing widen the relation, and a key
// that collapses genuinely different files would refuse ordinary merges forever.
var distinctPairs = [][2]string{
	{"Makefile", "Makefile2"},
	{"internal/review/review.go", "internal/reviewer/review.go"},
	{"content/skills/a.md", "content/skills/b.md"},
	{"AGENTS.md", "AGENT.md"},
	{"gate.go", "gates.go"},
}

// TestFSIdentityKeyMatchesTheFilesystem checks the guard's notion of "the same file" against
// the filesystem's, by creating both paths and seeing whether one overwrites the other.
//
// It replaces an earlier TestGateConfigPathsAreASCII, which asserted the wrong invariant. That
// test's premise — repeated in the docs — was that ASCII covered paths are safe because "ASCII
// has no decomposed form and no multi-character fold". That is true in one direction only: an
// ASCII path has no non-ASCII spelling of its own, but it is REACHABLE BY one, which is
// exactly the Makeﬁle attack. The test passed while the property it claimed to guarantee did
// not hold.
//
// Measuring beats asserting here. If a future Go, Unicode revision or filesystem changes
// either side, this fails rather than continuing to agree with a table nobody rechecked.
func TestFSIdentityKeyMatchesTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	if !caseFoldingDir(t, dir) {
		t.Skip("case-sensitive filesystem: it collapses nothing, so there is nothing to agree with")
	}
	for _, tc := range collidingPairs {
		fsCollides := filesCollide(t, dir, tc.a, tc.b)
		keyCollides := fsIdentityKey(tc.a) == fsIdentityKey(tc.b)
		if fsCollides && !keyCollides {
			t.Errorf("%s: the filesystem resolves %q and %q to one file but fsIdentityKey does not — a diff adding %q would evade the guard and substitute %q on checkout",
				tc.why, tc.a, tc.b, tc.b, tc.a)
		}
		if !fsCollides && keyCollides {
			t.Logf("%s: fsIdentityKey collapses %q/%q but this filesystem does not; over-matching is the safe direction", tc.why, tc.a, tc.b)
		}
	}
	for _, p := range distinctPairs {
		if fsIdentityKey(p[0]) == fsIdentityKey(p[1]) {
			t.Errorf("fsIdentityKey collapses %q and %q, which are different files; the key is too wide and would refuse ordinary merges", p[0], p[1])
		}
	}
}

// TestFSIdentityKeySweep is a bounded version of the sweep that settled the pipeline order: a
// sample of runes, each turned into a pair with its lower/upper/title/fold/NFC/NFD/orbit image
// and checked against the filesystem. The full version covered every printable rune —
// 16,304 pairs, 0 misses, 0 false positives — and is what the ordering in fsIdentityKey's
// comment is based on. This keeps a representative slice of it in the suite, including the
// Cherokee block, at a cost of a few thousand file creations rather than sixteen.
func TestFSIdentityKeySweep(t *testing.T) {
	skipIfShort(t)
	dir := t.TempDir()
	if !caseFoldingDir(t, dir) {
		t.Skip("case-sensitive filesystem: nothing folds, so there is nothing to agree with")
	}
	// Every 7th printable rune through the BMP, plus the whole Cherokee range, which is
	// where cases.Fold stops being a canonicalisation and where the last regression lived.
	var runes []rune
	for r := rune(0x20); r <= 0xFFFF; r += 7 {
		runes = append(runes, r)
	}
	for r := rune(0x13A0); r <= 0x13F5; r++ {
		runes = append(runes, r)
	}
	for r := rune(0xAB70); r <= 0xABBF; r++ {
		runes = append(runes, r)
	}
	for _, r := range []rune{0x017F, 0x212A, 0xFB01, 0xFB06, 0x00DF, 0x0130, 0x1E9E} {
		runes = append(runes, r)
	}
	var pairs, misses, falsePos int
	for _, r := range runes {
		if !unicode.IsPrint(r) || (r >= 0xD800 && r <= 0xDFFF) {
			continue
		}
		base := string(r)
		for _, c := range foldImages(base) {
			if c == base || c == "" || strings.ContainsAny(c, "/\x00") {
				continue
			}
			a, b := "x"+base+".t", "x"+c+".t"
			pairs++
			fs := filesCollide(t, dir, a, b)
			same := fsIdentityKey(a) == fsIdentityKey(b)
			switch {
			case fs && !same:
				misses++
				t.Errorf("MISS U+%04X: the filesystem resolves %q and %q to one file, fsIdentityKey does not", r, a, b)
			case !fs && same:
				falsePos++
				t.Errorf("FALSE POSITIVE U+%04X: fsIdentityKey collapses %q and %q, the filesystem keeps them apart", r, a, b)
			}
		}
	}
	if pairs < 1500 {
		t.Fatalf("the sweep only produced %d pairs; it is not covering anything", pairs)
	}
	t.Logf("%d pairs created and stat'd: %d misses, %d false positives", pairs, misses, falsePos)
}

// foldImages returns the spellings a filesystem might consider equal to s.
func foldImages(s string) []string {
	out := map[string]bool{
		strings.ToLower(s): true, strings.ToUpper(s): true, strings.ToTitle(s): true,
		norm.NFC.String(s): true, norm.NFD.String(s): true,
		cases.Fold().String(s): true,
	}
	for _, r := range s {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			out[string(f)] = true
		}
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestEntriesWithDirs pins the expansion the directory-collision check depends on.
func TestEntriesWithDirs(t *testing.T) {
	got := entriesWithDirs([]string{".github/workflows/ci.yml", "main.go", "a/b/c/d.txt", "a/b/e.txt"})
	sort.Strings(got)
	want := []string{".github", ".github/workflows", ".github/workflows/ci.yml",
		"a", "a/b", "a/b/c", "a/b/c/d.txt", "a/b/e.txt", "main.go"}
	if len(got) != len(want) {
		t.Fatalf("entriesWithDirs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entriesWithDirs = %q, want %q", got, want)
		}
	}
	if n := len(entriesWithDirs(nil)); n != 0 {
		t.Errorf("entriesWithDirs(nil) returned %d entries", n)
	}
}

// TestMatchesGateConfig_FullFoldSpellings: the covered set must be matched under the same
// relation, so a full-fold spelling of a covered path is recognised as naming it.
func TestMatchesGateConfig_FullFoldSpellings(t *testing.T) {
	for _, tc := range []struct {
		path string
		why  string
	}{
		{"Make\ufb01le", "U+FB01: the Makefile the gate executes via .ttorch/validate.sh"},
		{"internal/in\ufb06aller/installer.go", "U+FB06 in a covered prefix"},
		{"internal/orche\ufb06rator/gate.go", "U+FB06 in a covered deciding file"},
		{"agent\u017f.md", "U+017F: the delivery-mode config"},
		// The fold must not MANUFACTURE a prefix match. U+FB06 folds to "st", so this
		// becomes "contentst/skills/x.md", which shares no prefix relationship with
		// "content/". Before the prefix was widened to "content/" this case read
		// "content/skill\ufb06/x.md" and relied on "skills" not containing "st"; that stopped
		// testing anything once the covered unit became the whole tree.
		{"content\ufb06/skills/x.md", "U+FB06 folds to 'st', making 'contentst/'; must NOT match"},
	} {
		got := matchesGateConfig(tc.path, ttorchScope)
		want := tc.path != "content\ufb06/skills/x.md"
		if got != want {
			t.Errorf("%s: matchesGateConfig(%q, ttorchScope) = %v, want %v", tc.why, tc.path, got, want)
		}
	}
}

// caseFoldingDir reports whether dir's filesystem folds case at all.
func caseFoldingDir(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe.tmp"))
	return err == nil
}

// filesCollide writes a and b in dir and reports whether they turned out to be one file.
func filesCollide(t *testing.T, dir, a, b string) bool {
	t.Helper()
	pa, pb := filepath.Join(dir, a), filepath.Join(dir, b)
	for _, p := range []string{pa, pb} {
		_ = os.Remove(p)
	}
	defer func() {
		for _, p := range []string{pa, pb} {
			_ = os.Remove(p)
		}
	}()
	if err := os.WriteFile(pa, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pa)
	if err != nil {
		t.Fatal(err)
	}
	return string(got) == "B"
}

// TestTrailingNFDWouldBeAFalsePositive holds the claim in fsIdentityKey's comment about why
// there is no trailing NFD pass. That comment has twice carried a quantitative statement that
// turned out to be unverifiable, so the statement now lives here, executable, against the real
// filesystem.
//
// Case (a) is the exhaustive single-rune search, which finds NOTHING — the same result a
// reviewer got, and the reason the claim looked false. Case (b) shows why both results are
// correct: the effect needs TWO runes, so no single-rune search can see it.
func TestTrailingNFDWouldBeAFalsePositive(t *testing.T) {
	withTrailingNFD := func(p string) string { return norm.NFD.String(fsIdentityKey(p)) }

	// (a) Exhaustive over single runes: does a trailing NFD ever MERGE two paths the shipped
	// key separates? It does not. Kept because its absence is what makes (b) surprising.
	byTrailing := map[string]map[string]bool{}
	for r := rune(0x20); r <= 0x10FFFF; r++ {
		if (r >= 0xD800 && r <= 0xDFFF) || !unicode.IsPrint(r) {
			continue
		}
		n := "x" + string(r) + ".t"
		k := withTrailingNFD(n)
		if byTrailing[k] == nil {
			byTrailing[k] = map[string]bool{}
		}
		byTrailing[k][fsIdentityKey(n)] = true
	}
	singleRuneMerges := 0
	for _, shippedKeys := range byTrailing {
		if len(shippedKeys) > 1 {
			singleRuneMerges++
		}
	}
	if singleRuneMerges != 0 {
		t.Errorf("(a) single-rune search found %d merges; fsIdentityKey's comment says the effect is multi-rune only", singleRuneMerges)
	}

	// (b) The two unordered pairs named in that comment. A trailing NFD merges each; the
	// shipped key separates them; APFS keeps the files apart. So the pass would be a false
	// positive, not insurance.
	dir := t.TempDir()
	folds := caseFoldingDir(t, dir)
	for _, tc := range [][2]string{
		{"qᾜ.t", "qἨΊ.t"},
		{"qᾔ.t", "qἨΊ.t"},
	} {
		a, b := tc[0], tc[1]
		if withTrailingNFD(a) != withTrailingNFD(b) {
			t.Errorf("a trailing NFD no longer merges %q/%q; the comment's example is stale", a, b)
			continue
		}
		if fsIdentityKey(a) == fsIdentityKey(b) {
			t.Errorf("the shipped key now merges %q/%q too, so the pair no longer shows a difference", a, b)
			continue
		}
		if !folds {
			continue // a case-sensitive filesystem cannot adjudicate
		}
		if filesCollide(t, dir, a, b) {
			t.Errorf("the filesystem DOES collide %q/%q, so merging them would be correct and the comment is wrong", a, b)
		}
	}
}

// TestTtorchRuntimeFileIsIgnored is the false-positive bound on making .ttorch/ a prefix.
//
// The manager writes .ttorch/task into every worker worktree. It was excluded only through
// .git/info/exclude, which is local to a clone and does not travel — so in a fresh clone a
// worker's `git add -A` would stage it, and the .ttorch/ prefix would then refuse the merge as
// a gate-config change, on every task. It is in .gitignore now so it cannot reach a diff.
//
// If someone removes that line, this fails rather than the guard starting to refuse ordinary
// work for a reason nobody would connect to this change.
func TestTtorchRuntimeFileIsIgnored(t *testing.T) {
	root := repoRootForGateConfig(t)
	// Read .gitignore directly rather than asking `git check-ignore`, which would also be
	// satisfied by this clone's local .git/info/exclude — the very thing that misled us.
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == ".ttorch/task" {
			found = true
			break
		}
	}
	if !found {
		t.Error(".gitignore no longer ignores .ttorch/task. The manager writes it into every " +
			"worker worktree, and matchesGateConfig covers .ttorch/ by prefix, so a worker's " +
			"`git add -A` would stage it and the gate would refuse the merge as a gate-config " +
			"change. Restore the line, or narrow the prefix and re-measure.")
	}
	// And the guard really would trip on it, which is why the ignore matters.
	if !matchesGateConfig(".ttorch/task", ttorchScope) {
		t.Error(".ttorch/task is no longer covered by the guard; this test's premise is stale")
	}
}

// TestEveryInstalledContentFileIsCovered walks the REAL embedded payload — the same
// embed.FS the shipped binary carries and installer.desiredFiles walks — and asserts the
// guard covers every file in it.
//
// This is the answer to "derive the content/ subtree list from desiredFiles instead of
// maintaining it by hand". Deriving it at runtime would couple the guard to installer
// internals, so that editing desiredFiles narrows what the guard covers; internal/installer/
// is in the covered set precisely because that mapping is worth attacking. Asserting the
// superset in a test gives the same safety property — a new content/ subtree cannot be
// installed without the guard covering it — and leaves the guard's rule independent of the
// installer's.
//
// The prefix is a superset by construction rather than by luck: content.go embeds
// `all:content` and nothing else, so desiredFiles has no file outside content/ to install.
//
// This test is what a hand-maintained list failed at three times. With "content/skills/" and
// the filename prefix "content/agents/ttorch-reviewer-" it reported 7 covered and 35
// uncovered: all 32 non-reviewer agent definitions, assets/AGENTS.global.md (merged into the
// global AGENTS.md every session on the machine reads), commands/ttorch.md (the /ttorch entry
// point) and hooks/prompt-reminders.sh (runs on every prompt).
func TestEveryInstalledContentFileIsCovered(t *testing.T) {
	var files []string
	if err := fs.WalkDir(ttorchembed.Content(), "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the embedded content tree: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the embedded content tree is empty; this test is asserting nothing")
	}

	var uncovered []string
	for _, f := range files {
		if !matchesGateConfig(f, ttorchScope) {
			uncovered = append(uncovered, f)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("%d of %d embedded content files are not covered by the gate guard.\n"+
			"Every file under content/ is installed into ~/.claude by installer.desiredFiles,\n"+
			"so a landed change to any of them alters what the gate does on its next run.\n"+
			"uncovered:\n  %s", len(uncovered), len(files), strings.Join(uncovered, "\n  "))
	}
}

// TestNoEmbedRootOutsideContent scans EVERY Go file in the repository for a `//go:embed`
// directive and fails on any that embeds a tree other than content/.
//
// This replaces a version that read content.go alone. Its comment claimed it would fail if
// "a second embed directive appears", and for a directive in content.go that was true, but
// the bypass does not put one there:
//
//	payload/content/agents/ttorch-reviewer-security.md   uncovered — the prefix is
//	                                                      "content/", not "*/content/"
//	payload/embed.go  with //go:embed all:content        uncovered
//
// installer.embedRoot is the constant "content", so ANY fs.FS with a top-level content
// directory installs. A second embed root is therefore a second payload, and the covered
// set reaches neither the tree nor the directive. Scanning the whole repo is the check the
// narrow version was assumed to be.
//
// The directive alone is inert — something has to hand the FS to the installer — and
// ApplyEmbedded is what closes that half, with TestInstallerExposesNoFSChoice holding it.
// This test is the other half: it fails on the payload tree rather than on its delivery.
//
// Only a "content" root is a hazard, so this is not a ban on embedding. internal/db embeds
// migrations/*.sql today and is fine: apply walks embedRoot ("content") in whatever FS it
// gets, so an FS rooted at "migrations" has nothing at that path and the walk errors rather
// than installing.
func TestNoEmbedRootOutsideContent(t *testing.T) {
	// Deliberately a filesystem walk and not `git ls-files`. The trusted gate runs this
	// through .ttorch/validate.sh, and on this machine that goes to a build host that gets
	// an rsync WITHOUT .git — where git exits 128. TestGateConfigFilesAreRealPaths takes the
	// t.Skipf escape in that situation, so it silently defends nothing in the lane the gate
	// actually runs. A guard test that skips where the gate runs is not a guard.
	root := repoRootForGateConfig(t)
	var scanned int
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "bin", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		body, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		scanned++
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			rest, ok := strings.CutPrefix(line, "//go:embed ")
			if !ok {
				continue
			}
			for _, pat := range strings.Fields(rest) {
				// Strip the all: / any leading prefix form and take the first path element.
				if _, after, found := strings.Cut(pat, ":"); found {
					pat = after
				}
				top := pat
				if i := strings.IndexByte(top, '/'); i >= 0 {
					top = top[:i]
				}
				// Only a "content" root matters. installer.embedRoot is the constant
				// "content", so fs.WalkDir(theFS, "content") is what installs — an FS rooted
				// anywhere else has no "content" entry at top level and the walk fails
				// instead of installing. internal/db embeds migrations/*.sql on exactly that
				// basis and is not an install channel.
				if top != "content" {
					continue
				}
				if rel != "content.go" {
					t.Errorf("%s embeds the content tree, but only content.go is supposed to.\n"+
						"A second package embedding content/ is a second install payload; the guard\n"+
						"covers content/ and content.go by name and would not see this file.", rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; this test is asserting nothing")
	}
}

// TestEmbeddedPayloadIsNotAssignable fails if the root package exports a variable again.
//
// installer.ApplyEmbedded reads ttorch.Content at call time. While that was
// `var Content embed.FS` it was exported and assignable, so an init() in any package linked
// into the binary could point it at another tree: five lines and one markdown file, none of
// it on a path the guard covers. Unexporting the variable behind an accessor closes it at
// the language level, which is why this test checks for an exported VAR rather than for
// embed directives. Directive spellings are unbounded; assignability is not.
func TestEmbeddedPayloadIsNotAssignable(t *testing.T) {
	root := repoRootForGateConfig(t)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, root, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the root package: %v", err)
	}
	var checked int
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				checked++
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, id := range vs.Names {
						if id.IsExported() {
							t.Errorf("%s declares exported var %s. An exported package-level var is "+
								"assignable from any package linked into the binary, and the embedded "+
								"payload is read at call time by installer.ApplyEmbedded, so an exported "+
								"one can be redirected to another tree with nothing in the diff the gate "+
								"guard matches. Expose a read accessor instead.", filepath.Base(name), id.Name)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no package-level var declarations in the root package; this test is asserting nothing")
	}
}

// TestInstallerExposesNoFSChoice holds the other half: the installer must not let code
// outside its own package choose the tree it installs.
//
// installer.Apply used to take an fs.FS and internal/cli picked it, which made the guard's
// superset argument conditional on a file the gate deliberately does not cover (32 of 196
// commits). Covering internal/cli/ instead would have taken the set to 116/196 = 59%, past
// the more-than-half line that is the stated reason internal/orchestrator/ is not covered
// wholesale — so the fix was to unexport apply and add ApplyEmbedded, which picks
// ttorch.Content inside the covered package. That costs 0 commits.
//
// If an exported function in internal/installer takes an fs.FS again, the parameter is back
// and so is the bypass, so this fails.
func TestInstallerExposesNoFSChoice(t *testing.T) {
	root := repoRootForGateConfig(t)
	dir := filepath.Join(root, "internal", "installer")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing internal/installer: %v", err)
	}
	var checked int
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !fn.Name.IsExported() {
					continue
				}
				checked++
				for _, param := range fn.Type.Params.List {
					sel, ok := param.Type.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != "fs" || sel.Sel.Name != "FS" {
						continue
					}
					t.Errorf("%s: exported %s takes an fs.FS.\n"+
						"That lets uncovered code choose the tree the installer walks into ~/.claude,\n"+
						"which is the bypass ApplyEmbedded exists to close. Keep the FS choice inside\n"+
						"this package, or cover whatever picks it.", filepath.Base(name), fn.Name.Name)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no exported functions in internal/installer; this test is asserting nothing")
	}
}

// gateCostBase is the corpus every cost figure in docs/ARCHITECTURE.md is measured over:
// the non-merge commits reachable from this sha. Pinned so the figures are reproducible as
// history grows.
const gateCostBase = "b642ba6"

// TestGateCostFiguresMatchTheDoc re-measures the cost figures and fails if docs/ARCHITECTURE.md
// disagrees.
//
// The figures are the whole justification for where the covered set stops, and for three
// rounds running a commit updated the main table and left one stale elsewhere — the Rejected
// table, the prose under it, and a "roughly double" claim in the skill that was the only
// stated cost reason for leaving internal/cli/ uncovered, which is where a real bypass sat.
// Transcription is the failure mode, so this measures instead of trusting.
//
// It derives the covered set from the live gateConfigFiles/gateConfigPrefixes/
// gateConfigBasenames rather than restating them, so the table cannot drift from the guard
// either.
//
// Limit worth knowing: it needs the corpus. CI checks out shallow (actions/checkout defaults
// to depth 1), so gateCostBase is unreachable there and this skips; only a full clone
// exercises it. Raising fetch-depth would fix that but changes a covered file.
func TestGateCostFiguresMatchTheDoc(t *testing.T) {
	skipIfShort(t)
	root := repoRootForGateConfig(t)
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		return string(out), err
	}
	if _, err := git("rev-parse", "--verify", gateCostBase+"^{commit}"); err != nil {
		t.Skipf("%s is unreachable (shallow clone?), so the cost corpus is not available: %v", gateCostBase, err)
	}
	log, err := git("log", "--no-merges", "--format=%H", gateCostBase)
	if err != nil {
		t.Fatalf("listing the corpus: %v", err)
	}
	shas := strings.Fields(log)

	touched := make([][]string, 0, len(shas))
	for _, sha := range shas {
		out, err := git("show", "--no-renames", "--name-only", "--format=", sha)
		if err != nil {
			t.Fatalf("reading %s: %v", sha, err)
		}
		touched = append(touched, strings.Split(strings.TrimSpace(out), "\n"))
	}

	// count returns how many commits touch any path the real matcher covers, optionally with
	// extra prefixes layered on top for the rejected rows.
	count := func(extra ...string) int {
		var n int
		for _, files := range touched {
			for _, f := range files {
				if f == "" {
					continue
				}
				hit := matchesGateConfig(f, ttorchScope)
				for _, p := range extra {
					hit = hit || strings.HasPrefix(f, p)
				}
				if hit {
					n++
					break
				}
			}
		}
		return n
	}

	doc, err := os.ReadFile(filepath.Join(root, "docs", "ARCHITECTURE.md"))
	if err != nil {
		t.Fatalf("reading ARCHITECTURE.md: %v", err)
	}
	body := string(doc)

	total := count()
	for _, tc := range []struct {
		what  string
		want  string
		extra []string
	}{
		{"the whole set", fmt.Sprintf("| **the whole set** | **%d / %d = %d%%** | |", total, len(shas), int(math.Round(100*float64(total)/float64(len(shas))))), nil},
		{"+ internal/db/", fmt.Sprintf("| + `internal/db/` | %d | %.1f%% |", count("internal/db/"), 100*float64(count("internal/db/"))/float64(len(shas))), []string{"internal/db/"}},
		{"+ orchestrator and review wholesale", fmt.Sprintf("| + `internal/orchestrator/**` and `internal/review/**` wholesale | %d | %.1f%% |",
			count("internal/orchestrator/", "internal/review/"),
			100*float64(count("internal/orchestrator/", "internal/review/"))/float64(len(shas))), nil},
	} {
		if !strings.Contains(body, tc.want) {
			t.Errorf("docs/ARCHITECTURE.md does not contain the measured row for %s.\nmeasured: %s\nUpdate the doc, or explain the difference — do not adjust this test to match a transcribed figure.", tc.what, tc.want)
		}
	}
	if n := len(shas); n != 196 {
		t.Logf("the corpus is now %d commits, not the 196 the doc's prose cites; the table rows above are measured either way", n)
	}
}

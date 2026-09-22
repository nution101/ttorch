package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
)

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
// gateConfigPrefixes can name whole directories because content/skills/, internal/review/,
// internal/approval/ and internal/validate/ are each small and single-purpose. This package
// is not: it is 39% of the repo's commits, so covering "internal/orchestrator/" wholesale
// would put 54% of all commits behind --allow-gate-change (measured over the 196 non-merge
// commits reachable from b642ba6) and turn the flag into a formality. gateConfigFiles
// therefore names four files instead.
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

	covered := map[string]bool{}
	for _, f := range gateConfigFiles {
		covered[f] = true
	}
	for _, fn := range decidingFunctions {
		file, ok := declaredIn[fn]
		if !ok {
			t.Errorf("%s is listed as gate-deciding but no longer exists in this package; if it was renamed, rename it in decidingFunctions too — do not just drop it", fn)
			continue
		}
		if !covered[file] {
			t.Errorf("%s now lives in %s, which gateConfigFiles does not name: a diff rewriting it would merge without --allow-gate-change. Move it back, or add %s to gateConfigFiles and re-measure the blast radius.", fn, file, file)
		}
	}
}

// absentByDesign are covered entries that do NOT exist in this repo and are not expected to.
// Each is exempt from the dead-coverage check below for a stated reason, so the exemption is
// a decision on the record rather than a quiet hole in it.
var absentByDesign = map[string]string{
	".ttorch/validate.sh": "describes every managed repo, not this one; a repo may not have one yet",
	"AGENTS.md":           "same — the repo-local delivery-mode config",

	// These three are covered PRECISELY BECAUSE they should not exist. Each silently changes
	// what `go test` compiles, which is what .ttorch/validate.sh runs via `make test-fast`,
	// so a worker introducing one substitutes the thing the gate validates against. Their
	// appearance in a diff is the event being guarded, so requiring them to exist would
	// invert the test.
	"go.work":     "auto-discovered via GOWORK; its replace directives override go.mod",
	"go.work.sum": "inert without go.work, covered alongside it so the pair cannot drift",
	"vendor/":     "a consistent vendor/ makes the toolchain build from it instead of the module cache",
}

// TestGateConfigFilesAreRealPaths guards the other direction: an entry that names nothing is
// dead coverage that reads as protection. Entries in absentByDesign are exempt, and the test
// also fails if one of THOSE turns up — an entry justified as "should not exist" that now
// exists needs its reason rechecked, not silently kept.
func TestGateConfigFilesAreRealPaths(t *testing.T) {
	root := repoRootForGateConfig(t)
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(rel, "/"))))
		return err == nil
	}
	for _, f := range gateConfigFiles {
		if _, exempt := absentByDesign[f]; exempt {
			continue
		}
		if !exists(f) {
			t.Errorf("gateConfigFiles names %s, which does not exist. Either it is dead coverage, or it belongs in absentByDesign with a reason.", f)
		}
	}
	for _, p := range gateConfigPrefixes {
		if _, exempt := absentByDesign[p]; exempt {
			continue
		}
		dir := p
		if !strings.HasSuffix(p, "/") { // a filename prefix: check its directory
			dir = path0Dir(p)
		}
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(dir, "/"))))
		if err != nil || !fi.IsDir() {
			t.Errorf("gateConfigPrefixes names %s, whose directory does not exist. Either it is dead coverage, or it belongs in absentByDesign with a reason.", p)
		}
	}
	// Every exemption must still be a covered entry, and the three "should not exist" ones
	// must still not exist.
	covered := map[string]bool{}
	for _, f := range gateConfigFiles {
		covered[f] = true
	}
	for _, p := range gateConfigPrefixes {
		covered[p] = true
	}
	for entry, why := range absentByDesign {
		if !covered[entry] {
			t.Errorf("absentByDesign exempts %s, which is not in the covered set at all", entry)
			continue
		}
		if entry == ".ttorch/validate.sh" || entry == "AGENTS.md" {
			continue // these two may legitimately exist here; they do
		}
		if exists(entry) {
			t.Errorf("%s now exists in this repo, but it is covered on the grounds that it should not (%s). Recheck the reason before keeping the exemption.", entry, why)
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
	covered := map[string]bool{}
	for _, f := range gateConfigFiles {
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
		{"content/skill\ufb06/x.md", "U+FB06 does not appear in 'skills'; must NOT match"},
	} {
		got := matchesGateConfig(tc.path)
		want := tc.path != "content/skill\ufb06/x.md"
		if got != want {
			t.Errorf("%s: matchesGateConfig(%q) = %v, want %v", tc.why, tc.path, got, want)
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

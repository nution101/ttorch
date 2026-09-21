package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

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
	"foldRune", "foldKey", "hostilePath", "collidesInTree", "sanitizePathForMessage",
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

// TestGateConfigFilesAreRealPaths guards the other direction: an entry that names nothing
// is dead coverage that reads as protection. The two repo-local config entries are exempt —
// .ttorch/validate.sh and AGENTS.md describe every managed repo, not this one, and a repo
// legitimately may not have them yet.
func TestGateConfigFilesAreRealPaths(t *testing.T) {
	root := repoRootForGateConfig(t)
	for _, f := range gateConfigFiles {
		if f == ".ttorch/validate.sh" || f == "AGENTS.md" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Errorf("gateConfigFiles names %s, which does not exist: %v", f, err)
		}
	}
	for _, p := range gateConfigPrefixes {
		dir := p
		if !strings.HasSuffix(p, "/") { // a filename prefix: check its directory
			dir = path0Dir(p)
		}
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(dir, "/")))); err != nil || !fi.IsDir() {
			t.Errorf("gateConfigPrefixes names %s, whose directory does not exist: %v", p, err)
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

// TestGateConfigPathsAreASCII pins the precondition that lets foldKey be enough.
//
// foldKey applies Unicode SIMPLE case folding. It does not apply full folding or NFC/NFD
// normalization, and APFS applies both. While every covered path is pure ASCII that gap is
// unreachable: ASCII has no decomposed form and no multi-character fold. The first non-ASCII
// entry makes normalization a live bypass — a composed and a decomposed spelling of the same
// covered path would fold to different keys while the filesystem sees one file.
//
// If this test fails, the entry is not necessarily wrong, but foldKey must normalize (NFC)
// before folding and the limits recorded in validate.go and the docs must be rewritten.
func TestGateConfigPathsAreASCII(t *testing.T) {
	for _, set := range [][]string{gateConfigFiles, gateConfigPrefixes} {
		for _, p := range set {
			for _, r := range p {
				if r > unicode.MaxASCII {
					t.Errorf("gate-config entry %q contains non-ASCII %q: foldKey does not normalize, so a decomposed spelling of this path would evade the guard. Add NFC normalization to foldKey and update the stated limits before adding it.", p, r)
					break
				}
			}
		}
	}
}

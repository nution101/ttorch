package orchestrator

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
	"github.com/nution101/ttorch/internal/worktree"
)

// skipIfShort skips a slow test under `go test -short`, the fast local lane (`make
// test-fast`, run by .ttorch/validate.sh). The orchestrator e2e tests drive real tmux
// windows, git worktrees, rebases and validate runs and dominate the package's wall-clock.
// Skipping them locally is a speed optimization, not a weaker gate: `make test-gate` runs
// the gate's proofs without -short, and the full suite runs in CI. Any new test that
// performs a real Spawn must call this.
//
// It lives here, in a covered file, because two of the gate's own proofs call it:
// TestFSIdentityKeySweep and TestGateCostFiguresMatchTheDoc. It used to live in
// orchestrator_test.go, which the gate does not cover, so a one-line edit there made both
// proofs skip in every lane while the diff auto-merged unflagged. The e2e tests in
// orchestrator_test.go still call it; same package.
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow integration (e2e) test: skipped in the fast local lane (go test -short); the full suite runs in CI")
	}
}

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
	"validateForAuthority", "loadProcessValidate",
	"validateCacheKey", "loadValidateCache", "storeValidateCache",
	// the process-only memo that makes a green an authority, and the key it is stored under
	"gateContentKey", "runAndRecordGate", "storeProcessValidate",
	// the gate-config guard
	"matchesGateConfig", "diffTouchesGateConfig",
	// the approval token: what it grants, how it is read, and when it is re-minted
	"Approve", "approvalPayload", "splitApprovalPayload", "remintFromVerdict",
	// the merge and review decisions themselves
	"MergeLocal", "TrustPrep", "TrustRecord", "carryVerdictForward", "gateCoversRebased",
	// what a verdict must cover and fold, what a re-prep keeps, and the daemon that records
	"requiredDimensions", "derivedFloor", "foldDimensions",
	"archivePriorReports", "carryReportsPastStamp", "gateOnceAt",
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
	"internal/orchestrator/livesignal.go":   "reads a worker's hook record for `ttorch status`; only TaskState calls it",
	"internal/orchestrator/orchestrator.go": "Manager construction, task CRUD and status",
	"internal/orchestrator/overlap.go":      "plans dispatch order from file footprints",
	"internal/orchestrator/spawn.go":        "launches worker sessions",
}

// TestGateConfigCoversTheDecidingCode is the anchor for the file-granular half of
// gateConfigFiles.
//
// gateConfigPrefixes can name whole directories because content/, internal/review/,
// internal/approval/ and internal/validate/ are each small and single-purpose. This package
// is not, and covering "internal/orchestrator/" wholesale is declined on cost; the last row of
// the generated table in docs/ARCHITECTURE.md (gate-cost block) is what that rests on.
// gateConfigFiles therefore names five files instead.
//
// This comment used to restate the figures and drifted twice. They are generated into that
// block now. TestGateCostFiguresMatchTheDoc proves the block matches the matcher. It does not
// prove this comment is free of figures, only that none is written here in a shape
// costFigureFor knows.
//
// A file list over a package that gets refactored decays silently — this package has already
// been re-split once (140d2b91, "split god-file into focused single-responsibility files"),
// and a later split that moved MergeLocal into mergegate.go would drop it out of coverage
// with nothing failing. This test is what makes that loud: move a deciding function to a file
// gateConfigFiles does not name and it fails here, at `make test`, before the guard has
// quietly stopped guarding.
//
// Fixing a failure means either moving the function back, or adding its new file to
// gateConfigFiles and regenerating the gate-cost block, not deleting the entry.
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
	proofs := map[string]bool{}
	for _, n := range proofTestFiles(t) {
		proofs[n] = true
	}
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

// guardSymbols returns the identifiers the gate-config guard is MADE OF, derived from the
// call graph rather than listed.
//
// It starts at diffTouchesGateConfig and matchesGateConfig and takes the transitive closure
// over validate.go's package-level declarations. The enumerated version listed eleven names
// and omitted six real ones, including hostilePath, which is the only thing standing
// between a newline in a committed path and a desynced blob read. A hand list of things
// that must not be deleted is a list that drifts, the same lesson that removed the "24
// deciding functions" count two rounds ago.
//
// Scoped to the guard, not to all of validate.go. That file also holds the gate RUNNER
// (runGate, stagedGreen, validateCommitted), and pulling its tests in would mark
// orchestrator_test.go a proof file and bring it into the covered set, which this branch has
// repeatedly declined.
//
// Limit worth stating: matching is by identifier NAME, not by resolved symbol, so a local
// that shadows a guard name marks its file a proof. One did, a `gateScope` local in a
// splitApprovalPayload test, and it was renamed rather than special-cased. A false positive
// is not safe here: TestOrchestratorFilesAreClassified then demands that the file be covered,
// which is a change to the covered set and to the generated cost block, not a free fix.
func guardSymbols(t *testing.T) map[string]bool {
	t.Helper()
	return guardClosure(t).symbols
}

// guardReach is what the guard's transitive closure touches: the symbols declared in
// validate.go, and the other packages it reads the repository through.
type guardReach struct {
	symbols map[string]bool
	// packages maps an imported package's qualifier to its import path, for the packages
	// the closure actually calls into. internal/worktree is the live one: it holds the
	// blob read and the record framing the scope decision rests on.
	packages map[string]string
}

func guardClosure(t *testing.T) guardReach {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "validate.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing validate.go: %v", err)
	}
	// Qualifier -> import path, for this module's own packages only. A guard that starts
	// reading the repo through a new internal package has moved part of its evidence
	// layer there, and the lane that proves the guard has to follow it.
	imports := map[string]string{}
	for _, im := range f.Imports {
		ip := strings.Trim(im.Path.Value, `"`)
		if !strings.HasPrefix(ip, "github.com/nution101/ttorch/internal/") {
			continue
		}
		name := ip[strings.LastIndex(ip, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = ip
	}
	decls := map[string]ast.Node{}
	// Methods are keyed by RECEIVER TYPE, not by their own name, and they are reached by
	// walking that type rather than by seeing their name. Keying them by name would put
	// `files` and `prefixes` in the symbol set, and a test declaring a local called
	// `files` would be misread as a proof file.
	methods := map[string][]*ast.FuncDecl{}
	for _, d := range f.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			if n.Recv == nil {
				decls[n.Name.Name] = n
				break
			}
			if recv := receiverTypeName(n); recv != "" {
				methods[recv] = append(methods[recv], n)
			}
		case *ast.GenDecl:
			for _, sp := range n.Specs {
				switch v := sp.(type) {
				case *ast.ValueSpec:
					for _, id := range v.Names {
						decls[id.Name] = v
					}
				case *ast.TypeSpec:
					decls[v.Name.Name] = v
				}
			}
		}
	}
	seen := map[string]bool{}
	var walk func(string)
	var walkNode func(ast.Node)
	usedPkgs := map[string]string{}
	walkNode = func(n ast.Node) {
		ast.Inspect(n, func(nd ast.Node) bool {
			if se, ok := nd.(*ast.SelectorExpr); ok {
				if q, ok := se.X.(*ast.Ident); ok {
					if ip, isImport := imports[q.Name]; isImport {
						usedPkgs[q.Name] = ip
					}
				}
			}
			if id, ok := nd.(*ast.Ident); ok {
				if _, isDecl := decls[id.Name]; isDecl {
					walk(id.Name)
				}
			}
			return true
		})
	}
	walk = func(name string) {
		n, ok := decls[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		walkNode(n)
		// Reaching a type reaches its methods. gateScope is a bare struct whose own
		// declaration mentions nothing, while files() and prefixes() are where the four
		// covered-path lists are actually read, so stopping at the type loses them.
		for _, m := range methods[name] {
			walkNode(m)
		}
	}
	for _, entry := range []string{"diffTouchesGateConfig", "matchesGateConfig"} {
		if _, ok := decls[entry]; !ok {
			t.Fatalf("%s is not declared in validate.go; the guard's entry points moved and "+
				"this derivation measures nothing", entry)
		}
		walk(entry)
	}
	if len(seen) < 10 {
		t.Fatalf("the guard closure came back with %d symbols, too few to be real; the "+
			"derivation is broken and every proof file would read as ordinary", len(seen))
	}
	// A canary on the traversal, not a second copy of the covered set. These four ARE the
	// covered-path lists, so a derivation that cannot reach them is not measuring
	// coverage at all: a test file asserting only on those lists would read as ordinary
	// and could be deleted in the merge that widened them. They were unreachable until
	// the method walk above existed, and the suite stayed green throughout. If one is
	// renamed this fatals, which is the point: the rename should be accompanied by
	// someone checking the derivation still reaches the new name.
	for _, list := range []string{"gateConfigFiles", "ttorchSourceFiles", "gateConfigPrefixes", "ttorchSourcePrefixes"} {
		if !seen[list] {
			// Fatal, not Errorf. With Errorf this returns the incomplete symbol set and
			// every caller carries on deriving proof files from it, so one broken
			// traversal reports a second, unrelated-looking failure in each of them.
			t.Fatalf("the guard closure does not reach %s, one of the four lists that "+
				"define the covered set; a test asserting only on it reads as ordinary", list)
		}
	}
	if len(usedPkgs) == 0 {
		t.Error("the guard closure reads no other package, which cannot be right: it reads " +
			"the base tree through internal/worktree. The package walk is broken, and " +
			"TestGateLaneRunsTheEvidencePackages is asserting nothing.")
	}
	return guardReach{symbols: seen, packages: usedPkgs}
}

// receiverTypeName is the bare type name a method hangs off, pointer or not.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch rt := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return rt.Name
	case *ast.StarExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// proofTestFiles returns the _test.go files in this package that reference the guard.
func proofTestFiles(t *testing.T) []string {
	t.Helper()
	syms := guardSymbols(t)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0) // no ParseComments: prose is not a use
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		var hit bool
		ast.Inspect(f, func(nd ast.Node) bool {
			if id, ok := nd.(*ast.Ident); ok && syms[id.Name] {
				hit = true
			}
			return !hit
		})
		if hit {
			out = append(out, n)
		}
	}
	return out
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
	"go.work":        "auto-discovered via GOWORK; its replace directives override go.mod",
	"go.work.sum":    "inert without go.work, covered alongside it so the pair cannot drift",
	"vendor/":        "a consistent vendor/ makes the toolchain build from it instead of the module cache",
	".claude/":       "project-level agent config; a .claude/agents/ttorch-reviewer-*.md would REPLACE a gate reviewer",
	".mcp.json":      "project-level MCP servers for those same sessions",
	".gitattributes": "no attributes are set in this repo; covered so that landing one is the event",
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
	proofs := map[string]bool{}
	for _, n := range proofTestFiles(t) {
		proofs[n] = true
	}
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
		// Still the control, and now deliberately so: internal/cli/ is left uncovered (see
		// the gate-cost block in docs/ARCHITECTURE.md), and the bypass that ran through it
		// is closed by installer.ApplyEmbedded instead.
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
				if _, err := m.Approve(id, time.Minute, false); err != nil {
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
				if _, err := m.Approve(id, time.Minute, true); err != nil {
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
	proofs := map[string]bool{}
	for _, n := range proofTestFiles(t) {
		proofs[n] = true
	}
	covered := map[string]bool{}
	for _, f := range ttorchScope.files() {
		covered[f] = true
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") {
			continue
		}
		rel := "internal/orchestrator/" + n
		if strings.HasSuffix(n, "_test.go") {
			// Parsed, not grepped. A substring scan counts a guard name MENTIONED in a
			// comment, which would have marked orchestrator_test.go a proof file for two
			// passing references in prose, and so demanded that it be covered.
			isProof := proofs[n]
			if isProof && !covered[rel] {
				t.Errorf("%s references the guard but is not covered. A diff can then "+
					"delete the proof in the SAME unflagged merge as the attack it catches, "+
					"which is how TestTtorchRepoIsScopedIn was removable. Add it to "+
					"ttorchSourceFiles.", rel)
			}
			if !isProof && covered[rel] {
				t.Errorf("%s is covered but references none of the guard's symbols, so it is "+
					"costing a flag for nothing. Drop it from ttorchSourceFiles, or add the "+
					"assertion that makes it a proof.", rel)
			}
			continue
		}
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
// superset argument conditional on a file the gate deliberately does not cover. Rather than
// cover internal/cli/, the fix was to unexport apply and add ApplyEmbedded, which picks
// ttorch.Content inside internal/installer, a package that was already covered.
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

// gateCostBase is the corpus the generated cost block in docs/ARCHITECTURE.md is measured over:
// the non-merge commits reachable from this sha. Pinned so the figures are reproducible as
// history grows.
const gateCostBase = "b642ba6"

// TestGateCostFiguresMatchTheDoc generates the cost table and fails if the gate-cost block in
// docs/ARCHITECTURE.md differs from it, then looks for hand-written figures elsewhere.
//
// The figures are the justification for where the covered set stops, and for three rounds
// running a commit updated the main table and left one stale elsewhere: the Rejected table,
// the prose under it, and a "roughly double" claim in the skill that was the only stated
// cost reason for leaving internal/cli/ uncovered, which is where a real bypass sat.
// Transcription was the failure mode, so the table is generated instead of maintained.
//
// It reads the covered set from the live lists rather than restating them, so the table
// cannot drift from the guard either.
//
// Limit worth knowing: it needs the corpus. It skips when gateCostBase is unreachable, which
// includes CI's depth-1 checkout and any copy of the tree without its git history. Raising
// fetch-depth would fix CI but changes a covered file.
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

	// Both columns come from the REAL matcher, by changing what it is given rather than by
	// reimplementing what it does. "alone" empties the covered lists down to one entry;
	// "marginal" removes that one entry and re-counts. A second implementation of the
	// matching rules would be a second thing to keep in step, which is the whole disease
	// being treated here.
	restore := func(a, b, c, d, e []string) {
		gateConfigFiles, ttorchSourceFiles = a, b
		gateConfigPrefixes, ttorchSourcePrefixes = c, d
		gateConfigBasenames = e
	}
	withLists := func(keep func([]string) []string, f func()) {
		a, b, c, d, e := gateConfigFiles, ttorchSourceFiles, gateConfigPrefixes, ttorchSourcePrefixes, gateConfigBasenames
		defer restore(a, b, c, d, e)
		gateConfigFiles, ttorchSourceFiles = keep(a), keep(b)
		gateConfigPrefixes, ttorchSourcePrefixes = keep(c), keep(d)
		gateConfigBasenames = keep(e)
		f()
	}
	alone := func(entry string) int {
		var n int
		withLists(func(xs []string) []string {
			for _, x := range xs {
				if x == entry {
					return []string{entry}
				}
			}
			return nil
		}, func() { n = count() })
		return n
	}
	without := func(entry string) int {
		var n int
		withLists(func(xs []string) []string {
			out := make([]string, 0, len(xs))
			for _, x := range xs {
				if x != entry {
					out = append(out, x)
				}
			}
			return out
		}, func() { n = count() })
		return n
	}

	total := count()
	entries := append([]string{}, ttorchScope.files()...)
	entries = append(entries, ttorchScope.prefixes()...)
	entries = append(entries, gateConfigBasenames...)
	sort.Strings(entries)

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", costBlockOpen)
	fmt.Fprintf(&b, "Measured over the %d non-merge commits reachable from `%s`, counting a commit\n", len(shas), gateCostBase)
	fmt.Fprint(&b, "once if it touches any covered path.\n\n")
	fmt.Fprint(&b, "| Covered entry | commits touching it | marginal, given the rest |\n|---|---|---|\n")
	seenRow := map[string]bool{}
	for _, e := range entries {
		if seenRow[e] {
			continue
		}
		seenRow[e] = true
		fmt.Fprintf(&b, "| `%s` | %d | +%d |\n", e, alone(e), total-without(e))
	}
	fmt.Fprintf(&b, "| **the whole set** | **%d** | **%.1f%% of the corpus** |\n\n", total, 100*float64(total)/float64(len(shas)))
	fmt.Fprint(&b, "Rejected, measured against that same set:\n\n")
	fmt.Fprint(&b, "| Rejected | commits touching it | set would become | share |\n|---|---|---|---|\n")
	rejected := append([]string{}, readFence(t, notCoveredFence)...)
	sort.Strings(rejected)
	for _, r := range rejected {
		with := count(r)
		fmt.Fprintf(&b, "| + `%s` | %d | %d | %.1f%% |\n", r, own(r, touched), with, 100*float64(with)/float64(len(shas)))
	}
	wholesale := count("internal/orchestrator/", "internal/review/")
	fmt.Fprintf(&b, "| + `internal/orchestrator/**` and `internal/review/**` wholesale | %d | %d | %.1f%% |\n",
		own("internal/orchestrator/", touched), wholesale, 100*float64(wholesale)/float64(len(shas)))
	fmt.Fprintf(&b, "\n%s", costBlockClose)
	want := b.String()

	got, err := costBlockOf(body)
	if err != nil {
		t.Fatalf("docs/ARCHITECTURE.md: %v", err)
	}
	if got != want {
		out := filepath.Join(t.TempDir(), "gate-cost-block.md")
		_ = os.WriteFile(out, []byte(want), 0o644)
		t.Errorf("the generated cost block in docs/ARCHITECTURE.md is out of date.\n"+
			"Replace everything between the markers with the block written to %s.\n\n%s", out, want)
	}

	if n := len(shas); n != 196 {
		t.Logf("the corpus is now %d commits; every figure above is measured from it, so nothing needs editing", n)
	}

	// The previous version swept for figures that matched a measured value. It knew two
	// shapes, N/196 and N%, so bare commit counts went unchecked and five marginals in this
	// table were wrong while it passed.
	//
	// This one inverts the question: outside the block, a figure in one of the shapes
	// costFigureFor knows is a failure whether or not the number is right. It still knows
	// only those shapes, in the files assertNoHandWrittenFigures reads, and three of them
	// only on one line. A count split from its noun across lines, separated from it by other
	// words, spelled out, or replaced by a comparison gets past it. The rule is wider than the check, and review holds the
	// rest.
	//
	// Strips the block that is IN the file, not the one just generated. When the two
	// differ the block is already reported as out of date, and stripping the expected text
	// would leave the actual rows in place to be re-reported one per figure.
	assertNoHandWrittenFigures(t, root, got, len(shas))
}

// costBlockOpen and costBlockClose delimit the generated cost block.
const (
	costBlockOpen  = "<!-- gate-cost: generated by TestGateCostFiguresMatchTheDoc. Do not edit by hand. -->"
	costBlockClose = "<!-- /gate-cost -->"
)

// costBlockOf returns the generated block, markers included.
func costBlockOf(body string) (string, error) {
	i := strings.Index(body, costBlockOpen)
	if i < 0 {
		return "", fmt.Errorf("the generated cost block is gone; it is the only place a cost figure may live, so removing it does not remove the requirement")
	}
	j := strings.Index(body[i:], costBlockClose)
	if j < 0 {
		return "", fmt.Errorf("the generated cost block has an opening marker and no closing one")
	}
	return body[i : i+j+len(costBlockClose)], nil
}

// own counts commits in the corpus that touch any path at or under p. It is the "alone"
// column for a path that is NOT in the covered set, where emptying the lists down to it is
// not available.
func own(p string, touched [][]string) int {
	var n int
	for _, files := range touched {
		for _, f := range files {
			if f != "" && (f == p || strings.HasPrefix(f, p)) {
				n++
				break
			}
		}
	}
	return n
}

// costFigureFor matches five shapes of a cost figure, not a known value: a number over the
// corpus size, a number "of" the corpus size (optionally "of the"), a percentage, a number
// followed by a single space and "commit", "commits" or "marginal", and a parenthesised
// commit count. The fraction is found across a line break on either side of the slash,
// because \s* matches a newline, and a percentage split inside the number still trips on its
// trailing digits. The other three shapes need the number and its word on one line.
//
// That is what it catches, and it is less than the rule it serves. It misses a figure split
// across a line break in those three shapes ("cost 0" then "commits"), a number with other words
// between it and its noun ("0 additional commits"), a number written as a word, a fraction
// over any denominator other than the corpus size, and a cost stated as a comparison with
// no number at all ("the cheaper of the two"). All but the other-denominator case have
// occurred in this repo and got past it. A green run means none of the five shapes is present, not that no figure is.
//
// The denominator is bound to the corpus size because "(1/2/101/...)" in a comment about ssh
// exit codes is not a cost figure and rewording that sentence would make it worse. The
// corpus is commits reachable from a fixed base, so that size does not drift.
func costFigureFor(corpus int) *regexp.Regexp {
	n := fmt.Sprint(corpus)
	return regexp.MustCompile(`\d+\s*/\s*` + n + `|\d+ of (the )?` + n + `|[+-]?\d+(\.\d+)?%|[+-]?\d+ (commits?|marginal)|\(\d+ commits?\)`)
}

// assertNoHandWrittenFigures fails on text matching costFigureFor outside the generated block.
//
// It reads docs/ARCHITECTURE.md, the reviewer skill, and the comments of the .go files in the
// covered set, derived from the covered set itself. Go files are read as comments only,
// through the parser, so a format verb or a real numeric constant is not mistaken for prose.
// Anything else in the repository is not read. See costFigureFor for the shapes it misses.
func assertNoHandWrittenFigures(t *testing.T, root, block string, corpus int) {
	t.Helper()
	costFigure := costFigureFor(corpus)
	check := func(name, text string) {
		for _, m := range costFigure.FindAllString(text, -1) {
			t.Errorf("%s writes the cost figure %q by hand. Figures live in the generated "+
				"block in docs/ARCHITECTURE.md and nowhere else: delete it, or keep the "+
				"argument and let the block carry the number.", name, strings.TrimSpace(m))
		}
	}
	for _, rel := range []string{"docs/ARCHITECTURE.md", "content/skills/ttorch-review/SKILL.md"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		check(rel, strings.Replace(string(b), block, "", 1))
	}
	fset := token.NewFileSet()
	for _, rel := range coveredGoFiles(t) {
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}
		for _, cg := range f.Comments {
			check(rel, cg.Text())
		}
	}
}

// coveredGoFiles are the .go files the gate covers, derived from the covered set itself so
// that a newly covered file is swept without anyone remembering to add it here.
func coveredGoFiles(t *testing.T) []string {
	t.Helper()
	root := repoRootForGateConfig(t)
	var out []string
	for _, f := range ttorchScope.files() {
		if strings.HasSuffix(f, ".go") {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err == nil {
				out = append(out, f)
			}
		}
	}
	for _, p := range ttorchScope.prefixes() {
		dir := filepath.Join(root, filepath.FromSlash(p))
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil {
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	if len(out) == 0 {
		t.Fatal("no covered .go files found; the figure sweep is reading nothing")
	}
	sort.Strings(out)
	return out
}

// notCoveredFence is the language tag on the machine-readable block in the reviewer skill.
const notCoveredFence = "gate-not-covered"

// ttorchSourceFence is the block listing entries covered ONLY in ttorch's own repository.
const ttorchSourceFence = "gate-ttorch-source-only"

// readFence extracts a fenced block's non-empty lines from the reviewer skill.
func readFence(t *testing.T, tag string) []string {
	t.Helper()
	path := filepath.Join(repoRootForGateConfig(t), "content", "skills", "ttorch-review", "SKILL.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the reviewer skill: %v", err)
	}
	text := string(b)
	i := strings.Index(text, "```"+tag)
	if i < 0 {
		t.Fatalf("%s has no ```%s block. These lists must stay machine-readable so the "+
			"honesty check stays wired to them; a missing fence is how a check gets unwired "+
			"without anyone deciding to unwire it.", path, tag)
	}
	rest := text[i+len(tag)+3:]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatalf("the ```%s block in %s is not closed", tag, path)
	}
	var out []string
	for _, line := range strings.Split(rest[:j], "\n") {
		if tok := strings.TrimSpace(line); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// TestTtorchSourceListIsHonest reconciles the published tier split against the matcher.
//
// The reviewer skill INSTALLS into ~/.claude and instructs every reviewer in every gated
// repo, and it listed content/** and the internal/** prefixes flat, with no hint that they
// apply only to ttorch's own source. A reviewer in a Hugo repo reading it would expect
// content/ to be gate config. docs/ARCHITECTURE.md had the split right, which is exactly
// the shape of drift that keeps recurring: the accurate copy is not the installed one.
//
// Each entry must be covered in ttorch's scope AND uncovered outside it. Checking only the
// first half is what let the old honesty test miss this entirely.
func TestTtorchSourceListIsHonest(t *testing.T) {
	entries := readFence(t, ttorchSourceFence)
	if len(entries) == 0 {
		t.Fatal("the ttorch-source list is empty; the tier split exists, so an empty list is false")
	}
	elsewhere := gateScope{}
	for _, e := range entries {
		probe := e
		if strings.HasSuffix(e, "/") {
			probe = e + "probe.go"
		}
		if !matchesGateConfig(probe, ttorchScope) {
			t.Errorf("%s is published as ttorch-source coverage but is not covered even here", e)
		}
		if matchesGateConfig(probe, elsewhere) {
			t.Errorf("%s is published as ttorch-source ONLY, but it is covered in every repo "+
				"too. Either move it to the universal list in the skill, or narrow the set: "+
				"as written the skill understates what a user's repo is gated on.", e)
		}
	}
}

// TestNotCoveredListIsHonest reconciles the published "not covered" list against the live
// matcher, and it is deliberately SELF-CONTAINED: it reads this tree and nothing else.
//
// An earlier version of this idea, on a sibling branch, resolved a cross-branch ref and
// t.Fatalf'd when it did not resolve. That hard-fails in any fresh clone and in CI, because
// actions/checkout fetches only the pushed ref, and CI is the one environment that runs the
// non-short lane. It also shipped a TTORCH_GATE_RECONCILE_REF=none escape, which disables the
// check exactly where it is the only thing running. A check that cannot run in CI, or that
// can be switched off there, is not a check. There is no ref and no env var here.
//
// The direction that matters is claiming something is uncovered when it is covered: that
// understates the guard and sends a reviewer looking for a hole that is already closed. The
// reverse, a covered path missing from the list, is not detectable from the list alone and is
// not claimed to be.
func TestNotCoveredListIsHonest(t *testing.T) {
	entries := readFence(t, notCoveredFence)
	// Non-empty on purpose. internal/cli/ and internal/db/ are deliberate, measured
	// exclusions (see the gate-cost block in docs/ARCHITECTURE.md), so an empty list here
	// would be false rather than an achievement.
	if len(entries) == 0 {
		t.Fatal("the not-covered list is empty. internal/cli/ and internal/db/ are deliberate " +
			"exclusions, so an empty list is a false claim rather than a finished job.")
	}
	for _, e := range entries {
		probe := e
		if strings.HasSuffix(e, "/") {
			probe = e + "probe.go"
		}
		if matchesGateConfig(probe, ttorchScope) {
			t.Errorf("%s is published as NOT covered, but the matcher covers it. Either the "+
				"guard widened and this list was not updated, or the entry was wrong when "+
				"written. Fix the list; do not relax this test.", e)
		}
	}
}

// TestMatchesGateConfig_CoveredDirectoryOwnPath exercises the own-path branch of
// matchesGateConfig directly, with no symlink anywhere in the test.
//
// This exists because the branch was VACUOUSLY covered. Deleting it entirely left the whole
// internal/orchestrator suite green: every test that reached an own-path match did so
// through a symlink fixture, and linksOverGateConfig's ancestor-prefix check catches those
// same fixtures by a different mechanism, since a prefix trivially matches itself. The tests
// credited one mechanism and were held up by another, so the next person to simplify the
// branch away would have lost the own-path match with nothing going red.
//
// Deliberately a unit assertion rather than an end-to-end one. A bare directory path reaches
// a diff only when the entry IS a link, so any end-to-end fixture reintroduces the symlink
// that made the coverage vacuous in the first place. Calling the matcher is the only way to
// isolate this branch.
func TestMatchesGateConfig_CoveredDirectoryOwnPath(t *testing.T) {
	for _, p := range ttorchScope.prefixes() {
		own := strings.TrimSuffix(p, "/")
		if own == p {
			continue // a filename prefix, not a directory
		}
		if !matchesGateConfig(own, ttorchScope) {
			t.Errorf("matchesGateConfig(%q) = false. The covered directory's OWN path must "+
				"match: a symlink standing in its place reports exactly this string, and "+
				"without it the .claude -> payload attack matched nothing at all.", own)
		}
		// The bound that stops the fix becoming a prefix-trim: a longer name sharing the
		// covered directory's prefix must NOT match.
		if near := own + "ious"; matchesGateConfig(near, ttorchScope) {
			t.Errorf("matchesGateConfig(%q) = true. Matching the own path must compare whole "+
				"paths; trimming the trailing slash off the prefix would pull in every name "+
				"that merely starts the same.", near)
		}
	}
	// The same, stated as the concrete case from the round-10 report so a reader can find it.
	if !matchesGateConfig(".claude", ttorchScope) {
		t.Error("matchesGateConfig(\".claude\") must be true; this is the exact call that " +
			"returned false and let a symlink replace the project-level security reviewer")
	}
}

// TestGateTestsSelectorCoversTheProofs asserts the Makefile's GATE_TESTS regex selects every
// test in the gate's proof files.
//
// .ttorch/validate.sh runs `make test-gate` because the fast lane skips the end-to-end
// attacks. That only helps for tests the selector actually names, and it silently did not
// name three: TestMergeLocal_GateConfigChangeRefusedWithoutAllowGateChange,
// TestMergeLocal_AllowGateChangeAuthorizesGateConfigChange and
// TestMergeLocal_GateInstructionChangeNeedsAllowGateChange, the last being the one that
// proves the reviewer skill itself needs --allow-gate-change. They are skipped by -short
// too, so they ran in neither local lane.
//
// A hand-maintained regex drifts the moment someone adds a test, which is exactly what
// happened. This derives the expected set the same way the covered set derives its proof
// files, so a new test in a proof file must be selected or this fails.
func TestGateTestsSelectorCoversTheProofs(t *testing.T) {
	root := repoRootForGateConfig(t)
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^GATE_TESTS = '(.*)'$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("the Makefile has no GATE_TESTS assignment; .ttorch/validate.sh runs " +
			"`make test-gate` against it, so losing it silently unruns every gate proof")
	}
	// Model make's expansion before compiling. The Makefile must write $$ for a literal $,
	// and reading the raw text instead of the expanded value is how this check passed while
	// the recipe was broken: an earlier per-name form "^A$|^B$" contained "$|", which is
	// make's order-only-prerequisite automatic variable, so make silently removed both the
	// $ and the | and handed go test "^A^B". The regex stayed valid and matched nothing.
	raw := strings.ReplaceAll(string(m[1]), "$$", "$")
	if strings.Contains(strings.ReplaceAll(raw, "$", ""), "$") {
		t.Fatalf("GATE_TESTS still contains an unescaped $: %q", m[1])
	}
	sel, err := regexp.Compile(raw)
	if err != nil {
		t.Fatalf("GATE_TESTS is not a valid regex after make expansion: %v", err)
	}
	// It must actually anchor. An unanchored selector lets TestFoo pull in TestFooBar and,
	// worse, a typo match nothing while still compiling.
	if !strings.HasPrefix(raw, "^") || !strings.HasSuffix(raw, "$") {
		t.Errorf("GATE_TESTS must be anchored at both ends, got %q", raw)
	}

	// Derived from the proof files that EXIST, not from a fixed list of three. The previous
	// version scanned a hardcoded file list, so a proof added in a NEW file was required to
	// appear nowhere and ran in neither gating lane: verified both ways, the same proof
	// inside gateconfig_test.go went red and in a new file it passed.
	decl := regexp.MustCompile(`(?m)^func (Test\w+)`)
	var proofs []string
	for _, f := range proofTestFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for _, mm := range decl.FindAllStringSubmatch(string(b), -1) {
			proofs = append(proofs, mm[1])
		}
	}
	if len(proofs) == 0 {
		t.Fatal("found no proof tests; this check is asserting nothing")
	}
	for _, name := range proofs {
		if !sel.MatchString(name) {
			t.Errorf("GATE_TESTS does not select %s, so `make test-gate` does not run it and "+
				"-short skips it: it executes in neither lane that gates a merge. Add it to "+
				"the selector.", name)
		}
	}

	// Coverage alone leaves the selector hand-maintained, and the other half of
	// hand-maintained is a name that selects nothing: a typo, or a test that was renamed
	// or deleted, both of which compile and quietly shrink the lane. So the selector is
	// GENERATED from the proof files and compared byte for byte. The failure prints the
	// line to paste, which is why this stays a check rather than a code generator: the
	// Makefile is covered ground, and a test that rewrote it would be a test that can
	// edit the gate.
	sort.Strings(proofs)
	want := "^(" + strings.Join(proofs, "|") + ")$"
	if raw != want {
		t.Errorf("GATE_TESTS is not the derived set. Replace the Makefile line with:\n\nGATE_TESTS = '%s'\n",
			strings.ReplaceAll(want, "$", "$$"))
	}
}

// onTopicTestName matches a test that is about the gate BY NAME.
//
// This is the sweep that 93ea45a dropped. proofTestFiles derives proof-ness from whether a
// file uses the guard's identifiers, which is the right primary signal, but it misses a
// genuine proof that asserts only through merge behaviour: a test that drives mergeLocal
// with --allow-gate-change touches no guard symbol and reads as ordinary. One such test was
// written, in orchestrator_test.go, and nothing swept it.
//
// orchestrator_test.go stays uncovered, so the fix is not to widen the covered set. It is to
// refuse to let an on-topic test live somewhere no sweep reaches, and say where it should go
// instead.
//
// Grant and ApprovalPayload cover the --allow-gate-change grant and the token it is recorded
// in. Those tests prove what an approval authorizes, and three of them were written outside a
// proof file and ran in no gate lane because no keyword here named them. Approve and Approval
// are deliberately not keywords: they name ordinary merge and land tests too.
var onTopicTestName = regexp.MustCompile(`GateConfig|GateGuard|GateScope|GateCost|GateTests|AllowGateChange|GateInstruction|MatchesGateConfig|Grant|ApprovalPayload`)

// TestOnTopicTestsLiveInAProofFile fails when a test named for the gate sits in a file the
// proof derivation does not reach.
//
// What it still does NOT catch, stated rather than implied: a test that is about the gate,
// uses none of its identifiers, AND is named for none of it. Nothing mechanical can see
// that one. Naming is the only handle left once the symbol signal is gone, so the pattern
// is deliberately broad — a false positive costs moving a test into a file that is already
// covered, which is free, while a false negative costs a deletable proof.
func TestOnTopicTestsLiveInAProofFile(t *testing.T) {
	proofs := map[string]bool{}
	for _, n := range proofTestFiles(t) {
		proofs[n] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var swept int
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if !onTopicTestName.MatchString(fn.Name.Name) {
				continue
			}
			swept++
			if !proofs[n] {
				t.Errorf("%s is named for the gate but lives in %s, which no sweep covers: "+
					"it is not selected by GATE_TESTS and it can be deleted in the same "+
					"unflagged merge as the attack it catches. Move it into a proof file "+
					"(gateconfig_test.go or gateattacks_test.go).", fn.Name.Name, n)
			}
		}
	}
	if swept == 0 {
		t.Fatal("the on-topic sweep matched no test at all; the pattern is broken and this " +
			"check is asserting nothing")
	}
}

// TestGateLaneRunsTheEvidencePackages is the "runs in the gate's lane" rule one level out.
//
// make test-gate ran ./internal/orchestrator/ only. The blob read and the record framing
// the scope decision rests on moved to internal/worktree in 0f73e17, and its proofs then
// executed only because they happen to carry no -short skip, which is luck rather than a
// guarantee. Rather than name that package in the Makefile and call it done, the packages
// the guard reads the repository through are DERIVED from its own call closure, so the
// next time part of the evidence layer moves to a new package the lane has to follow it.
//
// Those packages run UNFILTERED. A -run selector aimed at another package's test names
// matches nothing and still exits 0, which is the vacuous-pass shape this branch has hit
// twice, so the check refuses a filtered invocation.
func TestGateLaneRunsTheEvidencePackages(t *testing.T) {
	root := repoRootForGateConfig(t)
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	m := regexp.MustCompile(`(?ms)^test-gate:\n(\t.*?)(?:\n[^\t\n]|\z)`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("the Makefile has no test-gate recipe; .ttorch/validate.sh runs it as the " +
			"third step of the trusted gate")
	}
	recipe := strings.Split(string(m[1]), "\n")
	// Generating GATE_TESTS proves nothing if the recipe stops reading it. A recipe
	// changed to a different -run value leaves the derivation green while the lane runs
	// something else, which is the same vacuous-pass shape one level out.
	if !strings.Contains(string(m[1]), "-run $(GATE_TESTS) ./internal/orchestrator/") {
		t.Errorf("test-gate must run ./internal/orchestrator/ under -run $(GATE_TESTS); "+
			"GATE_TESTS is generated and checked, so a recipe that does not use it runs "+
			"an unchecked selection. Recipe:\n%s", m[1])
	}
	for _, ip := range guardClosure(t).packages {
		pkg := "./" + ip[strings.Index(ip, "internal/"):] + "/"
		var found bool
		for _, line := range recipe {
			if !strings.Contains(line, pkg) {
				continue
			}
			found = true
			if strings.Contains(line, "-run") {
				t.Errorf("test-gate runs %s behind a -run selector aimed at another "+
					"package's tests, which matches nothing and exits 0. Run it "+
					"unfiltered.", pkg)
			}
		}
		if !found {
			t.Errorf("the guard reads the repository through %s, so that package holds part "+
				"of the evidence the gate rests on, but `make test-gate` does not run it. "+
				"Its proofs execute in neither lane that gates a merge.", pkg)
		}
	}
}

// Moved here from orchestrator_test.go. These are the gate's own proofs, and a proof
// that is not covered can be deleted in the same unflagged merge as the attack it
// catches. Rather than cover orchestrator_test.go where they used to live, they moved into
// a file that was already covered.

// The case-fold cases used to pin a KNOWN LIMIT here (the match was byte-exact, so a
// differently-cased path naming a covered file slipped through). They now pin the behaviour:
// both sides of every comparison are folded, including the "AGENTS.md" literal, which is the
// side a half-fix leaves behind. The boundary cases below hold the fold to what it is for —
// a differently-spelled name for the SAME file — not a licence to match anything adjacent.
func TestMatchesGateConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"the validate script", ".ttorch/validate.sh", true},
		{"the learnings ledger, which writes into AGENTS.md", ".ttorch/learnings.jsonl", true},
		{"anything else under .ttorch/", ".ttorch/offload/run.sh", true},
		{"the published shell installer", "docs/install.sh", true},
		{"the published powershell installer", "docs/install.ps1", true},
		{"the delivery-mode config", "AGENTS.md", true},
		{"the symlink to it that every session loads", "CLAUDE.md", true},
		{"the gate procedure skill", "content/skills/ttorch-review/SKILL.md", true},
		{"the manager skill", "content/skills/ttorch-manager/SKILL.md", true},
		{"the validate skill", "content/skills/ttorch-validate/SKILL.md", true},
		{"a reviewer definition", "content/agents/ttorch-reviewer-security.md", true},
		{"another reviewer definition", "content/agents/ttorch-reviewer-scope.md", true},
		// Every file under content/ is installed into ~/.claude by installer.desiredFiles,
		// which walks the tree rather than working from a list. These four were pinned as
		// NOT covered on the argument that the gate dispatches only the reviewers; that was
		// the wrong criterion, and it left 35 of the 42 embedded files open.
		{"the worker agent definition", "content/agents/ttorch-worker.md", true},
		{"a curated agent profile", "content/agents/golang-pro.md", true},
		{"the /ttorch entry point", "content/commands/ttorch.md", true},
		{"the global AGENTS.md source", "content/assets/AGENTS.global.md", true},
		{"the hook that runs on every prompt", "content/hooks/prompt-reminders.sh", true},

		// The deciding Go code. The four orchestrator files are named exactly; the three
		// supporting packages are covered wholesale because each is small and single-purpose.
		{"the gate implementation", "internal/orchestrator/gate.go", true},
		{"the merge gate", "internal/orchestrator/merge.go", true},
		{"the gate-config guard itself", "internal/orchestrator/validate.go", true},
		{"the validate cache", "internal/orchestrator/validatecache.go", true},
		{"the findings contract", "internal/review/review.go", true},
		{"the reviewer-set classifier", "internal/review/size.go", true},
		{"the approval token", "internal/approval/approval.go", true},
		{"the check runner", "internal/validate/validate.go", true},
		{"the delivery-mode parser", "internal/projectinit/projectinit.go", true},
		{"a workspace file", "go.work", true},
		{"a vendored dependency", "vendor/example.com/dep/d.go", true},
		{"the module file", "go.mod", true},
		{"the module checksums", "go.sum", true},
		{"a project-level reviewer definition", ".claude/agents/ttorch-reviewer-security.md", true},
		{"project-level MCP servers", ".mcp.json", true},
		{"a nested instruction file", "internal/orchestrator/CLAUDE.md", true},
		{"a nested delivery-mode file", "internal/AGENTS.md", true},
		// Reclassified deliberately: a session reading files under docs/ loads this, so it is
		// an instruction file, not a copy of one. An earlier table pinned it as a near-miss.
		{"an instruction file under docs", "docs/AGENTS.md", true},
		{"the full-suite CI authority", ".github/workflows/ci.yml", true},

		// Case-folding: both sides are folded, so a differently-cased spelling of a covered
		// file is caught. "AGENTS.md" is the entry a one-sided fold would miss.
		{"the mode config, lowercased", "agents.md", true},
		{"the mode config, mixed case", "Agents.Md", true},
		{"a skill path, mixed case", "Content/Skills/ttorch-review/SKILL.md", true},
		{"a reviewer definition, mixed case", "Content/Agents/TTorch-Reviewer-Security.md", true},
		{"the validate script, mixed case", ".TTorch/Validate.SH", true},
		{"a deciding go file, mixed case", "Internal/Orchestrator/Gate.go", true},

		// Unicode case FOLDING, not simple lowercasing. A case-insensitive filesystem folds;
		// strings.ToLower does not, and these are the spellings that exploited the gap.
		{"U+017F long s for AGENTS.md", "agent\u017f.md", true},
		{"U+017F long s in a covered prefix", "content/skill\u017f/x.md", true},
		{"U+017F in the validate script", ".ttorch/validate.\u017fh", true},
		{"U+212A under the content tree", "content/\u212Askills/x.md", true},
		{"U+212A where a k is covered", "content/agents/ttorch-reviewer-\u212A.md", true},
		// U+212A inside a prefix that is still narrow, so the fold is doing real work here
		// rather than being subsumed by "content/". Both of these used to be carried by the
		// content/skills/ entry, which no longer distinguishes them.
		{"U+212A in a narrow covered prefix", ".github/wor\u212Aflows/ci.yml", true},
		{"U+212A must not widen past that prefix", ".github/wor\u212Aflow/ci.yml", false},

		{"a file merely mentioning agents", "docs/AGENTS-guide.md", false},
		{"a directory that merely starts like .claude", ".claude-backup/x.md", false},
		{"a validate script somewhere else", "sub/.ttorch/validate.sh", false},
		{"a ledger somewhere else", "sub/.ttorch/learnings.jsonl", false},
		{"a directory that merely starts like .ttorch", ".ttorch-backup/x.sh", false},
		{"ordinary docs prose", "docs/ONBOARDING.md", false},
		{"a doc that merely mentions install", "docs/installing.md", false},
		{"a directory that merely starts the same", "contents/skills/x.md", false},
		{"ordinary orchestrator source", "internal/orchestrator/spawn.go", false},
		{"the land queue", "internal/orchestrator/landqueue.go", false},
		{"ordinary source elsewhere", "internal/cli/cli.go", false},
		{"a non-workflow github file", ".github/CODEOWNERS", false},
		{"a directory that merely starts like vendor", "vendored/x.go", false},
		{"a package that merely starts the same", "internal/reviewer/x.go", false},

		// Folding must not widen the match past a same-file respelling.
		{"a mixed-case near-miss", "Contents/Skills/x.md", false},
		{"a mixed-case nested instruction file", "Docs/Agents.md", true},
	} {
		if got := matchesGateConfig(tc.path, ttorchScope); got != tc.want {
			t.Errorf("%s: matchesGateConfig(%q, ttorchScope) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

// TestMergeLocal_GateConfigChangeRefusedWithoutAllowGateChange: a worker's diff that touches
// the gate definition (.ttorch/validate.sh) must NOT merge in trusted mode on an auto-mint
// (changing the gate is never autonomous) NOR on a plain human approval. Before the
// allow-gate-change scope existed, a human token skipped diffTouchesGateConfig outright and
// this merge succeeded with nothing in the audit log naming the file; the second half of this
// test is the red proof for that. The scope does not make the bypass unreachable — a process
// running as the lead can still write the token file with the marker in it — it makes an
// ordinary approval stop authorizing a gate change by accident.
func TestMergeLocal_GateConfigChangeRefusedWithoutAllowGateChange(t *testing.T) {
	m, repo := deliveryHarness(t, "gateconfig")
	commitGateScript(t, repo, "exit 0") // a passing default-branch gate
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("gc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// The worker's diff changes the gate definition itself.
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "validate.sh"), []byte("exit 0\n# tweaked by worker\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "edit gate")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("gc1"), head, nil)
	if _, err := m.TrustRecord("gc1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A diff touching the gate definition must NOT auto-approve.
	if approval.Valid(m.P.ApprovalFile("gc1")) {
		t.Fatal("a diff touching the gate definition must not auto-approve in trusted mode")
	}
	if reloaded, _, _ := m.Store.GetTask(context.Background(), "gc1"); reloaded.ApprovedBy != "" {
		t.Fatalf("a gate-config change must not record an auto approver: %+v", reloaded)
	}
	// A PLAIN human approval does not authorize a gate change either: the check still runs,
	// the refusal names the offending file, and nothing merges.
	if _, err := m.Approve("gc1", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defHead := gitIn(t, repo, "rev-parse", "HEAD")
	_, err = m.MergeLocal("gc1", false)
	if err == nil {
		t.Fatal("a plain human approval must not authorize a gate-definition change")
	}
	if !strings.Contains(err.Error(), ".ttorch/validate.sh") {
		t.Fatalf("the refusal must name the offending gate-definition file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--allow-gate-change") {
		t.Fatalf("the refusal must say which explicit approval is needed, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the gate-definition change must not have merged")
	}
	_, _ = m.Teardown("gc1", true)
}

// TestMergeLocal_AllowGateChangeAuthorizesGateConfigChange is the green half: the same diff
// merges once the lead approves it with --allow-gate-change, and the merge's audit line NAMES
// the gate-definition file it changed, so a gate change is never reconstructable only by
// re-reading the diff afterwards.
func TestMergeLocal_AllowGateChangeAuthorizesGateConfigChange(t *testing.T) {
	m, repo := deliveryHarness(t, "gateallow")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("ga1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "validate.sh"), []byte("exit 0\n# tweaked by worker\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "edit gate")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("ga1"), head, nil)
	if _, err := m.TrustRecord("ga1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve("ga1", time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("ga1", false); err != nil {
		t.Fatalf("an explicitly gate-change-approved diff should merge: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("the approved gate-definition change should have fast-forwarded the default branch")
	}
	b, _ := os.ReadFile(m.P.AuditLog())
	if !strings.Contains(string(b), "gate-change=.ttorch/validate.sh") {
		t.Fatalf("the merge audit line must name the gate-definition file that changed: %s", b)
	}
	if !strings.Contains(string(b), "approver=human") {
		t.Fatalf("the gate-change merge must audit as approver=human: %s", b)
	}
	if !strings.Contains(string(b), "scope=allow-gate-change") {
		t.Fatalf("the approval itself must be audited as scoped to a gate change: %s", b)
	}
	_, _ = m.Teardown("ga1", true)
}

// oldDiffTouchesGateConfig is a FROZEN REFERENCE: diffTouchesGateConfig exactly as it stood on
// main at 297741d, before its match loop was split into diffGateConfigHits so an approval can
// be bound to every matched path. Only the name differs. Do not edit it to track the guard.
// TestDiffGateConfigHits_AgreesWithFrozenGuard compares the split against it, so the split can
// be shown to leave every blocking decision and every first hit where it was.
func oldDiffTouchesGateConfig(repo, base, rev string) (*gateConfigHit, error) {
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

// gateHitsFixture builds a plain repository on main and one commit on top of it, and returns
// the repo, main's sha and the commit's sha. It spawns nothing, so the comparison below runs
// in the fast lane. ttorch seeds the //go:embed marker resolveGateScope reads, which puts the
// repo in ttorch's own source scope. change stages the second commit itself, with
// writeAndStage for ordinary files and stageEntry for an index entry that should not touch the
// filesystem (a control character, a link, a colliding name). Nothing runs `git add -A` after
// it, because that would stage the deletion of every entry that exists only in the index.
func gateHitsFixture(t *testing.T, ttorch bool, change func(repo string)) (repo, base, rev string) {
	t.Helper()
	repo = t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "config", "user.email", "t@example.com")
	gitIn(t, repo, "config", "user.name", "t")
	for name, body := range map[string]string{
		"AGENTS.md":           "delivery-mode: trusted\n",
		".ttorch/validate.sh": "exit 0\n",
		"README.md":           "readme\n",
		"docs/guide.md":       "guide\n",
	} {
		writeFixtureFile(t, repo, name, body)
	}
	if ttorch {
		writeFixtureFile(t, repo, "content/.keep", "")
		writeFixtureFile(t, repo, "content.go", "package p\n\nimport \"embed\"\n\n//go:embed all:content\nvar payload embed.FS\n")
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base = gitIn(t, repo, "rev-parse", "HEAD")
	if change == nil {
		return repo, base, base // the empty diff
	}
	change(repo)
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "change")
	return repo, base, gitIn(t, repo, "rev-parse", "HEAD")
}

func writeFixtureFile(t *testing.T, repo, name, body string) {
	t.Helper()
	p := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeAndStage(t *testing.T, repo, name, body string) {
	t.Helper()
	writeFixtureFile(t, repo, name, body)
	gitIn(t, repo, "add", "--", name)
}

// stageEntry adds an index entry with the given mode and blob content at path, without
// writing path to disk. For a gitlink the "blob" is the commit it points at.
func stageEntry(t *testing.T, repo, mode, path, content string) {
	t.Helper()
	sha := content
	if mode != "160000" {
		tmp := filepath.Join(repo, "stage.tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sha = gitIn(t, repo, "hash-object", "-w", "stage.tmp")
		if err := os.Remove(tmp); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "update-index", "--add", "--cacheinfo", mode+","+sha+","+path)
}

// TestDiffGateConfigHits_AgreesWithFrozenGuard shows that splitting the guard changed no
// decision. For every fixture it runs the frozen pre-split guard, the new diffGateConfigHits
// and the new diffTouchesGateConfig wrapper over the same diff, and requires the same blocking
// decision, the same hit (Path, Reason and Blocking all equal) and, for a flaggable diff, the
// same first path. Each row also states what the old guard itself returns, so a fixture that
// silently stopped exercising its case fails here instead of passing vacuously.
//
// The second half checks the list the binding depends on: for every non-blocking row,
// diffGateConfigHits returns exactly the changed paths the old loop would have hit, in git's
// order, not just the first. For a blocking row the old loop never ran (every blocking check
// returns before it), so there is no set to compare, and the new function returns none.
func TestDiffGateConfigHits_AgreesWithFrozenGuard(t *testing.T) {
	const (
		clean = "clean"
		flag  = "flag"
		block = "block"
	)
	for _, tc := range []struct {
		name      string
		ttorch    bool
		change    func(repo string)
		want      string
		wantFirst string
	}{
		{name: "covered exact path", change: func(r string) {
			writeAndStage(t, r, ".ttorch/validate.sh", "exit 0\n# tweaked\n")
		}, want: flag, wantFirst: ".ttorch/validate.sh"},
		{name: "covered prefix", change: func(r string) {
			writeAndStage(t, r, ".github/workflows/ci.yml", "on: push\n")
		}, want: flag, wantFirst: ".github/workflows/ci.yml"},
		{name: "AGENTS.md at depth", change: func(r string) {
			writeAndStage(t, r, "pkg/sub/AGENTS.md", "nested\n")
		}, want: flag, wantFirst: "pkg/sub/AGENTS.md"},
		{name: "CLAUDE.md at depth", change: func(r string) {
			writeAndStage(t, r, "pkg/CLAUDE.md", "nested\n")
		}, want: flag, wantFirst: "pkg/CLAUDE.md"},
		{name: "ttorch source path in a ttorch-shaped repo", ttorch: true, change: func(r string) {
			writeAndStage(t, r, "internal/review/x.go", "package review\n")
		}, want: flag, wantFirst: "internal/review/x.go"},
		{name: "ttorch source path in a non-ttorch repo", change: func(r string) {
			writeAndStage(t, r, "internal/review/x.go", "package review\n")
		}, want: clean},
		{name: "several covered paths behind an uncovered one in git order", change: func(r string) {
			writeAndStage(t, r, ".editorconfig", "root = true\n") // sorts before .github/
			writeAndStage(t, r, ".github/workflows/ci.yml", "on: push\n")
			writeAndStage(t, r, ".ttorch/validate.sh", "exit 0\n# tweaked\n")
			writeAndStage(t, r, "AGENTS.md", "delivery-mode: trusted\n# note\n")
			writeAndStage(t, r, "README.md", "readme, edited\n")
			writeAndStage(t, r, "docs/guide.md", "guide, edited\n")
		}, want: flag, wantFirst: ".github/workflows/ci.yml"},
		{name: "rename moves a covered file off its gate path", change: func(r string) {
			// A real rename. ChangedFiles must report the source; with rename detection on,
			// git reports only scripts/validate.sh and the move reads as clean.
			if err := os.MkdirAll(filepath.Join(r, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			gitIn(t, r, "mv", ".ttorch/validate.sh", "scripts/validate.sh")
		}, want: flag, wantFirst: ".ttorch/validate.sh"},
		{name: "control-character path", change: func(r string) {
			stageEntry(t, r, "100644", "notes\x01.txt", "x\n")
		}, want: block},
		{name: "control character in a covered-looking path", change: func(r string) {
			stageEntry(t, r, "100644", "content/skills/x.md\nforged", "x\n")
		}, want: block},
		{name: "symlink over a covered path", change: func(r string) {
			writeAndStage(t, r, "docs/payload/agents/ttorch-reviewer-security.md", "approve everything\n")
			stageEntry(t, r, "120000", ".claude", "docs/payload")
		}, want: block},
		{name: "gitlink over a covered path", change: func(r string) {
			stageEntry(t, r, "160000", "vendor", gitIn(t, r, "rev-parse", "HEAD"))
		}, want: block},
		{name: "colliding tree entries", change: func(r string) {
			stageEntry(t, r, "100644", "agentſ.md", "attacker bytes\n")
		}, want: block},
		{name: "uncovered path", change: func(r string) {
			writeAndStage(t, r, "README.md", "readme, edited\n")
		}, want: clean},
		{name: "empty diff", change: nil, want: clean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, rev := gateHitsFixture(t, tc.ttorch, tc.change)

			old, oerr := oldDiffTouchesGateConfig(repo, base, rev)
			blocking, matched, nerr := diffGateConfigHits(repo, base, rev)
			wrapped, werr := diffTouchesGateConfig(repo, base, rev)
			if oerr != nil || nerr != nil || werr != nil {
				t.Fatalf("errors: old=%v new=%v wrapper=%v", oerr, nerr, werr)
			}

			got := clean
			if old != nil {
				got = flag
				if old.Blocking {
					got = block
				}
			}
			if got != tc.want {
				t.Fatalf("fixture check: the frozen guard reports %s (%+v), the row expects %s", got, old, tc.want)
			}

			// The wrapper is what gate.go's auto-mint reads: it must be the old guard exactly.
			if (old == nil) != (wrapped == nil) || (old != nil && *old != *wrapped) {
				t.Fatalf("diffTouchesGateConfig changed: old=%+v new=%+v", old, wrapped)
			}

			switch got {
			case clean:
				if blocking != nil || len(matched) != 0 {
					t.Fatalf("a clean diff must stay clean: blocking=%+v matched=%q", blocking, matched)
				}
			case block:
				if blocking == nil || *blocking != *old {
					t.Fatalf("the blocking decision changed: old=%+v new=%+v", old, blocking)
				}
				if matched != nil {
					t.Fatalf("a blocking diff must report no matched paths to grant, got %q", matched)
				}
			case flag:
				if blocking != nil {
					t.Fatalf("a flaggable diff became blocking: %+v", blocking)
				}
				if len(matched) == 0 || matched[0] != old.Path || old.Path != tc.wantFirst {
					t.Fatalf("first hit changed: old=%q new=%q, want %q", old.Path, matched, tc.wantFirst)
				}
				changed, err := worktree.ChangedFiles(repo, base, rev)
				if err != nil {
					t.Fatal(err)
				}
				sc := resolveGateScope(repo, base)
				var oldLoopHits []string
				for _, n := range changed {
					if matchesGateConfig(n, sc) {
						oldLoopHits = append(oldLoopHits, n)
					}
				}
				if !slices.Equal(matched, oldLoopHits) {
					t.Fatalf("matched = %q, want every path the old loop would have hit, in order: %q", matched, oldLoopHits)
				}
			}
		})
	}
}

// TestMergeLocal_GateChangeGrantIsBoundToItsFiles: the approval must authorize the
// gate-definition files the lead was actually shown, not "a gate change happened, and a
// boolean is set". A grant issued for AGENTS.md must not carry a .ttorch/validate.sh change
// riding in the same commit.
//
// The narrow grant is minted directly rather than through Approve, because that is the shape
// the binding exists to refuse: a token whose recorded scope is NARROWER than the diff. It
// arises from a token written by hand, and from a grant minted under a build whose
// gate-config set was smaller than the set the merge evaluates. Against an unbound boolean
// the merge succeeds.
func TestMergeLocal_GateChangeGrantIsBoundToItsFiles(t *testing.T) {
	m, repo := deliveryHarness(t, "gatebind")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "AGENTS.md")
	gitIn(t, repo, "commit", "-q", "-m", "track the delivery-mode config")
	task, err := m.Spawn("gb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	// One commit touching TWO gate-definition files.
	if err := os.WriteFile(filepath.Join(wt, ".ttorch", "validate.sh"), []byte("exit 0\n# tweaked\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agents, err := os.ReadFile(filepath.Join(wt, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), append(agents, []byte("\nnote\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "edit both gate files")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("gb1"), head, nil)
	if _, err := m.TrustRecord("gb1", "", time.Minute); err != nil {
		t.Fatal(err)
	}

	// The diff really does touch both, so the scope below is genuinely narrower than the diff.
	blocking, touched, err := diffGateConfigHits(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	if blocking != nil {
		t.Fatalf("precondition: the diff must be a flaggable gate change, not a blocking one: %+v", blocking)
	}
	if len(touched) != 2 {
		t.Fatalf("precondition: the diff should touch both gate-definition files, got %q", touched)
	}

	// A grant scoped to AGENTS.md alone.
	if err := approval.Grant(m.P.ApprovalFile("gb1"), time.Minute, approvalPayload("human", head, []string{"AGENTS.md"})); err != nil {
		t.Fatal(err)
	}
	defHead := gitIn(t, repo, "rev-parse", "HEAD")
	_, err = m.MergeLocal("gb1", false)
	if err == nil {
		t.Fatal("a grant scoped to AGENTS.md must not authorize a .ttorch/validate.sh change in the same commit")
	}
	if !strings.Contains(err.Error(), ".ttorch/validate.sh") {
		t.Fatalf("the refusal must name the file the grant did NOT cover, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the unauthorized gate change must not have merged")
	}

	// A bare marker with no paths authorizes nothing. It is the exact token the unbound
	// boolean scope wrote, so a token minted before the binding fails closed, not open.
	if err := approval.Grant(m.P.ApprovalFile("gb1"), time.Minute, "human "+head+" "+approvalScopeGateChange); err != nil {
		t.Fatal(err)
	}
	if _, err = m.MergeLocal("gb1", false); err == nil || !strings.Contains(err.Error(), "does not authorize a gate change") {
		t.Fatalf("a bare allow-gate-change marker must authorize nothing, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the bare-marker token must not have merged the gate change")
	}

	// The legitimate case: the grant Approve actually mints covers every gate file in the
	// diff, and still merges. A guard that only ever refuses is as dead as one that passes.
	granted, err := m.Approve("gb1", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(granted, touched) {
		t.Fatalf("Approve granted %q, want every gate file in the diff %q", granted, touched)
	}
	if _, err := m.MergeLocal("gb1", false); err != nil {
		t.Fatalf("a grant covering every gate file in the diff should merge: %v", err)
	}
	b, _ := os.ReadFile(m.P.AuditLog())
	for _, want := range []string{"gate-change=.ttorch/validate.sh,AGENTS.md", "scope=allow-gate-change=.ttorch/validate.sh,AGENTS.md"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the audit must record %q: %s", want, b)
		}
	}
	_, _ = m.Teardown("gb1", true)
}

// TestMergeLocal_GateChangeGrantRefusesPathCoveredAfterApproval: the grant is compared with
// the covered set as it stands at MERGE time, so a path that was not a gate file when the lead
// approved, and is one by the time the merge runs, is refused rather than riding in on a grant
// that never named it.
//
// The widening here is real, not simulated. The repo starts outside ttorch's source scope, so
// internal/review/ is an ordinary directory. The worker's branch first restores the
// //go:embed marker (commit M) and then edits AGENTS.md and internal/review/rules.go (commit
// W). The lead approves W with --allow-gate-change while main is still the unscoped base, so
// the grant is {AGENTS.md}. Then M lands on main by itself, the way the lower half of a stack
// lands first. W's sha does not change, so the token still pins it, but main now reads as
// ttorch source and internal/review/rules.go is covered. Under an unbound grant this merged.
func TestMergeLocal_GateChangeGrantRefusesPathCoveredAfterApproval(t *testing.T) {
	m, repo := deliveryHarness(t, "gatewiden")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "AGENTS.md")
	gitIn(t, repo, "commit", "-q", "-m", "track the delivery-mode config")
	// deliveryHarness seeds the marker; take it out so the base is an ordinary repo.
	gitIn(t, repo, "rm", "-q", "content.go", "content/.keep")
	gitIn(t, repo, "commit", "-q", "-m", "not ttorch source")
	if resolveGateScope(repo, "main").TtorchSource {
		t.Fatal("setup: the base must resolve outside ttorch's source scope")
	}

	task, err := m.Spawn("gw1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("gw1", true) }()
	wt := task.Worktree
	writeFixtureFile(t, wt, "content/.keep", "")
	writeFixtureFile(t, wt, "content.go", "package p\n\nimport \"embed\"\n\n//go:embed all:content\nvar payload embed.FS\n")
	gitIn(t, wt, "add", "content.go", "content/.keep")
	gitIn(t, wt, "commit", "-q", "-m", "embed the payload")
	marker := gitIn(t, wt, "rev-parse", "HEAD")
	agents, err := os.ReadFile(filepath.Join(wt, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, wt, "AGENTS.md", string(agents)+"\nnote\n")
	writeFixtureFile(t, wt, "internal/review/rules.go", "package review\n")
	gitIn(t, wt, "add", "AGENTS.md", "internal/review/rules.go")
	gitIn(t, wt, "commit", "-q", "-m", "edit the config and the review rules")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("gw1"), head, nil)
	if _, err := m.TrustRecord("gw1", "", time.Minute); err != nil {
		t.Fatal(err)
	}

	granted, err := m.Approve("gw1", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(granted, []string{"AGENTS.md"}) {
		t.Fatalf("setup: at approve time only AGENTS.md is a gate file, so the grant should be {AGENTS.md}, got %q", granted)
	}

	// The lower half of the stack lands on its own.
	gitIn(t, repo, "merge", "-q", "--ff-only", marker)
	if !resolveGateScope(repo, "main").TtorchSource {
		t.Fatal("setup: after the marker lands, main must resolve as ttorch source")
	}
	if data, ok := approval.Data(m.P.ApprovalFile("gw1")); !ok {
		t.Fatal("setup: the approval must still be valid")
	} else if _, sha, _ := splitApprovalPayload(data); sha != head {
		t.Fatalf("setup: the approval must still pin %s, got %s", short(head), short(sha))
	}

	_, err = m.MergeLocal("gw1", false)
	if err == nil {
		t.Fatal("a path that became a gate file after the approval must not ride in on a grant that never named it")
	}
	if !strings.Contains(err.Error(), "internal/review/rules.go") || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("the refusal must name the path the grant does not cover, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != marker {
		t.Fatal("the change must not have merged")
	}
}

// TestApprovalPayloadScope pins the token wire format the gate reads its authority from: the
// gate-change scope round-trips WITH the paths it was granted for, an ordinary token carries
// none, a legacy provenance-less token still yields by=="" so the gated path fails closed, and
// a bare marker with no "=<paths>" authorizes NOTHING rather than everything.
func TestApprovalPayloadScope(t *testing.T) {
	sha := "abc123"
	for _, tc := range []struct {
		name    string
		data    string
		wantBy  string
		wantSHA string
		wantFor []string
	}{
		{"human plain", approvalPayload("human", sha, nil), "human", sha, nil},
		{"human scoped to one", approvalPayload("human", sha, []string{"AGENTS.md"}), "human", sha, []string{"AGENTS.md"}},
		{"human scoped to two", approvalPayload("human", sha, []string{".ttorch/validate.sh", "AGENTS.md"}), "human", sha, []string{".ttorch/validate.sh", "AGENTS.md"}},
		{"auto plain", approvalPayload("auto", sha, nil), "auto", sha, nil},
		{"legacy bare sha", sha, "", sha, nil},
		{"empty", "", "", "", nil},
		{"unknown trailing scope", "human " + sha + " something-else", "human", sha, nil},
		{"bare marker authorizes nothing", "human " + sha + " allow-gate-change", "human", sha, nil},
		{"marker with empty list authorizes nothing", "human " + sha + " allow-gate-change=", "human", sha, nil},
	} {
		by, gotSHA, gotFor := splitApprovalPayload(tc.data)
		if by != tc.wantBy || gotSHA != tc.wantSHA || !slices.Equal(gotFor, tc.wantFor) {
			t.Errorf("%s: splitApprovalPayload(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tc.name, tc.data, by, gotSHA, gotFor, tc.wantBy, tc.wantSHA, tc.wantFor)
		}
	}
}

// unicodeSpaceGrantPath is the path the round-13 review built: one covered file under
// .github/workflows/ whose name carries a Unicode space and then what reads as a second grant.
// It prints to the lead as one path. Before grantablePath refused every unicode.IsSpace rune,
// strings.Fields split it at the space and the token parsed back as two grants, one of them
// .ttorch/validate.sh, which the diff never touched.
func unicodeSpaceGrantPath(space rune) string {
	return ".github/workflows/x" + string(space) + "allow-gate-change=.ttorch/validate.sh"
}

// TestGrantablePath_RefusesEveryFieldsSeparator: grantablePath must refuse exactly the runes
// splitApprovalPayload splits on, which for strings.Fields is every rune unicode.IsSpace
// reports and not only ASCII space, tab and newline. The rows are the three the review named
// and the ASCII cases the check always covered.
func TestGrantablePath_RefusesEveryFieldsSeparator(t *testing.T) {
	for _, r := range []rune{' ', ' ', '　', ' ', '\t', '\n'} {
		p := unicodeSpaceGrantPath(r)
		if grantablePath(p) {
			_, _, back := splitApprovalPayload(approvalPayload("human", "abc123", []string{p}))
			t.Errorf("grantablePath accepted %q (space U+%04X); the token parses it back as %q", p, r, back)
		}
	}
	if grantablePath(".github/workflows/a,b.yml") {
		t.Error("grantablePath must refuse a comma, which separates the scope's paths")
	}
	if grantablePath("") {
		t.Error("grantablePath must refuse an empty path")
	}
}

// TestApprovalPayload_RoundTripsEveryGrantablePath is the property the binding rests on: for
// every path grantablePath accepts, splitApprovalPayload(approvalPayload(paths)) returns
// exactly those paths, alone and beside another grant. It walks every Unicode code point, and
// every byte that is invalid UTF-8 on its own, inside a covered path. The other direction is
// checked too, so the fix cannot pass by refusing more than it has to: a path is refused only
// for a comma or a rune unicode.IsSpace reports.
func TestApprovalPayload_RoundTripsEveryGrantablePath(t *testing.T) {
	const sha = "abc123"
	check := func(p string, r rune, isRune bool) {
		t.Helper()
		if !grantablePath(p) {
			if isRune && (r == ',' || unicode.IsSpace(r)) {
				return
			}
			t.Errorf("grantablePath refused %q, which contains no comma and no space", p)
			return
		}
		for _, grant := range [][]string{{p}, {".ttorch/validate.sh", p}, {p, "AGENTS.md"}} {
			by, gotSHA, back := splitApprovalPayload(approvalPayload("human", sha, grant))
			if by != "human" || gotSHA != sha || !slices.Equal(back, grant) {
				t.Errorf("round trip of %q gave (%q, %q, %q)", grant, by, gotSHA, back)
			}
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		check(".github/workflows/a"+string(r)+"b.yml", r, true)
	}
	for b := 0x80; b <= 0xff; b++ {
		check(".github/workflows/a"+string([]byte{byte(b)})+"b.yml", 0, false)
	}
}

// TestApprove_RefusesUnicodeSpaceInGrantPath: a covered path carrying a Unicode space is
// refused when the lead approves with --allow-gate-change, and no token is minted, rather
// than being recorded and read back as different grants.
func TestApprove_RefusesUnicodeSpaceInGrantPath(t *testing.T) {
	m, repo := deliveryHarness(t, "grantspace")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("gs1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("gs1", true) }()
	wt := task.Worktree
	for _, r := range []rune{' ', ' ', '　'} {
		p := unicodeSpaceGrantPath(r)
		full := filepath.Join(wt, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("on: push\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, wt, "add", "-A")
		gitIn(t, wt, "commit", "-q", "-m", "add a workflow")

		granted, err := m.Approve("gs1", time.Minute, true)
		if err == nil {
			data, _ := approval.Data(m.P.ApprovalFile("gs1"))
			_, _, back := splitApprovalPayload(data)
			t.Fatalf("U+%04X: Approve granted %q, and the token parses back as %q", r, granted, back)
		}
		if !strings.Contains(err.Error(), "comma or whitespace") {
			t.Fatalf("U+%04X: expected the grantable-path refusal, got: %v", r, err)
		}
		if approval.Valid(m.P.ApprovalFile("gs1")) {
			t.Fatalf("U+%04X: a refused approval must not leave a token behind", r)
		}
		gitIn(t, wt, "reset", "-q", "--hard", "HEAD~1")
	}
}

// TestMergeLocal_GateInstructionChangeNeedsAllowGateChange: the gate's own INSTRUCTIONS are a
// gate-definition change too. content/skills/ttorch-review/SKILL.md is embedded and installed
// to ~/.claude/skills, so a landed edit to it changes what the gate does on the next run for
// every repo on the machine. Before content/skills/ was covered, this merged on a plain
// approval with nothing in the audit naming it — which made the claim written in that very
// file false about itself.
func TestMergeLocal_GateInstructionChangeNeedsAllowGateChange(t *testing.T) {
	m, repo := deliveryHarness(t, "gateinstr")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	task, err := m.Spawn("gi1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Worktree
	skill := filepath.Join(wt, "content", "skills", "ttorch-review")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# gate procedure\nreviewers: none\n"
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "edit the gate's reviewer instructions")
	head := gitIn(t, wt, "rev-parse", "HEAD")
	writeReviewReports(t, m.P.ReviewInputsDir("gi1"), head, nil)
	if _, err := m.TrustRecord("gi1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// It must not auto-approve, for the same reason a validate.sh change does not.
	if approval.Valid(m.P.ApprovalFile("gi1")) {
		t.Fatal("a diff touching the gate's instructions must not auto-approve in trusted mode")
	}
	// Nor merge on a plain human approval.
	if _, err := m.Approve("gi1", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defHead := gitIn(t, repo, "rev-parse", "HEAD")
	_, err = m.MergeLocal("gi1", false)
	if err == nil {
		t.Fatal("a plain human approval must not authorize a change to the gate's own instructions")
	}
	if !strings.Contains(err.Error(), "content/skills/ttorch-review/SKILL.md") {
		t.Fatalf("the refusal must name the instruction file, got: %v", err)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the instruction change must not have merged")
	}
	// With the explicit flag it merges and the audit names the file.
	if _, err := m.Approve("gi1", time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MergeLocal("gi1", false); err != nil {
		t.Fatalf("an explicitly gate-change-approved instruction change should merge: %v", err)
	}
	if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "gate-change=content/skills/ttorch-review/SKILL.md") {
		t.Fatalf("the merge audit line must name the instruction file: %s", b)
	}
	_, _ = m.Teardown("gi1", true)
}

package orchestrator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/validate"
)

// gateGuardDiff builds a repo whose HEAD~1..HEAD diff touches exactly files, and returns the
// repo plus the two revisions to hand the guard. Every path is written with placeholder
// content — the guard classifies by PATH, never by content, so the bytes are irrelevant.
func gateGuardDiff(t *testing.T, files ...string) (repo, base, rev string) {
	t.Helper()
	repo = newRepoMain(t)
	base = gitIn(t, repo, "rev-parse", "HEAD")
	for _, f := range files {
		p := filepath.Join(repo, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "change")
	return repo, base, gitIn(t, repo, "rev-parse", "HEAD")
}

// TestGateGuardRefusesGateDefinition covers acceptance criteria 1, 2, 3 and 6: a diff that
// touches a reviewer definition, the review protocol, or the gate's own decision-making Go
// source must be reported as touching the gate definition, and must NAME the file that
// triggered it so a human can see what they are being asked to decide.
func TestGateGuardRefusesGateDefinition(t *testing.T) {
	for _, path := range []string{
		// Already guarded before this change.
		".ttorch/validate.sh",
		"AGENTS.md",
		// The reviewer definitions: their prompts ARE the adversarial review.
		"content/agents/ttorch-reviewer-correctness.md",
		"content/agents/ttorch-reviewer-scope.md",
		"content/agents/ttorch-reviewer-security.md",
		// The review protocol the gate runs.
		"content/skills/ttorch-review/SKILL.md",
		// The gate's own source: what passes, and the auto-mint path itself.
		"internal/orchestrator/gate.go",
		"internal/orchestrator/gateguard.go",
		"internal/orchestrator/merge.go",
		"internal/orchestrator/validate.go",
		"internal/orchestrator/validatecache.go",
		// Verdict aggregation and reviewer scaling.
		"internal/review/review.go",
		"internal/review/size.go",
		// The approval token the auto-mint writes and the merge consumes.
		"internal/approval/approval.go",
		// What "green" means, and which checks run.
		"internal/validate/validate.go",
		// The durable verdict row the merge gate reads.
		"internal/db/verdict.go",
		// The delivery-mode reader that decides whether the trusted path applies at all.
		"internal/projectinit/projectinit.go",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if !touched {
				t.Fatalf("a diff touching %s must refuse a trusted auto-merge, but the guard permitted it", path)
			}
			if name != path {
				t.Fatalf("the refusal must name the triggering file: got %q, want %q", name, path)
			}
		})
	}
}

// TestGateGuardPermitsOrdinarySource is the regression that would make people disable the
// guard, so it is also the honest record of what trusted mode still buys after the guard went
// package-granular. Measured against the 200 commits before this branch: 109 of them touch
// none of the guarded paths and would still auto-merge; 101 would now need a human, with
// internal/orchestrator alone accounting for 78. That is the price of the fix and it is
// recorded here rather than in prose only.
//
// Three paths moved OUT of this list when the guard went package-granular:
// internal/orchestrator/spawn.go, internal/orchestrator/landqueue.go and
// internal/worktree/worktree.go. Each is in a package whose files can rebind the gate's seams
// or feed the guard its own input, so each is now refused. That change is deliberate; see
// TestGateSeamsAreRebindableFromAnyFileInPackage for why.
func TestGateGuardPermitsOrdinarySource(t *testing.T) {
	for _, path := range []string{
		// Docs and entrypoints.
		"README.md",
		"docs/design.md",
		"cmd/ttorch/main.go",
		".github/workflows/ci.yml",
		// Packages that declare no state the gate reads and cannot reach into one that does.
		"internal/cli/cli.go",
		"internal/scheduler/scheduler.go",
		"internal/scheduler/gate.go",
		"internal/watch/watchdog.go",
		"internal/skills/skills.go",
		"internal/learnings/learnings.go",
		"internal/selfupdate/selfupdate.go",
		// The agent and skill library the gate never consults.
		"content/agents/golang-pro.md",
		"content/agents/ttorch-worker.md",
		"content/skills/ttorch-manager/SKILL.md",
		"content/skills/ttorch-validate/SKILL.md",
		"content/assets/AGENTS.global.md",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if touched {
				t.Fatalf("a diff touching only %s must still auto-merge, but the guard refused it as %q", path, name)
			}
		})
	}
}

// TestGateGuardRefusesUnevaluablePath covers acceptance criterion 5: a path the guard cannot
// evaluate must REFUSE, not permit. `git diff --name-only` C-quotes a path containing
// non-ASCII bytes, so the name the guard compares is an escaped token, not the path — and a
// prefix-matched gate file whose name carries a non-ASCII byte therefore slips past a naive
// comparison. The guard must refuse and name the token it could not read.
func TestGateGuardRefusesUnevaluablePath(t *testing.T) {
	const path = "content/agents/ttorch-reviewer-café.md"
	repo, base, rev := gateGuardDiff(t, path)
	touched, name, err := diffTouchesGateConfig(repo, base, rev)
	if err == nil && !touched {
		t.Fatalf("a changed path the guard cannot evaluate must refuse the auto-merge, but it permitted it")
	}
	got := name
	if err != nil {
		got = err.Error()
	}
	if !strings.Contains(got, "ttorch-reviewer-caf") {
		t.Fatalf("the refusal must name the path it could not evaluate: got %q", got)
	}
}

// TestGateGuardPermitsUnevaluableNonGatePath is the other half of criterion 5's trade-off:
// refusing on an unreadable name must not degenerate into refusing every diff that happens
// to carry a non-ASCII filename. A quoted name whose verbatim prefix already rules out every
// gate-definition path is decidable, so it still auto-merges.
func TestGateGuardPermitsUnevaluableNonGatePath(t *testing.T) {
	repo, base, rev := gateGuardDiff(t, "docs/café.md")
	touched, name, err := diffTouchesGateConfig(repo, base, rev)
	if err != nil {
		t.Fatalf("a non-gate path must be decidable from its verbatim prefix: %v", err)
	}
	if touched {
		t.Fatalf("docs/café.md is not a gate-definition file, but the guard refused it as %q", name)
	}
}

// TestGateGuardRefusesRenamedGateSource covers criterion 3 for the rename case: moving the
// gate's own source out from under the guard must not permit the auto-merge. With git's
// rename detection on, `diff --name-only` reports only the DESTINATION path, which is not a
// gate path — so the guard must list changed files with rename detection OFF and see the
// source path too.
func TestGateGuardRefusesRenamedGateSource(t *testing.T) {
	repo, _, base := gateGuardDiff(t, "internal/orchestrator/merge.go")
	gitIn(t, repo, "mv", "internal/orchestrator/merge.go", "internal/orchestrator/landing.go")
	gitIn(t, repo, "commit", "-q", "-m", "rename")
	rev := gitIn(t, repo, "rev-parse", "HEAD")

	touched, _, err := diffTouchesGateConfig(repo, base, rev)
	if err != nil {
		t.Fatalf("guard could not evaluate the diff: %v", err)
	}
	if !touched {
		t.Fatal("renaming the gate's own source away must refuse a trusted auto-merge, but the guard permitted it")
	}
}

// TestGateGuardRefusesUnresolvableDiff covers the other half of criterion 5: when the diff
// cannot be listed at all, the guard must return an error so both call sites fail closed
// (merge.go propagates it; gate.go's auto-mint requires a nil error before minting).
func TestGateGuardRefusesUnresolvableDiff(t *testing.T) {
	repo, base, _ := gateGuardDiff(t, "README.md")
	touched, _, err := diffTouchesGateConfig(repo, base, "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("an unresolvable revision must refuse the auto-merge, not permit it")
	}
	if touched {
		t.Fatal("a failed evaluation must not report a definite answer")
	}
}

// TestGateDefinitionPathsAnchored keeps the hardcoded list honest. Every entry must still
// name real content in THIS repo, so a rename or deletion that orphans an entry — and
// thereby moves a gate file out from under the guard — turns the gate's own validate red
// instead of silently opening a hole. A trusted auto-merge requires a fresh green validate,
// so a red anchor here is itself a refusal.
//
// It also pins the property couldBeGateDefinition relies on: every guarded path is plain
// printable ASCII, so git reports it verbatim and never as a quoted token.
func TestGateDefinitionPathsAnchored(t *testing.T) {
	root := filepath.Join("..", "..")
	// Case-SENSITIVE existence. os.Stat is not usable here: this filesystem folds case, so
	// Stat("makefile") happily finds Makefile and both assertions below would be meaningless.
	exists := func(rel string) bool {
		dir, name := path.Split(rel)
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Name() == name {
				return true
			}
		}
		return false
	}
	for _, f := range gateDefinitionFiles {
		if !exists(f) {
			t.Errorf("gateDefinitionFiles names %s, which is not in the repo under that exact spelling", f)
		}
	}
	// The pre-emptive entries are guarded so they can never be ADDED. If one has appeared, the
	// guard still covers it, but somebody should look at why it is there.
	for _, f := range gateDefinitionPreemptiveFiles {
		if exists(f) {
			t.Errorf("%s is in the repo; it is guarded as a file that should not exist, so confirm it is legitimate and move it into gateDefinitionFiles", f)
		}
		if !isGateDefinition(f) {
			t.Errorf("%s must be covered by the guard", f)
		}
	}
	for _, p := range gateDefinitionPrefixes {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(p)) + "*")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Errorf("gateDefinitionPrefixes names %s, which matches nothing in the repo", p)
		}
	}
	all := append([]string{}, gateDefinitionFiles...)
	all = append(all, gateDefinitionPreemptiveFiles...)
	all = append(all, gateDefinitionPrefixes...)
	for _, p := range all {
		for _, r := range p {
			if r < 0x20 || r > 0x7e || r == '\\' || r == '"' {
				t.Errorf("guarded path %q contains %q, which git would report as a quoted token", p, r)
			}
		}
	}
}

// TestGateGuardUnevaluablePathIsUndecidableNotIgnored unit-tests the fail-closed branch
// directly, for the quoted forms real git will not produce but a future change to the lister
// might. A readable prefix that could still reach a guarded path is undecidable; one that
// cannot is decidable.
func TestGateGuardUnevaluablePathIsUndecidableNotIgnored(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quote bool
		undec bool
	}{
		{name: `"content/agents/ttorch-reviewer-x\303\251.md"`, quote: true, undec: true},
		{name: `"content/skills/ttorch-review/SKILL\303\251.md"`, quote: true, undec: true},
		{name: `"internal/review/\303\251.go"`, quote: true, undec: true},
		{name: `"internal/\303\251/x.go"`, quote: true, undec: true},
		{name: `"\303\251"`, quote: true, undec: true},
		{name: `"docs/\303\251.md"`, quote: true, undec: false},
		{name: `"content/agents/golang-\303\251.md"`, quote: true, undec: false},
		{name: `internal/termtab/termtab.go`, quote: false, undec: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix, quoted := verbatimPrefix(tc.name)
			if quoted != tc.quote {
				t.Fatalf("verbatimPrefix(%q) quoted = %v, want %v", tc.name, quoted, tc.quote)
			}
			if !quoted {
				return
			}
			if got := couldBeGateDefinition(prefix); got != tc.undec {
				t.Fatalf("couldBeGateDefinition(%q) = %v, want %v", prefix, got, tc.undec)
			}
		})
	}
}

// TestGateDecisionSourceStaysGuarded is the structural half of the guard: for Go source, a
// path list is only as good as the code still being at those paths. It asserts every
// gate-deciding declaration still exists and is declared only in files the guard covers -
// which also covers the case a path-only rule would miss, reviewer PROMPT TEXT living in Go
// source rather than in content/ (reviewerBrief). Moving or renaming one of these out of a
// guarded file fails this test, so the gate's own validate goes red and no trusted auto-merge
// can carry the move.
//
// It resolves declarations by PARSING, never by scanning source text. gateguard.go quotes
// every identity in gateDecisionDeclarations, so a text scan would find them all in
// gateguard.go, which is itself guarded: the "still declared somewhere" half would be
// unreachable and a rename-and-relocate would pass. TestGateDecisionCheckIsNotVacuous pins
// that this is not how it works.
func TestGateDecisionSourceStaysGuarded(t *testing.T) {
	byDecl, err := declaringFiles(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range gateDecisionDeclarations {
		files := byDecl[decl]
		if len(files) == 0 {
			t.Errorf("%s decides gate outcomes but no source file declares it; if it was renamed or removed, update gateDecisionDeclarations and make sure its new home is guarded", decl)
			continue
		}
		for _, f := range files {
			if !isGateDefinition(f) {
				t.Errorf("%s decides gate outcomes but is declared in %s, which the guard does not cover; add it to gateDefinitionFiles or move the code back", decl, f)
			}
		}
	}
}

// TestGateDecisionCheckIsNotVacuous proves the check above can actually fail, because a
// structural anchor that cannot fail is worse than none: it reads as protection while
// providing none. It asserts that gateguard.go, which QUOTES every identity in the list, is
// credited only with the declarations it genuinely makes - so a name that moved away or was
// renamed really does go missing.
func TestGateDecisionCheckIsNotVacuous(t *testing.T) {
	byDecl, err := declaringFiles(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const self = "internal/orchestrator/gateguard.go"
	declaredHere := map[string]bool{
		"diffTouchesGateConfig": true, "isGateDefinition": true,
		"couldBeGateDefinition": true, "verbatimPrefix": true,
	}
	for _, decl := range gateDecisionDeclarations {
		if declaredHere[decl] {
			continue
		}
		for _, f := range byDecl[decl] {
			if f == self {
				t.Errorf("%s is only QUOTED in %s, not declared there; crediting it would make TestGateDecisionSourceStaysGuarded vacuous", decl, self)
			}
		}
	}
	// A name this file quotes but nothing declares must resolve to nothing at all.
	if files := byDecl["reviewerBriefThatNoFileDeclares"]; len(files) != 0 {
		t.Errorf("a quoted-but-undeclared name resolved to %v, so the check is matching text, not declarations", files)
	}
	for _, decl := range declaredHere {
		_ = decl
	}
}

// declaringFiles parses every .go file under root and maps each package-level declaration to
// the repo-relative files declaring it. Methods are keyed "(recv).Name"; funcs, vars, consts
// and types by bare name. Parsing rather than text-matching is the point: a declaration name
// appearing inside a string literal is not a declaration.
func declaringFiles(root string) (map[string][]string, error) {
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "bin" || name == "dist" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		add := func(name string) {
			if name != "" && name != "_" {
				out[name] = append(out[name], rel)
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				add(declKey(d))
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, n := range s.Names {
							add(n.Name)
						}
					case *ast.TypeSpec:
						add(s.Name.Name)
					}
				}
			}
		}
		return nil
	})
	return out, err
}

// declKey renders a func declaration's identity: "Name" for a plain func, "(*T).Name" or
// "(T).Name" for a method.
func declKey(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	var recv string
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			recv = "*" + id.Name
		}
	case *ast.Ident:
		recv = t.Name
	}
	if recv == "" {
		return d.Name.Name
	}
	return "(" + recv + ")." + d.Name.Name
}

// TestGateSeamsAreRebindableFromAnyFileInPackage is the attack, written as a test. Go
// package-level vars are writable from ANY file in the same package, and the gate's decisions
// hang off five of them. This test performs the rebind from gateguard_test.go, which is a
// DIFFERENT file in package orchestrator from the ones declaring the vars, and shows the
// gate's notion of "green" following the rebind. A worker file doing this in an init() needs
// no guarded file at all.
//
// Having demonstrated that, it asserts the consequence: the guard's unit for Go source must be
// the PACKAGE, so every file in package orchestrator has to be covered.
func TestGateSeamsAreRebindableFromAnyFileInPackage(t *testing.T) {
	// The rebind. runGateOnCommitted is declared in validate.go; this file is not validate.go.
	orig := runGateOnCommitted
	t.Cleanup(func() { runGateOnCommitted = orig })
	runGateOnCommitted = func(repo, sha string, def gateDefinition) (bool, []validate.Result, error) {
		return true, []validate.Result{{Name: "gate", Passed: true}}, nil
	}
	green, results, err := runGateOnCommitted("/nonexistent-repo", "deadbeef", gateDefinition{})
	if err != nil || !green || len(results) != 1 {
		t.Fatalf("the rebind did not take: green=%v results=%v err=%v", green, results, err)
	}
	// Same for the other four seams, to show the channel is not one variable.
	for name, rebind := range map[string]func() func(){
		"requiredReviewers": func() func() {
			o := requiredReviewers
			requiredReviewers = nil
			return func() { requiredReviewers = o }
		},
		"reviewerDispatcher": func() func() {
			o := reviewerDispatcher
			reviewerDispatcher = func(*Manager, string, string, string, string, string, string) error { return nil }
			return func() { reviewerDispatcher = o }
		},
		"nowFunc": func() func() {
			o := nowFunc
			nowFunc = func() time.Time { return time.Unix(0, 0) }
			return func() { nowFunc = o }
		},
		"landRebase": func() func() {
			o := landRebase
			landRebase = func(wt, base string) error { return nil }
			return func() { landRebase = o }
		},
	} {
		restore := rebind()
		restore()
		_ = name
	}

	// The consequence: a partially guarded Go package is not a boundary.
	entries, err := os.ReadDir(filepath.Join("..", "..", "internal", "orchestrator"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		p := "internal/orchestrator/" + e.Name()
		if !isGateDefinition(p) {
			t.Errorf("%s is in package orchestrator, so it can rebind runGateOnCommitted in an init() and make every gate green; the guard must cover it", p)
		}
	}
}

// TestGuardedGoPackagesAreWholePackages encodes the rule the rebind attack forces: for Go
// source the guard's unit is the package, never the file. A package that is partly guarded is
// not a boundary at all, because any unguarded file in it can rewrite the guarded files'
// package-level state. It fails if any guarded .go path leaves a sibling .go file uncovered.
func TestGuardedGoPackagesAreWholePackages(t *testing.T) {
	root := filepath.Join("..", "..")
	dirs := map[string]bool{}
	for _, f := range gateDefinitionFiles {
		if strings.HasSuffix(f, ".go") {
			dirs[path.Dir(f)] = true
		}
	}
	for _, p := range gateDefinitionPrefixes {
		if d := strings.TrimSuffix(p, "/"); strings.HasPrefix(d, "internal/") || strings.HasPrefix(d, "cmd/") {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); err == nil {
				dirs[d] = true
			}
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no guarded Go packages found; the guard covers no Go source at all")
	}
	for dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Errorf("guarded Go package %s: %v", dir, err)
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			p := e.Name()
			if dir != "." {
				p = dir + "/" + e.Name()
			}
			if !isGateDefinition(p) {
				t.Errorf("%s is guarded only in part: %s is in the same Go package and is not covered, so it can rewrite that package's gate state", dir, p)
			}
		}
	}
}

// TestGateGuardRefusesNewlyIdentifiedGateInputs covers the brief's CRITICAL and three HIGHs:
// an in-package rebind vector, the Makefile that .ttorch/validate.sh actually executes, the
// verdict WRITE path, and the git primitives the guard feeds itself from.
func TestGateGuardRefusesNewlyIdentifiedGateInputs(t *testing.T) {
	for _, path := range []string{
		// CRITICAL: any file in package orchestrator can rebind the gate's seams.
		"internal/orchestrator/spawn.go",
		"internal/orchestrator/landqueue.go",
		"internal/orchestrator/audit.go",
		"internal/orchestrator/orchestrator.go",
		"internal/orchestrator/autostart.go",
		"internal/orchestrator/overlap.go",
		"internal/orchestrator/rebind.go", // a file that does not exist yet: the attack adds one
		// HIGH: validate.sh is pinned to the default branch, but it runs `make lint` and
		// `make test-fast` inside a checkout of the WORKER's sha, so the worker's Makefile
		// supplies the commands. `test-fast: @true` is a green gate.
		"Makefile",
		// HIGH: the verdict WRITE path. verdict.go was the read side; RecordDelivery, the
		// upsert TrustRecord calls, is in task.go.
		"internal/db/task.go",
		"internal/db/verdict.go",
		// HIGH: the guard's own input. diffTouchesGateConfig is only as honest as
		// ChangedFilesNoRenames, and DefaultBranch supplies its base.
		"internal/worktree/worktree.go",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if !touched {
				t.Fatalf("a diff touching %s must refuse a trusted auto-merge, but the guard permitted it", path)
			}
			if name != path {
				t.Fatalf("the refusal must name the triggering file: got %q, want %q", name, path)
			}
		})
	}
}

// gateGuardTreeDiff builds a repo whose diff adds each path as a TREE ENTRY, via
// `git update-index --cacheinfo`, bypassing the working tree entirely. That is the only way to
// test a case variant on this machine: macOS is case-insensitive, so `Internal/orchestrator`
// and `internal/orchestrator` are the same directory on disk but two distinct entries in a git
// tree, and `git diff --name-only` reports whichever spelling the tree holds.
func gateGuardTreeDiff(t *testing.T, paths ...string) (repo, base, rev string) {
	t.Helper()
	repo = newRepoMain(t)
	// A real lowercase file in the guarded package, so the tree holds both spellings.
	realPath := filepath.Join(repo, "internal", "orchestrator")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realPath, "real.go"), []byte("package orchestrator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base = gitIn(t, repo, "rev-parse", "HEAD")

	for _, p := range paths {
		cmd := exec.Command("git", "-C", repo, "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader("package orchestrator\n\nfunc init() {}\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		blob := strings.TrimSpace(string(out))
		gitIn(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	gitIn(t, repo, "commit", "-q", "-m", "case variant")
	return repo, base, gitIn(t, repo, "rev-parse", "HEAD")
}

// TestGateGuardRefusesCaseVariantGatePath is the case bypass, written against the real
// condition rather than against the matching helper. isGateDefinition compared case
// SENSITIVELY, so a tree entry spelled Internal/orchestrator/rebind.go walked straight through:
// the guard returned touched=false, name="", err=nil and the approval auto-minted. On this
// machine that is not cosmetic. macOS folds case, so the gate's detached checkout and the local
// fast-forward both put that file in internal/orchestrator/, and `go list` compiles it into the
// package, where its init() can rebind runGateOnCommitted.
//
// TestGuardedGoPackagesAreWholePackages could not see this, because it enumerates the directory
// under the lowercase prefix and the case-variant sibling looks covered.
func TestGateGuardRefusesCaseVariantGatePath(t *testing.T) {
	for _, path := range []string{
		"Internal/orchestrator/rebind.go",
		"INTERNAL/ORCHESTRATOR/rebind.go",
		"Internal/Db/verdict.go",
		"internal/Review/review.go",
		"MAKEFILE",
		"makeFile",
		"Agents.md",
		"content/agents/TTORCH-REVIEWER-evil.md",
		".TTORCH/validate.sh",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardTreeDiff(t, path)
			// Confirm the tree really carries the spelling under test, so a green result
			// cannot come from the fixture quietly normalizing it.
			if names := gitIn(t, repo, "diff", "--name-only", "--no-renames", base, rev); !strings.Contains(names, path) {
				t.Fatalf("fixture did not produce the tree entry under test: diff reported %q", names)
			}
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if !touched {
				t.Fatalf("%s folds to a gate-definition path and must refuse a trusted auto-merge, but the guard permitted it", path)
			}
			if name != path {
				t.Fatalf("the refusal must name the triggering file as the tree spells it: got %q, want %q", name, path)
			}
		})
	}
}

// TestGateGuardPermitsCaseVariantNonGatePath is the other side of folding: refusing on case
// must not start refusing every path that merely contains a capital letter.
func TestGateGuardPermitsCaseVariantNonGatePath(t *testing.T) {
	for _, path := range []string{
		"Docs/Design.md",
		"internal/Scheduler/scheduler.go",
		"internal/watch/Watchdog.go",
		"content/agents/Golang-Pro.md",
		"README.MD",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardTreeDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if touched {
				t.Fatalf("%s is not a gate-definition path under any casing, but the guard refused it as %q", path, name)
			}
		})
	}
}

// TestGateGuardRefusesMakeEntrypoints covers the second critical. .ttorch/validate.sh runs bare
// `make lint` and `make test-fast`, and GNU Make reads GNUmakefile in preference to Makefile,
// with lowercase makefile also ahead of it (checked against the GNU Make 3.81 on this machine;
// see EVIDENCE.md). Guarding only Makefile left the hole the round-2 commit message claimed to
// close: a worker commits an unguarded GNUmakefile whose targets are @true and the gate is green
// with nothing run.
func TestGateGuardRefusesMakeEntrypoints(t *testing.T) {
	for _, path := range []string{"Makefile", "GNUmakefile", "makefile"} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardTreeDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if !touched {
				t.Fatalf("%s is a make entrypoint the gate's validate would execute; it must refuse the auto-merge", path)
			}
			if name != path {
				t.Fatalf("refusal named %q, want %q", name, path)
			}
		})
	}
}

// TestGateGuardRefusesBuildAndDeliveryInputs covers the pattern behind the remaining highs: a
// guarded thing whose behaviour is defined by an unguarded thing.
//
//   - go.mod / go.sum choose the dependency versions every guarded package compiles against.
//   - the ROOT package declares `var Content embed.FS` (content.go), the payload that carries
//     the guarded reviewer definitions, and it is a mutable package var like any other. Matched
//     by rule, not by filename, so a file ADDED to that package is covered.
//   - internal/installer and internal/manifest are what lay those reviewer definitions down
//     under ~/.claude/agents, which is where the gate dispatches them from.
//   - internal/harness composes the reviewer's command line, model and effort and writes its
//     settings. The round-2 comment cleared it BY NAME as settled, which was wrong.
//   - internal/paths locates the approval token, the verdict's review inputs and the validate
//     cache. Redirect those and the gate reads an attacker's file.
//   - internal/tmux and internal/termtab are how the reviewer command actually gets run.
func TestGateGuardRefusesBuildAndDeliveryInputs(t *testing.T) {
	for _, path := range []string{
		"go.mod",
		"go.sum",
		"content.go",
		"content_test.go",
		"internal/harness/harness.go",
		"internal/paths/paths.go",
		"internal/tmux/tmux.go",
		"internal/termtab/termtab.go",
		"internal/installer/installer.go",
		"internal/manifest/manifest.go",
	} {
		t.Run(path, func(t *testing.T) {
			repo, base, rev := gateGuardTreeDiff(t, path)
			touched, name, err := diffTouchesGateConfig(repo, base, rev)
			if err != nil {
				t.Fatalf("guard could not evaluate the diff: %v", err)
			}
			if !touched {
				t.Fatalf("a diff touching %s changes what the gate decides or what it dispatches; it must refuse the auto-merge", path)
			}
			if name != path {
				t.Fatalf("refusal named %q, want %q", name, path)
			}
		})
	}
}

// TestGuardCoversEveryFirstPartyGateDependency derives the guarded Go set instead of trusting a
// hand-written list to keep up. Per-package triage of "could this one manufacture a pass" is the
// reasoning that failed twice: it permitted spawn.go in round 1 and cleared internal/harness by
// name in round 2. So the rule is mechanical - every first-party package in the transitive
// import closure of the gate and of the reviewer-delivery path is guarded - and this test
// re-derives that closure with `go list -deps` and fails if any member is uncovered.
func TestGuardCoversEveryFirstPartyGateDependency(t *testing.T) {
	root := filepath.Join("..", "..")
	args := append([]string{"list", "-deps"}, gateClosureRoots...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	const mod = "github.com/nution101/ttorch"
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != mod && !strings.HasPrefix(line, mod+"/") {
			continue // third-party or stdlib
		}
		seen++
		rel := strings.TrimPrefix(strings.TrimPrefix(line, mod), "/")
		probe := "content.go" // the root package is matched by its Go files
		if rel != "" {
			probe = rel + "/anything.go"
		}
		if !isGateDefinition(probe) {
			t.Errorf("the gate transitively imports %s, so any file in it can change what the gate decides, but %s is not guarded", line, probe)
		}
	}
	if seen < 15 {
		t.Fatalf("go list returned only %d first-party packages; the closure looks wrong, refusing to pass vacuously", seen)
	}
}

// TestRootPackageIsFullyGuarded covers the root package the way the prefixes cover the others.
// It cannot be a path rule in the guard, because this guard runs against every managed repo and
// "any .go at the repo root" would refuse an ordinary main.go in any project that keeps one. So
// the guard names content.go and content_test.go, and this test fails if the root package grows
// a file those entries do not cover. A red test here turns this repo's validate red, and a
// trusted auto-merge requires a fresh green, so a root file added to rebind Content cannot
// carry itself in.
func TestRootPackageIsFullyGuarded(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		found++
		if !isGateDefinition(e.Name()) {
			t.Errorf("%s is in the repo root package, which declares Content (the embedded reviewer definitions), so it can rebind it; add it to gateDefinitionFiles", e.Name())
		}
	}
	if found == 0 {
		t.Fatal("no .go files found in the repo root; this test would pass vacuously")
	}
}

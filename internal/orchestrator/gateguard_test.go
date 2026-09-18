package orchestrator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestGateGuardPermitsOrdinarySource covers acceptance criterion 4: trusted mode must stay
// useful. Ordinary source, docs and non-gate content agents are NOT the gate definition and
// must still auto-merge. This is the regression that would make people disable the guard, so
// it also pins the breadth line: internal/worktree and internal/scheduler are DELIBERATELY
// outside the guard (see gateguard.go for why).
func TestGateGuardPermitsOrdinarySource(t *testing.T) {
	for _, path := range []string{
		"README.md",
		"docs/design.md",
		"cmd/ttorch/main.go",
		"internal/orchestrator/spawn.go",
		"internal/orchestrator/landqueue.go",
		"internal/termtab/termtab.go",
		"internal/worktree/worktree.go",
		"internal/scheduler/scheduler.go",
		"content/agents/golang-pro.md",
		"content/agents/ttorch-worker.md",
		"content/skills/ttorch-manager/SKILL.md",
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
	for _, f := range gateDefinitionFiles {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("gateDefinitionFiles names %s, which is not in the repo: %v", f, err)
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
	for _, p := range append(append([]string{}, gateDefinitionFiles...), gateDefinitionPrefixes...) {
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

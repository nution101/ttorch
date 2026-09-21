package review

import (
	"strings"
	"testing"
)

func dimSet(dims []string) string { return strings.Join(dims, "+") }

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		files    []string
		lines    int
		binary   bool
		ok       bool
		wantSize Size
		wantDims string
	}{
		{
			name:  "docs-only markdown drops security",
			files: []string{"README.md", "docs/guide.md"},
			lines: 6, ok: true,
			wantSize: SizeDocsOnly,
			wantDims: "correctness+scope",
		},
		{
			name:  "extensionless LICENSE is docs-only",
			files: []string{"LICENSE"},
			lines: 2, ok: true,
			wantSize: SizeDocsOnly,
			wantDims: "correctness+scope",
		},
		{
			name:  "tiny single-file code change drops scope, keeps security",
			files: []string{"internal/paths/paths.go"},
			lines: 5, ok: true,
			wantSize: SizeTrivial,
			wantDims: "correctness+security",
		},
		{
			name:  "single code file over the line budget is substantial",
			files: []string{"internal/paths/paths.go"},
			lines: trivialLineBudget + 1, ok: true,
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:  "multi-file code change is substantial",
			files: []string{"a.go", "b.go"},
			lines: 4, ok: true,
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:  "code mixed with docs is substantial, not docs-only",
			files: []string{"README.md", "main.go"},
			lines: 4, ok: true,
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:  "config files (yaml/json) are code, not docs",
			files: []string{"config.yaml"},
			lines: 3, ok: true,
			wantSize: SizeTrivial, // single small file, but still gets security (code path)
			wantDims: "correctness+security",
		},
		{
			name:  "binary blob is never trivial",
			files: []string{"logo.png"},
			lines: 0, binary: true, ok: true,
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, dims := Classify(tc.files, tc.lines, tc.binary, tc.ok)
			if size != tc.wantSize {
				t.Errorf("size = %q, want %q", size, tc.wantSize)
			}
			if got := dimSet(dims); got != tc.wantDims {
				t.Errorf("dims = %q, want %q", got, tc.wantDims)
			}
		})
	}
}

// TestClassifyFailsClosed covers the security-critical fail-closed paths: any uncertainty
// about the authoritative file list must yield the full set, never a reduced one.
func TestClassifyFailsClosed(t *testing.T) {
	full := "correctness+scope+security"
	cases := []struct {
		name  string
		files []string
		ok    bool
	}{
		{name: "git stat failed (ok=false)", files: []string{"README.md"}, ok: false},
		{name: "empty file list", files: nil, ok: true},
		{name: "empty path entry (incomplete parse)", files: []string{"README.md", ""}, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, dims := Classify(tc.files, 1, false, tc.ok)
			if size != SizeSubstantial || dimSet(dims) != full {
				t.Fatalf("expected full set, got size=%q dims=%q", size, dimSet(dims))
			}
		})
	}
}

// TestClassifyQuotedCodeFilenameKeepsSecurity is the regression test for the gate-bypass:
// a non-ASCII / control-char code filename — which git QUOTES in a patch body and a
// scraper would silently drop — must, when sourced from the authoritative unquoted list,
// keep the change classified as code and KEEP the security reviewer, even when paired with
// an innocuous docs edit.
func TestClassifyQuotedCodeFilenameKeepsSecurity(t *testing.T) {
	hostile := []string{
		"café.go",        // non-ASCII bytes → quoted in a patch body
		"a\tb.go",        // embedded tab → quoted
		"evil\".go",      // embedded quote → quoted
		"back\\slash.go", // embedded backslash → quoted
	}
	for _, name := range hostile {
		// Paired with a docs edit, exactly the attack: make it look docs-only.
		files := []string{"README.md", name}
		size, dims := Classify(files, 3, false, true)
		if !contains(dims, DimensionSecurity) {
			t.Errorf("security dropped for code file %q paired with docs (size=%s dims=%v)", name, size, dims)
		}
		if size == SizeDocsOnly {
			t.Errorf("a diff containing code file %q must not classify as docs-only", name)
		}
	}
}

// TestClassifyNeverDropsSecurityForCode is the broad safety invariant: any file set
// carrying a non-doc (code/config) file must keep the security dimension.
func TestClassifyNeverDropsSecurityForCode(t *testing.T) {
	codeSets := [][]string{
		{"main.go"},
		{"deploy.sh"},
		{"Dockerfile"},
		{"secrets.yaml"},
		{"index.html"},
		{"README.md", "auth.go"},
	}
	for _, files := range codeSets {
		size, dims := Classify(files, 1, false, true)
		if !contains(dims, DimensionSecurity) {
			t.Errorf("security dropped for a code set (size=%s, files=%v, dims=%v)", size, files, dims)
		}
	}
}

func TestClassifyReturnsFreshSlice(t *testing.T) {
	_, a := Classify([]string{"a.go", "b.go"}, 4, false, true)
	a[0] = "mutated"
	_, b := Classify([]string{"a.go", "b.go"}, 4, false, true)
	if b[0] == "mutated" {
		t.Fatal("Classify returned a slice aliasing shared state")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestClassify_HarnessConfigIsNotInertProse pins the hole an adversarial review found in the
// trust-model step 2 pilot. A session's harness configuration — CLAUDE.md, AGENTS.md, anything
// under .claude/ — carries instructions and hooks that change how an agent behaves, so it is
// not inert prose however it is spelled. Two of those paths end in .md, and classifying a diff
// of them as docs-only drops the security reviewer, which is the dimension that runs outside
// the worker's worktree. A worker could then subvert every reviewer that did run by committing
// only .md files.
func TestClassify_HarnessConfigIsNotInertProse(t *testing.T) {
	cases := []struct {
		name  string
		files []string
	}{
		{"root CLAUDE.md alone", []string{"CLAUDE.md"}},
		{"AGENTS.md alone", []string{"AGENTS.md"}},
		{"a reviewer's own agent definition", []string{".claude/agents/ttorch-reviewer-security.md"}},
		{"harness config spelled entirely in .md", []string{
			"CLAUDE.md",
			".claude/agents/ttorch-reviewer-correctness.md",
			".claude/agents/ttorch-reviewer-scope.md",
		}},
		{"a nested package's CLAUDE.md", []string{"internal/orchestrator/CLAUDE.md"}},
		{"harness config mixed with real prose", []string{"README.md", ".claude/settings.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, dims := Classify(tc.files, 5, false, true)
			if size == SizeDocsOnly {
				t.Fatalf("harness config must not classify as docs-only: %v -> %s %v", tc.files, size, dims)
			}
			if !contains(dims, DimensionSecurity) {
				t.Fatalf("harness config must keep the security reviewer: %v -> %s %v", tc.files, size, dims)
			}
		})
	}
}

// TestClassify_RealProseIsStillDocsOnly is the must-not-trip side: ordinary documentation still
// gets the reduced set. Widening the code bucket to cover harness config must not turn every
// docs change into a three-dimension pass.
func TestClassify_RealProseIsStillDocsOnly(t *testing.T) {
	files := []string{"README.md", "docs/design.md", "CHANGELOG", "LICENSE", "notes/claude-usage.md"}
	size, dims := Classify(files, 500, false, true)
	if size != SizeDocsOnly {
		t.Fatalf("ordinary prose must stay docs-only: %v -> %s %v", files, size, dims)
	}
	if contains(dims, DimensionSecurity) {
		t.Fatalf("ordinary prose must not require a security review: %v", dims)
	}
}

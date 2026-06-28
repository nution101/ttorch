package review

import (
	"fmt"
	"strings"
	"testing"
)

// diffFor builds a minimal but realistic unified diff that adds n lines to each of the
// given paths, so the classifier sees both the file headers and the content line counts.
func diffFor(t *testing.T, lines int, paths ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", p, p)
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", p, p)
		b.WriteString("@@ -0,0 +1," + fmt.Sprint(lines) + " @@\n")
		for i := 0; i < lines; i++ {
			fmt.Fprintf(&b, "+line %d\n", i)
		}
	}
	return b.String()
}

func dimSet(dims []string) string { return strings.Join(dims, "+") }

func TestReviewers(t *testing.T) {
	cases := []struct {
		name     string
		diff     string
		wantSize Size
		wantDims string
	}{
		{
			name:     "docs-only markdown drops security",
			diff:     diffFor(t, 3, "README.md", "docs/guide.md"),
			wantSize: SizeDocsOnly,
			wantDims: "correctness+scope",
		},
		{
			name:     "extensionless LICENSE is docs-only",
			diff:     diffFor(t, 2, "LICENSE"),
			wantSize: SizeDocsOnly,
			wantDims: "correctness+scope",
		},
		{
			name:     "tiny single-file code change drops scope, keeps security",
			diff:     diffFor(t, 5, "internal/paths/paths.go"),
			wantSize: SizeTrivial,
			wantDims: "correctness+security",
		},
		{
			name:     "single code file over the line budget is substantial",
			diff:     diffFor(t, trivialLineBudget+1, "internal/paths/paths.go"),
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:     "multi-file code change is substantial",
			diff:     diffFor(t, 2, "a.go", "b.go"),
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:     "code mixed with docs is substantial, not docs-only",
			diff:     diffFor(t, 2, "README.md", "main.go"),
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:     "empty diff falls back to the full set",
			diff:     "",
			wantSize: SizeSubstantial,
			wantDims: "correctness+scope+security",
		},
		{
			name:     "config files (yaml/json) are code, not docs",
			diff:     diffFor(t, 3, "config.yaml"),
			wantSize: SizeTrivial, // single small file, but still gets security (code path)
			wantDims: "correctness+security",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, dims := Reviewers(tc.diff)
			if size != tc.wantSize {
				t.Errorf("size = %q, want %q", size, tc.wantSize)
			}
			if got := dimSet(dims); got != tc.wantDims {
				t.Errorf("dims = %q, want %q", got, tc.wantDims)
			}
		})
	}
}

// TestReviewersNeverDropsSecurityForCode is the safety invariant: any diff carrying a
// non-doc (code/config) file must keep the security dimension.
func TestReviewersNeverDropsSecurityForCode(t *testing.T) {
	codeDiffs := []string{
		diffFor(t, 1, "main.go"),
		diffFor(t, 100, "main.go"),
		diffFor(t, 3, "deploy.sh"),
		diffFor(t, 3, "Dockerfile"),
		diffFor(t, 3, "secrets.yaml"),
		diffFor(t, 3, "index.html"),
		diffFor(t, 3, "README.md", "auth.go"),
	}
	for _, d := range codeDiffs {
		size, dims := Reviewers(d)
		if !contains(dims, DimensionSecurity) {
			t.Errorf("security dropped for a code diff (size=%s, dims=%v):\n%s", size, dims, d)
		}
	}
}

// TestReviewersBinaryIsSubstantial verifies a binary blob is never treated as trivial —
// its change size is unknowable, so it falls back to the full set.
func TestReviewersBinaryIsSubstantial(t *testing.T) {
	diff := "diff --git a/logo.png b/logo.png\n" +
		"new file mode 100644\n" +
		"Binary files /dev/null and b/logo.png differ\n"
	size, dims := Reviewers(diff)
	if size != SizeSubstantial {
		t.Errorf("binary blob size = %q, want %q", size, SizeSubstantial)
	}
	if dimSet(dims) != "correctness+scope+security" {
		t.Errorf("binary blob dims = %q, want full set", dimSet(dims))
	}
}

func TestReviewersReturnsFreshSlice(t *testing.T) {
	_, a := Reviewers(diffFor(t, 2, "a.go", "b.go"))
	a[0] = "mutated"
	_, b := Reviewers(diffFor(t, 2, "a.go", "b.go"))
	if b[0] == "mutated" {
		t.Fatal("Reviewers returned a slice aliasing shared state")
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

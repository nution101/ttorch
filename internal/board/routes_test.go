package board

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// authority matches any name that sounds like approving, merging or landing.
var authority = regexp.MustCompile(`(?i)approv|merge|land|grant|consume|record`)

// TestNoRouteApprovesOrMerges pins the board's whole HTTP surface. Adding a route means
// editing this list, and no route may approve, merge or land: `ttorch approve` refuses to run
// without a terminal on purpose, and a board route would be a way round that.
func TestNoRouteApprovesOrMerges(t *testing.T) {
	h := newHarness(t)
	var got []string
	for path := range h.srv.routes() {
		got = append(got, path)
		if authority.MatchString(path) {
			t.Errorf("route %q looks like it approves, merges or lands", path)
		}
	}
	sort.Strings(got)
	want := []string{"/", "/api/answer", "/api/board", "/api/dispatch", "/api/gate-prep"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("board routes = %v, want exactly %v", got, want)
	}

	// Fully authorized requests to the obvious names find nothing to act through.
	h.addTask(taskDone("t1"))
	for _, path := range []string{
		"/approve", "/api/approve", "/api/approval", "/api/merge", "/api/merge-local",
		"/api/land", "/api/trust-record", "/api/approve/t1", "/api/answer/../approve",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			r := request{method: method, path: path, origin: h.base, headerToken: h.srv.token}
			if method == http.MethodPost {
				r.form = url.Values{"task": {"t1"}}
			}
			if got := h.do(r); got.code != http.StatusNotFound {
				t.Errorf("%s %s: status %d, want 404", method, path, got.code)
			}
		}
	}
}

// TestFleetCannotApproveOrMerge holds the interface the board acts through to methods that
// grant nothing. A method added to Fleet that approves, merges or records a verdict fails
// here.
func TestFleetCannotApproveOrMerge(t *testing.T) {
	ft := reflect.TypeOf((*Fleet)(nil)).Elem()
	var names []string
	for i := 0; i < ft.NumMethod(); i++ {
		name := ft.Method(i).Name
		names = append(names, name)
		if authority.MatchString(name) {
			t.Errorf("Fleet.%s looks like it approves, merges or records a verdict", name)
		}
	}
	sort.Strings(names)
	want := "ReviewersFor Send SpawnAutonomous TaskState TrustPrep"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("Fleet methods = %s, want exactly %s", got, want)
	}
}

// TestPackageReachesNoApprovalCode reads the package's own source: it must not import the
// approval store, and must not call anything named like an approval or merge operation.
func TestPackageReachesNoApprovalCode(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files; the check proved nothing")
	}
	banned := map[string]bool{
		"Approve": true, "MergeLocal": true, "Land": true, "LandSet": true,
		"TrustRecord": true, "Grant": true, "Consume": true, "Repin": true,
	}
	for name, f := range files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasSuffix(p, "/internal/approval") {
				t.Errorf("%s imports %s", name, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch x := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = x.Sel.Name
			case *ast.Ident:
				fn = x.Name
			}
			if banned[fn] {
				t.Errorf("%s calls %s at %s", name, fn, fset.Position(call.Pos()))
			}
			return true
		})
	}
}

// TestApprovalShowsTheCommandNotAButton: a task awaiting approval is listed with the exact
// command the lead runs, and the page offers no form or button for it.
func TestApprovalShowsTheCommandNotAButton(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Mode = func(string) string { return "local" }
	h.addTask(taskDone("needs-ok"))
	for _, r := range []response{h.page(), h.sections()} {
		if !strings.Contains(r.body, "<code>ttorch approve needs-ok</code>") {
			t.Fatalf("approval card is missing the command:\n%s", r.body)
		}
		if strings.Contains(r.body, `data-action="/api/approve`) || strings.Contains(strings.ToLower(r.body), ">approve<") {
			t.Fatalf("the page offers an approval control:\n%s", r.body)
		}
	}
}

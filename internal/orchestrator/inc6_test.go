package orchestrator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot walks up from this test file to the directory holding go.mod — the
// repository root — so the source-scanning invariants below cover the whole tree
// regardless of the working directory the test runs in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", file)
		}
		dir = parent
	}
}

// goSourceFiles returns every non-test .go file under root.
func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// isManagerWindowExpr reports whether an AST expression denotes the manager tmux window:
// the "manager" string literal (how the orchestrator's launch sites name it) or an
// identifier/selector spelled like managerWindow (how the retired supervisor poke named
// it, and how internal/watch refers to it via a const). The manager→worker `ttorch send`
// path addresses a *worker* window (t.Window), so it is not matched.
func isManagerWindowExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if v, err := strconv.Unquote(e.Value); err == nil {
				return v == "manager"
			}
		}
	case *ast.Ident:
		return strings.Contains(strings.ToLower(e.Name), "managerwindow")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(e.Sel.Name), "managerwindow")
	}
	return false
}

// managerWindowVars returns the identifiers that, anywhere in f, are bound to the manager
// window — `w := "manager"`, `w := managerWindow`, a `var`/`const` initializer, or a later
// `w = …` reassignment. This is exactly the indirection a line-by-line scan misses: a poke
// can stash the manager window in a variable on one line and SendLine(sess, that,
// directive) on the next. Collecting file-wide is a deliberate fail-closed
// over-approximation for a security invariant; no production file binds a worker-window
// variable to the "manager" value, so it yields no false positives here.
func managerWindowVars(f *ast.File) map[string]bool {
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(s.Rhs) && isManagerWindowExpr(s.Rhs[i]) {
					vars[id.Name] = true
				}
			}
		case *ast.ValueSpec: // var/const x = expr
			for i, name := range s.Names {
				if i < len(s.Values) && isManagerWindowExpr(s.Values[i]) {
					vars[name.Name] = true
				}
			}
		}
		return true
	})
	return vars
}

// mentionsManagerLaunch reports whether any sub-expression is a harness.Manager… call —
// the launch/resume bootstrap that types the `claude …` startup command into a freshly
// created manager window to *create* the session. That is categorically distinct from
// injecting a directive into a running manager (the retired poke), so it is exempt.
func mentionsManagerLaunch(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "harness" && strings.HasPrefix(sel.Sel.Name, "Manager") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// detectManagerInjection parses Go source and returns the 1-based line numbers of every
// forbidden send into the manager session: a tmux.SendLine/SendKey whose window argument
// resolves to the manager window — directly OR through a local variable — carrying
// anything other than a harness.Manager… launch command. Operating on the AST (rather
// than one line at a time) is what lets it catch the indirect, variable-laundered form.
func detectManagerInjection(fset *token.FileSet, f *ast.File) []int {
	mgrVars := managerWindowVars(f)
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "tmux" || (sel.Sel.Name != "SendLine" && sel.Sel.Name != "SendKey") {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		win := call.Args[1] // SendLine/SendKey(session, window, payload)
		toManager := isManagerWindowExpr(win)
		if id, ok := win.(*ast.Ident); ok && mgrVars[id.Name] {
			toManager = true
		}
		if toManager && !mentionsManagerLaunch(call) {
			lines = append(lines, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return lines
}

// TestNoInjectionIntoManagerSession is the increment-6 net invariant: after retiring the
// supervisor, NO code path types into the manager session. The supervisor's poke
// (tmux.SendLine into the "manager" window carrying a directive) was the only such path;
// with it gone, the ONLY remaining tmux.SendLine/SendKey calls that target the manager
// window are the launch/resume bootstrap, which start the session rather than inject into
// a running one. This parses all non-test Go source and fails if any send to the manager
// window — including one laundered through a local variable — carries anything other than
// a harness.Manager… launch command.
//
// The manager→worker `ttorch send` path (SendLine into a WORKER window) is unaffected: its
// window argument never resolves to the manager window, so it is never matched.
func TestNoInjectionIntoManagerSession(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range goSourceFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, line := range detectManagerInjection(fset, f) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, fmt.Sprintf("%s:%d", rel, line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("forbidden injection into the manager session — tmux.SendLine/SendKey to the manager "+
			"window carrying something other than a harness.Manager… launch command:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestManagerInjectionDetector pins the detector so the invariant test cannot pass
// vacuously: it must FLAG a poke under every spelling — including the indirect form where
// the manager window is laundered through a local variable — and PASS both a manager
// launch/resume (even when laundered) and a send to a worker window.
func TestManagerInjectionDetector(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		injection bool // want: flagged as injection into the manager session
	}{
		{"direct ident poke", `tmux.SendLine(s.Session, managerWindow, pokeDirective)`, true},
		{"direct literal poke", `_ = tmux.SendLine(m.Session, "manager", "ttorch wake: drain and advance")`, true},
		{"sendkey poke", `tmux.SendKey(m.Session, "manager", "Enter")`, true},
		{"indirect via local literal var", "w := \"manager\"\n_ = tmux.SendLine(m.Session, w, \"drain and advance\")", true},
		{"indirect via local ident var", "w := managerWindow\ntmux.SendLine(s.Session, w, pokeDirective)", true},
		{"indirect via reassignment", "w := t.Window\nw = \"manager\"\ntmux.SendLine(m.Session, w, directive)", true},
		{"manager launch (literal)", `_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(harness.Resolve(), sid, m.charterFile()))`, false},
		{"manager resume (literal)", `_ = tmux.SendLine(m.Session, "manager", harness.ManagerResumeOrFresh(h, mgr.SessionID, m.charterFile()))`, false},
		{"manager launch laundered still exempt", "w := \"manager\"\n_ = tmux.SendLine(m.Session, w, harness.ManagerCommand(h, sid, cf))", false},
		{"worker send", `return tmux.SendLine(m.Session, t.Window, text)`, false},
		{"worker launch via var", "window := t.Window\nif err := tmux.SendLine(m.Session, window, cmd); err != nil { _ = err }", false},
	}
	for _, c := range cases {
		src := "package p\nfunc f() {\n" + c.body + "\n}\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", src, 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		if flagged := len(detectManagerInjection(fset, f)) > 0; flagged != c.injection {
			t.Errorf("%s: detector = %v, want %v", c.name, flagged, c.injection)
		}
	}
}

// TestSupervisorAndWakeRetired proves the increment-6 deletions: the supervisor daemon
// and the wake-queue packages are gone, nothing imports them, spawn starts no daemon,
// and no source revives the daemon-start path or the retired `ttorch daemon` verb.
func TestSupervisorAndWakeRetired(t *testing.T) {
	root := moduleRoot(t)

	for _, dir := range []string{"internal/supervisor", "internal/wake"} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Errorf("%s must be deleted entirely (stat err = %v)", dir, err)
		}
	}

	// Tokens that, if present in production source, would mean the retired machinery
	// (or a path that starts the daemon) is still wired up. The detached daemon was
	// launched via `<binary> daemon run`, so that exact verb must be gone too.
	forbidden := []string{
		"internal/supervisor",
		"internal/wake",
		"ensureSupervisor",
		"supervisor.Start",
		"daemon run",
	}
	for _, path := range goSourceFiles(t, root) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		for _, tok := range forbidden {
			if strings.Contains(src, tok) {
				t.Errorf("%s still references retired machinery %q", rel, tok)
			}
		}
	}
}

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

// The ONE send into the manager window permitted outside the launch/resume bootstrap: the
// scheduler's production wiring of the API-stall recovery seam (scheduler.New →
// wireManagerStallNudgeSeams). The lead authorized resuming a genuinely-stalled manager — which
// cannot nudge itself — and isSanctionedStallNudge pins the exemption to THREE independent facts so
// it admits exactly that site and nothing else:
//
//   - sanctionedStallNudgeFile: the send must live in internal/scheduler/scheduler.go, so a
//     same-named function added in ANY other file/package is not exempt;
//   - sanctionedStallNudgeFunc, as a TOP-LEVEL (non-method) FuncDecl: a method of that name, or any
//     other function, is not exempt; and
//   - a fixed "continue" string LITERAL payload (isFixedContinueLiteral): NOT an identifier (which a
//     local could shadow to launder arbitrary content past a value-blind name check), not a
//     concatenation, not a call — so the manager can only ever receive the one literal resume word.
//
// The RUNTIME guard that the nudge fires ONLY when livestate.APIStalled(pane) holds, bounded per
// episode, lives in scheduler.recoverStall and is covered by the scheduler's stall-recovery tests (a
// HEALTHY manager is never injected into); this source-scan invariant guards the complementary
// property — that no OTHER write into the manager window can be introduced.
const (
	sanctionedStallNudgeFunc = "wireManagerStallNudgeSeams"
	sanctionedStallNudgeFile = "internal/scheduler/scheduler.go"
	sanctionedNudgePayload   = "continue"
)

// inSanctionedNudgeFunc reports whether pos lies inside the body of the ONE sanctioned wiring
// function: a TOP-LEVEL (non-method) FuncDecl named sanctionedStallNudgeFunc. A method of that name
// (Recv != nil), or pos outside every such function, is rejected — so the name alone cannot launder
// an injection (fail closed).
func inSanctionedNudgeFunc(f *ast.File, pos token.Pos) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil { // a method named the same is NOT the sanctioned wiring function
			continue
		}
		if fn.Name.Name == sanctionedStallNudgeFunc && fn.Pos() <= pos && pos <= fn.End() {
			return true
		}
	}
	return false
}

// isFixedContinueLiteral reports whether expr is the exact "continue" STRING LITERAL — not an
// identifier (an ident is value-blind: a local var of any name, even one matching a package const,
// could carry interpolated/attacker-influenced content), not a concatenation, not a call. Requiring
// a literal is what statically proves the manager pane can only ever receive the fixed resume word.
func isFixedContinueLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == sanctionedNudgePayload
}

// isSanctionedStallNudge reports whether a manager-targeting tmux send is THE one allow-listed
// API-stall recovery nudge: it is in the sanctioned file AND inside the sanctioned top-level wiring
// function AND carries the fixed "continue" string literal. Any of those failing — a different file,
// a different/method function, an ident or interpolated/arbitrary payload — leaves it flagged.
func isSanctionedStallNudge(f *ast.File, filename string, call *ast.CallExpr) bool {
	if !strings.HasSuffix(filepath.ToSlash(filename), sanctionedStallNudgeFile) {
		return false
	}
	if !inSanctionedNudgeFunc(f, call.Pos()) {
		return false
	}
	if len(call.Args) < 3 {
		return false // SendLine(session, window, payload) — no payload to verify
	}
	return isFixedContinueLiteral(call.Args[2])
}

// detectManagerInjection parses Go source and returns the 1-based line numbers of every
// forbidden send into the manager session: a tmux.SendLine/SendKey whose window argument
// resolves to the manager window — directly OR through a local variable — carrying anything
// other than (a) a harness.Manager… launch command or (b) the single allow-listed API-stall
// recovery nudge (a SendLine of the fixed "continue" literal from the sanctioned file+function;
// see isSanctionedStallNudge). Operating on the AST (rather than one line at a time) is what lets
// it catch the indirect, variable-laundered form, and locate each send's file + enclosing function
// for the allow-list.
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
		if !toManager {
			return true
		}
		if mentionsManagerLaunch(call) {
			return true // the launch/resume bootstrap — exempt (it creates the session, never injects)
		}
		// The single allow-listed API-stall recovery nudge — restricted to SendLine (a SendKey to the
		// manager is never sanctioned) in the pinned file+function carrying the fixed literal.
		if sel.Sel.Name == "SendLine" && isSanctionedStallNudge(f, fset.Position(call.Pos()).Filename, call) {
			return true
		}
		lines = append(lines, fset.Position(call.Pos()).Line)
		return true
	})
	return lines
}

// TestNoInjectionIntoManagerSession is the increment-6 net invariant, evolved: NO code path may
// type into the manager session EXCEPT two sanctioned ones. The supervisor's poke (tmux.SendLine
// into the "manager" window carrying a directive) was retired; the remaining permitted sends are
// (1) the launch/resume bootstrap, which START the session rather than inject into a running one,
// and (2) the single API-stall recovery nudge — the fixed "continue" resume the scheduler's
// production wiring (sanctionedStallNudgeFunc) sends to a genuinely-stalled manager, lead-authorized
// so the daemon can recover a manager that cannot recover itself. This parses all non-test Go source
// and FAILS if any send to the manager window — including one laundered through a local variable —
// carries anything other than a harness.Manager… launch command OR is anything but that one
// allow-listed nudge: a manager send from any other function, or carrying interpolated/arbitrary
// content even from the sanctioned function, still fails the build (see isSanctionedStallNudge).
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

// TestManagerInjectionDetector pins the detector so the invariant test cannot pass vacuously: it
// must FLAG a poke under every spelling — including the indirect form laundered through a local
// variable — and PASS a manager launch/resume (even when laundered), a send to a worker window, and
// the one allow-listed API-stall recovery nudge. It also pins the allow-list as TIGHT against the
// exact bypasses an adversarial review surfaced: the sanctioned exemption requires ALL THREE of the
// right file, the right top-level (non-method) function, and a fixed "continue" LITERAL — so the same
// send from another file, a same-named function/method elsewhere, an identifier payload (launderable
// via a local shadow), or any interpolated/arbitrary content STILL fails. file/fn/recv let a case
// place its send in a chosen file, function, and (optionally) on a receiver; defaults put it in a
// non-sanctioned file ("x.go") and a plain func "f".
func TestManagerInjectionDetector(t *testing.T) {
	cases := []struct {
		name      string
		file      string // parsed filename (defaults to a non-sanctioned "x.go")
		fn        string // enclosing function name (defaults to "f")
		recv      string // receiver spec, e.g. "a *T" → makes fn a METHOD (defaults to a plain func)
		body      string
		injection bool // want: flagged as injection into the manager session
	}{
		{name: "direct ident poke", body: `tmux.SendLine(s.Session, managerWindow, pokeDirective)`, injection: true},
		{name: "direct literal poke", body: `_ = tmux.SendLine(m.Session, "manager", "ttorch wake: drain and advance")`, injection: true},
		{name: "sendkey poke", body: `tmux.SendKey(m.Session, "manager", "Enter")`, injection: true},
		{name: "indirect via local literal var", body: "w := \"manager\"\n_ = tmux.SendLine(m.Session, w, \"drain and advance\")", injection: true},
		{name: "indirect via local ident var", body: "w := managerWindow\ntmux.SendLine(s.Session, w, pokeDirective)", injection: true},
		{name: "indirect via reassignment", body: "w := t.Window\nw = \"manager\"\ntmux.SendLine(m.Session, w, directive)", injection: true},
		{name: "manager launch (literal)", body: `_ = tmux.SendLine(m.Session, "manager", harness.ManagerCommand(harness.Resolve(), sid, m.charterFile()))`, injection: false},
		{name: "manager resume (literal)", body: `_ = tmux.SendLine(m.Session, "manager", harness.ManagerResumeOrFresh(h, mgr.SessionID, m.charterFile()))`, injection: false},
		{name: "manager launch laundered still exempt", body: "w := \"manager\"\n_ = tmux.SendLine(m.Session, w, harness.ManagerCommand(h, sid, cf))", injection: false},
		{name: "worker send", body: `return tmux.SendLine(m.Session, t.Window, text)`, injection: false},
		{name: "worker launch via var", body: "window := t.Window\nif err := tmux.SendLine(m.Session, window, cmd); err != nil { _ = err }", injection: false},
		// The ONE allow-listed API-stall recovery nudge: a SendLine of the fixed "continue" LITERAL,
		// in the sanctioned file AND the sanctioned top-level wiring function. Exempt under either
		// window spelling (literal "manager" or the managerWindow ident).
		{name: "sanctioned nudge (managerWindow ident)", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: false},
		{name: "sanctioned nudge (literal window)", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, "manager", "continue")`, injection: false},
		// TIGHT: the right function+literal but the WRONG file is still an injection (file scoping).
		{name: "sanctioned func+literal but wrong file", file: "internal/scheduler/other.go", fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: true},
		// TIGHT: the right file+literal but the WRONG (non-sanctioned) function is still an injection.
		{name: "right file+literal but wrong function", file: sanctionedStallNudgeFile, fn: "somethingElse", body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: true},
		// TIGHT: a same-named METHOD (receiver) — even in the sanctioned file — is NOT the wiring func.
		{name: "sanctioned name as a method is not exempt", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, recv: "a *T", body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: true},
		// TIGHT: an IDENTIFIER payload (even the const's name) is NOT exempt — only a literal is, so a
		// local shadow `stallNudgeText := <arbitrary>` cannot launder content past a value-blind check.
		{name: "ident payload in sanctioned site is not exempt", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, stallNudgeText)`, injection: true},
		// TIGHT: interpolated / arbitrary / wrong-literal payloads in the sanctioned site still fail.
		{name: "interpolated payload in sanctioned site", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: "return tmux.SendLine(session, managerWindow, \"con\"+\"tinue\")", injection: true},
		{name: "arbitrary ident payload in sanctioned site", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, attacker)`, injection: true},
		{name: "wrong literal in sanctioned site", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, "rm -rf /")`, injection: true},
		// TIGHT: a SendKey is never the sanctioned nudge, even of the literal in the sanctioned site.
		{name: "sendkey in sanctioned site is not exempt", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `tmux.SendKey(session, managerWindow, "continue")`, injection: true},
	}
	for _, c := range cases {
		fn := c.fn
		if fn == "" {
			fn = "f"
		}
		file := c.file
		if file == "" {
			file = "x.go"
		}
		recv := ""
		if c.recv != "" {
			recv = "(" + c.recv + ") "
		}
		src := "package p\nfunc " + recv + fn + "() {\n" + c.body + "\n}\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, src, 0)
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

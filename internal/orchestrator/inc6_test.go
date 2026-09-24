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

// The sends into the manager window permitted outside the launch/resume bootstrap. Each is
// pinned to THREE independent facts that must hold together, so the allow-list admits exactly
// those sites and nothing else:
//
//   - file: the send must live in that exact source file, compared as the path from the module
//     root, so a same-named function added in ANY other file/package is not exempt, and neither
//     is a copy of the sanctioned file under another directory (vendor/…/internal/watch/daemon.go);
//   - fn, as a TOP-LEVEL (non-method) FuncDecl: a method of that name, or any other function, is
//     not exempt; and
//   - payload, as a fixed string LITERAL (isFixedLiteral): NOT an identifier (which a local could
//     shadow to launder arbitrary content past a value-blind name check), not a concatenation, not
//     a call — so the manager can only ever receive that one literal line.
//
// The entries are matched whole: the stall-nudge site sending the wake line, or the wake site
// sending "continue", is still an injection.
//
// (1) The API-stall recovery nudge: the scheduler's production wiring (scheduler.New →
// wireManagerStallNudgeSeams). The lead authorized resuming a genuinely-stalled manager, which
// cannot nudge itself. The RUNTIME guard that it fires ONLY when livestate.APIStalled(pane) holds,
// bounded per episode, lives in scheduler.recoverStall and is covered by the scheduler's
// stall-recovery tests.
//
// (2) The scheduler watch loop's wake (watch.NewDaemon → wireManagerWake): the always-on
// watcher types one fixed line (tmux.TypeLine, no Enter) telling an idle manager to run
// `ttorch inbox`, re-reads the pane, and presses Enter (tmux.SendKey "Enter") only if the input
// holds exactly that line. Those are two entries, each pinned to its method. The RUNTIME guards
// (only with unread updates, never while awaiting the lead, only when the harness leads the
// pane's foreground, never into a busy pane or a non-empty prompt, Enter only after the typed
// line is confirmed, at most one outstanding wake) live in watch.Daemon.Tick and are covered by
// internal/watch/daemon_test.go.
//
// This source-scan invariant guards the complementary property: that no OTHER write into the
// manager window can be introduced.
const (
	sanctionedStallNudgeFunc = "wireManagerStallNudgeSeams"
	sanctionedStallNudgeFile = "internal/scheduler/scheduler.go"
	sanctionedNudgePayload   = "continue"

	sanctionedWakeFunc    = "wireManagerWake"
	sanctionedWakeFile    = "internal/watch/daemon.go"
	sanctionedWakePayload = "Automated notice from the ttorch scheduler, not the lead: unread worker updates, run ttorch inbox"
)

// sanctionedManagerSend is one allow-listed send: the tmux function it calls, the file and
// top-level function it sits in, and its literal payload (the key name, for SendKey).
type sanctionedManagerSend struct{ method, file, fn, payload string }

var sanctionedManagerSends = []sanctionedManagerSend{
	{"SendLine", sanctionedStallNudgeFile, sanctionedStallNudgeFunc, sanctionedNudgePayload},
	{"TypeLine", sanctionedWakeFile, sanctionedWakeFunc, sanctionedWakePayload},
	{"SendKey", sanctionedWakeFile, sanctionedWakeFunc, "Enter"},
}

// managerSendFuncs are the tmux functions that write into a window, and so are scanned.
var managerSendFuncs = map[string]bool{"SendLine": true, "SendKey": true, "TypeLine": true}

// inTopLevelFunc reports whether pos lies inside the body of a TOP-LEVEL (non-method) FuncDecl
// named name. A method of that name (Recv != nil), or pos outside every such function, is
// rejected — so the name alone cannot launder an injection (fail closed).
func inTopLevelFunc(f *ast.File, name string, pos token.Pos) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil { // a method named the same is NOT the sanctioned wiring function
			continue
		}
		if fn.Name.Name == name && fn.Pos() <= pos && pos <= fn.End() {
			return true
		}
	}
	return false
}

// isFixedLiteral reports whether expr is exactly the want STRING LITERAL — not an identifier (an
// ident is value-blind: a local var of any name, even one matching a package const, could carry
// interpolated/attacker-influenced content), not a concatenation, not a call. Requiring a literal
// is what statically proves the manager pane can only ever receive the fixed line.
func isFixedLiteral(expr ast.Expr, want string) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == want
}

// isSanctionedManagerSend reports whether a manager-targeting tmux send is one of the
// allow-listed sends: for a single entry, it calls that tmux function AND is in that file AND
// inside that top-level function AND carries that fixed literal. Any of those failing — another
// tmux function, a different file, a different/method function, an ident or
// interpolated/arbitrary payload, or another entry's payload — leaves it flagged.
func isSanctionedManagerSend(f *ast.File, filename, method string, call *ast.CallExpr) bool {
	if len(call.Args) < 3 {
		return false // Send…(session, window, payload) — no payload to verify
	}
	for _, s := range sanctionedManagerSends {
		if method == s.method &&
			filepath.ToSlash(filename) == s.file &&
			inTopLevelFunc(f, s.fn, call.Pos()) &&
			isFixedLiteral(call.Args[2], s.payload) {
			return true
		}
	}
	return false
}

// detectManagerInjection parses Go source and returns the 1-based line numbers of every
// forbidden send into the manager session: a SendLine/SendKey/TypeLine call on any receiver
// (package tmux, or a session backend.Backend however it is reached) whose window argument
// resolves to the manager window — directly OR through a local variable — carrying anything
// other than (a) a harness.Manager… launch command or (b) one of the allow-listed sends (an
// entry's method with its fixed literal, from that entry's file+function; see
// isSanctionedManagerSend). Operating on the AST (rather than one line at a time) is what lets
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
		// The receiver is deliberately not checked. Sends used to be tmux.SendLine only; the
		// orchestrator now makes them as Backend method calls (m.backend().SendLine,
		// w.Backend.SendLine), and a check pinned to the package name would pass a poke
		// spelled either way.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !managerSendFuncs[sel.Sel.Name] {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		win := call.Args[1] // SendLine/SendKey/TypeLine(session, window, payload)
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
		// An allow-listed send: that entry's tmux function, in its pinned file+function, carrying
		// its fixed literal.
		if isSanctionedManagerSend(f, fset.Position(call.Pos()).Filename, sel.Sel.Name, call) {
			return true
		}
		lines = append(lines, fset.Position(call.Pos()).Line)
		return true
	})
	return lines
}

// TestNoInjectionIntoManagerSession is the increment-6 net invariant, evolved: NO code path may
// type into the manager session EXCEPT the sanctioned ones. The supervisor's poke (tmux.SendLine
// into the "manager" window carrying a directive) was retired; the remaining permitted sends are
// (1) the launch/resume bootstrap, which START the session rather than inject into a running one,
// (2) the API-stall recovery nudge — the fixed "continue" resume the scheduler's production wiring
// (sanctionedStallNudgeFunc) sends to a genuinely-stalled manager — and (3) the scheduler watch
// loop's fixed wake line (sanctionedWakeFunc), typed into an idle manager when unread updates wait.
// This parses all non-test Go source and FAILS if any send to the manager window — including one
// laundered through a local variable — carries anything other than a harness.Manager… launch
// command OR is anything but an allow-listed send: a manager send from any other function, or
// carrying interpolated/arbitrary content even from a sanctioned function, still fails the build
// (see isSanctionedManagerSend).
//
// The manager→worker `ttorch send` path (SendLine into a WORKER window) is unaffected: its
// window argument never resolves to the manager window, so it is never matched.
func TestNoInjectionIntoManagerSession(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range goSourceFiles(t, root) {
		// Parse under the repo-relative name, so the allow-list's file check is an exact
		// match against a path from the module root rather than a suffix of an absolute one.
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}
		for _, line := range detectManagerInjection(fset, f) {
			offenders = append(offenders, fmt.Sprintf("%s:%d", rel, line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("forbidden injection into the manager session — SendLine/SendKey/TypeLine to the manager "+
			"window carrying something other than a harness.Manager… launch command:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestManagerInjectionDetector pins the detector so the invariant test cannot pass vacuously: it
// must FLAG a poke under every spelling — including the indirect form laundered through a local
// variable — and PASS a manager launch/resume (even when laundered), a send to a worker window, and
// the allow-listed sends (the API-stall recovery nudge and the watch loop's wake line). It also pins
// the allow-list as TIGHT against the exact bypasses an adversarial review surfaced: an exemption
// requires ALL THREE of one entry's file, its top-level (non-method) function, and its fixed
// LITERAL — so the same send from another file, a same-named function/method elsewhere, an
// identifier payload (launderable via a local shadow), another entry's payload, or any
// interpolated/arbitrary content STILL fails. file/fn/recv let a case
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
		{name: "sanctioned nudge (managerWindow ident)", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return be.SendLine(session, managerWindow, "continue")`, injection: false},
		{name: "sanctioned nudge (literal window)", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return be.SendLine(session, "manager", "continue")`, injection: false},
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
		// The orchestrator reaches tmux through a session Backend, so a send is a method call on
		// whatever holds the backend, not on package tmux. Every spelling of the receiver counts.
		{name: "backend accessor poke", body: `_ = m.backend().SendLine(m.Session, "manager", "ttorch wake: drain and advance")`, injection: true},
		{name: "backend field poke", body: `m.Backend.SendLine(m.Session, managerWindow, pokeDirective)`, injection: true},
		{name: "backend local var poke", body: "b := m.Backend\nw := \"manager\"\n_ = b.SendLine(m.Session, w, directive)", injection: true},
		{name: "backend sendkey poke", body: `b.SendKey(s, "manager", "Enter")`, injection: true},
		{name: "backend manager launch", body: `_ = m.backend().SendLine(m.Session, "manager", harness.ManagerCommand(h, sid, m.charterFile()))`, injection: false},
		{name: "backend worker send", body: `return m.backend().SendLine(m.Session, t.Window, text)`, injection: false},
		{name: "backend sanctioned nudge", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return be.SendLine(session, managerWindow, "continue")`, injection: false},
		// The watch loop's wake line: exempt only as the fixed literal from wireManagerWake in
		// internal/watch/daemon.go.
		{name: "sanctioned wake (managerWindow ident)", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: false},
		{name: "sanctioned wake (literal window)", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: "return tmux.TypeLine(session, \"manager\", " + strconv.Quote(sanctionedWakePayload) + ")", injection: false},
		{name: "sanctioned wake Enter", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.SendKey(session, managerWindow, "Enter")`, injection: false},
		// TIGHT: TypeLine is scanned like the other sends, anywhere.
		{name: "direct TypeLine poke", body: `_ = tmux.TypeLine(m.Session, "manager", "drain and advance")`, injection: true},
		{name: "TypeLine via local var", body: "w := managerWindow\n_ = tmux.TypeLine(s.Session, w, directive)", injection: true},
		// TIGHT: the wake must be typed and confirmed; submitting it in one SendLine skips the check.
		{name: "wake line by SendLine from the wake site", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: "return tmux.SendLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		// TIGHT: entries are matched whole, never as a cross-product of method/file/func/payload.
		{name: "wake line from the stall-nudge site", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		{name: "Enter from the stall-nudge site", file: sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendKey(session, managerWindow, "Enter")`, injection: true},
		{name: "continue from the wake site", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: true},
		{name: "continue typed from the wake site", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.TypeLine(session, managerWindow, "continue")`, injection: true},
		{name: "Enter as typed text from the wake site", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.TypeLine(session, managerWindow, "Enter")`, injection: true},
		{name: "wake func+literal but wrong file", file: "internal/watch/other.go", fn: sanctionedWakeFunc, body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		// TIGHT: the file is anchored at the module root; a copy under another directory whose path
		// merely ENDS in the sanctioned path is not exempt.
		{name: "wake site copied under vendor/", file: "vendor/example.com/fork/" + sanctionedWakeFile, fn: sanctionedWakeFunc, body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		{name: "stall-nudge site copied under third_party/", file: "third_party/" + sanctionedStallNudgeFile, fn: sanctionedStallNudgeFunc, body: `return tmux.SendLine(session, managerWindow, "continue")`, injection: true},
		{name: "wake file+literal but wrong function", file: sanctionedWakeFile, fn: "somethingElse", body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		{name: "Enter from the wake file but wrong function", file: sanctionedWakeFile, fn: "somethingElse", body: `return tmux.SendKey(session, managerWindow, "Enter")`, injection: true},
		{name: "wake name as a method is not exempt", file: sanctionedWakeFile, fn: sanctionedWakeFunc, recv: "d *Daemon", body: "return tmux.TypeLine(session, managerWindow, " + strconv.Quote(sanctionedWakePayload) + ")", injection: true},
		{name: "ident payload in wake site is not exempt", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.TypeLine(session, managerWindow, wakeLine)`, injection: true},
		{name: "event payload in wake site is not exempt", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `return tmux.TypeLine(session, managerWindow, "Automated notice: "+e.Payload)`, injection: true},
		{name: "another key in wake site is not exempt", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `tmux.SendKey(session, managerWindow, "C-c")`, injection: true},
		{name: "key name by ident in wake site is not exempt", file: sanctionedWakeFile, fn: sanctionedWakeFunc, body: `tmux.SendKey(session, managerWindow, key)`, injection: true},
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

package tmuxtest

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeTmux puts a tmux first on PATH that records each invocation's argv, one call per line
// with the arguments tab-separated, and returns the log path and the PATH it set. No test in
// this file runs the real tmux: every kill path is exercised against this stand-in, so a
// regression shows up as a recorded argv instead of a killed server.
func fakeTmux(t *testing.T) (log, path string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\n(IFS=\"$(printf '\\t')\"; printf '%s\\n' \"$*\") >> \"" + log + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path = bin + string(os.PathListSeparator) + "/usr/bin:/bin"
	t.Setenv("PATH", path)
	return log, path
}

// calls reads the fake's log; a log it never wrote means tmux was never run.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	f, err := os.Open(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, strings.Split(sc.Text(), "\t"))
	}
	return out
}

// testServer is a Server on a directory of the test's own, with a listening unix socket where
// tmux would put the server, so the reaper's [ -S ] check sees a live socket. The directory
// comes from os.MkdirTemp rather than t.TempDir because t.TempDir's longer path can push the
// socket past the unix path limit.
func testServer(t *testing.T) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := newServer(dir)
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return s
}

// requirePinned fails unless every recorded call starts with -S <socket> and names no other
// socket or socket name, and at least one of them is a kill-server.
func requirePinned(t *testing.T, got [][]string, socket string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatal("tmux was never run; want a kill-server on the private socket")
	}
	killed := false
	for _, argv := range got {
		if len(argv) < 2 || argv[0] != "-S" || argv[1] != socket {
			t.Errorf("tmux %q does not start with -S %s: it reaches whatever server $TMUX or $TMUX_TMPDIR names, or the default socket", argv, socket)
			continue
		}
		for i, a := range argv[2:] {
			if a == "-S" || a == "-L" {
				t.Errorf("tmux %q names a second socket at argument %d", argv, i+2)
			}
		}
		if argv[len(argv)-1] == "kill-server" {
			killed = true
		}
	}
	if !killed {
		t.Errorf("no kill-server among %q", got)
	}
}

// Close kills through the pinned socket and leaves the directory in place: a tmux that runs
// after it in this process must still find TMUX_TMPDIR, or it falls back to the default socket.
func TestClose_KillsOnlyThePrivateSocketAndKeepsTheDirectory(t *testing.T) {
	log, _ := fakeTmux(t)
	s := testServer(t)
	s.Close()
	requirePinned(t, calls(t, log), s.Socket)
	if fi, err := os.Stat(s.Dir); err != nil || !fi.IsDir() {
		t.Errorf("Close removed %s while the process lives (stat err %v)", s.Dir, err)
	}
}

func TestReaper_KillsOnlyThePrivateSocket(t *testing.T) {
	log, path := fakeTmux(t)
	s := testServer(t)
	cmd := s.reaper(exitedPID(t))
	cmd.Env = append(os.Environ(), "PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper: %v: %s", err, out)
	}
	requirePinned(t, calls(t, log), s.Socket)
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("reaper left %s behind (stat err %v)", s.Dir, err)
	}
}

// After a normal exit Close has removed the directory, and with it the socket. That is the
// state in which a bare kill-server falls back to the default socket, so the reaper must not
// run tmux at all.
func TestReaper_RunsNoTmuxOnceTheSocketIsGone(t *testing.T) {
	log, path := fakeTmux(t)
	s := testServer(t)
	if err := os.RemoveAll(s.Dir); err != nil {
		t.Fatal(err)
	}
	cmd := s.reaper(exitedPID(t))
	cmd.Env = append(os.Environ(), "PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper: %v: %s", err, out)
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("reaper ran tmux with no private socket left: %q", got)
	}
}

func TestCommand_PinsTheSocket(t *testing.T) {
	log, _ := fakeTmux(t)
	s := testServer(t)
	if err := s.Command("kill-session", "-t", "x").Run(); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	want := []string{"-S", s.Socket, "kill-session", "-t", "x"}
	if len(got) != 1 || strings.Join(got[0], "\t") != strings.Join(want, "\t") {
		t.Errorf("Command ran tmux %q, want %q", got, want)
	}
}

// repoRoot is the module root, two directories up from this package.
const repoRoot = "../.."

// goFile is one parsed Go file of the repository.
type goFile struct {
	path string
	dir  string // relative to repoRoot, slash-separated
	test bool
	ast  *ast.File
	fset *token.FileSet
}

// repoFiles parses every Go file in the repository, skipping hidden directories, vendor and
// testdata.
func repoFiles(t *testing.T) []goFile {
	t.Helper()
	var out []goFile
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != repoRoot && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return err
		}
		out = append(out, goFile{path: path, dir: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go"), ast: f, fset: fset})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// strLit is the value of a string literal expression, or ok=false for anything else.
func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// pkgCall reports whether call is pkg.name(...) for one of names.
func pkgCall(call *ast.CallExpr, pkg string, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != pkg {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

var (
	tmuxWord = regexp.MustCompile(`\btmux\b`)
	shells   = map[string]bool{"sh": true, "bash": true, "zsh": true, "/bin/sh": true, "/bin/bash": true, "/bin/zsh": true, "/usr/bin/env": true, "env": true}
)

// runsTmux reports whether call runs tmux by name: exec.Command or exec.CommandContext with
// tmux as the program, or with a shell as the program and a literal argument that runs tmux;
// exec.LookPath("tmux"); or syscall.Exec of tmux. It reads the call's syntax, so a call split
// over several lines is the same call.
func runsTmux(call *ast.CallExpr) bool {
	isTmux := func(s string) bool { return s == "tmux" || strings.HasSuffix(s, "/tmux") }
	switch {
	case pkgCall(call, "exec", "Command", "CommandContext"):
		args := call.Args
		if pkgCall(call, "exec", "CommandContext") {
			if len(args) == 0 {
				return false
			}
			args = args[1:]
		}
		if len(args) == 0 {
			return false
		}
		prog, ok := strLit(args[0])
		if !ok {
			return false
		}
		if isTmux(prog) {
			return true
		}
		if shells[prog] {
			for _, a := range args[1:] {
				if v, ok := strLit(a); ok && tmuxWord.MatchString(v) {
					return true
				}
			}
		}
	case pkgCall(call, "exec", "LookPath"), pkgCall(call, "syscall", "Exec"):
		if len(call.Args) > 0 {
			if v, ok := strLit(call.Args[0]); ok && isTmux(v) {
				return true
			}
		}
	}
	return false
}

// TestNoBareTmuxInTests keeps every tmux a test runs itself on a pinned socket: a test that
// builds its own exec.Command("tmux", ...), on one line or several, or hands tmux to a shell,
// reaches whatever server the environment names when it runs. Tests run tmux through
// Server.Command. A fake tmux written to a file and put on PATH is not a call and is not
// flagged.
func TestNoBareTmuxInTests(t *testing.T) {
	for _, f := range repoFiles(t) {
		if !f.test {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && pkgCall(call, "exec", "Command", "CommandContext") && runsTmux(call) {
				t.Errorf("%s runs tmux without the private socket; use tmuxtest.Server.Command", f.fset.Position(call.Pos()))
			}
			return true
		})
	}
}

// TestNoBareTmuxInTests_SeesThroughLineBreaksAndShells is the negative control for the check
// above: the shapes it exists to catch, as source, each flagged.
func TestNoBareTmuxInTests_SeesThroughLineBreaksAndShells(t *testing.T) {
	src := `package p
func f() {
	exec.Command(
		"tmux",
		"kill-server",
	).Run()
	exec.CommandContext(ctx, "/opt/homebrew/bin/tmux", "kill-session", "-t", "s")
	exec.Command("/bin/sh", "-c", "sleep 1; tmux kill-server")
	exec.Command("tmux", "-S", sock, "kill-server") // pinned, but not through Server.Command
	exec.Command("git", "status")
	exec.Command("/bin/sh", "-c", "echo tmuxinator")
}`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flagged int
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && runsTmux(call) {
			flagged++
		}
		return true
	})
	if flagged != 4 {
		t.Errorf("flagged %d calls, want the 4 that run tmux", flagged)
	}
}

// TestEveryTmuxReachingPackageIsIsolated holds that a test binary which can reach tmux at all
// runs on a private server. A package's tests reach tmux when the package, its test files'
// imports, or anything those import in this module runs tmux by name (internal/tmux does, for
// every caller of the production tmux package). Each such package with tests must call
// tmuxtest.Run or tmuxtest.Isolate from its TestMain; without that, a test calling
// tmux.NewWindow or tmux.KillSession acts on the caller's own server.
//
// tmuxtest itself is the exception: it is what isolates, and its tests run only a fake tmux.
func TestEveryTmuxReachingPackageIsIsolated(t *testing.T) {
	modBytes, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var module string
	for _, l := range strings.Split(string(modBytes), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			module = strings.TrimSpace(m)
		}
	}
	if module == "" {
		t.Fatal("no module line in go.mod")
	}
	const self = "internal/tmuxtest"

	deps := map[string]map[string]bool{}     // dir -> module dirs its non-test files import
	testDeps := map[string]map[string]bool{} // dir -> module dirs its test files import
	roots := map[string]bool{}               // dirs whose non-test code runs tmux by name
	hasTests := map[string]bool{}
	isolated := map[string]bool{} // dirs whose TestMain calls tmuxtest.Run or tmuxtest.Isolate
	add := func(m map[string]map[string]bool, dir, dep string) {
		if m[dir] == nil {
			m[dir] = map[string]bool{}
		}
		m[dir][dep] = true
	}
	for _, f := range repoFiles(t) {
		for _, imp := range f.ast.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel, ok := strings.CutPrefix(p, module+"/")
			if !ok {
				continue
			}
			if f.test {
				add(testDeps, f.dir, rel)
			} else {
				add(deps, f.dir, rel)
			}
		}
		if f.test {
			hasTests[f.dir] = true
			for _, d := range f.ast.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok && pkgCall(call, "tmuxtest", "Run", "Isolate") {
						isolated[f.dir] = true
					}
					return true
				})
			}
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && runsTmux(call) {
				roots[f.dir] = true
			}
			return true
		})
	}
	delete(roots, self)
	if !roots["internal/tmux"] {
		t.Fatalf("internal/tmux was not found to run tmux (roots %v); the check is reading nothing", roots)
	}

	reaches := func(dir string) bool {
		seen := map[string]bool{}
		queue := []string{dir}
		for d := range testDeps[dir] {
			queue = append(queue, d)
		}
		for len(queue) > 0 {
			d := queue[0]
			queue = queue[1:]
			if seen[d] || d == self {
				continue
			}
			seen[d] = true
			if roots[d] {
				return true
			}
			for next := range deps[d] {
				queue = append(queue, next)
			}
		}
		return false
	}
	var reaching []string
	for dir := range hasTests {
		if dir == self || !reaches(dir) {
			continue
		}
		reaching = append(reaching, dir)
		if !isolated[dir] {
			t.Errorf("%s: its tests can reach tmux, and no TestMain in it calls tmuxtest.Run or tmuxtest.Isolate, so they run against the caller's own tmux server", dir)
		}
	}
	for _, must := range []string{"internal/tmux", "internal/orchestrator", "internal/cli"} {
		if !slices.Contains(reaching, must) {
			t.Errorf("%s was not found to reach tmux (found %v); the import walk is wrong", must, reaching)
		}
	}
}

// exitedPID is the pid of a process that has exited and been waited for, so the reaper's
// kill -0 loop ends at once.
func exitedPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

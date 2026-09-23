package backend

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSessionCallersDoNotImportTmux keeps the seam whole. The orchestrator and the
// watcher reach their session only through a Backend, so no non-test file in either
// package may import package tmux: one direct call would be a call site a second backend
// silently skips. Tests may still import it to drive a real tmux.
func TestSessionCallersDoNotImportTmux(t *testing.T) {
	const tmuxPkg = "github.com/nution101/ttorch/internal/tmux"
	for _, dir := range []string{"../orchestrator", "../watch"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files under %s (err = %v)", dir, err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == tmuxPkg {
					t.Errorf("%s imports %s; call the session through its Backend instead", path, tmuxPkg)
				}
			}
		}
	}
}

// TestResolve pins which names select a backend. Unset and "tmux" give tmux; anything
// else is refused with an error that names the variable and the value, so the lead can
// see what to fix. Matching is exact apart from surrounding space: "TMUX" is refused
// rather than guessed at.
func TestResolve(t *testing.T) {
	for _, name := range []string{"", "tmux", " tmux\n"} {
		b, err := Resolve(name)
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error %v", name, err)
			continue
		}
		if _, ok := b.(Tmux); !ok {
			t.Errorf("Resolve(%q) = %T, want Tmux", name, b)
		}
	}
	for _, name := range []string{"bogus", "TMUX", "herdr", "tmux2"} {
		b, err := Resolve(name)
		if err == nil {
			t.Errorf("Resolve(%q) = %T, want an error", name, b)
			continue
		}
		if b != nil {
			t.Errorf("Resolve(%q) returned backend %T alongside its error", name, b)
		}
		msg := err.Error()
		if !strings.Contains(msg, Env) || !strings.Contains(msg, `"`+name+`"`) || !strings.Contains(msg, `"tmux"`) {
			t.Errorf("Resolve(%q) error = %q, want it to name %s, the value and the supported value", name, msg, Env)
		}
	}
}

// TestFromEnv covers the variable itself: unset and "tmux" select tmux, and
// TTORCH_BACKEND=bogus is refused.
func TestFromEnv(t *testing.T) {
	t.Setenv(Env, "")
	if b, err := FromEnv(); err != nil {
		t.Fatalf("unset %s: %v", Env, err)
	} else if _, ok := b.(Tmux); !ok {
		t.Fatalf("unset %s selected %T, want Tmux", Env, b)
	}

	t.Setenv(Env, "tmux")
	if b, err := FromEnv(); err != nil {
		t.Fatalf("%s=tmux: %v", Env, err)
	} else if _, ok := b.(Tmux); !ok {
		t.Fatalf("%s=tmux selected %T, want Tmux", Env, b)
	}

	t.Setenv(Env, "bogus")
	if b, err := FromEnv(); err == nil {
		t.Fatalf("%s=bogus selected %T, want it refused", Env, b)
	} else if !strings.Contains(err.Error(), `TTORCH_BACKEND="bogus"`) {
		t.Fatalf("%s=bogus error = %q, want it to quote the bad value", Env, err)
	}
}

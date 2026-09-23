package backend

import (
	"strings"
	"testing"
)

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

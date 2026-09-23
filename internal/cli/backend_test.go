package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
)

// TestUnknownBackendRefusedAtStartup covers both places the CLI picks a session backend:
// `ttorch watch`, which builds its watcher directly, and mgr(), which every
// Manager-backed command goes through. Each must refuse TTORCH_BACKEND=bogus with an
// error naming the variable before it opens the state store.
func TestUnknownBackendRefusedAtStartup(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-cli-backend-test-no-session")
	t.Setenv("TTORCH_BACKEND", "bogus")

	err := cmdWatch([]string{"--timeout", "1ms"})
	if err == nil || !strings.Contains(err.Error(), `TTORCH_BACKEND="bogus"`) {
		t.Fatalf("ttorch watch with TTORCH_BACKEND=bogus: err = %v, want a refusal naming the variable and value", err)
	}
	if m, err := mgr(); err == nil {
		_ = m.Close()
		t.Fatal("mgr() with TTORCH_BACKEND=bogus succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), `TTORCH_BACKEND="bogus"`) {
		t.Fatalf("mgr() with TTORCH_BACKEND=bogus: err = %v, want it to name the variable and value", err)
	}
	if _, err := os.Stat(paths.Default().StateDB()); !os.IsNotExist(err) {
		t.Fatalf("a refused backend still opened the state store (stat err = %v)", err)
	}
}

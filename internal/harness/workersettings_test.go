package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteWorkerSettingsInstallsStopHook: a worker's worktree-local settings.local.json
// disables the AI co-author trailer AND registers the worker Stop hook (`ttorch stop-hook`),
// which enforces `ttorch report` when the worker idles. The hook is worker-scoped (this
// worktree-local file), never global.
func TestWriteWorkerSettingsInstallsStopHook(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir()) // so paths.Default().Binary() resolves under a temp home
	wt := t.TempDir()
	if err := WriteWorkerSettings("claude", wt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("worker settings not written: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"includeCoAuthoredBy": false`) {
		t.Errorf("settings should disable the co-author trailer, got:\n%s", s)
	}
	if !strings.Contains(s, `"Stop"`) || !strings.Contains(s, "stop-hook") {
		t.Errorf("settings should register the Stop hook, got:\n%s", s)
	}
	if !strings.Contains(s, `"type": "command"`) {
		t.Errorf("Stop hook should be a command hook, got:\n%s", s)
	}
}

// TestWriteWorkerSettingsNonClaudeNoOp: non-claude harnesses get no settings file.
func TestWriteWorkerSettingsNonClaudeNoOp(t *testing.T) {
	wt := t.TempDir()
	if err := WriteWorkerSettings("codex", wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("non-claude harness must write no settings file (stat err = %v)", err)
	}
}

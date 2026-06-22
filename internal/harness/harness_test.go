package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadProjects(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config not valid JSON: %v", err)
	}
	p, _ := cfg["projects"].(map[string]any)
	return p
}

func trusted(projects map[string]any, path string) bool {
	e, _ := projects[path].(map[string]any)
	if e == nil {
		return false
	}
	v, _ := e["hasTrustDialogAccepted"].(bool)
	return v
}

func TestEffortArgs(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", ` --settings '{"ultracode":true}'`}, // default
		{"ultracode", ` --settings '{"ultracode":true}'`},
		{"MAX", " --effort max"}, // case-insensitive
		{"xhigh", " --effort xhigh"},
		{"high", " --effort high"},
		{"off", ""},
		{"none", ""},
		{"default", ""},
		{"bogus", ` --settings '{"ultracode":true}'`}, // unrecognized -> ultracode
	}
	for _, c := range cases {
		t.Setenv("TTORCH_EFFORT", c.env)
		if got := EffortArgs("claude"); got != c.want {
			t.Errorf("TTORCH_EFFORT=%q: got %q, want %q", c.env, got, c.want)
		}
	}
	// Non-claude harnesses never get effort args.
	t.Setenv("TTORCH_EFFORT", "ultracode")
	if got := EffortArgs("other"); got != "" {
		t.Errorf("non-claude harness should get no effort args, got %q", got)
	}
}

func TestLaunchCommandsCarryEffort(t *testing.T) {
	t.Setenv("TTORCH_EFFORT", "") // default: ultracode
	want := `claude --dangerously-skip-permissions --settings '{"ultracode":true}'`
	if got := InteractiveCommand("claude"); got != want {
		t.Errorf("interactive: got %q, want %q", got, want)
	}
	brief := BriefCommand("claude", "/tmp/b.md")
	if !strings.Contains(brief, `--settings '{"ultracode":true}'`) {
		t.Errorf("brief command missing ultracode setting: %q", brief)
	}
	if !strings.HasSuffix(brief, `"$(cat '/tmp/b.md')"`) {
		t.Errorf("brief command should end with the brief prompt: %q", brief)
	}

	t.Setenv("TTORCH_EFFORT", "off")
	if got := InteractiveCommand("claude"); got != "claude --dangerously-skip-permissions" {
		t.Errorf("off should drop effort args, got %q", got)
	}
}

func TestEnsureTrusted_CreatesAndPreserves(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	// Pre-existing config with unrelated content + an existing project.
	seed := `{"numStartups":7,"projects":{"/other":{"hasTrustDialogAccepted":true,"foo":1}}}`
	if err := os.WriteFile(cfg, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrusted(cfg, []string{"/repo/wt"}); err != nil {
		t.Fatal(err)
	}
	p := loadProjects(t, cfg)
	if !trusted(p, "/repo/wt") {
		t.Fatal("new path should be trusted")
	}
	if !trusted(p, "/other") {
		t.Fatal("existing trusted project must be preserved")
	}
	// Unrelated top-level keys preserved.
	b, _ := os.ReadFile(cfg)
	var full map[string]any
	json.Unmarshal(b, &full)
	if full["numStartups"] == nil {
		t.Fatal("unrelated top-level keys must be preserved")
	}
}

func TestEnsureTrusted_CreatesMissingFile(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	if err := ensureTrusted(cfg, []string{"/a", "/b"}); err != nil {
		t.Fatal(err)
	}
	p := loadProjects(t, cfg)
	if !trusted(p, "/a") || !trusted(p, "/b") {
		t.Fatal("both paths should be trusted in a freshly created config")
	}
}

func TestEnsureTrusted_RefusesMalformed(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrusted(cfg, []string{"/a"}); err == nil {
		t.Fatal("expected an error for malformed config")
	}
	// The malformed file must be left untouched, not clobbered.
	b, _ := os.ReadFile(cfg)
	if string(b) != "{not json" {
		t.Fatal("malformed config must not be modified")
	}
}

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	brief := BriefCommand("claude", "/tmp/b.md", "sid-123")
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

func TestManagerCommand(t *testing.T) {
	t.Setenv("TTORCH_MANAGER_EFFORT", "") // default: high (NOT ultracode)
	// With a charter file the charter is referenced (short command), not inlined.
	cmd := ManagerCommand("claude", "mgr-sid", "/tmp/charter.md")
	if !strings.Contains(cmd, " --effort high") {
		t.Errorf("manager should default to --effort high, got %q", cmd)
	}
	if strings.Contains(cmd, "ultracode") {
		t.Errorf("manager must not default to ultracode, got %q", cmd)
	}
	if !strings.Contains(cmd, " --append-system-prompt-file '/tmp/charter.md'") {
		t.Errorf("manager should reference the charter file, got %q", cmd)
	}
	if strings.Contains(cmd, "You are the ttorch MANAGER") {
		t.Errorf("charter should live in the file, not be inlined, got %q", cmd)
	}
	if !strings.Contains(cmd, " --session-id 'mgr-sid'") {
		t.Errorf("manager should launch with the session id, got %q", cmd)
	}

	// With no charter file it falls back to inlining the charter (shell-escaped).
	inline := ManagerCommand("claude", "mgr-sid", "")
	if !strings.Contains(inline, " --append-system-prompt '") ||
		!strings.Contains(inline, "You are the ttorch MANAGER") ||
		!strings.Contains(inline, "ttorch spawn") ||
		!strings.Contains(inline, `lead'\''s`) {
		t.Errorf("inline fallback charter wrong: %q", inline)
	}
}

func TestManagerEffortOverride(t *testing.T) {
	t.Setenv("TTORCH_MANAGER_EFFORT", "ultracode")
	if got := ManagerCommand("claude", "x", ""); !strings.Contains(got, `--settings '{"ultracode":true}'`) {
		t.Errorf("override to ultracode should add --settings, got %q", got)
	}
	t.Setenv("TTORCH_MANAGER_EFFORT", "max")
	if got := ManagerCommand("claude", "x", ""); !strings.Contains(got, " --effort max") {
		t.Errorf("override to max should add --effort max, got %q", got)
	}
}

func TestManagerResumeCommand(t *testing.T) {
	t.Setenv("TTORCH_MANAGER_EFFORT", "")
	cmd := ManagerResumeCommand("claude", "mgr-sid", "/tmp/charter.md")
	if !strings.Contains(cmd, " --resume 'mgr-sid'") {
		t.Errorf("resume should carry --resume <id>, got %q", cmd)
	}
	if strings.Contains(cmd, "--session-id") {
		t.Errorf("resume must not use --session-id, got %q", cmd)
	}
	if !strings.Contains(cmd, " --effort high") || !strings.Contains(cmd, "--append-system-prompt-file '/tmp/charter.md'") {
		t.Errorf("resume should keep effort + charter file, got %q", cmd)
	}
	// No id -> --continue, no --resume.
	cont := ManagerResumeCommand("claude", "", "")
	if !strings.Contains(cont, " --continue") || strings.Contains(cont, "--resume") {
		t.Errorf("empty id should use --continue, got %q", cont)
	}
}

func TestWriteManagerCharter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manager-charter.md")
	if err := WriteManagerCharter(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "You are the ttorch MANAGER") || !strings.Contains(string(b), "ttorch spawn") {
		t.Errorf("charter file content wrong: %q", b)
	}
}

func TestBriefCommandCarriesSessionID(t *testing.T) {
	t.Setenv("TTORCH_EFFORT", "off")
	cmd := BriefCommand("claude", "/tmp/b.md", "wk-sid")
	if !strings.Contains(cmd, " --session-id 'wk-sid'") {
		t.Errorf("brief should carry --session-id, got %q", cmd)
	}
	// The session id flag must come before the positional brief prompt.
	si := strings.Index(cmd, "--session-id")
	bp := strings.Index(cmd, "$(cat")
	if si < 0 || bp < 0 || si > bp {
		t.Errorf("session id must precede the brief prompt, got %q", cmd)
	}
}

func TestResumeCommand(t *testing.T) {
	t.Setenv("TTORCH_EFFORT", "off")
	cmd := ResumeCommand("claude", "wk-sid")
	if !strings.Contains(cmd, " --resume 'wk-sid'") {
		t.Errorf("worker resume should carry --resume <id>, got %q", cmd)
	}
	if strings.Contains(cmd, "$(cat") || strings.Contains(cmd, "--session-id") {
		t.Errorf("worker resume must not carry a brief or --session-id, got %q", cmd)
	}
	cont := ResumeCommand("claude", "")
	if !strings.Contains(cont, " --continue") || strings.Contains(cont, "--resume") {
		t.Errorf("empty id should use --continue, got %q", cont)
	}
}

func TestManagerResumeOrFresh(t *testing.T) {
	t.Setenv("TTORCH_MANAGER_EFFORT", "")
	cmd := ManagerResumeOrFresh("claude", "mgr-sid", "/tmp/charter.md")
	if !strings.Contains(cmd, " --resume 'mgr-sid'") {
		t.Errorf("should attempt resume, got %q", cmd)
	}
	if !strings.Contains(cmd, " || ") {
		t.Errorf("should fall back with ||, got %q", cmd)
	}
	// The fallback is a fresh manager with the SAME id (self-healing).
	if !strings.Contains(cmd, " --session-id 'mgr-sid'") {
		t.Errorf("fallback should start a fresh manager with the same id, got %q", cmd)
	}
}

func TestWorkerResumeOrFresh(t *testing.T) {
	t.Setenv("TTORCH_EFFORT", "off")
	cmd := WorkerResumeOrFresh("claude", "wk-sid", "/tmp/b.md")
	if !strings.Contains(cmd, " --resume 'wk-sid'") || !strings.Contains(cmd, " || ") {
		t.Errorf("should attempt resume then fall back, got %q", cmd)
	}
	// Fallback relaunches from the brief with the same id.
	if !strings.Contains(cmd, " --session-id 'wk-sid'") || !strings.Contains(cmd, `"$(cat '/tmp/b.md')"`) {
		t.Errorf("fallback should re-brief with the same id, got %q", cmd)
	}
}

func TestNewSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a := NewSessionID()
	b := NewSessionID()
	if !re.MatchString(a) {
		t.Errorf("session id %q is not a v4 UUID", a)
	}
	if !re.MatchString(b) {
		t.Errorf("session id %q is not a v4 UUID", b)
	}
	if a == b {
		t.Errorf("two session ids should differ, both = %q", a)
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

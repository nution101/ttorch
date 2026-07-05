package harness

import (
	"strings"
	"testing"
)

func TestValidModel(t *testing.T) {
	valid := []string{
		"haiku", "sonnet", "opus", "fable", "opusplan", "default",
		"HAIKU", " Sonnet ", // case / whitespace tolerant
		"claude-opus-4-8", "claude-haiku-4-5-20251001",
		"us.anthropic.claude-3-5-sonnet", "anthropic/claude-opus", // prefixed full ids
	}
	for _, s := range valid {
		if !ValidModel(s) {
			t.Errorf("ValidModel(%q) = false, want true", s)
		}
	}
	// Empty and bare-word typos (no separator, not an alias) are rejected loudly.
	invalid := []string{"", "   ", "opuss", "sonet", "haiky", "gpt", "random"}
	for _, s := range invalid {
		if ValidModel(s) {
			t.Errorf("ValidModel(%q) = true, want false", s)
		}
	}
}

func TestNormalizeModel(t *testing.T) {
	// A known alias is lowercased (claude matches aliases exactly, lowercase).
	for in, want := range map[string]string{
		"opus": "opus", "Opus": "opus", "HAIKU": "haiku", " Sonnet ": "sonnet",
		"OpusPlan": "opusplan", "default": "default",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
	// A full/prefixed id keeps its original case (ids can be case-sensitive); empty stays empty.
	for _, s := range []string{"claude-opus-4-8", "us.anthropic.claude-3-5-Sonnet"} {
		if got := NormalizeModel(s); got != s {
			t.Errorf("NormalizeModel(%q) = %q, want it unchanged (full id)", s, got)
		}
	}
	if got := NormalizeModel("   "); got != "" {
		t.Errorf("NormalizeModel(whitespace) = %q, want empty", got)
	}
}

func TestModelArgs(t *testing.T) {
	if got := ModelArgs("claude", "opus"); got != " --model 'opus'" {
		t.Errorf("ModelArgs opus = %q", got)
	}
	if got := ModelArgs("claude", "claude-opus-4-8"); got != " --model 'claude-opus-4-8'" {
		t.Errorf("ModelArgs full id = %q", got)
	}
	// A mixed-case alias is normalized to the lowercase claude alias (mirrors the effort dial).
	if got := ModelArgs("claude", "Opus"); got != " --model 'opus'" {
		t.Errorf("ModelArgs Opus = %q, want normalized to lowercase alias", got)
	}
	// Empty and the sentinels all mean "no flag" (leave claude's own default).
	for _, empty := range []string{"", "  ", "off", "none", "default", "OFF", "Default"} {
		if got := ModelArgs("claude", empty); got != "" {
			t.Errorf("ModelArgs(%q) = %q, want empty", empty, got)
		}
	}
	// A non-claude harness never gets model args.
	if got := ModelArgs("codex", "opus"); got != "" {
		t.Errorf("non-claude harness should get no model args, got %q", got)
	}
}

func TestResolveWorkerModel(t *testing.T) {
	t.Setenv("TTORCH_MODEL", "")
	if got := ResolveWorkerModel(""); got != "" {
		t.Errorf("unset should resolve to empty, got %q", got)
	}
	if got := ResolveWorkerModel("opus"); got != "opus" {
		t.Errorf("explicit should win, got %q", got)
	}
	t.Setenv("TTORCH_MODEL", "sonnet")
	if got := ResolveWorkerModel(""); got != "sonnet" {
		t.Errorf("env fallback = %q, want sonnet", got)
	}
	if got := ResolveWorkerModel("opus"); got != "opus" {
		t.Errorf("explicit should win over env, got %q", got)
	}
}

func TestLaunchCommandsCarryModel(t *testing.T) {
	t.Setenv("TTORCH_EFFORT", "off")
	t.Setenv("TTORCH_MODEL", "")
	// An explicit model flows onto the brief command.
	if got := BriefCommand("claude", "/tmp/b.md", "sid", "", "opus"); !strings.Contains(got, " --model 'opus'") {
		t.Errorf("brief should carry --model, got %q", got)
	}
	// An unset model adds no --model, leaving claude's own default.
	if got := BriefCommand("claude", "/tmp/b.md", "sid", "", ""); strings.Contains(got, "--model") {
		t.Errorf("unset model should add no --model, got %q", got)
	}
	// The persisted model flows into BOTH the resume and the re-brief fallback.
	rf := WorkerResumeOrFresh("claude", "wk", "/tmp/b.md", "", "haiku")
	if strings.Count(rf, " --model 'haiku'") != 2 {
		t.Errorf("both resume and fallback should carry the model, got %q", rf)
	}
	// TTORCH_MODEL flows into `ttorch cc` (InteractiveCommand).
	t.Setenv("TTORCH_MODEL", "haiku")
	if got := InteractiveCommand("claude"); !strings.Contains(got, " --model 'haiku'") {
		t.Errorf("cc should carry TTORCH_MODEL, got %q", got)
	}
}

func TestManagerCommandCarriesModel(t *testing.T) {
	t.Setenv("TTORCH_MANAGER_EFFORT", "")
	t.Setenv("TTORCH_MODEL", "")
	t.Setenv("TTORCH_MANAGER_MODEL", "fable") // a non-default value, to prove the env is carried
	if got := ManagerCommand("claude", "sid", ""); !strings.Contains(got, " --model 'fable'") {
		t.Errorf("manager should carry TTORCH_MANAGER_MODEL, got %q", got)
	}
	// The manager model must survive a resume too (reboot/upgrade), not just a fresh launch.
	if got := ManagerResumeCommand("claude", "sid", ""); !strings.Contains(got, " --model 'fable'") {
		t.Errorf("manager resume should carry TTORCH_MANAGER_MODEL, got %q", got)
	}
	// The worker default (TTORCH_MODEL) must NOT leak into the manager command — the two
	// dials are independent, mirroring TTORCH_MANAGER_EFFORT / TTORCH_EFFORT. With
	// TTORCH_MANAGER_MODEL unset the manager falls back to its OWN default (sonnet), never the
	// worker's TTORCH_MODEL.
	t.Setenv("TTORCH_MANAGER_MODEL", "")
	t.Setenv("TTORCH_MODEL", "haiku")
	if got := ManagerCommand("claude", "sid", ""); !strings.Contains(got, " --model 'opus'") || strings.Contains(got, "haiku") {
		t.Errorf("manager should default to opus and not inherit TTORCH_MODEL, got %q", got)
	}
	if got := ManagerResumeCommand("claude", "sid", ""); !strings.Contains(got, " --model 'opus'") || strings.Contains(got, "haiku") {
		t.Errorf("manager resume should default to opus and not inherit TTORCH_MODEL, got %q", got)
	}
}

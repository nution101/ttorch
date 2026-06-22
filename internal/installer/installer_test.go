package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nution101/orcha/internal/manifest"
	"github.com/nution101/orcha/internal/paths"
)

func sandbox(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	return paths.Paths{
		Home:     filepath.Join(root, "orcha"),
		Claude:   filepath.Join(root, "claude"),
		Agents:   filepath.Join(root, "agents"),
		LocalBin: filepath.Join(root, "bin"),
	}
}

func content(skill string) fstest.MapFS {
	return fstest.MapFS{
		"content/skills/orcha-manager/SKILL.md": {Data: []byte(skill)},
		"content/agents/orcha-worker.md":        {Data: []byte("worker")},
		"content/commands/orcha.md":             {Data: []byte("cmd")},
		"content/assets/AGENTS.global.md":       {Data: []byte("GLOBAL GUIDANCE")},
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestApply_LaysDownDualMirrorAndGuidance(t *testing.T) {
	p := sandbox(t)
	res, err := Apply(content("skill-v1"), p, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}

	claudeSkill := filepath.Join(p.ClaudeSkills(), "orcha-manager", "SKILL.md")
	agentsSkill := filepath.Join(p.AgentsSkills(), "orcha-manager", "SKILL.md")
	if read(t, claudeSkill) != "skill-v1" {
		t.Fatal("skill not installed into ~/.claude")
	}
	if read(t, agentsSkill) != "skill-v1" {
		t.Fatal("skill not mirrored into ~/.agents")
	}
	if read(t, filepath.Join(p.ClaudeAgents(), "orcha-worker.md")) != "worker" {
		t.Fatal("worker agent not installed")
	}

	agentsMD := read(t, p.GlobalAgentsMD())
	if !strings.Contains(agentsMD, "GLOBAL GUIDANCE") || !strings.Contains(agentsMD, markerBegin) {
		t.Fatalf("AGENTS.md missing managed block: %q", agentsMD)
	}
	if target, err := os.Readlink(p.GlobalClaudeMD()); err != nil || target != "AGENTS.md" {
		t.Fatalf("CLAUDE.md symlink wrong: target=%q err=%v", target, err)
	}
	if _, err := os.Stat(p.ManifestFile()); err != nil {
		t.Fatal("manifest not written")
	}

	// Second apply with identical content -> everything unchanged.
	res, err = Apply(content("skill-v1"), p, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Count(manifest.Unchanged) == 0 {
		t.Fatal("expected unchanged files on idempotent re-apply")
	}
}

func TestApply_PreservesUserEditsAcrossUpdate(t *testing.T) {
	p := sandbox(t)
	if _, err := Apply(content("skill-v1"), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	claudeSkill := filepath.Join(p.ClaudeSkills(), "orcha-manager", "SKILL.md")

	// Developer tunes the skill.
	if err := os.WriteFile(claudeSkill, []byte("my-custom-skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Upstream ships a new version.
	res, err := Apply(content("skill-v2"), p, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, claudeSkill) != "my-custom-skill" {
		t.Fatal("user edit was clobbered on update")
	}
	if read(t, claudeSkill+manifest.Suffix) != "skill-v2" {
		t.Fatal("new version not parked as .orcha-new")
	}
	if len(res.Report.Conflicts()) == 0 {
		t.Fatal("expected a reported conflict")
	}
}

func TestApply_PreservesUserGuidanceOutsideMarkers(t *testing.T) {
	p := sandbox(t)
	if err := os.MkdirAll(p.Claude, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing user AGENTS.md with their own content.
	if err := os.WriteFile(p.GlobalAgentsMD(), []byte("# My rules\nbe nice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(content("skill-v1"), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	got := read(t, p.GlobalAgentsMD())
	if !strings.Contains(got, "be nice") {
		t.Fatal("user AGENTS.md content was lost")
	}
	if !strings.Contains(got, "GLOBAL GUIDANCE") {
		t.Fatal("managed block not appended")
	}
}

package skills

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
)

func TestRecommendedIncludesAxi(t *testing.T) {
	recs := Recommended()
	if len(recs) == 0 {
		t.Fatal("expected at least one recommended skill")
	}
	found := false
	for _, s := range recs {
		if s.Ref == "kunchenguid/axi" {
			found = true
		}
		if s.Why == "" {
			t.Fatalf("skill %q is missing a rationale", s.Ref)
		}
	}
	if !found {
		t.Fatal("expected the axi skill to be recommended")
	}
}

func TestRecommendedIncludesPonytail(t *testing.T) {
	found := false
	for _, s := range Recommended() {
		if s.Ref == "DietrichGebert/ponytail" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the ponytail skill to be recommended")
	}
}

func TestInstallCmd(t *testing.T) {
	cmd := InstallCmd("owner/repo")
	want := []string{"npx", "skills", "add", "owner/repo"}
	if strings.Join(cmd, " ") != strings.Join(want, " ") {
		t.Fatalf("InstallCmd = %v, want %v", cmd, want)
	}
}

func TestEnsureInstalledSkipEnvIsInert(t *testing.T) {
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	var buf bytes.Buffer
	EnsureInstalled(&buf)
	if buf.Len() != 0 {
		t.Fatalf("skip env must make EnsureInstalled a silent no-op, got: %q", buf.String())
	}
}

// With every recommended skill already marked installed, EnsureInstalled is a no-op
// and never shells out to npx — the cheap already-installed fast path. This also keeps
// the test hermetic (no network) without relying on the skip env.
func TestEnsureInstalledNoOpWhenAllMarked(t *testing.T) {
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "")
	t.Setenv("TTORCH_HOME", t.TempDir())
	t.Setenv("TTORCH_CLAUDE_DIR", t.TempDir())
	p := paths.Default()
	for _, s := range Recommended() {
		if installed(p, s) {
			t.Fatalf("skill %q should not be installed in a fresh temp home", s.Ref)
		}
		mark(p, s.Ref)
		if !installed(p, s) {
			t.Fatalf("mark did not record skill %q as installed", s.Ref)
		}
	}
	var buf bytes.Buffer
	EnsureInstalled(&buf)
	if buf.Len() != 0 {
		t.Fatalf("EnsureInstalled must be silent when all skills are installed, got: %q", buf.String())
	}
}

func TestListMentionsInstall(t *testing.T) {
	var buf bytes.Buffer
	List(&buf)
	out := buf.String()
	if !strings.Contains(out, "kunchenguid/axi") || !strings.Contains(out, "ttorch skills install") {
		t.Fatalf("List output missing expected content:\n%s", out)
	}
}

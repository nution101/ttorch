package skills

import (
	"bytes"
	"strings"
	"testing"
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

func TestInstallCmd(t *testing.T) {
	cmd := InstallCmd("owner/repo")
	want := []string{"npx", "skills", "add", "owner/repo"}
	if strings.Join(cmd, " ") != strings.Join(want, " ") {
		t.Fatalf("InstallCmd = %v, want %v", cmd, want)
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

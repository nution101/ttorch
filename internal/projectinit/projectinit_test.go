package projectinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialized(t *testing.T) {
	dir := t.TempDir()
	if Initialized(dir) {
		t.Fatal("empty dir should not be initialized")
	}
	// An AGENTS.md without the managed marker is not "initialized".
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# hand-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Initialized(dir) {
		t.Fatal("AGENTS.md without the ttorch marker should not count as initialized")
	}
	if _, err := Init(dir, "pr"); err != nil {
		t.Fatal(err)
	}
	if !Initialized(dir) {
		t.Fatal("after Init the dir should be initialized")
	}
}

func TestInit_CreatesAgentsAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, "pr"); err != nil {
		t.Fatal(err)
	}
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "delivery-mode: pr") {
		t.Fatalf("AGENTS.md missing delivery mode: %s", agents)
	}
	target, err := os.Readlink(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || target != "AGENTS.md" {
		t.Fatalf("CLAUDE.md symlink wrong: target=%q err=%v", target, err)
	}
}

func TestInit_PreservesUserContentAndUpdatesMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# My project\nuse tabs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, "local"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(got), "use tabs") {
		t.Fatal("developer content lost")
	}
	if !strings.Contains(string(got), "delivery-mode: local") {
		t.Fatal("delivery mode not recorded")
	}

	// Re-init flips the mode in place without duplicating the block.
	if _, err := Init(dir, "validated"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if strings.Count(string(got), markerBegin) != 1 {
		t.Fatal("managed block duplicated on re-init")
	}
	if !strings.Contains(string(got), "delivery-mode: validated") {
		t.Fatal("mode not updated")
	}
}

func TestInit_DoesNotClobberRealClaudeMD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("hand-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes, err := Init(dir, "pr")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if string(got) != "hand-written" {
		t.Fatal("existing real CLAUDE.md was clobbered")
	}
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "real file") {
		t.Fatalf("expected a note about the existing file, got %v", notes)
	}
}

func TestInit_RejectsBadMode(t *testing.T) {
	if _, err := Init(t.TempDir(), "bogus"); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{"pr", "local", "validated", "trusted"} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "bogus", "PR", "trust"} {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true, want false", m)
		}
	}
}

func TestReadMode(t *testing.T) {
	// Default when there is no AGENTS.md at all.
	if got := ReadMode(t.TempDir()); got != "pr" {
		t.Fatalf("missing AGENTS.md: ReadMode = %q, want pr", got)
	}

	// Default when AGENTS.md exists but carries no managed block.
	noBlock := t.TempDir()
	if err := os.WriteFile(filepath.Join(noBlock, "AGENTS.md"), []byte("# hand-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadMode(noBlock); got != "pr" {
		t.Fatalf("no managed block: ReadMode = %q, want pr", got)
	}

	// Each valid mode round-trips through Init -> ReadMode.
	for _, mode := range []string{"pr", "local", "validated", "trusted"} {
		dir := t.TempDir()
		if _, err := Init(dir, mode); err != nil {
			t.Fatal(err)
		}
		if got := ReadMode(dir); got != mode {
			t.Fatalf("Init(%q) then ReadMode = %q", mode, got)
		}
	}

	// An unrecognized recorded mode falls back to the default.
	bad := t.TempDir()
	body := "x\n" + managedBlock("validated")
	body = strings.Replace(body, "delivery-mode: validated", "delivery-mode: bogus", 1)
	if err := os.WriteFile(filepath.Join(bad, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadMode(bad); got != "pr" {
		t.Fatalf("unrecognized mode: ReadMode = %q, want pr", got)
	}
}

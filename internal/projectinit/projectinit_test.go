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

package projectinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadAutoMintMaxAge(t *testing.T) {
	// Absent file / absent line / unparseable / non-positive ⇒ NO bound (the default; a
	// still-passing auto verdict always lands).
	for _, tc := range []struct {
		name, body string
	}{
		{"no file", ""},
		{"no line", "# notes\n- delivery-mode: trusted\n"},
		{"unparseable", "- auto-mint-max-age: soon\n"},
		{"zero", "- auto-mint-max-age: 0\n"},
		{"negative", "- auto-mint-max-age: -5h\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if d, ok := ReadAutoMintMaxAge(dir); ok || d != 0 {
				t.Fatalf("ReadAutoMintMaxAge = %v ok=%v, want 0/false", d, ok)
			}
		})
	}

	// A valid positive duration ⇒ that bound. Read from anywhere in the file (so a developer
	// can place it where `ttorch init` will not regenerate over it).
	dir := t.TempDir()
	if _, err := Init(dir, "trusted"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), append(b, []byte("\n- auto-mint-max-age: 72h\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, ok := ReadAutoMintMaxAge(dir); !ok || d != 72*time.Hour {
		t.Fatalf("ReadAutoMintMaxAge = %v ok=%v, want 72h/true", d, ok)
	}
}

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

func TestLiveMode(t *testing.T) {
	// A readable, initialized repo reports the same mode the gate's ReadMode resolves.
	dir := t.TempDir()
	if _, err := Init(dir, "trusted"); err != nil {
		t.Fatal(err)
	}
	if mode, ok := LiveMode(dir); !ok || mode != "trusted" {
		t.Fatalf("LiveMode(initialized) = (%q, %v), want (trusted, true)", mode, ok)
	}

	// A readable but uninitialized dir is genuinely live "pr" (ok=true), not a fallback.
	if mode, ok := LiveMode(t.TempDir()); !ok || mode != "pr" {
		t.Fatalf("LiveMode(uninitialized) = (%q, %v), want (pr, true)", mode, ok)
	}

	// A missing path reports ok=false so the caller falls back to a cached value
	// instead of masking a vanished repo as ReadMode's "pr" default.
	missing := filepath.Join(t.TempDir(), "gone")
	if mode, ok := LiveMode(missing); ok || mode != "" {
		t.Fatalf("LiveMode(missing) = (%q, %v), want (\"\", false)", mode, ok)
	}

	// A regular file is not a readable repo directory either.
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := LiveMode(file); ok {
		t.Fatal("LiveMode(regular file) reported ok=true")
	}
}

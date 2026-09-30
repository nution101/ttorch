package projectinit

import (
	"os"
	"path/filepath"
	"slices"
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

// TestInit_KeepsGateChangeApprovalLine: Init writes no gate-change-approval line, so a fresh
// repo gets the default (off), and the trusted block says how to turn the approval on. The
// line lives in the managed block, which Init regenerates, so re-running `ttorch init` (to
// refresh the block, or to change the mode) must carry the lead's line through. Dropping a
// `required` line would silently remove the approval the lead asked for.
func TestInit_KeepsGateChangeApprovalLine(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "AGENTS.md")
	if _, err := Init(dir, "trusted"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(agents)
	if strings.Contains(string(got), gateChangeApprovalKey) {
		t.Fatalf("a fresh Init must not write a gate-change-approval line:\n%s", got)
	}
	if !strings.Contains(string(got), "gate-change-approval: required") {
		t.Fatalf("the trusted block must tell the lead how to turn the gate-change approval on:\n%s", got)
	}
	if p, u := ParseGateChangeApproval(string(got)); p != GateChangeApprovalOff || u != nil {
		t.Fatalf("fresh Init: ParseGateChangeApproval = %q, %q; want off with nothing unrecognized (the prose must not parse as a line)", p, u)
	}

	withLine := strings.Replace(string(got), "- delivery-mode: trusted\n", "- delivery-mode: trusted\n- gate-change-approval: required\n", 1)
	if err := os.WriteFile(agents, []byte(withLine), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"trusted", "local", "trusted"} {
		if _, err := Init(dir, mode); err != nil {
			t.Fatal(err)
		}
		got, _ = os.ReadFile(agents)
		if n := strings.Count(string(got), gateChangeApprovalKey); n != 1 {
			t.Fatalf("re-Init(%s) left %d gate-change-approval lines, want 1:\n%s", mode, n, got)
		}
		if p, _ := ParseGateChangeApproval(string(got)); p != GateChangeApprovalRequired {
			t.Fatalf("re-Init(%s) dropped the lead's required line: ParseGateChangeApproval = %q\n%s", mode, p, got)
		}
		if ReadMode(dir) != mode {
			t.Fatalf("re-Init(%s): ReadMode = %q", mode, ReadMode(dir))
		}
	}

	// A misspelled value is carried through as-is, so it still reads as required and can still
	// be named, rather than being dropped and turning the approval off.
	typo := strings.Replace(string(got), "- gate-change-approval: required\n", "- gate-change-approval: requird\n", 1)
	if err := os.WriteFile(agents, []byte(typo), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, "trusted"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agents)
	if p, u := ParseGateChangeApproval(string(got)); p != GateChangeApprovalRequired || !slices.Equal(u, []string{"requird"}) {
		t.Fatalf("re-Init lost a misspelled line: ParseGateChangeApproval = %q, %q\n%s", p, u, got)
	}
}

// TestInit_ReportsTheGateChangeApproval: `ttorch init --mode trusted` says what the gate-change
// approval is set to and how to change it, names an unrecognized value, and says nothing about
// it in any other mode, where it has no effect.
func TestInit_ReportsTheGateChangeApproval(t *testing.T) {
	note := func(notes []string) string {
		for _, n := range notes {
			if strings.HasPrefix(n, "gate-change approval:") {
				return n
			}
		}
		return ""
	}
	dir := t.TempDir()
	notes, err := Init(dir, "trusted")
	if err != nil {
		t.Fatal(err)
	}
	if n := note(notes); !strings.Contains(n, "off (the default)") || !strings.Contains(n, "- gate-change-approval: required") {
		t.Fatalf("a fresh trusted init must say the approval is off and how to require it, got %q (notes %q)", n, notes)
	}

	agents := filepath.Join(dir, "AGENTS.md")
	b, _ := os.ReadFile(agents)
	for _, tc := range []struct{ line, want string }{
		{"- gate-change-approval: required", "required (kept from the existing block)"},
		{"- gate-change-approval: requird", `unrecognized value ("requird")`},
	} {
		body := strings.Replace(string(b), "- delivery-mode: trusted\n", "- delivery-mode: trusted\n"+tc.line+"\n", 1)
		if err := os.WriteFile(agents, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		notes, err := Init(dir, "trusted")
		if err != nil {
			t.Fatal(err)
		}
		if n := note(notes); !strings.Contains(n, tc.want) {
			t.Fatalf("%s: want the init note to contain %q, got %q", tc.line, tc.want, n)
		}
	}

	for _, mode := range []string{"pr", "local", "validated"} {
		notes, err := Init(t.TempDir(), mode)
		if err != nil {
			t.Fatal(err)
		}
		if n := note(notes); n != "" {
			t.Fatalf("%s mode: the approval has no effect, so init must not report it, got %q", mode, n)
		}
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

// TestParseMode: the text reader agrees with ReadMode, and a symlink's committed text (a path,
// with no managed block) reads as the pr default, as a missing file does.
func TestParseMode(t *testing.T) {
	for _, mode := range []string{"pr", "local", "validated", "trusted"} {
		if got := ParseMode("x\n" + managedBlock(mode)); got != mode {
			t.Errorf("ParseMode(block %s) = %q", mode, got)
		}
	}
	for _, text := range []string{"", "docs/AGENTS.md", "- delivery-mode: trusted\n"} {
		if got := ParseMode(text); got != "pr" {
			t.Errorf("ParseMode(%q) = %q, want pr", text, got)
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

// TestParseGateChangeApproval pins the reading of the gate-change-approval line. The approval
// is off unless the managed block asks for it: an exact `required` turns it on, and so does
// any value the reader does not recognize, which it also returns so the gate can name it. A
// typo in a line someone added to require the approval must not leave it off.
func TestParseGateChangeApproval(t *testing.T) {
	block := func(lines ...string) string {
		return "# notes\n\n" + markerBegin + "\n- delivery-mode: trusted\n" + strings.Join(lines, "\n") + "\n" + markerEnd + "\n"
	}
	for _, tc := range []struct {
		name, text, want string
		unrecognized     []string
	}{
		{"empty text", "", GateChangeApprovalOff, nil},
		{"no managed block", "- gate-change-approval: required\n", GateChangeApprovalOff, nil},
		{"block without the line", block(), GateChangeApprovalOff, nil},
		{"off", block("- gate-change-approval: off"), GateChangeApprovalOff, nil},
		{"off twice", block("- gate-change-approval: off", "- gate-change-approval: off"), GateChangeApprovalOff, nil},
		{"required", block("- gate-change-approval: required"), GateChangeApprovalRequired, nil},
		{"required with CRLF", strings.ReplaceAll(block("- gate-change-approval: required"), "\n", "\r\n"), GateChangeApprovalRequired, nil},
		{"required indented", block("  - gate-change-approval:   required  "), GateChangeApprovalRequired, nil},
		{"off then required", block("- gate-change-approval: off", "- gate-change-approval: required"), GateChangeApprovalRequired, nil},
		{"required then off", block("- gate-change-approval: required", "- gate-change-approval: off"), GateChangeApprovalRequired, nil},
		{"misspelled required", block("- gate-change-approval: requird"), GateChangeApprovalRequired, []string{"requird"}},
		{"capitalized Required", block("- gate-change-approval: Required"), GateChangeApprovalRequired, []string{"Required"}},
		{"capitalized Off", block("- gate-change-approval: Off"), GateChangeApprovalRequired, []string{"Off"}},
		{"trailing comment", block("- gate-change-approval: off # for now"), GateChangeApprovalRequired, []string{"off # for now"}},
		{"empty value", block("- gate-change-approval:"), GateChangeApprovalRequired, []string{""}},
		{"synonym", block("- gate-change-approval: yes"), GateChangeApprovalRequired, []string{"yes"}},
		{"garbage then off", block("- gate-change-approval: maybe", "- gate-change-approval: off"), GateChangeApprovalRequired, []string{"maybe"}},
		{"two unrecognized", block("- gate-change-approval: a", "- gate-change-approval: b"), GateChangeApprovalRequired, []string{"a", "b"}},
		{"required only outside the block", block() + "- gate-change-approval: required\n", GateChangeApprovalOff, nil},
		{"end marker before begin", markerEnd + "\n- gate-change-approval: required\n" + markerBegin + "\n", GateChangeApprovalOff, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, unrecognized := ParseGateChangeApproval(tc.text)
			if got != tc.want || !slices.Equal(unrecognized, tc.unrecognized) {
				t.Fatalf("ParseGateChangeApproval = %q, %q; want %q, %q\ntext:\n%s", got, unrecognized, tc.want, tc.unrecognized, tc.text)
			}
		})
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

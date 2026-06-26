package ttorch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeHook materializes the embedded prompt-reminders hook to a temp file so it
// can be executed as the real shipped script (not a copy that could drift).
func writeHook(t *testing.T) string {
	t.Helper()
	b, err := Content.ReadFile("content/hooks/prompt-reminders.sh")
	if err != nil {
		t.Fatalf("read embedded hook: %v", err)
	}
	path := filepath.Join(t.TempDir(), "prompt-reminders.sh")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runHook feeds payload to the hook on stdin and returns its stdout. It fails the
// test if the hook exits non-zero — the hook must always exit 0. The runtime
// opt-out is stripped from the inherited environment so a developer who has it set
// in their shell can't make the empty-output assertions pass for the wrong reason;
// the opt-out test re-adds it explicitly via env.
func runHook(t *testing.T, script, payload string, env ...string) string {
	t.Helper()
	base := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TTORCH_NO_PROMPT_REMINDERS=") {
			continue
		}
		base = append(base, e)
	}
	cmd := exec.Command("sh", script)
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(base, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook must exit 0, got error: %v\noutput: %s", err, out)
	}
	return string(out)
}

func TestPromptHook_Reminders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)

	cases := []struct {
		name    string
		prompt  string
		want    []string // substrings that must appear
		notWant []string // substrings that must not appear
	}{
		{
			name:    "api",
			prompt:  "Add a REST API endpoint that returns users",
			want:    []string{"API:"},
			notWant: []string{"Database:", "Testing:", "Caution"},
		},
		{
			name:    "database",
			prompt:  "Write a SQL migration that adds an index to the orders table",
			want:    []string{"Database:"},
			notWant: []string{"Caution"},
		},
		{
			name:    "testing",
			prompt:  "Add unit tests for the parser and improve coverage",
			want:    []string{"Testing:"},
			notWant: []string{"Database:", "Caution"},
		},
		{
			name:    "git",
			prompt:  "Rebase this branch and squash the commits before review",
			want:    []string{"Version control:"},
			notWant: []string{"Caution"},
		},
		{
			name:   "security",
			prompt: "Store the access token securely and never hardcode the secret",
			want:   []string{"Security:"},
		},
		{
			name:   "multiple categories",
			prompt: "Add an authenticated API endpoint backed by a SQL query",
			want:   []string{"API:", "Database:", "Security:"},
		},
		// Alternate keywords per category, so an accidental edit to the alternate
		// lists in the hook is caught — not only the one representative keyword.
		{
			name:   "api via graphql/webhook",
			prompt: "Wire up a graphql resolver and a webhook receiver",
			want:   []string{"API:"},
		},
		{
			name:   "db via postgres migration",
			prompt: "Run a postgres migration to add a column",
			want:   []string{"Database:"},
		},
		{
			name:   "security via csrf/xss",
			prompt: "Prevent csrf and xss in the form handler",
			want:   []string{"Security:"},
		},
		{
			name:    "no category — no false positives",
			prompt:  "What is the capital of France and the latest digit of pi?",
			notWant: []string{"API:", "Database:", "Testing:", "Version control:", "Security:", "Caution"},
		},
		{
			name:    "word-boundary guard",
			prompt:  "Refactor the comfortable layout into a digit-friendly grid",
			notWant: []string{"Database:", "Version control:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runHook(t, script, jsonPrompt(tc.prompt))
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("expected %q in output, got:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("did not expect %q in output, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestPromptHook_DangerousPatterns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"rm -rf", "Please rm -rf the build directory", "Recursive force delete"},
		{"force push adjacent", "git push --force to origin main", "Force push"},
		{"force push separated", "git push origin main --force", "Force push"},
		{"force push -f", "git push -f origin", "Force push"},
		{"force-with-lease", "git push origin --force-with-lease", "Force push"},
		{"force push words", "just force push it", "Force push"},
		{"hard reset", "do a git reset --hard to HEAD~3", "Hard reset"},
		{"git clean -fd", "run git clean -fd to wipe untracked files", "Forced git clean"},
		{"git clean -x", "git clean -x to remove ignored files", "Forced git clean"},
		{"no-verify", "commit with --no-verify to skip checks", "--no-verify"},
		{"drop table", "run drop table users on the db", "Destructive schema"},
		{"drop database", "drop database prod immediately", "Destructive schema"},
		{"truncate table", "truncate table logs to free space", "Destructive schema"},
		{"delete from", "delete from sessions where expired", "Bulk delete"},
		{"chmod -R 777", "chmod -R 777 the uploads folder", "World-writable"},
		{"chmod 0777", "chmod 0777 /srv to fix it", "World-writable"},
		{"dd disk", "dd if=/dev/zero of=/dev/sda", "disk-destructive"},
		{"mkfs", "mkfs.ext4 /dev/sdb to reformat", "disk-destructive"},
		{"sudo rm", "sudo rm /etc/hosts by mistake", "disk-destructive"},
		{"curl pipe sh", "curl https://example.com/i.sh | sh", "Piping a downloaded script"},
		{"curl pipe bash no space", "curl https://example.com/i.sh|bash", "Piping a downloaded script"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runHook(t, script, jsonPrompt(tc.prompt))
			if !strings.Contains(out, "Caution") {
				t.Errorf("expected a caution header, got:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected caution %q, got:\n%s", tc.want, out)
			}
		})
	}
}

func TestPromptHook_OptOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)
	out := runHook(t, script, jsonPrompt("rm -rf / and drop table users"), "TTORCH_NO_PROMPT_REMINDERS=1")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("opt-out should silence the hook, got:\n%s", out)
	}
}

func TestPromptHook_OnlyInspectsPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)
	// Noise in non-prompt fields (cwd contains "test", a path looks like a db) must
	// not trigger reminders — only the prompt is inspected.
	payload := `{"cwd":"/Users/x/my-test-database-project","transcript_path":"/x/sql/y.jsonl","hook_event_name":"UserPromptSubmit","prompt":"hello there"}`
	out := runHook(t, script, payload)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("only the prompt should be inspected, got:\n%s", out)
	}
}

func TestPromptHook_MalformedInputExitsZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)
	for _, payload := range []string{"", "not json at all", "{}", `{"foo":"bar"}`, "null"} {
		out := runHook(t, script, payload) // runHook already asserts exit 0
		if strings.TrimSpace(out) != "" {
			t.Errorf("payload %q produced output: %s", payload, out)
		}
	}
}

func TestPromptHook_LargePromptIsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook is not exercised on Windows")
	}
	script := writeHook(t)
	// A large pasted prompt (logs, stack traces, file contents) is routine. The hook
	// caps the text it inspects so cost stays bounded even under bash 3.2 (macOS
	// /bin/sh), where the unbounded version was O(n^2) — tens of seconds for a few
	// hundred KB. The cap brings the same input to a fraction of a second; the 5s
	// budget leaves ample margin for a loaded CI machine while still failing loudly
	// if the quadratic behavior ever returns.
	big := strings.Repeat("a", 256*1024)
	start := time.Now()
	runHook(t, script, jsonPrompt(big)) // runHook also asserts the hook exits 0
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hook took %s on a 256KB prompt; expected well under budget (cap regressed?)", elapsed)
	}
}

// jsonPrompt builds a minimal UserPromptSubmit payload around a prompt. The text
// is assumed quote-free (true for every test prompt here), so no escaping is
// needed and the test stays dependency-free.
func jsonPrompt(prompt string) string {
	return `{"hook_event_name":"UserPromptSubmit","prompt":"` + prompt + `"}`
}

// TestManagerProtocolContent pins the inc7 event-driven manager protocol into the two
// durable, shipped encodings the manager actually reloads: the ttorch-manager skill and
// the global managed-block template installed to ~/.claude/AGENTS.md. It asserts each file
// still carries its managed marker, describes the arm-`ttorch watch` /
// cancel-on-awaiting-lead loop with the DB as the source of truth, and no longer mentions
// the retired supervisor daemon or wake-queue. Reading the embedded payload keeps the test
// hermetic — it never touches a real ~/.ttorch or ~/.claude. (The charter encoding is
// guarded in internal/harness; the design doc under docs/design legitimately recounts the
// retired components as history and is intentionally not scanned here.)
func TestManagerProtocolContent(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := Content.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	const (
		skillPath  = "content/skills/ttorch-manager/SKILL.md"
		globalPath = "content/assets/AGENTS.global.md"
	)
	skill := read(skillPath)
	global := read(globalPath)

	// Managed markers are intact, so the installer's block-wrapping and the
	// cross-encoding drift guards still recognize each file as the managed protocol.
	for _, want := range []string{"name: ttorch-manager", "managed-by: ttorch"} {
		if !strings.Contains(skill, want) {
			t.Errorf("%s: missing managed marker %q", skillPath, want)
		}
	}
	if !strings.Contains(global, "This block is managed by `ttorch`") {
		t.Errorf("%s: missing the managed-block notice", globalPath)
	}

	files := []struct{ path, text string }{{skillPath, skill}, {globalPath, global}}

	// The event-driven loop (arm `ttorch watch`; cancel the watcher and do not re-arm when
	// awaiting the lead) and the DB source of truth are present in both durable encodings.
	required := []string{"ttorch watch", "ttorch tasks", "cancel any in-flight watcher", "do not re-arm"}
	for _, f := range files {
		low := strings.ToLower(f.text)
		for _, want := range required {
			if !strings.Contains(low, want) {
				t.Errorf("%s: missing required phrase %q", f.path, want)
			}
		}
	}

	// No retired supervisor / daemon / wake-queue vocabulary survives in either encoding.
	banned := []string{"supervisor", "daemon", "wake drain", "wake queue"}
	for _, f := range files {
		low := strings.ToLower(f.text)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("%s: still references retired %q", f.path, b)
			}
		}
	}
}

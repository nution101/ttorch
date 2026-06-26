package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/orchestrator"
)

// TestMain points TTORCH_HOME at a throwaway dir for the whole package so any test
// that reaches mgr()/orchestrator.New() (and thus db.Open against paths.StateDB())
// can never resolve to the real ~/.ttorch — where Open would create state.db and
// ImportLegacy would rename the live state/ dir away. The db.Open guard is the final
// fail-closed backstop.
func TestMain(m *testing.M) {
	os.Setenv("TTORCH_WORKER_TABS", "off")
	home, err := os.MkdirTemp("", "ttorch-cli-test-home-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("TTORCH_HOME", home)
	// Clear any inherited TTORCH_DB (a ttorch-managed worktree exports one pointing at the
	// real ~/.ttorch/state.db); StateDB() prefers it over TTORCH_HOME, so leaving it set
	// would resolve the DB back into the real home despite the pin above.
	os.Unsetenv("TTORCH_DB")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// nasty is a message body packed with the characters a shell would re-interpret:
// command substitution (backticks and $(...)), variable expansion, both quote
// kinds, angle-bracket redirects, pipes, and semicolons. The whole point of the
// stdin / --message-file paths is that these reach the worker untouched.
const nasty = "build `whoami` && echo $(rm -rf /) $HOME \"dq\" 'sq' <tag> a|b; c > d < e"

// failReader fails if anyone reads from it — used to prove the inline path never
// touches stdin.
type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Helper()
	f.t.Fatal("stdin must not be read for the inline send form")
	return 0, errors.New("unreachable")
}

func TestResolveSendMessage_InlineDoesNotReadStdin(t *testing.T) {
	got, err := resolveSendMessage([]string{"hello", "world"}, failReader{t}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("inline message = %q, want %q", got, "hello world")
	}
}

func TestResolveSendMessage_InlinePreservesSpecialChars(t *testing.T) {
	// Inline text that the caller's shell already delivered intact must pass
	// through byte-for-byte (joined on spaces, exactly as before).
	got, err := resolveSendMessage([]string{"a`b`", "$x", "<y>"}, failReader{t}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "a`b` $x <y>"; got != want {
		t.Fatalf("inline message = %q, want %q", got, want)
	}
}

func TestResolveSendMessage_StdinDashVerbatim(t *testing.T) {
	got, err := resolveSendMessage([]string{"-"}, strings.NewReader(nasty+"\n"), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nasty {
		t.Fatalf("stdin message = %q, want %q (verbatim, one trailing newline trimmed)", got, nasty)
	}
}

func TestResolveSendMessage_StdinImplicitWhenPiped(t *testing.T) {
	// No text at all + non-terminal stdin (a pipe) reads the message from stdin.
	got, err := resolveSendMessage(nil, strings.NewReader(nasty), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nasty {
		t.Fatalf("implicit-stdin message = %q, want %q", got, nasty)
	}
}

func TestResolveSendMessage_NoTextOnTerminalIsLoud(t *testing.T) {
	// No text + an interactive terminal must fail loudly, never block reading a TTY.
	if _, err := resolveSendMessage(nil, failReader{t}, true); err == nil {
		t.Fatal("textless send on a terminal must return an error, not read stdin")
	}
}

func TestResolveSendMessage_PreservesInternalNewlines(t *testing.T) {
	// Only the trailing newline run is trimmed; newlines inside the body stay.
	got, err := resolveSendMessage([]string{"-"}, strings.NewReader("line1\nline2\n\n"), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "line1\nline2"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestResolveSendMessage_MessageFileVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msg.txt")
	if err := os.WriteFile(path, []byte(nasty+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, form := range [][]string{
		{"--message-file", path},
		{"--message-file=" + path},
	} {
		got, err := resolveSendMessage(form, failReader{t}, false)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", form, err)
		}
		if got != nasty {
			t.Fatalf("%v: message = %q, want %q", form, got, nasty)
		}
	}
}

func TestResolveSendMessage_MessageFileErrors(t *testing.T) {
	cases := map[string][]string{
		"missing path":     {"--message-file"},
		"empty path":       {"--message-file="},
		"extra arguments":  {"--message-file", "a", "b"},
		"nonexistent file": {"--message-file", filepath.Join(t.TempDir(), "nope.txt")},
	}
	for name, rest := range cases {
		if _, err := resolveSendMessage(rest, failReader{t}, false); err == nil {
			t.Fatalf("%s: expected an error, got nil", name)
		}
	}
}

func TestResolveSendMessage_EmptyStdinResolvesEmpty(t *testing.T) {
	// An empty (or newline-only) stdin resolves to "" so cmdSend can reject it
	// rather than deliver a bare Enter.
	got, err := resolveSendMessage([]string{"-"}, strings.NewReader("\n\n"), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("newline-only stdin = %q, want empty", got)
	}
}

func TestResolveSendMessage_EmptyMessageFileResolvesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSendMessage([]string{"--message-file", path}, failReader{t}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("empty file = %q, want empty", got)
	}
}

func TestParseTouches(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"internal/cli,internal/orchestrator", []string{"internal/cli", "internal/orchestrator"}},
		// trims spaces, drops empties, cleans trailing slashes and ./
		{" internal/cli/ , , ./internal/state ", []string{"internal/cli", "internal/state"}},
		// de-duplicates (after cleaning) while preserving first-seen order
		{"internal/cli,internal/cli/,docs,internal/cli", []string{"internal/cli", "docs"}},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		got := parseTouches(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("parseTouches(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRenderStatus(t *testing.T) {
	var b strings.Builder
	renderStatus(&b, []statusRow{
		{ID: "a", Kind: "ship", State: "idle", Window: "wk-a", Project: "/repo", Footprint: []string{"internal/cli", "internal/state"}},
		{ID: "b", Kind: "ship", State: "working", Window: "wk-b", Project: "/repo"},
		// A gone worker that still carries a footprint: its row shows it, but the
		// summary must NOT count it (the conflict gate ignores gone workers).
		{ID: "d", Kind: "ship", State: "gone", Window: "wk-d", Project: "/repo", Footprint: []string{"internal/orchestrator"}},
	})
	out := b.String()
	// The footprint of a declaring worker is shown, including the gone one.
	if !strings.Contains(out, "touches: internal/cli, internal/state") {
		t.Fatalf("status should show worker a's footprint, got:\n%s", out)
	}
	if !strings.Contains(out, "touches: internal/orchestrator") {
		t.Fatalf("status should still show a gone worker's footprint for context, got:\n%s", out)
	}
	// Summary counts live only: a (idle) + b (working) are live; only a is idle;
	// only a declared a footprint among the live (d is gone, so not counted).
	if !strings.Contains(out, "2 live · 1 idle slots · 1 with footprints") {
		t.Fatalf("status summary line missing/wrong, got:\n%s", out)
	}
}

func TestCheckOverlapArgs(t *testing.T) {
	// One comma-separated argument.
	fp, repo, err := checkOverlapArgs([]string{"internal/cli,internal/orchestrator"})
	if err != nil || repo != "" || strings.Join(fp, "|") != "internal/cli|internal/orchestrator" {
		t.Fatalf("comma form: fp=%v repo=%q err=%v", fp, repo, err)
	}
	// Multiple space-separated positional arguments (the advertised alternative).
	fp, _, err = checkOverlapArgs([]string{"internal/cli", "internal/orchestrator"})
	if err != nil || strings.Join(fp, "|") != "internal/cli|internal/orchestrator" {
		t.Fatalf("space form: fp=%v err=%v", fp, err)
	}
	// --repo is parsed and the remaining positional is the footprint.
	fp, repo, err = checkOverlapArgs([]string{"--repo", "/r", "internal/cli"})
	if err != nil || repo != "/r" || strings.Join(fp, "|") != "internal/cli" {
		t.Fatalf("--repo form: fp=%v repo=%q err=%v", fp, repo, err)
	}
	// No paths -> a loud usage error (never a silent empty-footprint check).
	if _, _, err := checkOverlapArgs(nil); err == nil {
		t.Fatal("empty args must return a usage error")
	}
	if _, _, err := checkOverlapArgs([]string{"  ", ","}); err == nil {
		t.Fatal("whitespace/comma-only args must return a usage error")
	}
}

func TestRenderOverlap(t *testing.T) {
	// Disjoint -> a clear safe-to-dispatch line, no worker names.
	var safe strings.Builder
	renderOverlap(&safe, []string{"docs"}, nil)
	if !strings.Contains(safe.String(), "no conflicts") || !strings.Contains(safe.String(), "safe to dispatch") {
		t.Fatalf("disjoint report should say it is safe, got:\n%s", safe.String())
	}

	// Conflicts -> names each worker and the overlapping paths.
	var conf strings.Builder
	renderOverlap(&conf, []string{"internal/cli/cli.go"}, []orchestrator.Conflict{
		{TaskID: "a1", Window: "wk-a1", Project: "/repo", Overlaps: [][2]string{{"internal/cli/cli.go", "internal/cli"}}},
	})
	out := conf.String()
	if !strings.Contains(out, "a1") || !strings.Contains(out, "internal/cli/cli.go↔internal/cli") {
		t.Fatalf("conflict report should name the worker and overlap, got:\n%s", out)
	}
}

// TestCmdSend_EmptyMessageFailsLoudly guards the headline invariant — a send that
// resolves to nothing must fail loudly, never deliver a bare Enter. The guard
// lives in cmdSend (not resolveSendMessage), so it needs its own coverage; a
// regression that dropped it would otherwise keep every resolver test green. The
// --message-file path reaches the guard and returns before any manager/tmux call,
// so this stays hermetic.
func TestCmdSend_EmptyMessageFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmdSend([]string{"some-task", "--message-file", path})
	if err == nil {
		t.Fatal("cmdSend with an empty message must return an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "empty message") {
		t.Fatalf("error = %q, want it to mention an empty message", err)
	}
}

// TestCmdSecurityReview_ArgGuard pins the usage guards that run before any Manager call:
// too few args and an unknown subcommand both fail loudly with the usage line.
func TestCmdSecurityReview_ArgGuard(t *testing.T) {
	for _, args := range [][]string{nil, {"prep"}, {"bogus", "id1"}} {
		err := cmdSecurityReview(args)
		if err == nil {
			t.Fatalf("cmdSecurityReview(%v) must return a usage error", args)
		}
		if !strings.Contains(err.Error(), "security-review prep|record|show") {
			t.Fatalf("cmdSecurityReview(%v) error = %q, want the usage line", args, err)
		}
	}
}

// TestCmdQAReview_ArgGuard mirrors the security-review guard: too few args and an unknown
// subcommand both fail loudly with the qa-review usage line.
func TestCmdQAReview_ArgGuard(t *testing.T) {
	for _, args := range [][]string{nil, {"prep"}, {"bogus", "id1"}} {
		err := cmdQAReview(args)
		if err == nil {
			t.Fatalf("cmdQAReview(%v) must return a usage error", args)
		}
		if !strings.Contains(err.Error(), "qa-review prep|record|show") {
			t.Fatalf("cmdQAReview(%v) error = %q, want the usage line", args, err)
		}
	}
}

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

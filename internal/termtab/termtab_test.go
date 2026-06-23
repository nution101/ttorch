package termtab

import (
	"strings"
	"testing"
)

func TestViewNameSanitizes(t *testing.T) {
	cases := map[string]string{
		"wk-123":          "ttv-wk-123",
		"wk.foo:bar baz":  "ttv-wkfoobarbaz",
		"a/b\\c":          "ttv-abc",
		"under_score-1":   "ttv-under_score-1",
		"":                "ttv-",
		"  spaced  name ": "ttv-spacedname",
	}
	for in, want := range cases {
		if got := viewName(in); got != want {
			t.Errorf("viewName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestViewNameHasNoBadChars(t *testing.T) {
	got := viewName("w.k:1 2\t3")
	for _, bad := range []string{".", ":", " ", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("viewName produced %q containing forbidden %q", got, bad)
		}
	}
}

func TestViewCommand(t *testing.T) {
	cmd := viewCommand("ttorch", "wk-42")
	checks := []string{
		"new-session -A",
		"-s 'ttv-wk-42'",
		"-t 'ttorch'",
		"set-option -t 'ttv-wk-42' destroy-unattached on",
		"select-window -t 'ttv-wk-42':'wk-42'",
	}
	for _, c := range checks {
		if !strings.Contains(cmd, c) {
			t.Errorf("viewCommand missing %q in %q", c, cmd)
		}
	}
}

func TestViewCommandUsesSanitizedView(t *testing.T) {
	cmd := viewCommand("ttorch", "wk.bad:name")
	if !strings.Contains(cmd, "-s 'ttv-wkbadname'") {
		t.Errorf("viewCommand did not use sanitized view name: %q", cmd)
	}
	// The raw window keeps its original name in select-window (the grouped view
	// shares the source window list), but it is single-quoted so its ':' cannot
	// break the shell command.
	if !strings.Contains(cmd, "select-window -t 'ttv-wkbadname':'wk.bad:name'") {
		t.Errorf("viewCommand select-window unexpected: %q", cmd)
	}
}

func TestShqQuotes(t *testing.T) {
	cases := map[string]string{
		"safe":       `'safe'`,
		"a'b":        `'a'\''b'`,
		"; rm -rf /": `'; rm -rf /'`,
		"$(whoami)":  `'$(whoami)'`,
		"a`b`c":      "'a`b`c'",
	}
	for in, want := range cases {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestViewCommandIsInjectionSafe is the regression test for the shell-injection
// fix: dangerous session/window names must be confined inside single quotes so
// they reach tmux as literal operands rather than executing in the shell that
// osascript drives.
func TestViewCommandIsInjectionSafe(t *testing.T) {
	cmd := viewCommand("s$(touch /tmp/pwn)", "wk-x; rm -rf ~ #")
	// The malicious operands appear only in their fully single-quoted form.
	if !strings.Contains(cmd, `-t 's$(touch /tmp/pwn)'`) {
		t.Errorf("session not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, `:'wk-x; rm -rf ~ #'`) {
		t.Errorf("window not single-quoted: %q", cmd)
	}
	// And never in a bare, shell-active position: each operand is preceded by a
	// single quote, so the unquoted forms (right after "-t " / ":") are absent.
	for _, bare := range []string{"-t s$(touch", ":wk-x;"} {
		if strings.Contains(cmd, bare) {
			t.Errorf("unquoted metacharacters leaked (%q) in %q", bare, cmd)
		}
	}
}

func TestAppleScriptITerm(t *testing.T) {
	s := appleScript("iterm", `tmux new-session -A -s ttv-x -t ttorch`)
	if !strings.Contains(s, `tell application "iTerm"`) {
		t.Errorf("iterm script missing iTerm tell: %q", s)
	}
	if !strings.Contains(s, "create tab with default profile") {
		t.Errorf("iterm script missing tab creation: %q", s)
	}
	if !strings.Contains(s, "write text") {
		t.Errorf("iterm script missing write text: %q", s)
	}
	if !strings.Contains(s, "tmux new-session -A -s ttv-x -t ttorch") {
		t.Errorf("iterm script missing embedded tmux cmd: %q", s)
	}
}

func TestAppleScriptTerminal(t *testing.T) {
	s := appleScript("terminal", `tmux new-session -A -s ttv-x -t ttorch`)
	if !strings.Contains(s, `tell application "Terminal"`) {
		t.Errorf("terminal script missing Terminal tell: %q", s)
	}
	if !strings.Contains(s, "do script") {
		t.Errorf("terminal script missing do script: %q", s)
	}
	if !strings.Contains(s, "tmux new-session -A -s ttv-x -t ttorch") {
		t.Errorf("terminal script missing embedded tmux cmd: %q", s)
	}
}

func TestAppleScriptEscapesQuotes(t *testing.T) {
	// A command containing a double-quote and a backslash must be escaped so the
	// AppleScript string literal stays valid.
	in := `echo "hi" \world`
	s := appleScript("terminal", in)
	if !strings.Contains(s, `echo \"hi\" \\world`) {
		t.Errorf("escaping wrong; script: %q", s)
	}
	// The raw (unescaped) double-quote+text must not appear as a bare literal.
	if strings.Contains(s, `script "echo "hi"`) {
		t.Errorf("unescaped quote leaked into script: %q", s)
	}
}

func TestEnabledDefault(t *testing.T) {
	t.Setenv("TTORCH_WORKER_TABS", "")
	if !enabled() {
		t.Error("expected enabled by default")
	}
}

func TestEnabledDisableValues(t *testing.T) {
	for _, v := range []string{"0", "off", "false", "no", "OFF", " No "} {
		t.Setenv("TTORCH_WORKER_TABS", v)
		if enabled() {
			t.Errorf("expected disabled for %q", v)
		}
	}
}

func TestEnabledOtherValuesEnable(t *testing.T) {
	for _, v := range []string{"1", "on", "true", "yes", "anything"} {
		t.Setenv("TTORCH_WORKER_TABS", v)
		if !enabled() {
			t.Errorf("expected enabled for %q", v)
		}
	}
}

func TestTerminalChoice(t *testing.T) {
	cases := map[string]string{
		"":          "auto",
		"auto":      "auto",
		"iterm":     "iterm",
		"ITERM":     "iterm",
		" terminal": "terminal",
		"bogus":     "auto",
	}
	for in, want := range cases {
		t.Setenv("TTORCH_TERMINAL", in)
		if got := terminalChoice(); got != want {
			t.Errorf("terminalChoice() with %q = %q, want %q", in, got, want)
		}
	}
}

func TestManagerCommand(t *testing.T) {
	cmd := managerCommand("ttorch", "manager")
	checks := []string{
		"tmux select-window -t 'ttorch':'manager'",
		"attach-session -t 'ttorch'",
		`\;`,
	}
	for _, c := range checks {
		if !strings.Contains(cmd, c) {
			t.Errorf("managerCommand missing %q in %q", c, cmd)
		}
	}
}

// TestManagerCommandIsInjectionSafe confirms untrusted session/window names stay
// confined inside single quotes rather than executing in the shell.
func TestManagerCommandIsInjectionSafe(t *testing.T) {
	cmd := managerCommand("s$(touch /tmp/pwn)", "m; rm -rf ~ #")
	if !strings.Contains(cmd, `select-window -t 's$(touch /tmp/pwn)':'m; rm -rf ~ #'`) {
		t.Errorf("operands not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, `attach-session -t 's$(touch /tmp/pwn)'`) {
		t.Errorf("session not single-quoted in attach: %q", cmd)
	}
	for _, bare := range []string{"-t s$(touch", ":m;"} {
		if strings.Contains(cmd, bare) {
			t.Errorf("unquoted metacharacters leaked (%q) in %q", bare, cmd)
		}
	}
}

func TestITermNewWindowScript(t *testing.T) {
	s := itermNewWindowScript(`tmux attach-session -t ttorch`)
	checks := []string{
		`tell application "iTerm"`,
		"activate",
		"create window with default profile",
		"tell current session of current window to write text",
		"tmux attach-session -t ttorch",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("itermNewWindowScript missing %q in %q", c, s)
		}
	}
}

func TestITermNewWindowScriptEscapes(t *testing.T) {
	s := itermNewWindowScript(`echo "hi" \world`)
	if !strings.Contains(s, `echo \"hi\" \\world`) {
		t.Errorf("escaping wrong; script: %q", s)
	}
}

func TestITermLaunchScript(t *testing.T) {
	s := itermLaunchScript(`tmux attach-session -t ttorch`)
	// Cold launch must reuse the window iTerm opens, not unconditionally create one.
	checks := []string{
		`tell application "iTerm"`,
		"activate",
		"count of windows", // waits for / checks the launch window
		"tell current session of current window to write text", // reuses the current window
		"tmux attach-session -t ttorch",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("itermLaunchScript missing %q in %q", c, s)
		}
	}
	// It must escape the command for the AppleScript literal, same as the other path.
	if !strings.Contains(itermLaunchScript(`a "b" \c`), `a \"b\" \\c`) {
		t.Error("itermLaunchScript should escape quotes/backslashes")
	}
}

// TestOpenManagerSessionGatedOff confirms the launcher is a no-op (never calls
// osascript) when the feature is disabled, when Terminal.app is forced, when
// inside tmux, or when inside iTerm. These are the GUI-free gates.
func TestOpenManagerSessionGatedOff(t *testing.T) {
	// Feature disabled.
	t.Setenv("TTORCH_WORKER_TABS", "off")
	t.Setenv("TTORCH_TERMINAL", "")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TMUX", "")
	if OpenManagerSession("ttorch", "manager") {
		t.Error("expected no-op when disabled")
	}

	// Forced Terminal.app.
	t.Setenv("TTORCH_WORKER_TABS", "")
	t.Setenv("TTORCH_TERMINAL", "terminal")
	if OpenManagerSession("ttorch", "manager") {
		t.Error("expected no-op when Terminal.app forced")
	}

	// Already inside tmux.
	t.Setenv("TTORCH_TERMINAL", "")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	if OpenManagerSession("ttorch", "manager") {
		t.Error("expected no-op when inside tmux")
	}

	// Already inside iTerm.
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if OpenManagerSession("ttorch", "manager") {
		t.Error("expected no-op when inside iTerm")
	}
}

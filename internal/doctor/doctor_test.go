package doctor

import (
	"strings"
	"testing"
)

func TestWSLKind(t *testing.T) {
	cases := []struct {
		name, procVersion, want string
	}{
		{"wsl2", "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)", "wsl2"},
		{"wsl1", "Linux version 4.4.0-19041-Microsoft (...)", "wsl1"},
		{"native-linux", "Linux version 6.5.0-generic (buildd@...)", ""},
		{"macos-empty", "", ""},
	}
	for _, c := range cases {
		if got := wslKind(c.procVersion); got != c.want {
			t.Errorf("%s: wslKind(%q) = %q, want %q", c.name, c.procVersion, got, c.want)
		}
	}
}

func TestInstallSpec(t *testing.T) {
	if cmd, ok := installSpec("brew", "tmux"); !ok || cmd[0] != "brew" {
		t.Fatalf("brew spec wrong: %v ok=%v", cmd, ok)
	}
	if cmd, ok := installSpec("apt-get", "git"); !ok || cmd[0] != "sudo" {
		t.Fatalf("apt-get spec should use sudo: %v", cmd)
	}
	if _, ok := installSpec("unknown-mgr", "tmux"); ok {
		t.Fatal("unknown manager should not be installable")
	}
}

func TestITermInstallCmd(t *testing.T) {
	if cmd, ok := itermInstallCmd("brew"); !ok || len(cmd) != 4 ||
		cmd[0] != "brew" || cmd[1] != "install" || cmd[2] != "--cask" || cmd[3] != "iterm2" {
		t.Fatalf("brew iterm cmd wrong: %v ok=%v", cmd, ok)
	}
	for _, m := range []string{"apt-get", "dnf", "pacman", ""} {
		if _, ok := itermInstallCmd(m); ok {
			t.Errorf("iterm should not be installable via %q", m)
		}
	}
}

// TestReportTmuxVersion covers the floor report. tmux's version decides whether a
// worker view tab can be attached read-only, and below the floor the tab is a live
// keyboard on a running agent — so doctor, which is where an operator looks, has to
// say which of the three situations they are in.
func TestReportTmuxVersion(t *testing.T) {
	cases := []struct {
		banner string
		want   []string
		absent []string
	}{
		{"tmux 3.5a", []string{"tmux 3.5a"}, []string{"WRITABLE", "unrecognized"}},
		{"tmux 3.0a", []string{"tmux 3.0a", "below 3.2", "WRITABLE", "TTORCH_WORKER_TABS=0"}, []string{"unrecognized"}},
		{"tmux master", []string{"tmux master", "unrecognized"}, []string{"WRITABLE"}},
		{"", []string{"could not be read"}, []string{"WRITABLE", "unrecognized"}},
	}
	for _, c := range cases {
		var b strings.Builder
		reportTmuxVersion(&b, c.banner)
		got := b.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("reportTmuxVersion(%q) = %q, want it to mention %q", c.banner, got, w)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("reportTmuxVersion(%q) = %q, should not mention %q", c.banner, got, a)
			}
		}
	}
}

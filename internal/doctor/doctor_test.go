package doctor

import "testing"

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

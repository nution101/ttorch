package doctor

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestParseGitVersion(t *testing.T) {
	for banner, want := range map[string][3]int{
		"git version 2.50.1 (Apple Git-155)": {2, 50, 1},
		"git version 2.45.1.windows.1":       {2, 45, 1},
		"git version 2.45.0.rc1":             {2, 45, 0},
		"git version 2.32.0":                 {2, 32, 0},
		"git version 2.31.8\n":               {2, 31, 8},
		"git version 3.0":                    {3, 0, 0},
	} {
		got, ok := parseGitVersion(banner)
		if !ok || got != want {
			t.Errorf("parseGitVersion(%q) = %v, %v; want %v", banner, got, ok, want)
		}
	}
	for _, banner := range []string{"", "git version", "git version x.y", "hub version 2.50.1", "git version 2"} {
		if got, ok := parseGitVersion(banner); ok {
			t.Errorf("parseGitVersion(%q) = %v, want unreadable", banner, got)
		}
	}
}

// TestReportGitCloneFloor: with the flag on, a git below 2.45.1 is called out, including the
// 2.45.0 just before the security fixes and the 2.32 that met the old floor; 2.45.1 and newer
// pass, and an unreadable banner says so. With the flag off nothing is printed.
func TestReportGitCloneFloor(t *testing.T) {
	cases := []struct {
		banner  string
		enabled bool
		want    []string
		absent  []string
	}{
		{"git version 2.31.8", true, []string{"below 2.45.1", "GIT_CONFIG_GLOBAL", "CVE-2024-32004", "TTORCH_WORKER_CLONES"}, []string{"at or above"}},
		{"git version 2.32.0", true, []string{"below 2.45.1"}, []string{"at or above"}},
		{"git version 2.45.0", true, []string{"below 2.45.1"}, []string{"at or above"}},
		{"git version 2.45.0.rc1", true, []string{"below 2.45.1"}, []string{"at or above"}},
		{"git version 2.44.9", true, []string{"below 2.45.1"}, []string{"at or above"}},
		{"git version 2.45.1", true, []string{"at or above the 2.45.1"}, []string{"below"}},
		{"git version 2.46.0", true, []string{"at or above"}, []string{"below"}},
		{"git version 3.0", true, []string{"at or above"}, []string{"below"}},
		{"git version 2.50.1 (Apple Git-155)", true, []string{"2.50.1", "at or above"}, []string{"below"}},
		{"weird", true, []string{"could not be read", "2.45.1"}, nil},
	}
	for _, c := range cases {
		var b bytes.Buffer
		reportGitCloneFloor(&b, c.enabled, c.banner)
		got := b.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("reportGitCloneFloor(%q) = %q, want it to mention %q", c.banner, got, w)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("reportGitCloneFloor(%q) = %q, should not mention %q", c.banner, got, a)
			}
		}
	}
	var b bytes.Buffer
	reportGitCloneFloor(&b, false, "git version 2.20.0")
	if b.Len() != 0 {
		t.Errorf("flag off must print nothing, got %q", b.String())
	}
}

// TestRunReportsGitFloorOnlyWithFlag: the doctor report gains the git version line when
// TTORCH_WORKER_CLONES is on and is unchanged when it is off.
func TestRunReportsGitFloorOnlyWithFlag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	old := gitVersion
	t.Cleanup(func() { gitVersion = old })
	gitVersion = func() string { return "git version 2.30.1" }

	run := func(flag string) string {
		t.Setenv(WorkerClonesEnvVar, flag)
		var out bytes.Buffer
		if err := Run(&out, strings.NewReader("n\n"), false); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, off := range []string{"", "0", "false"} {
		if got := run(off); strings.Contains(got, "git version") {
			t.Errorf("%s=%q: report mentions the git version:\n%s", WorkerClonesEnvVar, off, got)
		}
	}
	if got := run("1"); !strings.Contains(got, "git version: 2.30.1 — below 2.45.1") {
		t.Errorf("%s=1: report lacks the floor warning:\n%s", WorkerClonesEnvVar, got)
	}
}

package doctor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/clonepool"
	"github.com/nution101/ttorch/internal/worktree"
)

// warnMarker is in doctor's git line exactly when it warns: it tells the lead what to do.
const warnMarker = "Upgrade git"

// TestReportGitCloneFloor: with the flag on, doctor warns about a git the clone import
// refuses and passes one it accepts, backports included: 2.43.4 carries the CVE-2024-32004 fix
// and passes, 2.43.0 does not and is warned. An unreadable banner says so. With the flag off
// nothing is printed.
func TestReportGitCloneFloor(t *testing.T) {
	cases := []struct {
		banner string
		warn   bool
		want   []string
	}{
		{"git version 2.43.4", false, []string{"2.43.4"}},
		{"git version 2.43.0", true, []string{"2.43.0", "CVE-2024-32004", "2.43.4", "TTORCH_WORKER_CLONES"}},
		{"git version 2.44.1", false, nil},
		{"git version 2.44.0", true, nil},
		{"git version 2.39.4", false, nil},
		{"git version 2.39.3", true, nil},
		{"git version 2.45.0", true, []string{"2.45.1"}},
		{"git version 2.45.0.rc1", true, nil},
		{"git version 2.45.1", false, nil},
		{"git version 2.31.8", true, []string{"GIT_CONFIG_GLOBAL"}},
		{"git version 2.46.0", false, nil},
		{"git version 2.50.1 (Apple Git-155)", false, []string{"2.50.1"}},
		{"weird", true, []string{"could not be read", "2.45.1"}},
	}
	for _, c := range cases {
		var b bytes.Buffer
		reportGitCloneFloor(&b, true, c.banner)
		got := b.String()
		if c.warn != strings.Contains(got, warnMarker) {
			t.Errorf("reportGitCloneFloor(%q) = %q; warn = %v, want %v", c.banner, got, !c.warn, c.warn)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("reportGitCloneFloor(%q) = %q, want it to mention %q", c.banner, got, w)
			}
		}
	}
	var b bytes.Buffer
	reportGitCloneFloor(&b, false, "git version 2.20.0")
	if b.Len() != 0 {
		t.Errorf("flag off must print nothing, got %q", b.String())
	}
}

// TestReportGitCloneFloorAgreesWithImport: doctor warns about exactly the versions the clone
// import refuses, across every series the import's table names and either side of each fix.
func TestReportGitCloneFloorAgreesWithImport(t *testing.T) {
	for minor := 30; minor <= 47; minor++ {
		for patch := 0; patch <= 5; patch++ {
			banner := fmt.Sprintf("git version 2.%d.%d", minor, patch)
			importOK, _ := worktree.ImportGitOK(banner)
			var b bytes.Buffer
			reportGitCloneFloor(&b, true, banner)
			if warned := strings.Contains(b.String(), warnMarker); warned == importOK {
				t.Errorf("%s: import accepts = %v but doctor warns = %v: %q", banner, importOK, warned, b.String())
			}
		}
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
		t.Setenv(clonepool.EnvVar, flag)
		var out bytes.Buffer
		if err := Run(&out, strings.NewReader("n\n"), false); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, off := range []string{"", "0", "false"} {
		if got := run(off); strings.Contains(got, "git version") {
			t.Errorf("%s=%q: report mentions the git version:\n%s", clonepool.EnvVar, off, got)
		}
	}
	if got := run("1"); !strings.Contains(got, "git version: 2.30.1") || !strings.Contains(got, warnMarker) {
		t.Errorf("%s=1: report lacks the floor warning:\n%s", clonepool.EnvVar, got)
	}
}

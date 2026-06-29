package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
)

// TestBriefForLaunchUsesStoredBrief proves the seam the scheduler daemon's dispatch relies on:
// SpawnWithEffort builds the worker's launch command from briefForLaunch, which returns the
// brief STORED for the task (by `ttorch task add --brief` / `spawn --brief-file`, via WriteBrief)
// rather than the generic stub. Both the manager's manual spawn and the daemon's autonomous
// dispatch go through this same path, so a stored brief becomes the worker's initial prompt
// either way — and a task with NO stored brief still launches, falling back to the stub.
func TestBriefForLaunchUsesStoredBrief(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}

	// No brief stored, INTERACTIVE spawn -> the generic stub (which tells the worker its
	// brief is on its way via the manager's `ttorch send`).
	p, err := m.briefForLaunch("nostored", "ship", false)
	if err != nil {
		t.Fatalf("interactive briefForLaunch with no brief must fall back to the stub, got err: %v", err)
	}
	stub := readBriefFile(t, p)
	if !strings.Contains(stub, "Your full task brief is on its way") {
		t.Fatalf("with no stored brief, interactive briefForLaunch must return the stub; got:\n%s", stub)
	}

	// A stored brief (what task add / spawn --brief-file write) is used verbatim, not the
	// stub — by BOTH the interactive and the autonomous (daemon) path.
	const body = "# Real brief\n\nImplement parts C and A.\n"
	if err := m.WriteBrief("stored", body); err != nil {
		t.Fatal(err)
	}
	for _, autonomous := range []bool{false, true} {
		p, err := m.briefForLaunch("stored", "ship", autonomous)
		if err != nil {
			t.Fatalf("briefForLaunch(stored, autonomous=%v): %v", autonomous, err)
		}
		got := readBriefFile(t, p)
		if got != body {
			t.Fatalf("briefForLaunch(autonomous=%v) should return the stored brief verbatim; got %q want %q", autonomous, got, body)
		}
		if strings.Contains(got, "Your full task brief is on its way") {
			t.Fatal("a stored brief must replace the stub, not sit beside it")
		}
	}
}

// TestBriefForLaunchAutonomousRefusesBriefless proves the second guard behind the
// scheduler's has_brief skip: an AUTONOMOUS dispatch (the daemon) of a task with no stored
// brief is REFUSED rather than launched on the "wait for ttorch send" stub that no
// autonomous send will satisfy. The interactive path keeps the stub fallback.
func TestBriefForLaunchAutonomousRefusesBriefless(t *testing.T) {
	t.Setenv("TTORCH_HOME", t.TempDir())
	m := &Manager{P: paths.Default()}

	if _, err := m.briefForLaunch("noautobrief", "ship", true); err == nil {
		t.Fatal("autonomous briefForLaunch with no stored brief must refuse, not write the manager-send stub")
	}
	// And it must NOT have written a stub as a side effect of refusing.
	if _, err := os.Stat(m.P.BriefPath("noautobrief")); err == nil {
		t.Fatal("a refused autonomous dispatch must not leave a brief stub behind")
	}
}

func readBriefFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read brief %s: %v", path, err)
	}
	return string(b)
}

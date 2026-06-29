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

	// No brief stored -> the generic stub (which tells the worker its brief is on its way).
	stub := readBriefFile(t, m.briefForLaunch("nostored", "ship"))
	if !strings.Contains(stub, "Your full task brief is on its way") {
		t.Fatalf("with no stored brief, briefForLaunch must return the stub; got:\n%s", stub)
	}

	// A stored brief (what task add / spawn --brief-file write) is used verbatim, not the stub.
	const body = "# Real brief\n\nImplement parts C and A.\n"
	if err := m.WriteBrief("stored", body); err != nil {
		t.Fatal(err)
	}
	got := readBriefFile(t, m.briefForLaunch("stored", "ship"))
	if got != body {
		t.Fatalf("briefForLaunch should return the stored brief verbatim; got %q want %q", got, body)
	}
	if strings.Contains(got, "Your full task brief is on its way") {
		t.Fatal("a stored brief must replace the stub, not sit beside it")
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

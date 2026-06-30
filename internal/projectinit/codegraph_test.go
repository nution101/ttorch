package projectinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/doctor"
)

// codegraphHarness installs test seams for the codegraph package vars and restores them.
// present controls whether codegraph is reported installed; the returned *[][]string
// records every runCodegraph invocation's args.
func codegraphHarness(t *testing.T, present bool) *[][]string {
	t.Helper()
	origFind, origRun := findCodegraph, runCodegraph
	t.Cleanup(func() { findCodegraph, runCodegraph = origFind, origRun })

	if present {
		findCodegraph = func() (string, bool) { return "/fake/codegraph", true }
	} else {
		findCodegraph = func() (string, bool) { return "", false }
	}
	var calls [][]string
	runCodegraph = func(dir string, args ...string) error {
		calls = append(calls, append([]string{dir}, args...))
		return nil
	}
	return &calls
}

func TestSetupCodegraphDisabledIsNoOp(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "") // default off
	calls := codegraphHarness(t, true)   // even with codegraph present, off wins

	dir := t.TempDir()
	notes, err := SetupCodegraph(dir)
	if err != nil {
		t.Fatalf("disabled SetupCodegraph errored: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("disabled SetupCodegraph must be silent, got notes: %v", notes)
	}
	if len(*calls) != 0 {
		t.Errorf("disabled SetupCodegraph must not run codegraph, got: %v", *calls)
	}
	assertNoFiles(t, dir)
}

func TestSetupCodegraphEnabledButAbsent(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "1")
	calls := codegraphHarness(t, false) // enabled but codegraph not installed

	dir := t.TempDir()
	notes, err := SetupCodegraph(dir)
	if err != nil {
		t.Fatalf("enabled+absent SetupCodegraph must not error, got: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "not installed") || !strings.Contains(notes[0], "no error") {
		t.Errorf("enabled+absent should give one clean no-op note, got: %v", notes)
	}
	if len(*calls) != 0 {
		t.Errorf("enabled+absent must not run codegraph, got: %v", *calls)
	}
	assertNoFiles(t, dir)
}

func TestSetupCodegraphEnabledPresentFreshBuild(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "true")
	calls := codegraphHarness(t, true)

	dir := t.TempDir()
	notes, err := SetupCodegraph(dir)
	if err != nil {
		t.Fatalf("enabled+present SetupCodegraph errored: %v", err)
	}

	// A fresh repo (no .codegraph) is init'd, not sync'd.
	if len(*calls) != 1 {
		t.Fatalf("want one codegraph call, got %v", *calls)
	}
	got := (*calls)[0]
	if got[0] != dir || got[1] != "init" || got[2] != dir {
		t.Errorf("want [%q init %q], got %v", dir, dir, got)
	}

	// .mcp.json written with the codegraph server.
	mcp, err := os.ReadFile(filepath.Join(dir, mcpConfigFile))
	if err != nil {
		t.Fatalf("expected %s to be written: %v", mcpConfigFile, err)
	}
	if !strings.Contains(string(mcp), `"codegraph"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf("%s missing codegraph server entry: %s", mcpConfigFile, mcp)
	}

	// .codegraph/ git-ignored.
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), codegraphIndexDir+"/") {
		t.Errorf(".gitignore should list %s/: %q err=%v", codegraphIndexDir, string(gi), err)
	}

	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "built the repo code index") {
		t.Errorf("expected a build note, got: %v", notes)
	}
}

func TestSetupCodegraphEnabledPresentRefresh(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "1")
	calls := codegraphHarness(t, true)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, codegraphIndexDir), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := SetupCodegraph(dir); err != nil {
		t.Fatalf("SetupCodegraph errored: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0][1] != "sync" {
		t.Errorf("an existing index must be sync'd, got %v", *calls)
	}
}

func TestSetupCodegraphMergeSafeMCP(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "1")
	codegraphHarness(t, true)

	dir := t.TempDir()
	mcpPath := filepath.Join(dir, mcpConfigFile)
	pre := `{"mcpServers":{"mine":{"type":"stdio","command":"mine"}}}`
	if err := os.WriteFile(mcpPath, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SetupCodegraph(dir); err != nil {
		t.Fatalf("SetupCodegraph errored: %v", err)
	}
	got, _ := os.ReadFile(mcpPath)
	if !strings.Contains(string(got), `"mine"`) {
		t.Errorf("merge clobbered the pre-existing 'mine' server: %s", got)
	}
	if !strings.Contains(string(got), `"codegraph"`) {
		t.Errorf("codegraph server not added: %s", got)
	}
}

func TestSetupCodegraphBuildFailureDegrades(t *testing.T) {
	t.Setenv(doctor.CodegraphEnvVar, "1")
	origFind, origRun := findCodegraph, runCodegraph
	t.Cleanup(func() { findCodegraph, runCodegraph = origFind, origRun })
	findCodegraph = func() (string, bool) { return "/fake/codegraph", true }
	runCodegraph = func(dir string, args ...string) error { return os.ErrPermission }

	dir := t.TempDir()
	notes, err := SetupCodegraph(dir)
	if err != nil {
		t.Fatalf("a codegraph build failure must NOT fail setup, got: %v", err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "skipped") {
		t.Errorf("expected a 'skipped' degrade note, got: %v", notes)
	}
	// Even on a build failure, the MCP wiring is still written so the tool is available
	// once codegraph can index.
	if _, err := os.Stat(filepath.Join(dir, mcpConfigFile)); err != nil {
		t.Errorf("%s should still be written after a build failure: %v", mcpConfigFile, err)
	}
}

func TestGraphExists(t *testing.T) {
	dir := t.TempDir()
	if GraphExists(dir) {
		t.Fatal("GraphExists should be false for a fresh dir")
	}
	if err := os.MkdirAll(filepath.Join(dir, codegraphIndexDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if !GraphExists(dir) {
		t.Fatal("GraphExists should be true once .codegraph/ exists")
	}
}

func TestEnsureGitignoreIdempotent(t *testing.T) {
	dir := t.TempDir()
	added, err := ensureGitignore(dir, ".codegraph/")
	if err != nil || !added {
		t.Fatalf("first ensureGitignore: added=%v err=%v", added, err)
	}
	added, err = ensureGitignore(dir, ".codegraph/")
	if err != nil || added {
		t.Fatalf("second ensureGitignore must be a no-op: added=%v err=%v", added, err)
	}
	// An existing, unrelated .gitignore must be preserved.
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, ".gitignore"), []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureGitignore(dir2, ".codegraph/"); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(dir2, ".gitignore"))
	if !strings.Contains(string(gi), "node_modules") || !strings.Contains(string(gi), ".codegraph/") {
		t.Errorf("existing .gitignore content lost: %q", gi)
	}
}

// assertNoFiles fails if SetupCodegraph left any artifact behind (the off / absent paths
// must write nothing).
func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{mcpConfigFile, ".gitignore", codegraphIndexDir} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("expected no %s to be written, but it exists", f)
		}
	}
}

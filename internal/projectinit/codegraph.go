package projectinit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/doctor"
	"github.com/nution101/ttorch/internal/profile"
)

// codegraphIndexDir is the directory codegraph writes its index into, at the root of the
// indexed project. It is a per-checkout build artifact, so ttorch git-ignores it.
const codegraphIndexDir = ".codegraph"

// mcpConfigFile is Claude Code's project-scoped MCP server config. ttorch writes the
// codegraph server entry here, merge-safe, so a worker picks the tool up the same way it
// picks up AGENTS.md: from the branch it is launched on.
const mcpConfigFile = ".mcp.json"

// findCodegraph reports the codegraph binary path and whether it is installed. Indirected
// through a var (over doctor.FindCodegraph) so the present/absent branches are testable
// without depending on whether codegraph is installed on the machine running the suite.
var findCodegraph = doctor.FindCodegraph

// runCodegraph runs `codegraph <args...>` in dir. Indirected through a var so tests drive
// the build/refresh branches without invoking the real binary — the suite must pass with
// codegraph absent.
var runCodegraph = func(dir string, args ...string) error {
	bin, ok := doctor.FindCodegraph()
	if !ok {
		return fmt.Errorf("codegraph not found")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codegraph %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// GraphExists reports whether a codegraph index has already been built at repoDir.
func GraphExists(repoDir string) bool {
	fi, err := os.Stat(filepath.Join(repoDir, codegraphIndexDir))
	return err == nil && fi.IsDir()
}

// SetupCodegraph wires the opt-in, default-off codegraph worker code-navigation feature
// into repoDir. It is evidence-gated and degrades cleanly in every case:
//
//   - feature disabled (the default): a silent no-op — no notes, no error, no files written,
//     so `ttorch init` behaves exactly as it does without codegraph.
//   - enabled but codegraph absent: a clean no-op plus one explanatory note; never an error,
//     never a file written.
//   - enabled and codegraph present: builds the repo's index (or refreshes an existing one),
//     writes the codegraph MCP server entry into .mcp.json (merge-safe — other servers and
//     top-level keys are preserved; an unparseable existing file is left untouched), and
//     ensures the index dir is git-ignored. A codegraph build failure is surfaced as a note,
//     not a hard error, so first-use setup never fails `ttorch init`.
//
// It returns human-readable notes for the caller to print and an error only for a genuinely
// unexpected I/O failure while writing ttorch's own files.
func SetupCodegraph(repoDir string) ([]string, error) {
	if !doctor.CodegraphEnabled() {
		return nil, nil
	}
	if _, ok := findCodegraph(); !ok {
		return []string{fmt.Sprintf("codegraph: %s=1 is set but codegraph is not installed — worker code-navigation stays off (no error)", doctor.CodegraphEnvVar)}, nil
	}

	var notes []string

	// Build or refresh the index: an existing index is sync'd (incremental); a fresh repo is
	// init'd. A codegraph failure degrades to a note — first-use setup must never fail on it.
	if GraphExists(repoDir) {
		if err := runCodegraph(repoDir, "sync", repoDir); err != nil {
			notes = append(notes, "codegraph: index refresh skipped: "+err.Error())
		} else {
			notes = append(notes, "codegraph: refreshed the repo code index")
		}
	} else {
		if err := runCodegraph(repoDir, "init", repoDir); err != nil {
			notes = append(notes, "codegraph: index build skipped: "+err.Error())
		} else {
			notes = append(notes, "codegraph: built the repo code index")
		}
	}

	// Wire the MCP server entry so workers get codegraph as a navigation tool.
	if note, err := writeMCPEntry(repoDir); err != nil {
		notes = append(notes, "codegraph: could not write "+mcpConfigFile+": "+err.Error())
	} else if note != "" {
		notes = append(notes, note)
	}

	// Keep the index out of git (it is a per-checkout build artifact).
	if added, err := ensureGitignore(repoDir, codegraphIndexDir+"/"); err != nil {
		notes = append(notes, "codegraph: could not update .gitignore: "+err.Error())
	} else if added {
		notes = append(notes, "codegraph: git-ignored "+codegraphIndexDir+"/")
	}

	return notes, nil
}

// writeMCPEntry adds the codegraph MCP server to repoDir/.mcp.json, merge-safe: it creates
// the file when absent and otherwise preserves every existing server. It returns "" (no
// note) when the entry is already present and current. An existing-but-invalid .mcp.json is
// left untouched and reported as an error so developer content is never clobbered.
func writeMCPEntry(repoDir string) (string, error) {
	path := filepath.Join(repoDir, mcpConfigFile)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	updated, changed, err := profile.UpsertCodegraphMCP(existing)
	if err != nil {
		return "", fmt.Errorf("existing %s is not valid JSON; left it untouched (%v)", mcpConfigFile, err)
	}
	if !changed {
		return "", nil
	}
	if err := atomicWrite(path, updated); err != nil {
		return "", err
	}
	if len(existing) == 0 {
		return "wrote " + mcpConfigFile + " with the codegraph MCP server — commit it so workers pick it up", nil
	}
	return "added the codegraph MCP server to " + mcpConfigFile + " — commit it so workers pick it up", nil
}

// ensureGitignore makes sure pattern appears as its own line in repoDir/.gitignore,
// creating the file or appending to it without disturbing existing entries. It returns
// whether it added the line (false when the pattern was already present).
func ensureGitignore(repoDir, pattern string) (bool, error) {
	path := filepath.Join(repoDir, ".gitignore")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return false, nil
		}
	}
	sep := ""
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		sep = "\n"
	}
	return true, atomicWrite(path, []byte(string(b)+sep+pattern+"\n"))
}

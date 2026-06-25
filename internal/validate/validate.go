// Package validate runs a repository's own checks (build / vet / lint / test) against
// a worker's worktree before delivery. Checks are auto-detected per ecosystem, or
// overridden by a repo-provided .ttorch/validate.sh.
//
// Trust: these are the repository's OWN commands (and any .ttorch/validate.sh),
// executed on the host with the operator's credentials. Run validation only against
// repositories and worker output you trust. Each step runs under a timeout
// (TTORCH_VALIDATE_TIMEOUT, default 10m) so a hung command cannot block indefinitely.
package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/ciparity"
)

// Step is one named check to run in the worktree.
type Step struct {
	Name string
	Cmd  []string
}

// Result is the outcome of running a Step.
type Result struct {
	Name   string
	Passed bool
	Output string
}

// Detect returns the checks for a worktree: an explicit .ttorch/validate.sh override if
// present, else an ecosystem-appropriate default set, else nil (no checks detected).
func Detect(dir string) []Step {
	if fileExists(filepath.Join(dir, ".ttorch", "validate.sh")) {
		return []Step{{Name: "custom", Cmd: []string{"sh", ".ttorch/validate.sh"}}}
	}
	return DetectDefaults(dir)
}

// DetectDefaults returns the built-in, ecosystem-detected checks for dir, IGNORING any
// repo-provided .ttorch/validate.sh override. Detect uses these as its fallback; the
// trust gate uses them directly so a worker cannot redefine the gate's commands by
// committing a .ttorch/validate.sh on its own branch — only the fixed ttorch-defined
// steps run. Returns nil when no ecosystem is detected (the caller treats that as a
// hard block).
func DetectDefaults(dir string) []Step {
	if fileExists(filepath.Join(dir, "go.mod")) {
		return []Step{
			{Name: "build", Cmd: []string{"go", "build", "./..."}},
			{Name: "vet", Cmd: []string{"go", "vet", "./..."}},
			{Name: "fmt", Cmd: []string{"sh", "-c", `o=$(gofmt -l .); test -z "$o" || { echo "unformatted:"; echo "$o"; exit 1; }`}},
			{Name: "test", Cmd: []string{"go", "test", "./..."}},
		}
	}
	if scripts := packageScripts(filepath.Join(dir, "package.json")); scripts != nil {
		var steps []Step
		if _, ok := scripts["build"]; ok {
			steps = append(steps, Step{Name: "build", Cmd: []string{"npm", "run", "build"}})
		}
		if _, ok := scripts["lint"]; ok {
			steps = append(steps, Step{Name: "lint", Cmd: []string{"npm", "run", "lint"}})
		}
		if _, ok := scripts["test"]; ok {
			steps = append(steps, Step{Name: "test", Cmd: []string{"npm", "test"}})
		}
		return steps
	}
	return nil
}

// CIParitySteps converts CI-parity steps extracted from a repository's GitHub Actions
// workflows (see internal/ciparity) into runnable validate Steps. This lets a CI-parity
// run report per-step PASS/FAIL through the same Run/Result path as the heuristic checks,
// while being clearly distinct in source: these are the repo's actual CI commands rather
// than the ecosystem defaults from Detect/DetectDefaults.
func CIParitySteps(ciSteps []ciparity.Step) []Step {
	steps := make([]Step, 0, len(ciSteps))
	for _, s := range ciSteps {
		steps = append(steps, Step{Name: s.Label(), Cmd: s.Command()})
	}
	return steps
}

// DefaultTimeout bounds each check; override with TTORCH_VALIDATE_TIMEOUT.
const DefaultTimeout = 10 * time.Minute

func stepTimeout() time.Duration {
	if v := os.Getenv("TTORCH_VALIDATE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}

// Run executes each step in dir under a per-step timeout, capturing combined output,
// and continues through all steps (so every failure is reported, not just the first).
func Run(dir string, steps []Step) []Result {
	to := stepTimeout()
	results := make([]Result, 0, len(steps))
	for _, s := range steps {
		results = append(results, runStep(dir, s, to))
	}
	return results
}

func runStep(dir string, s Step, to time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	c := exec.CommandContext(ctx, s.Cmd[0], s.Cmd[1:]...)
	c.Dir = dir
	// Run the command in its own process group and kill the whole group on timeout,
	// so a hung command's children (servers, watchers) are reaped too.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process != nil {
			return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return Result{Name: s.Name, Passed: false, Output: fmt.Sprintf("timed out after %s", to)}
	}
	return Result{
		Name:   s.Name,
		Passed: err == nil,
		Output: strings.TrimRight(string(out), "\n"),
	}
}

// Failures returns the results that did not pass.
func Failures(results []Result) []Result {
	var f []Result
	for _, r := range results {
		if !r.Passed {
			f = append(f, r)
		}
	}
	return f
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func packageScripts(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil
	}
	return pkg.Scripts
}

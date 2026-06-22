// Package validate runs a repository's own checks (build / vet / lint / test) against
// a worker's worktree before delivery. Checks are auto-detected per ecosystem, or
// overridden by a repo-provided .orcha/validate.sh.
package validate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Detect returns the checks for a worktree: an explicit .orcha/validate.sh override if
// present, else an ecosystem-appropriate default set, else nil (no checks detected).
func Detect(dir string) []Step {
	if fileExists(filepath.Join(dir, ".orcha", "validate.sh")) {
		return []Step{{Name: "custom", Cmd: []string{"sh", ".orcha/validate.sh"}}}
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		return []Step{
			{Name: "build", Cmd: []string{"go", "build", "./..."}},
			{Name: "vet", Cmd: []string{"go", "vet", "./..."}},
			{Name: "fmt", Cmd: []string{"sh", "-c", `test -z "$(gofmt -l .)"`}},
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

// Run executes each step in dir, capturing combined output, and continues through all
// steps (so every failure is reported, not just the first).
func Run(dir string, steps []Step) []Result {
	results := make([]Result, 0, len(steps))
	for _, s := range steps {
		c := exec.Command(s.Cmd[0], s.Cmd[1:]...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		results = append(results, Result{
			Name:   s.Name,
			Passed: err == nil,
			Output: strings.TrimRight(string(out), "\n"),
		})
	}
	return results
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

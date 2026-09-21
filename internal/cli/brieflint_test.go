package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lintRepo builds a neutral fixture repository: one commit on a base branch pushed to a
// local bare "remote", so the brief lint's remote and ref checks run offline.
func lintRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	run := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{
			"-c", "user.name=lint fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false",
		}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	remote := t.TempDir()
	run(remote, "init", "--bare", "-b", "main", "-q")
	repo := t.TempDir()
	run(repo, "init", "-b", "main", "-q")
	run(repo, "remote", "add", "origin", remote)
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Several lines, so a brief can cite a real line of it.
	if err := os.WriteFile(filepath.Join(repo, "pkg", "thing.go"), []byte("package pkg\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "base")
	run(repo, "push", "-q", "origin", "main")
	run(repo, "fetch", "-q", "origin")
	return repo
}

// cleanBrief satisfies every rule against a lintRepo fixture.
const cleanBrief = `# Task

Base is origin/main in this repository.

Follow this repo's demonstrated conventions and match the shape of pkg/thing.go.

No changes to any product branch and no PR until I have reviewed the work. Commit on your
own branch, leave the worktree clean, and report the sha.
`

// citingBrief is cleanBrief with the most ordinary citation a brief makes: a real line of
// existing code, with no citations ref supplied anywhere.
var citingBrief = strings.Replace(cleanBrief, "match the shape of pkg/thing.go",
	"fix the helper at pkg/thing.go:4", 1)

// defectiveBrief violates the target-branch and prohibition rules and points at no
// standards.
const defectiveBrief = "# Task\n\nJust do it. Do NOT push.\n"

func writeBrief(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// exitOf reports the process exit status run() would produce for err.
func exitOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.exitStatus()
	}
	return 1
}

// The three outcomes must be distinguishable by exit status alone: a check that could not
// run must never share a status with a pass.
func TestCmdBriefLintExitStatuses(t *testing.T) {
	repo := lintRepo(t)

	out, err := captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, cleanBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != 0 {
		t.Fatalf("a clean brief must exit 0, got %d (%v)\n%s", got, err, out)
	}
	if !strings.Contains(out, "all 5 enabled rules passed") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, defectiveBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != exitLintViolation {
		t.Fatalf("a violated rule must exit %d, got %d (%v)\n%s", exitLintViolation, got, err, out)
	}
	// Every violation in one run, each with its rule named and its text quoted.
	for _, want := range []string{"target-branch", "prohibition", "standards", `offending text: "Do NOT push"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must mention %q:\n%s", want, out)
		}
	}

	// The ordinary case: a real line of existing code, no citations ref. This must pass,
	// because pointing a worker at file:line is how briefs normally point at code.
	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, citingBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != 0 {
		t.Fatalf("a brief citing a real line must exit 0, got %d (%v)\n%s", got, err, out)
	}
	if !strings.Contains(out, "file:line citations resolved at origin/main (defaulted to the base") {
		t.Fatalf("the report must name the ref and that it was defaulted:\n%s", out)
	}

	// A line past end-of-file on the base is ambiguous, not a violation: it may exist at the
	// commit the citation was read at. Reported, never passed.
	brief := strings.Replace(cleanBrief, "pkg/thing.go", "pkg/thing.go:400", 1)
	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, brief), "--repo", repo})
	})
	if got := exitOf(t, err); got != exitLintIndeterminate {
		t.Fatalf("an unevaluable check must exit %d, got %d (%v)\n%s", exitLintIndeterminate, got, err, out)
	}
	if !strings.Contains(out, "CANNOT-EVALUATE") {
		t.Fatalf("output must mark the check unevaluable:\n%s", out)
	}
}

func TestCmdBriefLintUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no argument":  {},
		"flag first":   {"--repo", "."},
		"missing file": {filepath.Join(t.TempDir(), "nope.md")},
		"unknown flag": {writeBrief(t, cleanBrief), "--nope"},
	} {
		err := cmdBriefLint(args)
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if got := exitOf(t, err); got != exitLintUsage {
			t.Fatalf("%s: want exit %d, got %d (%v)", name, exitLintUsage, got, err)
		}
	}
	// An empty brief is a usage error, not a passing lint.
	if err := cmdBriefLint([]string{writeBrief(t, "   \n")}); exitOf(t, err) != exitLintUsage {
		t.Fatalf("an empty brief must be a usage error, got %v", err)
	}
}

// A project turning a rule off must see the override in the output, and an unknown rule id
// must not read as a pass.
func TestCmdBriefLintProjectOverrides(t *testing.T) {
	repo := lintRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")

	if err := os.WriteFile(agents, []byte("# Fixture\n\n- brief-lint-disable: target-branch, standards, prohibition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, defectiveBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != 0 {
		t.Fatalf("with those rules disabled the brief passes, got exit %d (%v)\n%s", got, err, out)
	}
	for _, want := range []string{"rule target-branch: DISABLED", "rule standards: DISABLED", "AGENTS.md", "all 2 enabled rules passed (3 disabled)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("an override must be visible in the output, want %q:\n%s", want, out)
		}
	}

	if err := os.WriteFile(agents, []byte("# Fixture\n\n- brief-lint-disable: target-brunch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, cleanBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != exitLintIndeterminate {
		t.Fatalf("an unknown disabled rule must not read as a pass, got exit %d (%v)\n%s", got, err, out)
	}
}

// Only this package's own errors may choose an exit status. *exec.ExitError carries an
// ExitCode method promoted from *os.ProcessState, so an exitCoder declared as
// `ExitCode() int` would match it — and cmdUpdate returns one unwrapped from the re-exec
// that applies a new install, which would have changed `ttorch update`'s exit code on a
// failed self-install from a flat 1 to the child's own status.
func TestRunOnlyHonoursThisPackagesExitStatuses(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 7").Run()
	if err == nil {
		t.Fatal("want an ExitError from the fixture command")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("fixture produced %T, want *exec.ExitError", err)
	}
	if got := ee.ExitCode(); got != 7 {
		t.Fatalf("fixture exit code = %d, want 7", got)
	}
	if got := run(err); got != 1 {
		t.Fatalf("run(*exec.ExitError) = %d, want 1: an unrelated command's exit code leaks through exitCoder", got)
	}
	// A brief-lint outcome still chooses its own.
	if got := run(lintError{"three rules could not be evaluated", exitLintIndeterminate}); got != exitLintIndeterminate {
		t.Fatalf("run(lintError) = %d, want %d", got, exitLintIndeterminate)
	}
}

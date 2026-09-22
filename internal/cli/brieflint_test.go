package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/brieflint"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/worktree"
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
	if !strings.Contains(out, "5 of 5 rules ran: passed") {
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
	if got := exitOf(t, err); got != exitLintPartial {
		t.Fatalf("with those rules disabled nothing is violated but coverage is reduced, want exit %d, got %d (%v)\n%s", exitLintPartial, got, err, out)
	}
	for _, want := range []string{"rule target-branch: DISABLED", "rule standards: DISABLED", "AGENTS.md", "2 of 5 rules ran (3 disabled by project config): everything that ran passed"} {
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

// A repository that turns off every rule must not get a clean gate out of it. The summary
// must not say passed, and the exit status must be the cannot-evaluate one, because every
// non-human consumer reads the status and not the notes.
func TestCmdBriefLintRefusesWhenEveryRuleIsDisabled(t *testing.T) {
	repo := lintRepo(t)
	var ids []string
	for _, id := range brieflint.Rules() {
		ids = append(ids, string(id))
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"),
		[]byte("# Fixture\n\n- brief-lint-disable: "+strings.Join(ids, ", ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, defectiveBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != exitLintIndeterminate {
		t.Fatalf("a run with every rule disabled must exit %d, got %d (%v)\n%s", exitLintIndeterminate, got, err, out)
	}
	if strings.Contains(out, "passed") {
		t.Fatalf("a run that evaluated nothing must not use the word passed:\n%s", out)
	}
	for _, want := range []string{"0 of 5 rules ran", "nothing was checked"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must state %q:\n%s", want, out)
		}
	}
}

// Every summary line says how much of the rule set ran, so an outcome cannot be read without
// its coverage. Checked on all three paths: pass, violation, and cannot-evaluate.
func TestCmdBriefLintAlwaysReportsCoverage(t *testing.T) {
	repo := lintRepo(t)
	briefs := map[string]string{
		"pass":      cleanBrief,
		"violation": defectiveBrief,
		"unevaluable": strings.Replace(cleanBrief, "Base is origin/main in this repository.",
			"Base is origin/main in this repository. See pkg/thing.go:4000.", 1),
	}
	for name, brief := range briefs {
		out, _ := captureStdout(t, func() error {
			return cmdBriefLint([]string{writeBrief(t, brief), "--repo", repo})
		})
		if !strings.Contains(out, "of 5 rules ran") {
			t.Fatalf("%s: summary must state coverage:\n%s", name, out)
		}
	}
}

// AGENTS.md belongs to the repository under review, so its values are untrusted text on a
// path a human reads. They must not be able to write their own line in the report.
func TestCmdBriefLintQuotesConfigValues(t *testing.T) {
	repo := lintRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"),
		[]byte("# Fixture\n\n- brief-standards: docs/x.md\u001b[31mINJECTED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, cleanBrief), "--repo", repo})
	})
	if strings.Contains(out, "\u001b") {
		t.Fatalf("a control byte from AGENTS.md reached the terminal raw:\n%q", out)
	}
	if !strings.Contains(out, `\x1b[31mINJECTED`) {
		t.Fatalf("the value must still be shown, escaped:\n%s", out)
	}
}

// A full pass and a partial one must not look the same to a caller that reads only the
// exit status, and the add path must still proceed on the partial one.
func TestCmdBriefLintDistinguishesReducedCoverage(t *testing.T) {
	repo := lintRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	brief := writeBrief(t, cleanBrief)

	out, err := captureStdout(t, func() error { return cmdBriefLint([]string{brief, "--repo", repo}) })
	if got := exitOf(t, err); got != 0 {
		t.Fatalf("a full pass must exit 0, got %d (%v)\n%s", got, err, out)
	}
	if !strings.Contains(out, "5 of 5 rules ran: passed") {
		t.Fatalf("a full pass must say so plainly:\n%s", out)
	}

	if err := os.WriteFile(agents, []byte("# Fixture\n\n- brief-lint-disable: standards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error { return cmdBriefLint([]string{brief, "--repo", repo}) })
	if got := exitOf(t, err); got != exitLintPartial {
		t.Fatalf("one disabled rule must exit %d, got %d (%v)\n%s", exitLintPartial, got, err, out)
	}
	if strings.Contains(out, "rules ran (1 disabled by project config): passed") {
		t.Fatalf("a partial run must not report a bare pass:\n%s", out)
	}

	// task add reads the report, not the exit status, and proceeds: a declared disable
	// narrows what is checked without breaking the project's own dispatch.
	out, err = captureStdout(t, func() error { return lintBriefForAdd(cleanBrief, repo, "", false) })
	if err != nil {
		t.Fatalf("task add must proceed on reduced coverage, got %v\n%s", err, out)
	}
}

// --offline keeps the four local rules and drops the one that leaves the machine. Before
// it, an unreachable remote left only --no-brief-lint, which skips five rules to get past
// one.
func TestCmdBriefLintOfflineKeepsTheLocalRules(t *testing.T) {
	repo := lintRepo(t)
	// A remote that cannot be reached: the target-branch rule is the only rule that asks.
	if out, err := exec.Command("git", "-C", repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")).CombinedOutput(); err != nil {
		t.Fatalf("pointing origin at nothing: %v: %s", err, out)
	}

	out, err := captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, cleanBrief), "--repo", repo})
	})
	if got := exitOf(t, err); got != exitLintIndeterminate {
		t.Fatalf("an unreachable remote must be unevaluable, got exit %d (%v)\n%s", got, err, out)
	}

	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, cleanBrief), "--repo", repo, "--offline"})
	})
	if got := exitOf(t, err); got != exitLintPartial {
		t.Fatalf("offline must report reduced coverage, want exit %d, got %d (%v)\n%s", exitLintPartial, got, err, out)
	}
	for _, want := range []string{"rule target-branch: SKIPPED for --offline", "4 of 5 rules ran (1 skipped for --offline)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in the output:\n%s", want, out)
		}
	}

	// The brief's defects are still caught with the network rule skipped.
	out, err = captureStdout(t, func() error {
		return cmdBriefLint([]string{writeBrief(t, defectiveBrief), "--repo", repo, "--offline"})
	})
	if got := exitOf(t, err); got != exitLintViolation {
		t.Fatalf("offline must still fail a defective brief, got exit %d (%v)\n%s", got, err, out)
	}
}

// captureStderr is captureStdout's twin: the add path writes its notes to stderr, and this
// test is about one of those sentences.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	runErr := fn()
	_ = w.Close()
	os.Stderr = old
	out := <-done
	_ = r.Close()
	return out, runErr
}

// The add path prints its own sentence next to coverage(), and it must not name a reason
// coverage() just contradicted: --brief-lint-offline disables nothing in AGENTS.md.
func TestLintBriefForAddNamesNoWrongReason(t *testing.T) {
	repo := lintRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")).CombinedOutput(); err != nil {
		t.Fatalf("pointing origin at nothing: %v: %s", err, out)
	}
	out, err := captureStderr(t, func() error { return lintBriefForAdd(cleanBrief, repo, "", true) })
	if err != nil {
		t.Fatalf("offline add must proceed, got %v\n%s", err, out)
	}
	if strings.Contains(out, "the project disabled") {
		t.Errorf("the add note blames project config for an --offline skip:\n%s", out)
	}
	if !strings.Contains(out, "1 skipped for --offline") {
		t.Errorf("the add note must carry the real reason:\n%s", out)
	}
}

// A brief over the cap is refused with the FILE's size, not the size of the capped read, and
// the coverage line must not blame a project that disabled nothing.
func TestCmdBriefLintRefusesAnOversizeBrief(t *testing.T) {
	repo := lintRepo(t)
	path := filepath.Join(t.TempDir(), "big.md")
	body := cleanBrief + strings.Repeat("filler text that says nothing. ", (brieflint.MaxBriefBytes/31)+2000)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBriefLint([]string{path, "--repo", repo}) })
	if got := exitOf(t, err); got != exitLintIndeterminate {
		t.Fatalf("an oversize brief must be unevaluable, got exit %d (%v)\n%s", got, err, out)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(len(body))) {
		t.Errorf("the refusal must state the file's real size (%d), got %v", len(body), err)
	}
	if strings.Contains(err.Error(), "disabled by project config") {
		t.Errorf("a size refusal must not be reported as a project disable: %v", err)
	}
}

// The helper spawn shares with task add, over a real repository, so the online rules run.
// It says nothing about whether cmdSpawn calls it: see TestCmdSpawnLintsTheBriefItStores,
// which is the one that fails when the gate is unwired.
func TestSpawnsLintHelperJudgesABrief(t *testing.T) {
	repo := lintRepo(t)
	if err := lintBriefBeforeStore("spawn", "spawned", defectiveBrief, repo, "", false); err == nil {
		t.Fatal("spawn must refuse a brief that violates a rule")
	}
	out, err := captureStdout(t, func() error {
		return lintBriefBeforeStore("spawn", "spawned", cleanBrief, repo, "", false)
	})
	if err != nil {
		t.Fatalf("spawn must accept a clean brief, got %v\n%s", err, out)
	}
}

// spawnBrief is cleanBrief with its one cited path removed. Everything else is unchanged:
// it declares its base, points at conventions, bounds its prohibition and states an end
// state. The path has to go because the fixture repository below is deliberately not a
// repository, and a citation would send rule 2 to git there.
var spawnBrief = strings.Replace(cleanBrief,
	"match the shape of pkg/thing.go", "match the surrounding code", 1)

// The gate has to be WIRED INTO cmdSpawn, not merely available to it. Deleting the gate
// block from cmdSpawn left `go test ./internal/cli/...` fully green: every spawn test
// either stopped at flag validation or called the helper directly, so nothing went through
// cmdSpawn at all. task add had end-to-end coverage of the same gate and spawn had none.
//
// The spawn itself is not exercised. The fixture repo is not a git repository, so
// SpawnWithEffort refuses once the brief has been stored, and what this test asserts is
// everything up to and including the store. That is where the gate sits, and storing the
// brief is the side effect a defective one has to be stopped before: the brief is a
// snapshot the worker starts on.
func TestCmdSpawnLintsTheBriefItStores(t *testing.T) {
	withSeedDB(t, nil)
	// A spawn installs the recommended agent skills before launch, which shells out to npx.
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	repo := t.TempDir()
	// Asserted, not assumed: if this directory were inside a repository, the cases below
	// that are meant to fail after the store would instead launch a real worker.
	if root, err := worktree.RepoRoot(repo); err == nil {
		t.Fatalf("the fixture directory must not be inside a git repository, got root %s", root)
	}
	// errors.As on lintError is what separates a refusal by the gate from the spawn's own
	// later failure. Exit status cannot: exitLintViolation is 1 and so is every other error.
	var le lintError

	// A defective brief stops the spawn before any side effect: no stored brief.
	out, err := captureStdout(t, func() error {
		return cmdSpawn([]string{"spawn-bad", repo, "--brief", defectiveBrief, "--brief-lint-offline"})
	})
	if !errors.As(err, &le) || le.code != exitLintViolation {
		t.Fatalf("a defective brief must fail the spawn with a lint violation (exit %d), got %v\n%s", exitLintViolation, err, out)
	}
	if _, statErr := os.Stat(paths.Default().BriefPath("spawn-bad")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused spawn must store no brief; stat err = %v", statErr)
	}

	// A clean brief reaches the store. The spawn fails after that for its own reason, which
	// must not be a lint outcome.
	out, err = captureStdout(t, func() error {
		return cmdSpawn([]string{"spawn-good", repo, "--brief", spawnBrief, "--brief-lint-offline"})
	})
	if err == nil {
		t.Fatalf("the fixture spawn cannot succeed: a worker was launched\n%s", out)
	}
	if errors.As(err, &le) {
		t.Fatalf("a clean brief must pass the gate, got a lint outcome: %v\n%s", err, out)
	}
	if got := mustReadFile(t, paths.Default().BriefPath("spawn-good")); got != spawnBrief {
		t.Fatalf("stored brief = %q, want the brief as written", got)
	}

	// The escape hatch stores the brief as written.
	out, err = captureStdout(t, func() error {
		return cmdSpawn([]string{"spawn-skipped", repo, "--brief", defectiveBrief, "--no-brief-lint"})
	})
	if errors.As(err, &le) {
		t.Fatalf("--no-brief-lint must not lint, got a lint outcome: %v\n%s", err, out)
	}
	if got := mustReadFile(t, paths.Default().BriefPath("spawn-skipped")); got != defectiveBrief {
		t.Fatalf("stored brief = %q, want the brief as written", got)
	}

	// --no-brief-lint with nothing to lint is a loud error rather than a silent no-op.
	if err := cmdSpawn([]string{"spawn-nobrief", repo, "--no-brief-lint"}); !errors.Is(err, errNoBriefLintWithoutBrief) {
		t.Fatalf("want errNoBriefLintWithoutBrief, got %v", err)
	}
}

// Every command that reads a brief FILE reads it under the cap, not only the standalone
// one. `task add` and `spawn` went through resolveBrief's bare os.ReadFile, so the same
// 419,444,022-byte file cost 21,020,672 bytes of resident memory through brief-lint and
// 1,278,476,288 through spawn. The verdict was already right; the memory was spent before
// Lint could say so.
func TestEveryBriefFilePathReadsUnderTheCap(t *testing.T) {
	repo := t.TempDir()
	var projID int64
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, _ := s.UpsertProject(ctx, repo, "fixture")
		projID = p.ID
	})
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	path := filepath.Join(t.TempDir(), "huge.md")
	body := cleanBrief + strings.Repeat("filler text that says nothing. ", (brieflint.MaxBriefBytes/31)+64)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		run  func() error
	}{
		{"brief-lint", func() error { return cmdBriefLint([]string{path, "--repo", repo, "--offline"}) }},
		{"task add", func() error {
			return cmdTaskAdd([]string{"r9-add", "--project", itoa(projID), "--brief-file", path})
		}},
		{"spawn", func() error {
			return cmdSpawn([]string{"r9-spawn", repo, "--brief-file", path, "--brief-lint-offline"})
		}},
		// The escape hatch skips the RULES, not the read. It used to store a 400 MB initial
		// prompt, at 1,705,967,616 bytes of resident memory, the worst of the three.
		{"spawn --no-brief-lint", func() error {
			return cmdSpawn([]string{"r9-spawn-skip", repo, "--brief-file", path, "--no-brief-lint"})
		}},
	}
	for _, tc := range cases {
		out, err := captureStdout(t, tc.run)
		var le lintError
		if !errors.As(err, &le) || le.code != exitLintIndeterminate {
			t.Errorf("%s: an oversize brief must be refused as unevaluable (exit %d), got %v\n%s",
				tc.name, exitLintIndeterminate, err, out)
			continue
		}
		// The file's real size, not the size of the capped read.
		if !strings.Contains(err.Error(), fmt.Sprint(len(body))) {
			t.Errorf("%s: the refusal must state the file's real size (%d), got %v", tc.name, len(body), err)
		}
	}
	// Nothing reached the store on any of the three storing paths.
	for _, id := range []string{"r9-add", "r9-spawn", "r9-spawn-skip"} {
		if _, statErr := os.Stat(paths.Default().BriefPath(id)); !os.IsNotExist(statErr) {
			t.Errorf("%s: a refused brief must not be stored; stat err = %v", id, statErr)
		}
	}
}

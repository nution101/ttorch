package brieflint

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- fixtures ---------------------------------------------------------------------------
//
// The fixture repository is deliberately neutral: a base branch pushed to a local bare
// "remote" (so the remote checks run offline and deterministically), plus one local commit
// on top that lengthens a file. That local commit stands in for a worker's HEAD, which is
// what a gate finding's file:line citation is about.

const (
	fixtureShortLines = 10 // lines of pkg/thing.go on the base
	fixtureLongLines  = 50 // lines of pkg/thing.go at the reviewed (local) commit
)

// The fixture repository is built ONCE per package run and copied per test. Each test needs
// a private repository (some write an AGENTS.md), but rebuilding one with git init / commit /
// push per test cost more than the whole rule set does to run.
var (
	fixtureRoot string // holds the prototype repo and its bare remote for the package run
	protoOnce   sync.Once
	protoRepo   string
	protoHead   string
	protoErr    error
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "brieflint-fixture-*")
	if err != nil {
		panic(err)
	}
	fixtureRoot = root
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func gitIn(dir string, args ...string) error {
	full := append([]string{
		"-c", "user.name=lint fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return nil
}

func writeLines(dir, rel string, lines int) error {
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "// line %d\n", i)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// buildPrototype creates the bare remote and the work repository both under fixtureRoot, so
// the remote outlives the test that happened to trigger the build.
func buildPrototype() {
	remote := filepath.Join(fixtureRoot, "remote")
	repo := filepath.Join(fixtureRoot, "proto")
	for _, dir := range []string{remote, repo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			protoErr = err
			return
		}
	}
	steps := []func() error{
		func() error { return gitIn(remote, "init", "--bare", "-b", "main", "-q") },
		func() error { return gitIn(repo, "init", "-b", "main", "-q") },
		func() error { return gitIn(repo, "remote", "add", "origin", remote) },
		func() error { return writeLines(repo, "pkg/thing.go", fixtureShortLines) },
		func() error { return writeLines(repo, "docs/guide.md", 5) },
		func() error { return gitIn(repo, "add", "-A") },
		func() error { return gitIn(repo, "commit", "-q", "-m", "base") },
		func() error { return gitIn(repo, "push", "-q", "origin", "main") },
		func() error { return gitIn(repo, "fetch", "-q", "origin") },
		// The reviewed commit: the file is longer here than on the base, and a new file
		// exists that the base has never seen.
		func() error { return writeLines(repo, "pkg/thing.go", fixtureLongLines) },
		func() error { return writeLines(repo, "pkg/added.go", 30) },
		func() error { return gitIn(repo, "add", "-A") },
		func() error { return gitIn(repo, "commit", "-q", "-m", "worker change") },
	}
	for _, step := range steps {
		if protoErr = step(); protoErr != nil {
			return
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		protoErr = err
		return
	}
	protoRepo, protoHead = repo, strings.TrimSpace(string(out))
}

// fixture returns a private copy of the prototype repository and the sha of its reviewed
// commit (the local HEAD, one commit ahead of origin/main).
func fixture(t *testing.T) (repo, reviewed string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	protoOnce.Do(buildPrototype)
	if protoErr != nil {
		t.Fatalf("building the fixture repository: %v", protoErr)
	}
	repo = filepath.Join(t.TempDir(), "repo")
	out, err := exec.Command("cp", "-R", protoRepo, repo).CombinedOutput()
	if err != nil {
		t.Fatalf("copying the fixture repository: %v: %s", err, out)
	}
	return repo, protoHead
}

// findingsFor returns the findings the named rule produced.
func findingsFor(r Report, id RuleID) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Rule == id {
			out = append(out, f)
		}
	}
	return out
}

// requireStatus asserts that rule id produced exactly one finding with the given status.
func requireStatus(t *testing.T, r Report, id RuleID, want Status) Finding {
	t.Helper()
	fs := findingsFor(r, id)
	if len(fs) != 1 {
		t.Fatalf("rule %s: want 1 %s finding, got %d: %+v (notes: %v)", id, want, len(fs), fs, r.Notes)
	}
	if fs[0].Status != want {
		t.Fatalf("rule %s: want status %s, got %s (%s)", id, want, fs[0].Status, fs[0].Detail)
	}
	return fs[0]
}

// requireClean asserts that rule id produced no finding at all — the brief satisfies it.
func requireClean(t *testing.T, r Report, id RuleID) {
	t.Helper()
	if fs := findingsFor(r, id); len(fs) != 0 {
		t.Fatalf("rule %s: want no findings, got %+v (notes: %v)", id, fs, r.Notes)
	}
}

// satisfying is a brief that passes every rule against the fixture repository. Each test
// mutates one aspect of it, so a failure names exactly one cause.
const satisfying = `# Task

## TARGET BRANCH
Base is origin/main in this repository.

## Approach
Follow this repo's demonstrated conventions and match the shape of the existing packages.

## Why
Touch pkg/thing.go and docs/guide.md.

## Rules
No changes to any product branch and no PR until I have reviewed the work. Commit on your
own branch, leave the worktree clean, and report the sha.
`

// --- Rule 1 -----------------------------------------------------------------------------

func TestRuleTargetBranchSatisfied(t *testing.T) {
	repo, _ := fixture(t)
	r := Lint(satisfying, Options{Repo: repo})
	requireClean(t, r, RuleTargetBranch)
	if !hasNote(r, "origin/main exists on remote origin") {
		t.Fatalf("want a note recording the verified target, got %v", r.Notes)
	}
}

func TestRuleTargetBranchViolatedWhenUndeclared(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Base is origin/main in this repository.", "Base is the usual branch.", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleTargetBranch, StatusFail)
	if !strings.Contains(f.Detail, "declares no target branch") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if r.Outcome() != OutcomeFail {
		t.Fatalf("outcome: want %s, got %s", OutcomeFail, r.Outcome())
	}
}

func TestRuleTargetBranchViolatedWhenBranchMissingOnRemote(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "origin/main", "origin/mian", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleTargetBranch, StatusFail)
	if !strings.Contains(f.Detail, "origin/mian") || !strings.Contains(f.Detail, "does not exist") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if f.Quote != "origin/mian" {
		t.Fatalf("want the offending text quoted, got %q", f.Quote)
	}
}

// --- Rule 2 -----------------------------------------------------------------------------

func TestRuleFilePathsSatisfied(t *testing.T) {
	repo, _ := fixture(t)
	r := Lint(satisfying, Options{Repo: repo})
	requireClean(t, r, RuleFilePaths)
}

func TestRuleFilePathsViolatedByMissingPath(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "docs/guide.md", "docs/missing.md", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleFilePaths, StatusFail)
	if !strings.Contains(f.Detail, "docs/missing.md") || !strings.Contains(f.Detail, "origin/main") {
		t.Fatalf("detail must name the path and the ref it was resolved at: %s", f.Detail)
	}
	if f.Quote != "docs/missing.md" {
		t.Fatalf("want the offending text quoted, got %q", f.Quote)
	}
}

// TestRuleFilePathsLineCitationBeyondBaseEOF is the regression this rule is written for: a
// gate finding cites a line that exists at the reviewed commit and is past end-of-file on
// the base. Resolved against the reviewed commit it is valid; resolved against the base it
// is a false positive.
func TestRuleFilePathsLineCitationBeyondBaseEOF(t *testing.T) {
	repo, reviewed := fixture(t)
	cited := fixtureLongLines - 5 // exists at the reviewed commit, far past the base's EOF
	if cited <= fixtureShortLines {
		t.Fatalf("fixture is not set up for this test: %d <= %d", cited, fixtureShortLines)
	}
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", fmt.Sprintf("Fix the finding at pkg/thing.go:%d", cited), 1)

	// Resolved against the reviewed commit: valid, no finding.
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed})
	requireClean(t, r, RuleFilePaths)
	if !hasNote(r, "file:line citations resolved at "+reviewed) {
		t.Fatalf("the citations ref must be named in the report, got %v", r.Notes)
	}

	// Resolved against the BASE — what the prototype did — the same citation is past
	// end-of-file. This asserts the ref choice is what makes the difference, not the brief.
	base := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: "origin/main"})
	f := requireStatus(t, base, RuleFilePaths, StatusFail)
	if !strings.Contains(f.Detail, fmt.Sprintf("cites line %d", cited)) {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// A path that is NEW in the worker's diff does not exist on the base, so it too must be
// resolved at the citations ref.
func TestRuleFilePathsCitationOfFileAddedByTheDiff(t *testing.T) {
	repo, reviewed := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "Fix the finding at pkg/added.go:12", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed})
	requireClean(t, r, RuleFilePaths)
}

func TestRuleFilePathsLineBeyondCitationsRefEOF(t *testing.T) {
	repo, reviewed := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", fmt.Sprintf("Fix the finding at pkg/thing.go:%d", fixtureLongLines+1), 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed})
	f := requireStatus(t, r, RuleFilePaths, StatusFail)
	if !strings.Contains(f.Detail, fmt.Sprintf("has %d line(s)", fixtureLongLines)) {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// With no citations ref there is no correct ref to resolve a file:line citation against, so
// the check is unevaluable — never a pass, and never silently resolved against the base.
func TestRuleFilePathsLineCitationWithoutCitationsRefIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", fmt.Sprintf("Fix the finding at pkg/thing.go:%d", fixtureLongLines-5), 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, "no citations ref was given") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s", OutcomeIndeterminate, r.Outcome())
	}
}

// --- Rule 3 -----------------------------------------------------------------------------

func TestRuleHardCountsViolated(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "There are 21 occurrences in pkg/thing.go. Fix them all.", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleHardCounts, StatusFail)
	if !strings.Contains(f.Quote, "21 occurrences") {
		t.Fatalf("want the offending text quoted, got %q", f.Quote)
	}
	if f.Line == 0 {
		t.Fatal("want the finding anchored to a line")
	}
}

// The hedge is matched by decomposed signals, so wording the prototype's phrase list would
// have missed still satisfies the rule.
func TestRuleHardCountsSatisfiedByVariedWording(t *testing.T) {
	repo, _ := fixture(t)
	hedges := []string{
		"There are 21 occurrences in pkg/thing.go. That figure may be wrong, so establish the true total yourself and tell me what you find.",
		"There are 21 occurrences in pkg/thing.go. Do not trust it; count them and report your own number.",
		"There are 21 occurrences in pkg/thing.go. My tally is indicative only. Come back with the actual count.",
		"I counted 21 matches in pkg/thing.go, approximately. Recheck and state the real total in your report.",
	}
	for _, h := range hedges {
		brief := strings.Replace(satisfying, "Touch pkg/thing.go", h, 1)
		r := Lint(brief, Options{Repo: repo})
		requireClean(t, r, RuleHardCounts)
	}
}

func TestRuleHardCountsIgnoresProseWithoutACount(t *testing.T) {
	repo, _ := fixture(t)
	r := Lint(satisfying, Options{Repo: repo})
	requireClean(t, r, RuleHardCounts)
	if !hasNote(r, "hard-counts: the brief states no hard count") {
		t.Fatalf("want a note that no count was found, got %v", r.Notes)
	}
}

// --- Rule 4 -----------------------------------------------------------------------------

func TestRuleProhibitionViolatedByBlanketBan(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying,
		"No changes to any product branch and no PR until I have reviewed the work. Commit on your\nown branch, leave the worktree clean, and report the sha.",
		"Do NOT push. Do not merge.", 1)
	r := Lint(brief, Options{Repo: repo})
	fs := findingsFor(r, RuleProhibition)
	if len(fs) == 0 {
		t.Fatalf("want a prohibition finding, got none (notes: %v)", r.Notes)
	}
	var sawBlanket, sawEndState bool
	for _, f := range fs {
		if f.Status != StatusFail {
			t.Fatalf("want FAIL, got %s", f.Status)
		}
		if strings.Contains(f.Detail, "blanket prohibition") {
			sawBlanket = true
			if !strings.Contains(strings.ToLower(f.Quote), "push") && !strings.Contains(strings.ToLower(f.Quote), "merge") {
				t.Fatalf("want the offending text quoted, got %q", f.Quote)
			}
		}
		if strings.Contains(f.Detail, "allowed end state") {
			sawEndState = true
		}
	}
	if !sawBlanket || !sawEndState {
		t.Fatalf("want both the blanket-ban and the missing-end-state findings, got %+v", fs)
	}
}

func TestRuleProhibitionSatisfied(t *testing.T) {
	repo, _ := fixture(t)
	r := Lint(satisfying, Options{Repo: repo})
	requireClean(t, r, RuleProhibition)
}

// A prohibition that names its invariant but leaves the worker no way to finish is still a
// violation: that is how a worker stalls.
func TestRuleProhibitionBoundedButWithoutAnEndState(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying,
		"No changes to any product branch and no PR until I have reviewed the work. Commit on your\nown branch, leave the worktree clean, and report the sha.",
		"Do not merge to main and do not open a PR.", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleProhibition, StatusFail)
	if !strings.Contains(f.Detail, "allowed end state") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// --- Rule 5 -----------------------------------------------------------------------------

func TestRuleStandardsWithDeclaredPointer(t *testing.T) {
	repo, _ := fixture(t)
	writeAgents(t, repo, "- brief-standards: docs/standards/\n")
	cfg := LoadConfig(repo)

	cited := strings.Replace(satisfying, "Follow this repo's demonstrated conventions",
		"Follow docs/standards/ for this project", 1)
	requireClean(t, Lint(cited, Options{Repo: repo, Config: cfg}), RuleStandards)

	r := Lint(satisfying, Options{Repo: repo, Config: cfg})
	f := requireStatus(t, r, RuleStandards, StatusFail)
	if !strings.Contains(f.Detail, "docs/standards/") {
		t.Fatalf("detail must name the declared pointer: %s", f.Detail)
	}
}

// A project that declares no pointer is not held to any other project's layout: any
// explicit standards reference satisfies the rule, and a brief with none fails it.
func TestRuleStandardsWithNoDeclaredPointer(t *testing.T) {
	repo, _ := fixture(t)
	cfg := LoadConfig(repo)
	if len(cfg.Standards) != 0 || cfg.StandardsEmpty {
		t.Fatalf("fixture must declare no pointer, got %+v", cfg)
	}
	requireClean(t, Lint(satisfying, Options{Repo: repo, Config: cfg}), RuleStandards)

	silent := strings.Replace(satisfying,
		"Follow this repo's demonstrated conventions and match the shape of the existing packages.",
		"Write the code.", 1)
	r := Lint(silent, Options{Repo: repo, Config: cfg})
	f := requireStatus(t, r, RuleStandards, StatusFail)
	if !strings.Contains(f.Detail, "points at no standards") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// A declared-but-empty pointer is a broken declaration, not a way to satisfy the rule.
func TestRuleStandardsWithEmptyDeclaredValue(t *testing.T) {
	repo, _ := fixture(t)
	writeAgents(t, repo, "- brief-standards:\n")
	cfg := LoadConfig(repo)
	if !cfg.StandardsEmpty {
		t.Fatalf("want the empty declaration recorded, got %+v", cfg)
	}
	r := Lint(satisfying, Options{Repo: repo, Config: cfg})
	f := requireStatus(t, r, RuleStandards, StatusIndeterminate)
	if !strings.Contains(f.Detail, "declared with no value") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s", OutcomeIndeterminate, r.Outcome())
	}
}

// --- "could not evaluate" is distinct from a pass ---------------------------------------

// failingGit stands in for a git that cannot answer: an unreachable remote, a missing
// binary. Every such answer must surface as CANNOT-EVALUATE, never as a pass.
func failingGit(_ string, args ...string) gitResult {
	return gitResult{err: errors.New("fatal: unable to access remote: network is unreachable"), stderr: args[0]}
}

func TestUnreachableRemoteIsIndeterminateNotPass(t *testing.T) {
	repo, _ := fixture(t)
	r := Lint(satisfying, Options{Repo: repo, git: failingGit})
	f := requireStatus(t, r, RuleTargetBranch, StatusIndeterminate)
	if !strings.Contains(f.Detail, "cannot verify origin/main") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if r.Violations() != 0 {
		t.Fatalf("want no violations, got %d: %+v", r.Violations(), r.Findings)
	}
	if got := r.Outcome(); got != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s", OutcomeIndeterminate, got)
	}
	if got := r.Outcome(); got == OutcomePass {
		t.Fatal("an unevaluated check must never read as a pass")
	}
}

func TestNoRepositoryIsIndeterminateNotPass(t *testing.T) {
	r := Lint(satisfying, Options{})
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s (%+v)", OutcomeIndeterminate, r.Outcome(), r.Findings)
	}
	requireStatus(t, r, RuleTargetBranch, StatusIndeterminate)
	requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
}

// A real violation outranks an unevaluated check: there is already something to fix.
func TestViolationOutranksIndeterminate(t *testing.T) {
	brief := strings.Replace(satisfying, "Follow this repo's demonstrated conventions and match the shape of the existing packages.", "Write the code.", 1)
	r := Lint(brief, Options{})
	if r.Indeterminates() == 0 || r.Violations() == 0 {
		t.Fatalf("want both kinds of finding, got %+v", r.Findings)
	}
	if r.Outcome() != OutcomeFail {
		t.Fatalf("outcome: want %s, got %s", OutcomeFail, r.Outcome())
	}
}

// --- whole-run behaviour ----------------------------------------------------------------

// One run must report every violation: fixing a brief one lint round at a time is slow.
func TestLintReportsEveryViolationInOneRun(t *testing.T) {
	repo, _ := fixture(t)
	brief := `# Task

Base is the usual branch.

Rewrite the 21 occurrences in docs/missing.md.

Do NOT push.
`
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	for _, id := range []RuleID{RuleTargetBranch, RuleHardCounts, RuleProhibition, RuleStandards} {
		if len(findingsFor(r, id)) == 0 {
			t.Fatalf("rule %s produced no finding; one run must report them all: %+v", id, r.Findings)
		}
	}
	if fs := findingsFor(r, RuleFilePaths); len(fs) != 1 || fs[0].Status != StatusFail {
		t.Fatalf("want the missing path reported: %+v", fs)
	}
}

// --- per-project rule overrides ---------------------------------------------------------

func TestDisabledRuleIsSkippedAndVisible(t *testing.T) {
	repo, _ := fixture(t)
	writeAgents(t, repo, "- brief-lint-disable: target-branch\n")
	brief := strings.Replace(satisfying, "Base is origin/main in this repository.", "Base is the usual branch.", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", Config: LoadConfig(repo)})
	requireClean(t, r, RuleTargetBranch)
	if !hasNote(r, "rule target-branch: DISABLED") {
		t.Fatalf("an override must be visible in the output, got %v", r.Notes)
	}
	if r.Outcome() != OutcomePass {
		t.Fatalf("outcome: want %s, got %s (%+v)", OutcomePass, r.Outcome(), r.Findings)
	}
}

func TestUnknownDisabledRuleIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	writeAgents(t, repo, "- brief-lint-disable: target-brunch\n")
	r := Lint(satisfying, Options{Repo: repo, Config: LoadConfig(repo)})
	f := requireStatus(t, r, RuleConfig, StatusIndeterminate)
	if !strings.Contains(f.Detail, "target-brunch") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("a configuration typo must not read as a pass, got %s", r.Outcome())
	}
}

func hasNote(r Report, substr string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func writeAgents(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, configFile), []byte("# Fixture\n\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A brief may legitimately cite a path that does not exist yet, when it is asking for the
// file to be written. Reporting that as a missing file would fail every brief that asks for
// a new file — so the citation is exempted, and the exemption is reported rather than
// silently applied.
func TestRuleFilePathsExemptsPathsTheBriefAsksToCreate(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"Write dev/report/EVIDENCE.md: the rule set as implemented.", 1)
	r := Lint(brief, Options{Repo: repo})
	requireClean(t, r, RuleFilePaths)
	if !hasNote(r, "the brief asks for them to be created: dev/report/EVIDENCE.md") {
		t.Fatalf("the exemption must be visible in the output, got %v", r.Notes)
	}

	// The exemption is scoped to the citing sentence: a path cited elsewhere is still
	// checked.
	brief = strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"Write dev/report/EVIDENCE.md. Then fix docs/missing.md.", 1)
	r = Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleFilePaths, StatusFail)
	if f.Quote != "docs/missing.md" {
		t.Fatalf("want the unexempted path reported, got %q", f.Quote)
	}
}

package brieflint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	if v := os.Getenv(childEnv); v != "" {
		runTestChild(v)
		return
	}
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

// A path that is NEW in the worker's diff does not exist on the base, so with the citations
// ref named it resolves there and passes.
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

// With no citations ref, a line that is past end-of-file on the base is AMBIGUOUS: it may
// exist at the commit the citation was read at. That is unevaluable — never a pass, and
// never a violation, because calling it one is the false positive this rule exists to avoid.
func TestRuleFilePathsLineBeyondBaseEOFWithoutCitationsRefIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", fmt.Sprintf("Fix the finding at pkg/thing.go:%d", fixtureLongLines-5), 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, "pass that commit") {
		t.Fatalf("the finding must name the remedy: %s", f.Detail)
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s", OutcomeIndeterminate, r.Outcome())
	}
}

// The ordinary case, and the one the whole rule turns on: a brief pointing at a real line of
// existing code, with no citations ref given. It must resolve against the base and PASS.
// Requiring a flag here blocked every brief that cites file:line, which is how briefs
// normally point at code.
func TestRuleFilePathsLineCitationResolvesAtTheBaseByDefault(t *testing.T) {
	repo, _ := fixture(t)
	cited := fixtureShortLines - 2 // a real line of the file as it exists on the base
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", fmt.Sprintf("The bug is at pkg/thing.go:%d", cited), 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	requireClean(t, r, RuleFilePaths)
	if r.Outcome() != OutcomePass {
		t.Fatalf("outcome: want %s, got %s (%+v)", OutcomePass, r.Outcome(), r.Findings)
	}
	// The ref, and the fact that it was defaulted, must both be in the report.
	if !hasNote(r, "file:line citations resolved at origin/main (defaulted to the base") {
		t.Fatalf("the report must name the ref and that it was defaulted, got %v", r.Notes)
	}
}

// A path that does not exist on the base at all, cited with a line and no citations ref, is
// equally ambiguous: the diff the citation is about may have added it. Unevaluable, not a
// violation.
func TestRuleFilePathsLineCitationOfPathAbsentFromBaseIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "Fix the finding at pkg/added.go:12", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, "does not exist at origin/main") || !strings.Contains(f.Detail, "pass that commit") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// Naming the ref makes the answer authoritative: the same miss that is unevaluable by
// default is a violation once the caller has said which commit the citation is about.
func TestRuleFilePathsNamedCitationsRefMakesAMissAViolation(t *testing.T) {
	repo, reviewed := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "Fix the finding at pkg/gone.go:12", 1)

	f := requireStatus(t, Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed}), RuleFilePaths, StatusFail)
	if !strings.Contains(f.Detail, "pkg/gone.go does not exist at "+reviewed) {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if strings.Contains(f.Detail, "pass that commit") {
		t.Fatalf("a named ref needs no remedy hint: %s", f.Detail)
	}
	requireStatus(t, Lint(brief, Options{Repo: repo, Ref: "origin/main"}), RuleFilePaths, StatusIndeterminate)
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
	if !hasNote(r, "hard-counts: no hard count was recognised") {
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

// A configuration file the run refused to read leaves rule 5 unevaluable, not passing on a
// fallback the project had opted out of, and leaves the rules the file disables RUNNING.
// A rule turned off by a file nobody read is a rule silently skipped.
func TestOversizeConfigIsIndeterminateAndDisablesNothing(t *testing.T) {
	repo, _ := fixture(t)
	body := "- brief-standards: docs/standards/\n- brief-lint-disable: prohibition\n" +
		strings.Repeat("filler line that declares nothing\n", (maxConfigBytes/34)+64)
	writeAgents(t, repo, body)
	cfg := LoadConfig(repo)
	if !cfg.Oversize {
		t.Fatalf("fixture did not exceed the cap: %+v", cfg)
	}

	r := Lint(satisfying, Options{Repo: repo, Config: cfg})
	f := requireStatus(t, r, RuleStandards, StatusIndeterminate)
	if !strings.Contains(f.Detail, "over the") || !strings.Contains(f.Detail, "never read") {
		t.Errorf("rule 5 must name the refusal as the reason, got %q", f.Detail)
	}
	if fs := findingsFor(r, RuleConfig); len(fs) != 1 || fs[0].Status != StatusIndeterminate {
		t.Errorf("want one indeterminate config finding, got %+v", fs)
	}
	// The file disabled rule 4. It was not read, so rule 4 ran.
	if hasNote(r, "rule prohibition: DISABLED") {
		t.Error("a disable inside a refused file must not be applied")
	}
	if r.Evaluated() != r.Total() {
		t.Errorf("every rule must still run: %d of %d", r.Evaluated(), r.Total())
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Errorf("outcome: want %s, got %s", OutcomeIndeterminate, r.Outcome())
	}
}

// Rule 5 scans the whole brief once per declared pointer, and the pointer list comes out of
// the repository's AGENTS.md, so its length is not the linter's to choose. The clock used to
// be read once, before that loop, which let the budget watch the phase run over without
// being able to stop it: 40,000 pointers against a 1 MB brief measured 13.92s.
//
// The other four rules are disabled so the whole budget belongs to rule 5 and the run cannot
// arrive at it already expired. Arriving expired would trip the check at the top of
// checkStandards instead, and prove nothing about the loop.
func TestRuleStandardsHonoursTheBudgetInsideItsLoop(t *testing.T) {
	const pointers = 200_000
	cfg := Config{Source: "fixture", Disabled: map[RuleID]bool{
		RuleTargetBranch: true, RuleFilePaths: true, RuleHardCounts: true, RuleProhibition: true,
	}}
	for i := 0; i < pointers; i++ {
		cfg.Standards = append(cfg.Standards, fmt.Sprintf("docs/absent-%d.md", i))
	}
	// Big enough that one scan is not free, small enough to stay under MaxBriefBytes. No
	// pointer appears in it, so every one of them scans the whole thing, and the filler
	// repeats the pointers' own prefix so the scan does real comparison work rather than
	// skipping on a first byte the brief never contains.
	big := satisfying + strings.Repeat("docs/absent-x.md is not one of them. ", 13_000)
	if len(big) > MaxBriefBytes {
		t.Fatalf("fixture brief is %d bytes, over the %d byte cap", len(big), MaxBriefBytes)
	}

	const budget = 200 * time.Millisecond
	start := time.Now()
	r := Lint(big, Options{Config: cfg, Budget: budget})
	elapsed := time.Since(start)

	f := requireStatus(t, r, RuleStandards, StatusIndeterminate)
	if !strings.Contains(f.Detail, "run budget") {
		t.Fatalf("want the budget finding, got %q", f.Detail)
	}
	// The announced bound has to hold, not merely be announced. 50x the budget is slack for
	// a loaded machine and still far short of what the unchecked loop costs: running every
	// pointer against this brief is tens of seconds of scanning.
	if limit := 50 * budget; elapsed > limit {
		t.Fatalf("%d pointers took %s against a %s budget, over the %s the run may not exceed", pointers, elapsed, budget, limit)
	}
}

// --- "could not evaluate" is distinct from a pass ---------------------------------------

// failingGit stands in for a git that cannot answer: an unreachable remote, a missing
// binary. Every such answer must surface as CANNOT-EVALUATE, never as a pass.
func failingGit(_ context.Context, _ string, args ...string) gitResult {
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
	if !hasNote(r, "read as asking for them to be created (wording only: a create verb governing the mention): dev/report/EVIDENCE.md") {
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

// --- bounding the work an untrusted brief can cause -------------------------------------

// spyGit wraps the real git, recording every argv and optionally answering some calls
// itself, so a test can assert how much work a rule actually did.
type spyGit struct {
	calls [][]string
	// answer may return a canned result for a call; false delegates to the real git.
	answer func(args []string) (gitResult, bool)
	// sawDeadline records whether the last call's context carried a deadline.
	sawDeadline bool
	deadline    time.Time
}

func (s *spyGit) fn(ctx context.Context, dir string, args ...string) gitResult {
	s.calls = append(s.calls, args)
	s.deadline, s.sawDeadline = ctx.Deadline()
	if s.answer != nil {
		if res, ok := s.answer(args); ok {
			return res
		}
	}
	return gitRun(ctx, dir, args...)
}

// count returns how many recorded calls began with the given argv prefix.
func (s *spyGit) count(prefix ...string) int {
	n := 0
	for _, c := range s.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if c[i] != p {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n
}

// A brief is untrusted input: it is pasted from issues and written by agents. Rule 1 queries
// the REMOTE once per target it finds, so the number of targets a brief names must not
// decide how many authenticated requests its reader fires, nor how long the reader waits.
func TestTargetBranchFanOutIsCapped(t *testing.T) {
	repo, _ := fixture(t)
	var b strings.Builder
	b.WriteString("# Task\n\nBase is origin/main in this repository.\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "Also origin/feature-%d is involved.\n", i)
	}
	b.WriteString("\nFollow this repo's conventions.\n\nNo changes to a product branch and no PR until I say so. Commit on your own branch and report the sha.\n")

	spy := &spyGit{}
	r := Lint(b.String(), Options{Repo: repo, Ref: "origin/main", git: spy.fn})
	if got := spy.count("ls-remote"); got != maxRemoteTargets {
		t.Fatalf("a 41-target brief fired %d remote queries, want the cap of %d", got, maxRemoteTargets)
	}
	// What was not checked must be named, and must not read as a pass.
	var capped *Finding
	for i := range r.Findings {
		if r.Findings[i].Rule == RuleTargetBranch && strings.Contains(r.Findings[i].Detail, "more origin/<branch> target(s)") {
			capped = &r.Findings[i]
		}
	}
	if capped == nil {
		t.Fatalf("the unchecked targets must be reported: %+v", r.Findings)
	}
	if capped.Status != StatusIndeterminate {
		t.Fatalf("unchecked targets must be %s, got %s", StatusIndeterminate, capped.Status)
	}
	if !strings.Contains(capped.Detail, "origin/feature-") {
		t.Fatalf("the report must name what went unchecked: %s", capped.Detail)
	}
	// Enough names to recognize what was skipped, not forty of them burying the report.
	if n := strings.Count(capped.Detail, "origin/feature-"); n > namesInDetail {
		t.Fatalf("the finding spells out %d names, want at most %d plus a count: %s", n, namesInDetail, capped.Detail)
	}
	if !strings.Contains(capped.Detail, "and 33 more") {
		t.Fatalf("the finding must say how many more went unchecked: %s", capped.Detail)
	}
	if r.Outcome() == OutcomePass {
		t.Fatal("a run that could not verify every target must not read as a pass")
	}
}

// The same bound one level down: the number of paths a brief cites decides how much local
// git work its reader does, and a line count reads the blob. This was the one cap of the
// four with no test.
func TestCitationFanOutIsCapped(t *testing.T) {
	repo, _ := fixture(t)
	var b strings.Builder
	b.WriteString("# Task\n\nBase is origin/main in this repository.\n\nFollow this repo's conventions.\n\n")
	const cited = maxCitations + 6
	for i := 0; i < cited; i++ {
		fmt.Fprintf(&b, "Touch pkg/gen-%d.go please.\n", i)
	}
	b.WriteString("\nNo changes to a product branch and no PR until I say so. Commit on your own branch and report the sha.\n")

	// Answer every path query with a real blob, so the only finding left is the cap itself.
	spy := &spyGit{answer: func(args []string) (gitResult, bool) {
		if args[0] == "ls-tree" {
			return gitResult{stdout: "100644 blob 0123456789abcdef\tpkg/gen.go\n"}, true
		}
		return gitResult{}, false
	}}
	r := Lint(b.String(), Options{Repo: repo, Ref: "origin/main", git: spy.fn})

	if got := spy.count("ls-tree"); got != maxCitations {
		t.Fatalf("a brief citing %d paths resolved %d of them, want the cap of %d", cited, got, maxCitations)
	}
	var capped *Finding
	for i := range r.Findings {
		if r.Findings[i].Rule == RuleFilePaths && strings.Contains(r.Findings[i].Detail, "past the first") {
			capped = &r.Findings[i]
		}
	}
	if capped == nil {
		t.Fatalf("the unchecked citations must be reported: %+v", r.Findings)
	}
	if capped.Status != StatusIndeterminate {
		t.Fatalf("unchecked citations must be %s, got %s", StatusIndeterminate, capped.Status)
	}
	if !strings.Contains(capped.Detail, fmt.Sprintf("%d cited path(s)", cited-maxCitations)) {
		t.Fatalf("the finding must say how many went unchecked: %s", capped.Detail)
	}
	if !strings.Contains(capped.Detail, "pkg/gen-") {
		t.Fatalf("the finding must name where the cap bit: %s", capped.Detail)
	}
	if r.Outcome() == OutcomePass {
		t.Fatal("a run that could not resolve every citation must not read as a pass")
	}
}

// Every git call must inherit the run's aggregate deadline, so N items in a brief cannot
// cost N times the per-call timeout.
func TestLintGivesEveryGitCallTheRunBudget(t *testing.T) {
	repo, _ := fixture(t)
	spy := &spyGit{}
	start := time.Now()
	Lint(satisfying, Options{Repo: repo, Ref: "origin/main", Budget: 7 * time.Second, git: spy.fn})
	if len(spy.calls) == 0 {
		t.Fatal("no git calls were made")
	}
	if !spy.sawDeadline {
		t.Fatal("git calls must carry the run's deadline")
	}
	// The deadline is set when Lint starts, a moment after start, so allow a little slack.
	// The point is that it comes from the 7s budget and not from the 20s per-call ceiling.
	if got := spy.deadline.Sub(start); got > 8*time.Second {
		t.Fatalf("deadline is %s past the start, want it to come from the 7s budget", got)
	}
}

// An exhausted budget must decline to start work rather than run past it.
func TestGitRunDeclinesAnExhaustedBudget(t *testing.T) {
	repo, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := gitRun(ctx, repo, "rev-parse", "HEAD")
	if res.err == nil {
		t.Fatal("want an error for an exhausted budget")
	}
	if !strings.Contains(res.err.Error(), "not attempted") {
		t.Fatalf("unexpected error: %v", res.err)
	}
}

// Running out of budget is unevaluable, never a violation and never a pass.
func TestExhaustedBudgetIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	spy := &spyGit{answer: func(args []string) (gitResult, bool) {
		if args[0] == "ls-remote" {
			return gitResult{err: fmt.Errorf("git ls-remote ran out of time: %w", context.DeadlineExceeded)}, true
		}
		return gitResult{}, false
	}}
	r := Lint(satisfying, Options{Repo: repo, Ref: "origin/main", git: spy.fn})
	f := requireStatus(t, r, RuleTargetBranch, StatusIndeterminate)
	if !strings.Contains(f.Detail, "ran out of time") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// A brief citing many lines of one file must read that file once, not once per citation.
func TestLineCountsAreMemoizedPerPath(t *testing.T) {
	repo, reviewed := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"See pkg/thing.go:2, pkg/thing.go:4 and pkg/thing.go:6.", 1)
	spy := &spyGit{}
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed, git: spy.fn})
	requireClean(t, r, RuleFilePaths)
	if got := spy.count("cat-file", "blob"); got != 1 {
		t.Fatalf("three citations of one file read the blob %d times, want 1", got)
	}
}

// A blob too large to count is declined, not read into memory, and the citation is
// unevaluable rather than a violation.
func TestOversizedBlobIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	spy := &spyGit{answer: func(args []string) (gitResult, bool) {
		if len(args) > 1 && args[0] == "cat-file" && args[1] == "-s" {
			return gitResult{stdout: fmt.Sprintf("%d\n", maxBlobBytes+1)}, true
		}
		return gitResult{}, false
	}}
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "See pkg/thing.go:3", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: "HEAD", git: spy.fn})
	f := requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, "line-count cap") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
	if spy.count("cat-file", "blob") != 0 {
		t.Fatal("an oversized blob must not be read")
	}
}

// A repository that cannot answer whether a path exists (a shallow clone, a corrupt object)
// is unevaluable — the same classification every other query in git.go uses. Reporting it as
// a missing file would turn a repository problem into a rule violation.
func TestUnclassifiableTreeQueryIsIndeterminate(t *testing.T) {
	repo, _ := fixture(t)
	spy := &spyGit{answer: func(args []string) (gitResult, bool) {
		if args[0] == "ls-tree" {
			return gitResult{exit: 128, stderr: "fatal: not our ref\n"}, true
		}
		return gitResult{}, false
	}}
	r := Lint(satisfying, Options{Repo: repo, Ref: "origin/main", git: spy.fn})
	if r.Violations() != 0 {
		t.Fatalf("a repository problem must not be a violation: %+v", r.Findings)
	}
	fs := findingsFor(r, RuleFilePaths)
	if len(fs) == 0 {
		t.Fatal("the failed query must be reported")
	}
	for _, f := range fs {
		if f.Status != StatusIndeterminate {
			t.Fatalf("want %s, got %s (%s)", StatusIndeterminate, f.Status, f.Detail)
		}
	}
}

// Every brief-derived value reaches git as a positional argument, BEHIND an end-of-options
// guard, so a future change to the extraction regexes cannot smuggle in an option. The
// position is the point: a guard that sits after the value it is supposed to protect guards
// nothing, so this asserts the index and not merely the presence.
func TestBriefDerivedValuesAreGuardedFromOptionParsing(t *testing.T) {
	repo, reviewed := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go", "Fix pkg/thing.go:3", 1)
	spy := &spyGit{}
	Lint(brief, Options{Repo: repo, Ref: "origin/main", CitationsRef: reviewed, git: spy.fn})
	if len(spy.calls) == 0 {
		t.Fatal("no git calls were made")
	}
	// Everything here came out of the brief or out of the caller's options: the remote, the
	// branch, the refs, the cited path.
	fromBrief := []string{"origin", "main", "pkg/thing.go", "docs/guide.md", reviewed}
	checked := 0
	for _, c := range spy.calls {
		v := firstUserValue(c, fromBrief)
		if v < 0 {
			continue // a call carrying no brief-derived value needs no guard
		}
		checked++
		g := guardIndex(c)
		if g < 0 {
			t.Fatalf("git %v passes %q with no -- or --end-of-options guard", c, c[v])
		}
		if g > v {
			t.Fatalf("git %v puts its guard at %d, after the brief-derived %q at %d, so it guards nothing", c, g, c[v], v)
		}
	}
	if checked == 0 {
		t.Fatal("no call carried a brief-derived value, so this asserted nothing")
	}
}

// guardIndex is the position of the first end-of-options guard, or -1.
func guardIndex(args []string) int {
	for i, a := range args {
		if a == "--" || a == "--end-of-options" {
			return i
		}
	}
	return -1
}

// firstUserValue is the position of the first argument carrying one of the given values, or
// -1. Substring rather than equality, because a value can arrive inside a composite argument
// such as "<ref>:<path>" or "<ref>^{commit}".
func firstUserValue(args, values []string) int {
	for i, a := range args {
		for _, v := range values {
			if strings.Contains(a, v) {
				return i
			}
		}
	}
	return -1
}

// --- a check that did not run is not a pass ---------------------------------------------

// The project under review supplies this configuration, and a worker can commit it, so one
// line in AGENTS.md can turn off every rule. That run must read as "nothing was checked".
func TestEveryRuleDisabledIsNotAPass(t *testing.T) {
	repo, _ := fixture(t)
	var ids []string
	for _, id := range Rules() {
		ids = append(ids, string(id))
	}
	writeAgents(t, repo, "- brief-lint-disable: "+strings.Join(ids, ", ")+"\n")

	// A brief that violates several rules, so a pass here could only come from not looking.
	r := Lint("# Task\n\nJust do it. Do NOT push.\n", Options{Repo: repo, Ref: "origin/main", Config: LoadConfig(repo)})
	if r.Evaluated() != 0 {
		t.Fatalf("want no rule evaluated, got %d", r.Evaluated())
	}
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome: want %s, got %s (%+v)", OutcomeIndeterminate, r.Outcome(), r.Findings)
	}
	f := requireStatus(t, r, RuleConfig, StatusIndeterminate)
	if !strings.Contains(f.Detail, "nothing was checked") {
		t.Fatalf("the finding must say nothing was checked: %s", f.Detail)
	}
	if r.Total() != len(Rules()) {
		t.Fatalf("Total() = %d, want %d", r.Total(), len(Rules()))
	}
}

// A project turning off SOME rules is a legitimate override: what ran still decides the
// outcome. The count of what ran is what keeps that honest, and it is asserted at the CLI.
func TestPartialDisableStillJudgesWhatRan(t *testing.T) {
	repo, _ := fixture(t)
	writeAgents(t, repo, "- brief-lint-disable: hard-counts\n")
	r := Lint(satisfying, Options{Repo: repo, Config: LoadConfig(repo)})
	if r.Outcome() != OutcomePass {
		t.Fatalf("outcome: want %s, got %s (%+v)", OutcomePass, r.Outcome(), r.Findings)
	}
	if r.Evaluated() != r.Total()-1 {
		t.Fatalf("evaluated %d of %d, want one fewer than the whole set", r.Evaluated(), r.Total())
	}
}

// --- the create exemption is earned per mention, by a verb that governs the path ---------

// A retrospective verb does not exempt anything: this citation is fabricated and the
// sentence only mentions adding in passing.
func TestCreateExemptionNeedsTheVerbToGovernTheCitation(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"The regression at pkg/nope/ghost.go:9999 came from adding the cache.", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, "pkg/nope/ghost.go") {
		t.Fatalf("the fabricated citation must be reported, not exempted: %s", f.Detail)
	}
	if hasNote(r, "read as asking for them to be created") {
		t.Fatalf("nothing should have been exempted: %v", r.Notes)
	}
}

// An exemption belongs to the mention that earned it. A later reference to the same path is
// still checked, so one "Add x" near the top cannot cover the whole brief.
func TestCreateExemptionDoesNotCarryToOtherMentions(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"Add pkg/new/helper.go with the new code. Then fix the bug in pkg/new/helper.go that the old code left behind.", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	f := requireStatus(t, r, RuleFilePaths, StatusFail)
	if !strings.Contains(f.Detail, "pkg/new/helper.go does not exist") {
		t.Fatalf("the second mention must be checked: %s", f.Detail)
	}
}

// The exemption still works for the case it exists for: a file the brief asks to be written,
// mentioned only that way.
func TestCreateExemptionStillCoversAFileTheBriefAsksFor(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"Write dev/report/EVIDENCE.md with the results.", 1)
	r := Lint(brief, Options{Repo: repo, Ref: "origin/main"})
	requireClean(t, r, RuleFilePaths)
	if !hasNote(r, "dev/report/EVIDENCE.md") {
		t.Fatalf("the exemption must still be reported: %v", r.Notes)
	}
}

// --- the hedge must be attached to the count ---------------------------------------------

// The brief that motivated this rule said 21 when the answer was 7. Prose elsewhere that
// mentions checking and results is not a hedge.
func TestRuleHardCountsRejectsBoilerplate(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"There are 21 occurrences of the old helper. Fix them. Check the build afterwards and note the results.", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleHardCounts, StatusFail)
	if !strings.Contains(f.Detail, "no hedge wording near it") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// A hedge that is attached but never asks for the number is a different failure, and says so.
func TestRuleHardCountsRejectsAHedgeThatAsksForNothing(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"There are 21 occurrences of the old helper, though that figure may be wrong. Fix them.", 1)
	r := Lint(brief, Options{Repo: repo})
	f := requireStatus(t, r, RuleHardCounts, StatusFail)
	if !strings.Contains(f.Detail, "never asks for the worker's own number") {
		t.Fatalf("unexpected detail: %s", f.Detail)
	}
}

// An exempted citation skips the existence check, not the line bound. Skipping both let a
// fabricated line number through on any path the brief could be read as asking to create.
func TestCreateExemptionStillBoundsTheLine(t *testing.T) {
	repo, _ := fixture(t)

	// pkg/thing.go exists on the base with fixtureShortLines lines, so the line is checkable
	// and out of range even though the sentence asks for a write.
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		fmt.Sprintf("Write pkg/thing.go:%d with the new helper.", fixtureShortLines+9000), 1)
	f := requireStatus(t, Lint(brief, Options{Repo: repo}), RuleFilePaths, StatusIndeterminate)
	if !strings.Contains(f.Detail, fmt.Sprintf("has %d line(s)", fixtureShortLines)) {
		t.Fatalf("want the line bound reported against the real file, got %q", f.Detail)
	}

	// A file the brief asks for that genuinely does not exist keeps its exemption, line
	// number and all: there is nothing to bound it against yet.
	brief = strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"Write dev/report/EVIDENCE.md:120 with the findings.", 1)
	requireClean(t, Lint(brief, Options{Repo: repo}), RuleFilePaths)
}

// The rule matches vocabulary, so a sentence forbidding a recount passes it. That is not
// fixable by extending the vocabulary, and the point of this test is that nothing in the
// output describes such a brief as hedged.
func TestRuleHardCountsClaimsOnlyWhatItChecks(t *testing.T) {
	repo, _ := fixture(t)
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.",
		"There are 21 occurrences; I verified the count myself, so do not re-count it and do not report a different number.", 1)
	r := Lint(brief, Options{Repo: repo})
	requireClean(t, r, RuleHardCounts)
	note := ""
	for _, n := range r.Notes {
		if strings.HasPrefix(n, "hard-counts:") {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("no hard-counts note in %v", r.Notes)
	}
	if !strings.Contains(note, "wording only") {
		t.Errorf("the note must say what was matched, got %q", note)
	}
	for _, claim := range []string{"each hedged", "is hedged", "carries a hedge"} {
		if strings.Contains(note, claim) {
			t.Errorf("the note claims %q, which this rule cannot establish: %q", claim, note)
		}
	}
}

// Git's stderr is the remote's text, not ours: every host relays a server's "remote:"
// output verbatim. Reproduced live against a remote emitting ESC, CR and BEL, which reached
// the terminal intact and could erase the line a manager had just read.
func TestGitDiagnosticsAreQuoted(t *testing.T) {
	repo, _ := fixture(t)
	const hostile = "remote: \x1b[2K\rEVERYTHING IS FINE\a"
	hostileGit := func(_ context.Context, _ string, args ...string) gitResult {
		return gitResult{exit: 128, stderr: hostile + "\nsecond line"}
	}
	r := Lint(satisfying, Options{Repo: repo, git: hostileGit})
	if len(r.Findings) == 0 {
		t.Fatal("a failing git must produce a finding to quote into")
	}
	for _, f := range r.Findings {
		if strings.ContainsAny(f.Detail, "\x1b\r\a") {
			t.Errorf("raw control bytes reach the terminal: %q", f.Detail)
		}
		if !strings.Contains(f.Detail, `\x1b[2K\rEVERYTHING IS FINE\a`) {
			t.Errorf("the diagnostic must still be readable, escaped: %q", f.Detail)
		}
	}
}

// A remote that talks at length must not fill the report with its own text.
func TestGitDiagnosticsAreCapped(t *testing.T) {
	repo, _ := fixture(t)
	loudGit := func(_ context.Context, _ string, args ...string) gitResult {
		return gitResult{exit: 128, stderr: strings.Repeat("A", 4000)}
	}
	r := Lint(satisfying, Options{Repo: repo, git: loudGit})
	if len(r.Findings) == 0 {
		t.Fatal("a failing git must produce a finding to quote into")
	}
	for _, f := range r.Findings {
		if n := strings.Count(f.Detail, "A"); n > maxMsgBytes {
			t.Errorf("%d bytes of remote output reached the report, cap is %d", n, maxMsgBytes)
		}
	}
}

// The same brief must get the same verdict whether or not its sentences are capitalised.
//
// The splitter keeps a lowercase continuation inside one span on purpose, so every rule that
// then asks a question about "this sentence" has to narrow its scope back. When one of them
// does not, capitalisation alone decides whether a brief passes, and agent-authored briefs
// are written in lowercase.
func TestCapitalisationDoesNotDecideTheVerdict(t *testing.T) {
	repo, _ := fixture(t)
	const rules = "No changes to any product branch and no PR until I have reviewed the work. Commit on your\nown branch, leave the worktree clean, and report the sha."
	cases := map[string][2]string{
		// Every rule that reports a count or quotes a span belongs here, not just the one
		// whose defect prompted the test: rule 3 counted and quoted spans for a round after
		// rule 4 stopped.
		"two hard counts, unhedged": {
			"there are 21 occurrences. there are 19 files. commit on your own branch, leave the worktree clean, and report the sha.",
			"There are 21 occurrences. There are 19 files. Commit on your own branch, leave the worktree clean, and report the sha.",
		},
		"two hard counts, hedged": {
			"there are 21 occurrences. there are 19 files. both figures may be wrong, so count them yourself and report the number you find. commit on your own branch, leave the worktree clean, and report the sha.",
			"There are 21 occurrences. There are 19 files. Both figures may be wrong, so count them yourself and report the number you find. Commit on your own branch, leave the worktree clean, and report the sha.",
		},
		"three bare bans": {
			"do not push to main. do not commit anything at all. do not merge anything either. commit on your own branch, leave the worktree clean, and report the sha.",
			"Do not push to main. Do not commit anything at all. Do not merge anything either. Commit on your own branch, leave the worktree clean, and report the sha.",
		},
		"all bounded": {
			"do not push to main. do not merge without my approval. commit on your own branch, leave the worktree clean, and report the sha.",
			"Do not push to main. Do not merge without my approval. Commit on your own branch, leave the worktree clean, and report the sha.",
		},
	}
	for name, tc := range cases {
		lo := Lint(strings.Replace(satisfying, rules, tc[0], 1), Options{Repo: repo})
		up := Lint(strings.Replace(satisfying, rules, tc[1], 1), Options{Repo: repo})
		if got, want := len(lo.Findings), len(up.Findings); got != want {
			t.Errorf("%s: lowercase produced %d finding(s), capitalised %d\nlower: %v\nupper: %v",
				name, got, want, lo.Findings, up.Findings)
			continue
		}
		for i := range lo.Findings {
			l, u := lo.Findings[i], up.Findings[i]
			if l.Rule != u.Rule || l.Status != u.Status || !strings.EqualFold(l.Detail, u.Detail) {
				t.Errorf("%s: finding %d differs by case alone:\nlower: %+v\nupper: %+v", name, i, l, u)
			}
		}
		if !strings.EqualFold(strings.Join(lo.Notes, "|"), strings.Join(up.Notes, "|")) {
			t.Errorf("%s: notes differ by case alone:\nlower: %v\nupper: %v", name, lo.Notes, up.Notes)
		}
	}
}

// Rule 3 counts the numbers stated and quotes the sentence carrying each one. It counted
// spans and quoted spans for a round after rule 4 stopped doing both, so a merged span
// certified one hard count where the same words capitalised certified two.
func TestRuleHardCountsCountsAndQuotesSentences(t *testing.T) {
	repo, _ := fixture(t)
	const rules = "No changes to any product branch and no PR until I have reviewed the work. Commit on your\nown branch, leave the worktree clean, and report the sha."

	hedged := "there are 21 occurrences. there are 19 files. both figures may be wrong, so count them yourself and report the number you find. " + strings.ToLower(rules)
	r := Lint(strings.Replace(satisfying, rules, hedged, 1), Options{Repo: repo})
	requireClean(t, r, RuleHardCounts)
	if !hasNote(r, "hard-counts: 2 hard count(s)") {
		t.Errorf("want both counts counted, got %v", r.Notes)
	}

	// Unhedged, and the quote must be the sentence with the count, not the merged span.
	bare := "the helper is fine. there are 21 occurrences to fix. " + strings.ToLower(rules)
	r = Lint(strings.Replace(satisfying, rules, bare, 1), Options{Repo: repo})
	f := requireStatus(t, r, RuleHardCounts, StatusFail)
	if f.Quote != "there are 21 occurrences to fix" {
		t.Errorf("want the sentence carrying the count quoted, got %q", f.Quote)
	}
}

// A brief past the size cap is refused, and refused as unevaluable: reading part of a brief
// says nothing about the whole of it.
func TestBriefOverTheSizeCapIsRefused(t *testing.T) {
	repo, _ := fixture(t)
	big := satisfying + strings.Repeat("filler text that says nothing. ", (MaxBriefBytes/31)+1)
	if len(big) <= MaxBriefBytes {
		t.Fatalf("fixture must exceed the cap, got %d bytes", len(big))
	}
	r := Lint(big, Options{Repo: repo})
	if r.Outcome() != OutcomeIndeterminate {
		t.Fatalf("outcome = %s, want %s", r.Outcome(), OutcomeIndeterminate)
	}
	if r.Evaluated() != 0 {
		t.Errorf("no rule can have run, got %d", r.Evaluated())
	}
	if f := r.Findings[0]; !strings.Contains(f.Detail, "over the") || !strings.Contains(f.Detail, "nothing was checked") {
		t.Errorf("the refusal must state the limit and that nothing ran: %q", f.Detail)
	}
	// And it must not be reported as a project disable: nothing was disabled.
	if !r.Refused() {
		t.Error("a size refusal must be distinguishable from a project disabling every rule")
	}
}

// The text rules honour the budget they accept. They were given a context they never read,
// which is how a pathological brief ran to completion for 69 seconds.
func TestTextRulesStopWhenTheBudgetIsSpent(t *testing.T) {
	repo, _ := fixture(t)
	hostile := "Base is origin/main in this repository.\n\n" + strings.Repeat("do not push ", 4000) + "\n"
	r := Lint(hostile, Options{Repo: repo, Budget: time.Nanosecond})
	var stopped bool
	for _, f := range r.Findings {
		if strings.Contains(f.Detail, "the run budget expired before this rule finished") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("a spent budget must stop a text rule and say so, got %v", r.Findings)
	}
	if r.Outcome() == OutcomePass {
		t.Error("a rule that stopped early must not read as a pass")
	}
}

// Every text rule must be linear in the brief, and the guard has to feed every shape. The
// first version of it fed bans only, so it covered rule 4 alone: rule 3 acquired the same
// quadratic in the commit after rule 4 lost it, and this test stayed green throughout.
//
// The assertion is complexity, not a wall-clock number. Each input is sized so that the
// quadratic form cannot finish inside the budget while the linear form finishes in a
// fraction of it, so a regression reports a spent budget where a healthy rule reports its
// findings.
func TestEveryTextRuleIsLinearInTheBrief(t *testing.T) {
	repo, _ := fixture(t)
	cases := map[string]struct {
		rule   RuleID
		body   string
		budget time.Duration
		was    string // measured at 0e5f11b, the sha that carried the defect
	}{
		"rule 4, bans": {
			RuleProhibition, strings.Repeat("do not push ", 4000), 10 * time.Second,
			"17s at 4000 before cf73928",
		},
		"rule 3, counts": {
			RuleHardCounts, strings.Repeat("there are 21 occurrences. ", 16000), 5 * time.Second,
			"8.5s at 16000 before this round",
		},
		"rule 2, citations": {
			RuleFilePaths, strings.Repeat("see internal/pkg/file.go and internal/pkg/other.go ", 4000), 10 * time.Second,
			"44s at 8000 tokens before this round",
		},
	}
	for name, tc := range cases {
		brief := "Base is origin/main in this repository.\n\n" + tc.body + "\n"
		r := Lint(brief, Options{Repo: repo, Budget: tc.budget})
		for _, f := range r.Findings {
			if f.Rule == tc.rule && strings.Contains(f.Detail, "the run budget expired") {
				t.Errorf("%s: exhausted a %s budget, which is the quadratic returning (%s)", name, tc.budget, tc.was)
			}
		}
	}
}

// A brief is untrusted input, so what a finding prints from it is bounded. A 60 KB token
// produced 121 KB of terminal output.
func TestOneAbsurdTokenCannotFillAReport(t *testing.T) {
	repo, _ := fixture(t)
	token := "internal/" + strings.Repeat("a", 60000) + ".go"
	brief := strings.Replace(satisfying, "Touch pkg/thing.go and docs/guide.md.", "Fix the finding at "+token+" first.", 1)
	r := Lint(brief, Options{Repo: repo})
	for _, f := range r.Findings {
		if len(f.Quote) > maxTokenBytes+3 {
			t.Errorf("a %d byte quote reached the report", len(f.Quote))
		}
		if len(f.Detail) > 1000 {
			t.Errorf("a %d byte detail reached the report", len(f.Detail))
		}
	}
	for _, n := range r.Notes {
		if len(n) > 1000 {
			t.Errorf("a %d byte note reached the report", len(n))
		}
	}
}

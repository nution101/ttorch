// Package ciparity reproduces a repository's actual CI checks locally so that a worker
// that is green locally is also green in CI. It parses the repo's GitHub Actions workflow
// files (.github/workflows/*.yml), extracts the shell `run:` steps of the CI job(s), and
// classifies each step as either reproducible locally or skipped — with a reason for every
// skip, so nothing is dropped silently.
//
// Execution safety fails CLOSED. Because a CI-parity run auto-executes the extracted steps,
// a step is reproduced only when EVERY command in its script is on a conservative allowlist
// of safe build/test/lint commands (go build/test/vet/run, gofmt, golangci-lint,
// staticcheck, make build/test/lint/vet/check, npm/pnpm/yarn/bun test/lint/build) plus
// harmless shell builtins (echo, test, set, cd, exit, …). The script is parsed into its
// individual commands — across `;`, `&&`, `||`, `|`, newlines, grouping, and `$(…)`/backtick
// substitutions — and ANY command that is not a bare, recognized executable causes the step
// to be skipped rather than run: an unknown tool, `sudo`, a package install, a pipe-to-shell,
// a deploy/publish, a filesystem mutation, a path-qualified executable (./go, bin/make), or a
// command carrying a leading inline `VAR=val` assignment (GOFLAGS=…, PATH=…). Path prefixes
// and inline assignments are deliberately NOT normalized away, since either could inject
// arbitrary execution or redirect command resolution past the allowlist.
//
// Caveat: the allowlist guarantees the ENTRYPOINT, not its effects. An allowlisted `make`,
// `npm run`, or `go test` still executes attacker-defined Makefile recipes, package.json
// scripts, or repository test code — ci-parity extends the same trust in a repository's own
// checks that internal/validate does (only run CI-parity against repos and worker output you
// trust).
//
// Steps are also skipped (and reported) when they otherwise cannot or should not run
// locally: `uses:` action steps, steps in a workflow that is not a pull_request/push-branch
// CI workflow (e.g. a tag-triggered release), jobs or steps guarded by a non-trivial `if:`,
// steps whose script contains an unresolved GitHub Actions expression (${{ … }}, including
// matrix/secret references), and steps using a non-shell `shell:` (whether set on the step
// or inherited from `defaults.run`). Structurally unexpected jobs/steps (a reusable-workflow
// `uses:` job, a non-mapping job or step) are reported as skips too, never dropped.
//
// Known limitation (documented rather than silently ignored): `env:` is not injected into
// the local run. A reproducible step carries a Note naming the workflow-, job-, and
// step-level env that applies to it but is not reproduced.
//
// Parsing is dependency-free and deliberately narrow (see yaml.go); it is not a general
// YAML parser. Extract reads the filesystem; ParseWorkflow operates on bytes so tests can
// feed fixtures without touching the filesystem or the network.
package ciparity

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Step is a CI run-step that can be reproduced locally.
type Step struct {
	Workflow string // workflow file name, e.g. "ci.yml"
	Job      string // job id
	Name     string // step name, or a label derived from the run script
	Run      string // the shell script body
	Shell    string // explicit shell ("" means the default, bash)
	Note     string // an advisory note (e.g. CI env not reproduced), empty when none
}

// Skip is a workflow step — or a whole workflow/job — that was not reproduced, and why.
type Skip struct {
	Workflow string
	Job      string // empty for a whole-workflow skip
	Name     string // empty for a workflow/job-level skip
	Reason   string
}

// Plan is the result of extracting CI-parity steps from a repository's workflows.
type Plan struct {
	Files       []string          // workflow files parsed, sorted
	Steps       []Step            // reproducible steps, in workflow/job/step order
	Skipped     []Skip            // everything not reproduced, with reasons
	ParseErrors map[string]string // workflow file -> parse/read error (reported, non-fatal)
}

// Command returns the argv that reproduces the step locally, honoring its shell. bash
// steps run with errexit + pipefail to mirror GitHub Actions' default run-step semantics.
func (s Step) Command() []string {
	switch s.Shell {
	case "", "bash":
		return []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", s.Run}
	case "sh":
		return []string{"sh", "-e", "-c", s.Run}
	default:
		// classifyStep skips unsupported shells, so this is only a defensive fallback.
		return []string{"bash", "-c", s.Run}
	}
}

// Label is a stable, human-readable identifier for the step, used when reporting results.
func (s Step) Label() string {
	return fmt.Sprintf("ci(%s/%s): %s", s.Workflow, s.Job, s.Name)
}

// Extract reads dir/.github/workflows/*.{yml,yaml} and returns the CI-parity plan. A
// missing workflows directory yields an empty plan (not an error).
func Extract(dir string) (*Plan, error) {
	wfDir := filepath.Join(dir, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &Plan{ParseErrors: map[string]string{}}, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			files = append(files, n)
		}
	}
	sort.Strings(files)

	plan := &Plan{ParseErrors: map[string]string{}}
	for _, f := range files {
		plan.Files = append(plan.Files, f)
		data, err := os.ReadFile(filepath.Join(wfDir, f))
		if err != nil {
			plan.ParseErrors[f] = err.Error()
			continue
		}
		steps, skips, err := ParseWorkflow(f, data)
		if err != nil {
			plan.ParseErrors[f] = err.Error()
			continue
		}
		plan.Steps = append(plan.Steps, steps...)
		plan.Skipped = append(plan.Skipped, skips...)
	}
	return plan, nil
}

// ParseWorkflow extracts reproducible steps and skips from one workflow file's bytes.
// It is the byte-level entry point used by tests (no filesystem or network access).
func ParseWorkflow(file string, data []byte) (steps []Step, skipped []Skip, err error) {
	root := parseYAML(string(data))
	if root == nil {
		return nil, nil, nil // empty or comment-only file: nothing to reproduce
	}
	top, ok := root.(*mapping)
	if !ok {
		return nil, nil, fmt.Errorf("%s: top level is not a YAML mapping", file)
	}

	onNode, _ := top.get("on")
	if ci, triggers := isCIWorkflow(onNode); !ci {
		skipped = append(skipped, Skip{
			Workflow: file,
			Reason:   fmt.Sprintf("workflow triggers on %s — not a pull_request/push-branch CI workflow", triggerDesc(triggers)),
		})
		return steps, skipped, nil
	}

	jobs, ok := mappingValue(top, "jobs")
	if !ok {
		return steps, skipped, nil
	}
	wfShell, wfWorkdir := runDefaults(top)
	wfEnvKeys := envKeys(top)

	for _, jobID := range jobs.keys {
		job, ok := jobs.values[jobID].(*mapping)
		if !ok {
			skipped = append(skipped, Skip{
				Workflow: file, Job: jobID,
				Reason: "job definition is not a mapping — cannot extract steps",
			})
			continue
		}
		if cond := scalarOf(job, "if"); !trivialIf(cond) {
			skipped = append(skipped, Skip{
				Workflow: file, Job: jobID,
				Reason: fmt.Sprintf("job guarded by if: %s", cond),
			})
			continue
		}
		stepSeq, hasSteps := job.get("steps")
		if !hasSteps {
			reason := "job has no run steps"
			if uses := scalarOf(job, "uses"); uses != "" {
				reason = fmt.Sprintf("reusable-workflow job (uses: %s) — no local run steps", uses)
			}
			skipped = append(skipped, Skip{Workflow: file, Job: jobID, Reason: reason})
			continue
		}
		seq, ok := stepSeq.([]node)
		if !ok {
			skipped = append(skipped, Skip{
				Workflow: file, Job: jobID,
				Reason: "job steps are not a sequence — cannot extract steps",
			})
			continue
		}
		jobShell, jobWorkdir := runDefaults(job)
		jobEnvKeys := envKeys(job)
		for idx, sn := range seq {
			sm, ok := sn.(*mapping)
			if !ok {
				skipped = append(skipped, Skip{
					Workflow: file, Job: jobID,
					Name:   fmt.Sprintf("step %d", idx+1),
					Reason: "step is not a mapping — cannot classify",
				})
				continue
			}
			// A step's effective shell/working-directory may be inherited from the job's
			// or the workflow's defaults.run; the step-level value wins.
			shell := firstNonEmpty(scalarOf(sm, "shell"), jobShell, wfShell)
			workdir := firstNonEmpty(scalarOf(sm, "working-directory"), jobWorkdir, wfWorkdir)
			if st, sk := classifyStep(file, jobID, idx, sm, shell, workdir); st != nil {
				st.Note = envNote(wfEnvKeys, jobEnvKeys, sm)
				steps = append(steps, *st)
			} else if sk != nil {
				skipped = append(skipped, *sk)
			}
		}
	}
	return steps, skipped, nil
}

// classifyStep decides whether a single step is reproducible locally, returning exactly
// one of (*Step, nil) or (nil, *Skip). shell and workdir are the step's effective values
// (already resolved against the job's and workflow's defaults.run by the caller).
func classifyStep(file, job string, idx int, sm *mapping, shell, workdir string) (*Step, *Skip) {
	name := scalarOf(sm, "name")
	uses := scalarOf(sm, "uses")
	run := scalarOf(sm, "run")
	cond := scalarOf(sm, "if")

	label := name
	switch {
	case label != "":
	case run != "":
		label = firstLine(run)
	case uses != "":
		label = uses
	default:
		label = fmt.Sprintf("step %d", idx+1)
	}
	skip := func(reason string) (*Step, *Skip) {
		return nil, &Skip{Workflow: file, Job: job, Name: label, Reason: reason}
	}

	switch {
	case run == "" && uses != "":
		return skip(fmt.Sprintf("action step (uses: %s) — not a local shell command", uses))
	case run == "":
		return skip("no run command")
	case !trivialIf(cond):
		return skip(fmt.Sprintf("guarded by if: %s", cond))
	case shell != "" && shell != "bash" && shell != "sh":
		return skip(fmt.Sprintf("unsupported shell: %s", shell))
	case workdir != "":
		return skip(fmt.Sprintf("sets working-directory: %s", workdir))
	case strings.Contains(run, "${{"):
		return skip("contains a GitHub Actions expression (${{ … }}) that cannot be resolved locally")
	}
	// Execution safety fails CLOSED: a step is auto-run only when EVERY command in it is on
	// the safe allowlist. Any unrecognized, ambiguous, or host-mutating command skips it.
	if seg, unsafe := unsafeCommand(run); unsafe {
		return skip(fmt.Sprintf("not on the local auto-run allowlist (%s) — skipped (fail closed)", firstLine(seg)))
	}
	return &Step{Workflow: file, Job: job, Name: label, Run: run, Shell: shell}, nil
}

// envNote describes the env that applies to a reproducible step but is NOT reproduced
// locally — at workflow, job, and step scope — so the gap is surfaced rather than silent.
// It returns "" when no env applies.
func envNote(wfKeys, jobKeys []string, sm *mapping) string {
	var parts []string
	if len(wfKeys) > 0 {
		parts = append(parts, "workflow ("+strings.Join(wfKeys, ", ")+")")
	}
	if len(jobKeys) > 0 {
		parts = append(parts, "job ("+strings.Join(jobKeys, ", ")+")")
	}
	if k := envKeys(sm); len(k) > 0 {
		parts = append(parts, "step ("+strings.Join(k, ", ")+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return "env not reproduced locally — " + strings.Join(parts, "; ")
}

// envKeys returns the keys of a mapping's `env:` block, in document order, or nil.
func envKeys(m *mapping) []string {
	if env, ok := m.get("env"); ok {
		if em, ok := env.(*mapping); ok {
			return em.keys
		}
	}
	return nil
}

// isCIWorkflow reports whether a workflow's `on:` triggers make it a pull_request or
// push-branch CI workflow (the parity target), and returns the trigger names for reporting.
// A push that targets only tags (e.g. a release workflow) is not treated as CI.
func isCIWorkflow(on node) (bool, []string) {
	triggers, pushPresent, pushBranch := triggersOf(on)
	hasPR := contains(triggers, "pull_request") || contains(triggers, "pull_request_target")
	return hasPR || (pushPresent && pushBranch), triggers
}

func triggersOf(on node) (triggers []string, pushPresent, pushBranch bool) {
	switch v := on.(type) {
	case string:
		for _, t := range parseFlowList(v) {
			triggers = append(triggers, t)
			if t == "push" {
				pushPresent, pushBranch = true, true
			}
		}
	case []node:
		for _, e := range v {
			if s, ok := e.(string); ok {
				triggers = append(triggers, s)
				if s == "push" {
					pushPresent, pushBranch = true, true
				}
			}
		}
	case *mapping:
		for _, k := range v.keys {
			triggers = append(triggers, k)
			if k == "push" {
				pushPresent = true
				pushBranch = pushTargetsBranches(v.values[k])
			}
		}
	}
	return triggers, pushPresent, pushBranch
}

// pushTargetsBranches reports whether a `push:` trigger fires on branches (and is thus
// CI-like) rather than on tags only (release-like).
func pushTargetsBranches(n node) bool {
	m, ok := n.(*mapping)
	if !ok {
		return true // bare or scalar push fires on all branches
	}
	if _, ok := m.get("branches"); ok {
		return true
	}
	if _, ok := m.get("tags"); ok {
		return false // tags only
	}
	return true
}

// parseFlowList splits a scalar that may hold a single event ("push") or an inline flow
// list ("[push, pull_request]") into individual event names.
func parseFlowList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(unquote(strings.TrimSpace(part))); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func triggerDesc(triggers []string) string {
	if len(triggers) == 0 {
		return "an unrecognized event set"
	}
	return strings.Join(triggers, ", ")
}

// trivialIf reports whether an `if:` condition always evaluates true in normal CI (so a
// step/job carrying it can be reproduced). Anything else is treated as non-reproducible.
func trivialIf(cond string) bool {
	c := strings.TrimSpace(cond)
	c = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(c, "${{"), "}}"))
	switch c {
	case "", "success()", "always()", "true":
		return true
	}
	return false
}

// unsafeCommand returns the first command segment in run that is NOT on the local auto-run
// allowlist, or ok=false when every segment is allowlisted. Because ci-parity auto-executes
// the steps it extracts, it fails CLOSED: a step is only run when every command in it is
// recognized as a safe build/test/lint command (or a harmless shell builtin). Anything
// unknown, ambiguous, or host-mutating — sudo, package installs, pipe-to-shell, deploy,
// filesystem mutation — causes the step to be skipped rather than run.
func unsafeCommand(run string) (segment string, ok bool) {
	for _, seg := range commandSegments(run) {
		exe, args := executable(seg)
		if !commandAllowed(exe, args) {
			return seg, true
		}
	}
	return "", false
}

// commandSegments splits a shell run script into the individual command segments that
// would execute, descending into $(...) and `...` command substitutions so every command
// is surfaced. It is quote-aware (operators and substitutions inside single quotes are
// literal) so quoted arguments are not mis-split, and it over-segments on ambiguity since
// the caller fails closed.
func commandSegments(run string) []string {
	var segs []string
	var buf strings.Builder
	flush := func() {
		if s := strings.TrimSpace(buf.String()); s != "" {
			segs = append(segs, s)
		}
		buf.Reset()
	}
	rs := []rune(run)
	inSingle, inDouble := false, false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			} else {
				buf.WriteRune(c)
			}
			continue
		}
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			buf.WriteRune(rs[i])
		case c == '\'' && !inDouble:
			inSingle = true
		case c == '"':
			inDouble = !inDouble
		case c == '`':
			j := i + 1
			for j < len(rs) && rs[j] != '`' {
				j++
			}
			segs = append(segs, commandSegments(string(rs[i+1:j]))...)
			i = j
			buf.WriteRune(' ')
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			depth, j := 1, i+2
			for j < len(rs) && depth > 0 {
				switch rs[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 {
					break
				}
				j++
			}
			segs = append(segs, commandSegments(string(rs[i+2:j]))...)
			i = j
			buf.WriteRune(' ')
		case !inDouble && c == '#' && endsWithSpaceOrEmpty(&buf):
			// shell comment: skip to end of line so it is not mistaken for a command
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case !inDouble && (c == ';' || c == '\n' || c == '&' || c == '|' || c == '(' || c == ')' || c == '{' || c == '}'):
			flush()
		default:
			buf.WriteRune(c)
		}
	}
	flush()
	return segs
}

// executable extracts the program a command segment runs, after stripping leading
// command/exec/env-style prefixes. The executable token is returned exactly as written:
// neither a path prefix nor a leading inline `VAR=val` assignment is stripped, because both
// are treated as unknown by commandAllowed (see there). A leading inline assignment is left
// in place precisely so it is NOT silently ignored — `GOFLAGS=-toolexec=… go build` and
// `PATH=. go test` would otherwise look like clean go commands while injecting arbitrary
// execution. It also returns the remaining arguments for subcommand checks.
func executable(seg string) (string, []string) {
	tokens := strings.Fields(seg)
	i := 0
	for i < len(tokens) {
		switch tokens[i] {
		case "command", "exec", "env", "time", "nice", "builtin":
			i++
			continue
		}
		break
	}
	if i >= len(tokens) {
		return "", nil
	}
	return strings.Trim(tokens[i], `"'`), tokens[i+1:]
}

// commandAllowed reports whether a single command (its executable and arguments) is safe to
// auto-run locally. This is the heart of the fail-closed model: only the listed build/test/
// lint entrypoints and harmless shell builtins return true; everything else returns false.
//
// A path-qualified executable (e.g. ./go, bin/make, /usr/bin/sudo) is always treated as
// unknown: the repository could commit a malicious binary by a safe-looking name, so only a
// bare, known-safe executable name is ever auto-run — never one resolved from a repo path.
func commandAllowed(exe string, args []string) bool {
	// A leading inline VAR=val assignment (e.g. GOFLAGS=-toolexec=… or PATH=.) is treated as
	// unknown: it can inject arbitrary execution into, or redirect command resolution of, an
	// otherwise safe-looking command, so a segment beginning with one is never auto-run.
	if isAssignment(exe) {
		return false
	}
	if exe != "" && strings.ContainsRune(exe, '/') {
		return false
	}
	switch exe {
	case "":
		return true // a bare VAR=val assignment or empty segment runs nothing
	case "echo", "printf", "test", "[", "true", "false", ":", "exit",
		"set", "cd", "pwd", "export", "unset", "pushd", "popd":
		return true
	case "gofmt", "goimports", "golangci-lint", "staticcheck":
		return true
	case "go":
		switch firstNonFlag(args) {
		case "build", "test", "vet", "run", "fmt":
			return goFlagsSafe(args)
		}
		return false
	case "make":
		return makeTargetsSafe(args)
	case "npm", "pnpm", "yarn", "bun":
		return jsRunnerSafe(args)
	}
	return false
}

// goFlagsSafe reports whether a go command is free of flags that run an arbitrary external
// program: -exec, -toolexec, and -vettool (in any -/-- and =value form). Their presence
// means the step would execute an attacker-chosen binary, so it must be skipped.
func goFlagsSafe(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		switch name {
		case "exec", "toolexec", "vettool":
			return false
		}
	}
	return true
}

// makeTargetsSafe reports whether a make invocation runs only safe targets: every non-flag,
// non-assignment argument must be a known-safe target, and there must be at least one (a
// bare `make` runs an unknown default target, so it fails closed).
func makeTargetsSafe(args []string) bool {
	safe := map[string]bool{"build": true, "test": true, "lint": true, "vet": true, "check": true, "fmt": true}
	found := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") || isAssignment(a) {
			continue
		}
		if !safe[a] {
			return false
		}
		found = true
	}
	return found
}

// jsRunnerSafe reports whether an npm/pnpm/yarn/bun invocation runs only a safe script
// (test/lint/build), directly or via `run`.
func jsRunnerSafe(args []string) bool {
	scripts := map[string]bool{"test": true, "lint": true, "build": true}
	first, rest := "", args
	for idx, a := range args {
		if !strings.HasPrefix(a, "-") {
			first, rest = a, args[idx+1:]
			break
		}
	}
	if scripts[first] {
		return true
	}
	if first == "run" {
		return scripts[firstNonFlag(rest)]
	}
	return false
}

// endsWithSpaceOrEmpty reports whether the current segment buffer is empty or ends in
// whitespace — i.e. a '#' here begins a shell comment rather than continuing a word.
func endsWithSpaceOrEmpty(b *strings.Builder) bool {
	s := b.String()
	return len(s) == 0 || s[len(s)-1] == ' ' || s[len(s)-1] == '\t'
}

func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// isAssignment reports whether a token is a shell environment assignment (NAME=value).
func isAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range t[:eq] {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// runDefaults returns the shell and working-directory declared under a mapping's
// `defaults: { run: { shell, working-directory } }`. GitHub Actions applies these to every
// run step in scope (the whole workflow, or a single job). Empty strings mean "not set".
func runDefaults(m *mapping) (shell, workdir string) {
	defaults, ok := mappingValue(m, "defaults")
	if !ok {
		return "", ""
	}
	runDef, ok := mappingValue(defaults, "run")
	if !ok {
		return "", ""
	}
	return scalarOf(runDef, "shell"), scalarOf(runDef, "working-directory")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func mappingValue(m *mapping, key string) (*mapping, bool) {
	v, ok := m.get(key)
	if !ok {
		return nil, false
	}
	mm, ok := v.(*mapping)
	return mm, ok
}

func scalarOf(m *mapping, key string) string {
	if v, ok := m.get(key); ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstLine(s string) string {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	line = strings.TrimSpace(line)
	const max = 72
	// Truncate on a rune boundary so the appended ellipsis never lands mid-rune.
	if r := []rune(line); len(r) > max {
		line = string(r[:max-1]) + "…"
	}
	return line
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Package ciparity reproduces a repository's actual CI checks locally so that a worker
// that is green locally is also green in CI. It parses the repo's GitHub Actions workflow
// files (.github/workflows/*.yml), extracts the shell `run:` steps of the CI job(s), and
// classifies each step as either reproducible locally or skipped — with a reason for every
// skip, so nothing is dropped silently.
//
// Steps are skipped (and reported) when they cannot or should not run locally: `uses:`
// action steps, steps in a workflow that is not a pull_request/push-branch CI workflow
// (e.g. a tag-triggered release), jobs or steps guarded by a non-trivial `if:`, steps
// whose script contains an unresolved GitHub Actions expression (${{ … }}, including
// matrix/secret references), deploy/publish steps, steps that mutate or escalate host
// state (e.g. `git config --global`, `sudo`, or global/system package installs such as
// `apt-get install` or `npm install -g`), and steps using a non-shell `shell:` (whether set
// on the step or inherited from `defaults.run`). Structurally unexpected jobs/steps (a
// reusable-workflow `uses:` job, a non-mapping job or step) are reported as skips too,
// never dropped.
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
	if tok := publishToken(run); tok != "" {
		return skip(fmt.Sprintf("looks like a deploy/publish step (%s)", tok))
	}
	if tok := hostMutationToken(run); tok != "" {
		return skip(fmt.Sprintf("would mutate the host or require elevation (%s) — not auto-run locally", tok))
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

// publishToken returns the first deploy/publish indicator found in a run script, or "".
func publishToken(run string) string {
	tokens := []string{
		"gh release", "npm publish", "yarn publish", "pnpm publish",
		"docker push", "cosign sign", "goreleaser release",
		"aws s3", "aws deploy", "aws cloudformation", "terraform apply",
		"kubectl apply", "helm upgrade", "helm install", "twine upload",
	}
	low := strings.ToLower(run)
	for _, t := range tokens {
		if strings.Contains(low, t) {
			return t
		}
	}
	return ""
}

// hostInstallPatterns are command prefixes that install packages globally / system-wide.
// Reproduced locally they would mutate the developer's machine (and several need sudo,
// which would hang on an interactive password prompt), so steps that run them are skipped.
var hostInstallPatterns = []string{
	"apt-get install", "apt-get update", "apt-get upgrade",
	"apt install", "apt update", "apt upgrade",
	"brew install", "brew bundle",
	"dnf install", "yum install", "zypper install",
	"apk add", "pacman -S", "snap install",
	"npm install -g", "npm install --global", "npm i -g", "npm i --global",
	"pnpm add -g", "pnpm install -g", "yarn global add",
	"gem install",
	"pip install --global", "pip3 install --global",
}

// hostMutationToken returns a short description of the first host-mutating or
// privilege-escalating command found in a run script, or "". Such commands run cleanly on
// an ephemeral CI runner but, reproduced on a developer's machine, would clobber global
// state, trigger an interactive sudo prompt (hanging the run), or install packages
// system-wide. Because ci-parity auto-executes the steps it extracts, these are skipped
// (reported, never run) rather than reproduced.
func hostMutationToken(run string) string {
	if containsCommand(run, "sudo") {
		return "sudo"
	}
	for _, t := range []string{"git config --global", "git config --system"} {
		if strings.Contains(run, t) {
			return t
		}
	}
	for _, p := range hostInstallPatterns {
		if containsCommand(run, p) {
			return p
		}
	}
	return ""
}

// containsCommand reports whether cmd appears in run as a command invocation — at the
// start of the script or right after a shell separator, and ending on a word boundary —
// so it is not matched inside an unrelated word, path, or string literal.
func containsCommand(run, cmd string) bool {
	for idx := 0; ; {
		i := strings.Index(run[idx:], cmd)
		if i < 0 {
			return false
		}
		pos := idx + i
		before := pos == 0
		if !before {
			switch run[pos-1] {
			case ' ', '\t', '\n', '\r', ';', '&', '|', '(', '{':
				before = true
			}
		}
		end := pos + len(cmd)
		after := end >= len(run)
		if !after {
			switch run[end] {
			case ' ', '\t', '\n', '\r', ';':
				after = true
			}
		}
		if before && after {
			return true
		}
		idx = pos + 1
	}
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

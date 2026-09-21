// Package brieflint checks a task brief for the defects that have actually cost dispatch
// cycles, BEFORE the brief is stored on a task.
//
// A stored brief is a snapshot: `ttorch task add --brief-file` copies the file's contents
// into the task, so editing the file afterwards reaches nobody. A defective brief is
// therefore discovered only once a worker has already acted on it. Every rule here exists
// because a brief lacking it produced a concrete failure — work on the wrong base, a worker
// hunting a file that was never on its branch, a worker fixing exactly the stated count and
// stopping, a worker stalling on a blanket prohibition, a worker guessing at conventions it
// was never given.
//
// Two properties are load-bearing:
//
//   - A check that CANNOT be evaluated (the remote is unreachable, no ref to resolve a
//     citation against) is reported as StatusIndeterminate and never as a pass. A gate that
//     exits 0 when it could not run is a gate that can never fail.
//   - One run reports EVERY violation, not the first. Fixing a brief one lint round at a
//     time is slow.
package brieflint

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RuleID names a rule in output, in a project's disable list, and in tests.
type RuleID string

const (
	// RuleTargetBranch — the brief must declare its target branch as <remote>/<branch>,
	// and that branch must exist on the remote.
	RuleTargetBranch RuleID = "target-branch"
	// RuleFilePaths — every file path the brief cites must exist at the ref the citation
	// is about (see Options.Ref / Options.CitationsRef).
	RuleFilePaths RuleID = "file-paths"
	// RuleHardCounts — a brief stating a hard count ("there are 21 occurrences") must also
	// tell the worker to verify it and report their own number.
	RuleHardCounts RuleID = "hard-counts"
	// RuleProhibition — a blanket prohibition ("do NOT push") must be replaced by the
	// invariant it protects plus the allowed end state.
	RuleProhibition RuleID = "prohibition"
	// RuleStandards — the brief must point at the standards the project expects. What
	// counts as that pointer is per-project configuration (see Config.Standards).
	RuleStandards RuleID = "standards"
	// RuleConfig is not a brief rule: it carries defects in the project's own brief-lint
	// configuration, which are reported as indeterminate rather than silently ignored.
	RuleConfig RuleID = "config"
)

// Status is the outcome of one check.
type Status string

const (
	// StatusFail is a rule the brief violates.
	StatusFail Status = "FAIL"
	// StatusIndeterminate is a rule that could NOT be evaluated — the remote was
	// unreachable, a ref did not resolve, no ref was supplied for a file:line citation.
	// It is deliberately distinct from a pass: an unevaluated check must never read as green.
	StatusIndeterminate Status = "CANNOT-EVALUATE"
)

// Finding is one violation or one unevaluated check.
type Finding struct {
	Rule   RuleID
	Status Status
	// Detail says what is wrong and what would fix it.
	Detail string
	// Quote is the offending text, verbatim from the brief. Empty when the defect is an
	// ABSENCE (nothing to quote) or belongs to the project's configuration.
	Quote string
	// Line is the 1-based line of Quote in the brief; 0 when the finding is not anchored
	// to a line.
	Line int
}

// Outcome is a whole run's verdict, which the caller maps to an exit status.
type Outcome string

const (
	// OutcomePass — every enabled rule was evaluated and passed.
	OutcomePass Outcome = "pass"
	// OutcomeFail — at least one rule was violated.
	OutcomeFail Outcome = "fail"
	// OutcomeIndeterminate — nothing was violated, but at least one check could not be
	// evaluated. Reported distinctly from a pass so a check that could not run cannot be
	// mistaken for one that ran and passed.
	OutcomeIndeterminate Outcome = "cannot-evaluate"
)

// Report is one lint run.
type Report struct {
	// Findings holds every violation and every unevaluated check, in rule order.
	Findings []Finding
	// Notes are visible, non-blocking facts about the run: which refs citations were
	// resolved against, which rules a project disabled, what was deliberately not checked.
	// A disabled rule or an unchecked citation is surfaced here rather than passing silently.
	Notes []string
	// evaluated is how many rules ran (see Evaluated).
	evaluated int
}

// Violations counts the StatusFail findings.
func (r Report) Violations() int { return r.count(StatusFail) }

// Indeterminates counts the checks that could not be evaluated.
func (r Report) Indeterminates() int { return r.count(StatusIndeterminate) }

func (r Report) count(s Status) int {
	n := 0
	for _, f := range r.Findings {
		if f.Status == s {
			n++
		}
	}
	return n
}

// Outcome reduces the report to a verdict. A real violation outranks an unevaluated check:
// a run with both is a fail, because there is already something to fix.
func (r Report) Outcome() Outcome {
	switch {
	case r.Violations() > 0:
		return OutcomeFail
	case r.Indeterminates() > 0:
		return OutcomeIndeterminate
	default:
		return OutcomePass
	}
}

// Evaluated is how many rules actually ran: the rule set less whatever the project
// disabled. Every summary states it against Total, so no run can claim more coverage than
// it had.
func (r Report) Evaluated() int { return r.evaluated }

// Total is how many rules exist, disabled ones included.
func (r Report) Total() int { return len(rules) }

// Options describes what a single run may consult. The zero value is valid: it lints the
// brief's text alone and reports every ref-dependent check as indeterminate rather than
// quietly skipping it.
type Options struct {
	// Repo is the repository refs are resolved in. Empty means no repository is available,
	// which makes every ref-dependent check indeterminate.
	Repo string
	// Remote is the remote a declared target branch must exist on, and the prefix a brief
	// declares its target with. Empty means "origin".
	Remote string
	// Ref is the ref a cited path (one WITHOUT a line number) must exist at — the base the
	// work targets. Empty means the target branch the brief itself declares.
	Ref string
	// CitationsRef is the ref a file:line citation is resolved against — the commit the
	// citation is ABOUT, which is normally a worker's HEAD and not the base. Naming it makes
	// the answer authoritative: a citation that does not resolve there is a violation.
	//
	// Empty means the citation is resolved against the base ref instead (Ref, or the target
	// the brief declares), because pointing a worker at a line of existing code is the most
	// ordinary thing a brief does and must not require a flag. A citation that resolves
	// there passes; one that does not is reported as INDETERMINATE rather than failed,
	// because on the base a miss is ambiguous — a gate finding legitimately cites a line
	// that exists at the reviewed commit and is past end-of-file on the base, and calling
	// that a violation is the false positive this rule exists to avoid.
	CitationsRef string

	// Offline skips the rules that reach the network, and only those. Without it the
	// choice on an unreachable remote was CANNOT-EVALUATE or --no-brief-lint, which skips
	// all five rules to get past one. A skipped rule is counted as not evaluated and named
	// in the notes, so an offline run reports reduced coverage rather than a pass.
	Offline bool
	// Config is the project's per-rule configuration (see LoadConfig).
	Config Config
	// Budget bounds the git work ONE run may do, in aggregate. A brief is untrusted input
	// — pasted from an issue, or written by an agent — and two rules query git once per
	// item they find in it, one of them over the network. Without an aggregate bound a
	// brief naming N refs stalls its caller for N times the per-call timeout, which puts
	// the caller (every `ttorch task add`) at the mercy of the text it was handed. Zero
	// means defaultBudget. Whatever the budget does not cover is reported as unevaluable,
	// naming what went unchecked — never passed over in silence.
	//
	// The deadline is enforced, not merely declared: see gitCommand, which kills the child's
	// whole process group and caps the wait on pipes a fork might still hold. Killing git
	// alone leaves the ssh it forked holding the output pipe, and the caller waits
	// regardless of any deadline.
	Budget time.Duration
	// git runs a git command in Repo. Nil means the real git (see gitRun); tests substitute.
	git gitFunc
}

func (o Options) remote() string {
	if o.Remote == "" {
		return "origin"
	}
	return o.Remote
}

// defaultBudget is the aggregate git budget for one run when Options.Budget is unset. It
// is generous for an honest brief (one or two refs and a handful of citations) and short
// enough that a caller is never held for long by a brief that names hundreds.
const defaultBudget = 45 * time.Second

// rule is one check. Splitting the registry from the checks keeps the disable/order/report
// plumbing in one place and each rule a pure function of the brief plus the options.
type rule struct {
	id  RuleID
	run func(context.Context, *brief, Options) ([]Finding, []string)
	// network is set on the one rule that leaves the machine. Options.Offline skips those
	// and only those; every other rule reads the local repository.
	network bool
}

// rules is the rule set, in report order.
var rules = []rule{
	{id: RuleTargetBranch, run: checkTargetBranch, network: true},
	{id: RuleFilePaths, run: checkFilePaths},
	{id: RuleHardCounts, run: checkHardCounts},
	{id: RuleProhibition, run: checkProhibition},
	{id: RuleStandards, run: checkStandards},
}

// Rules returns every rule id, for help text and for validating a disable list.
func Rules() []RuleID {
	ids := make([]RuleID, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.id)
	}
	return ids
}

// ValidRule reports whether id names a real rule.
func ValidRule(id RuleID) bool {
	for _, r := range rules {
		if r.id == id {
			return true
		}
	}
	return false
}

// Lint runs every enabled rule over text and returns one report carrying all of them.
// It never stops at the first violation.
func Lint(text string, opt Options) Report {
	if opt.git == nil {
		opt.git = gitRun
	}
	budget := opt.Budget
	if budget <= 0 {
		budget = defaultBudget
	}
	// One deadline for the whole run, shared by every git call (each still capped
	// individually by gitTimeout). A rule that runs out of it reports what it could not
	// check.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	b := parse(text)
	var rep Report
	if src := opt.Config.Source; src != "" {
		rep.Notes = append(rep.Notes, "config: "+src+" ("+opt.Config.describe()+")")
	} else {
		rep.Notes = append(rep.Notes, "config: none found ("+opt.Config.describe()+")")
	}
	// A project naming an unknown rule in its disable list is a configuration defect, not a
	// no-op: the rule it meant to disable is still running, or the one it meant to keep is
	// misspelled. Report it as unevaluable so it cannot pass unnoticed.
	for _, unknown := range opt.Config.UnknownDisabled {
		rep.Findings = append(rep.Findings, Finding{
			Rule:   RuleConfig,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("brief-lint-disable names unknown rule %q; known rules: %s", unknown, joinRules(Rules())),
		})
	}
	for _, r := range rules {
		switch {
		case opt.Config.Disabled[r.id]:
			// Visible, never silent: an override the reader can see is an override they can
			// question.
			rep.Notes = append(rep.Notes, fmt.Sprintf("rule %s: DISABLED by project config (%s)", r.id, opt.Config.disableSource()))
			continue
		case opt.Offline && r.network:
			rep.Notes = append(rep.Notes, fmt.Sprintf("rule %s: SKIPPED for --offline, so the remote was never asked", r.id))
			continue
		}
		findings, notes := r.run(ctx, b, opt)
		rep.Findings = append(rep.Findings, findings...)
		rep.Notes = append(rep.Notes, notes...)
		rep.evaluated++
	}
	// A run that evaluated nothing is not a pass. The project under review supplies this
	// configuration, ttorch itself writes AGENTS.md through learnings promotion, and any
	// worker can commit it, so the brief being reviewed can turn off its own reviewer. One
	// line doing that must read as "nothing was checked", not as "checked and fine".
	if rep.evaluated == 0 {
		rep.Findings = append(rep.Findings, Finding{
			Rule:   RuleConfig,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("nothing was checked: none of the %d rules ran (%s%s). A lint that evaluates no rule cannot report a pass", len(rules), opt.Config.disableSource(), offlineSuffix(opt.Offline)),
		})
	}
	return rep
}

// brief is a parsed brief: the raw text plus the line offsets needed to anchor a quote to
// a line number.
type brief struct {
	raw string
	// lower is the whole brief lowercased, for the brief-wide signal scans.
	lower string
	lines []string
	// starts[i] is the byte offset of line i+1 in raw.
	starts []int
}

func parse(text string) *brief {
	b := &brief{raw: text, lower: strings.ToLower(text)}
	off := 0
	for _, line := range strings.Split(text, "\n") {
		b.lines = append(b.lines, line)
		b.starts = append(b.starts, off)
		off += len(line) + 1
	}
	return b
}

// lineAt returns the 1-based line containing byte offset off.
func (b *brief) lineAt(off int) int {
	i := sort.SearchInts(b.starts, off+1) // first start strictly greater than off
	if i < 1 {
		return 1
	}
	return i
}

func joinRules(ids []RuleID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, ", ")
}

// offlineSuffix names --offline in the nothing-was-checked detail when it is the reason, or
// part of it, so the reader is not sent to AGENTS.md to look for a rule that is enabled.
func offlineSuffix(offline bool) string {
	if offline {
		return ", plus --offline"
	}
	return ""
}

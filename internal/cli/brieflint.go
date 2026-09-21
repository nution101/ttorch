package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nution101/ttorch/internal/brieflint"
)

const briefLintUsage = `usage: ttorch brief-lint <brief-file> [--repo <dir>] [--remote <name>] [--ref <rev>] [--citations-ref <rev>]`

// Exit statuses for `ttorch brief-lint`. The three outcomes are deliberately distinct:
// a check that could NOT run must never be indistinguishable from one that ran and passed.
// That class of bug — a step exiting 0 on failure, so it could never fail — has bitten this
// project before.
const (
	// exitLintViolation: the brief violates at least one enabled rule.
	exitLintViolation = 1
	// exitLintUsage: the lint itself could not be started (bad flags, unreadable brief).
	exitLintUsage = 2
	// exitLintIndeterminate: nothing was violated, but at least one check could not be
	// evaluated (an unreachable remote, a ref that does not resolve, a file:line citation
	// with no ref to resolve it against).
	exitLintIndeterminate = 3
)

// lintError carries a brief-lint outcome's exit status out to Main. Its message is what the
// user sees on stderr after the report itself.
type lintError struct {
	msg  string
	code int
}

func (e lintError) Error() string { return e.msg }

// exitStatus implements the exitCoder interface run() consults. It is unexported on
// purpose: see exitCoder.
func (e lintError) exitStatus() int { return e.code }

// cmdBriefLint checks a task brief before it is stored on a task, standalone.
//
// A stored brief is a snapshot: `ttorch task add --brief-file` copies the file's contents
// into the task, so editing the file afterwards reaches nobody and a defective brief is
// discovered only once a worker has acted on it. This is the same check `task add` runs,
// exposed separately so a brief can be fixed before it is added.
func cmdBriefLint(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return lintError{briefLintUsage, exitLintUsage}
	}
	path := args[0]
	fs := flag.NewFlagSet("brief-lint", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository the brief targets; refs and cited paths are resolved here")
	remote := fs.String("remote", "origin", "remote the declared target branch must exist on")
	ref := fs.String("ref", "", "ref a cited path must exist at (default: the target branch the brief declares)")
	citationsRef := fs.String("citations-ref", "", "ref a file:line citation is resolved against — the commit the citation was read at, such as a worker's HEAD or a reviewed sha (default: the base ref, where an unresolved citation is reported as unevaluable rather than failed, because the line may exist at the commit it was read at)")
	if err := fs.Parse(args[1:]); err != nil {
		return lintError{err.Error(), exitLintUsage}
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return lintError{fmt.Sprintf("brief-lint: reading %s: %v", path, err), exitLintUsage}
	}
	if strings.TrimSpace(string(text)) == "" {
		return lintError{fmt.Sprintf("brief-lint: %s is empty", path), exitLintUsage}
	}
	rep := brieflint.Lint(string(text), brieflint.Options{
		Repo:         *repo,
		Remote:       *remote,
		Ref:          *ref,
		CitationsRef: *citationsRef,
		Config:       brieflint.LoadConfig(*repo),
	})
	printBriefLint(os.Stdout, path, rep)
	return briefLintOutcome(rep, "brief-lint")
}

// briefLintOutcome maps a report to the error (and so the exit status) the caller returns.
// who names the caller in the message, so a failure from `task add` reads as one.
func briefLintOutcome(rep brieflint.Report, who string) error {
	switch rep.Outcome() {
	case brieflint.OutcomeFail:
		return lintError{fmt.Sprintf("%s: %s", who, describeLintCounts(rep)), exitLintViolation}
	case brieflint.OutcomeIndeterminate:
		return lintError{fmt.Sprintf("%s: %s — resolve them (or disable the rule in AGENTS.md) rather than treating this as a pass", who, describeLintCounts(rep)), exitLintIndeterminate}
	default:
		return nil
	}
}

func describeLintCounts(rep brieflint.Report) string {
	var parts []string
	if n := rep.Violations(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d violation(s)", n))
	}
	if n := rep.Indeterminates(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d check(s) could not be evaluated", n))
	}
	return strings.Join(parts, ", ")
}

// printBriefLint renders one run: the notes first (what the rules were judged against, and
// every rule a project disabled — an override is surfaced, never silent), then every
// finding with its rule named and its offending text quoted. All of them, in one pass:
// fixing a brief one lint round at a time is slow.
func printBriefLint(w io.Writer, path string, rep brieflint.Report) {
	fmt.Fprintf(w, "brief-lint: %s\n", path)
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  · %s\n", n)
	}
	if len(rep.Findings) == 0 {
		// Count what actually RAN, so a run with rules disabled cannot claim the whole set
		// passed.
		fmt.Fprintf(w, "\nall %d enabled rules passed%s\n", rep.Evaluated(), disabledSuffix(rep))
		return
	}
	fmt.Fprintln(w)
	for _, f := range rep.Findings {
		where := ""
		if f.Line > 0 {
			where = fmt.Sprintf(" (line %d)", f.Line)
		}
		fmt.Fprintf(w, "  [%s] %s%s\n", f.Status, f.Rule, where)
		fmt.Fprintf(w, "        %s\n", f.Detail)
		if f.Quote != "" {
			fmt.Fprintf(w, "        offending text: %q\n", f.Quote)
		}
	}
	fmt.Fprintf(w, "\n%s\n", describeLintCounts(rep))
}

// disabledSuffix names how many rules a project turned off, for the summary line.
func disabledSuffix(rep brieflint.Report) string {
	n := len(brieflint.Rules()) - rep.Evaluated()
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d disabled)", n)
}

// lintBriefForAdd runs the same rules in front of `ttorch task add`, where the cost of a
// defective brief is actually paid: the brief is copied into the task at add time, so this
// is the last moment a fix reaches the worker. Both a violation and an unevaluable check
// stop the add — an unevaluable check is not a pass — and both name the escape hatches.
func lintBriefForAdd(text, repo, citationsRef string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	rep := brieflint.Lint(text, brieflint.Options{
		Repo:         repo,
		CitationsRef: citationsRef,
		Config:       brieflint.LoadConfig(repo),
	})
	printBriefLint(os.Stdout, "the supplied brief", rep)
	err := briefLintOutcome(rep, "task add")
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: nothing was added. Fix the brief, or re-run with --no-brief-lint to add it as written.\n")
	}
	return err
}

// errNoBriefLintWithoutBrief keeps --no-brief-lint from reading as meaningful on an add that
// supplies no brief at all (where there is nothing to lint).
var errNoBriefLintWithoutBrief = errors.New("task add: --no-brief-lint needs a brief (--brief / --brief-file) to skip linting")

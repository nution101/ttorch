package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nution101/ttorch/internal/ciparity"
	"github.com/nution101/ttorch/internal/validate"
)

// cmdCIParity reproduces a repository's actual CI run-steps locally, so that "green here"
// matches "green in CI". It extracts the shell `run:` steps from .github/workflows, reports
// exactly what it will and will not reproduce (and why — nothing is dropped silently), then
// runs the reproducible steps through the same per-step PASS/FAIL path as `validate`.
//
// It operates on a directory (default: the current directory / the worker's worktree)
// rather than a task id, so it is self-contained and does not touch the orchestrator.
func cmdCIParity(args []string) error {
	fs := flag.NewFlagSet("ci-parity", flag.ContinueOnError)
	list := fs.Bool("list", false, "show the extracted/skipped plan and exit without running anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if rest := fs.Args(); len(rest) > 0 {
		dir = rest[0]
	}

	plan, err := ciparity.Extract(dir)
	if err != nil {
		return err
	}
	printCIParityPlan(os.Stdout, plan)
	if *list {
		return nil
	}
	if len(plan.Steps) == 0 {
		fmt.Println("\nno reproducible CI steps found")
		return nil
	}

	fmt.Printf("\nrunning %d CI step(s):\n", len(plan.Steps))
	results := validate.Run(dir, validate.CIParitySteps(plan.Steps))
	failed := printResults(os.Stdout, results)
	if failed > 0 {
		return fmt.Errorf("%d of %d CI-parity steps failed", failed, len(results))
	}
	fmt.Printf("all %d CI-parity steps passed\n", len(results))
	return nil
}

// printCIParityPlan renders the extraction plan: workflows parsed, reproducible steps,
// and every skip with its reason (so coverage is never silently truncated).
func printCIParityPlan(w io.Writer, p *ciparity.Plan) {
	if len(p.Files) == 0 {
		fmt.Fprintln(w, "no GitHub Actions workflows found under .github/workflows")
		return
	}
	fmt.Fprintf(w, "workflows: %s\n", strings.Join(p.Files, ", "))

	if len(p.ParseErrors) > 0 {
		files := make([]string, 0, len(p.ParseErrors))
		for f := range p.ParseErrors {
			files = append(files, f)
		}
		sort.Strings(files)
		fmt.Fprintf(w, "parse errors (%d):\n", len(files))
		for _, f := range files {
			fmt.Fprintf(w, "  ! %s: %s\n", f, p.ParseErrors[f])
		}
	}

	fmt.Fprintf(w, "reproducible steps (%d):\n", len(p.Steps))
	for _, s := range p.Steps {
		fmt.Fprintf(w, "  + %s/%s: %s\n", s.Workflow, s.Job, s.Name)
		if s.Note != "" {
			fmt.Fprintf(w, "      note: %s\n", s.Note)
		}
	}

	fmt.Fprintf(w, "skipped (%d):\n", len(p.Skipped))
	for _, s := range p.Skipped {
		loc := s.Workflow
		if s.Job != "" {
			loc += "/" + s.Job
		}
		if s.Name != "" {
			loc += ": " + s.Name
		}
		fmt.Fprintf(w, "  - %s — %s\n", loc, s.Reason)
	}
}

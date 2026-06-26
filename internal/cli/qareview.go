package cli

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/nution101/ttorch/internal/review"
)

// cmdQAReview runs the standalone, advisory test-adequacy (QA) audit. It brackets the
// manager's dispatch of the ttorch-reviewer-qa agent, mirroring `ttorch trust prep|record`
// but folding only the QA dimension:
//
//	ttorch qa-review prep <id>     materialize the reviewer's inputs (reuses trust prep)
//	  → manager runs the ttorch-reviewer-qa agent, which writes qa.json
//	ttorch qa-review record <id>   fold qa.json into an advisory verdict
//	ttorch qa-review show <id>     show the latest advisory verdict
//
// The verdict is ADVISORY: it surfaces findings to the manager but never mints an approval,
// never touches the trust gate, and never blocks a merge. The trusted-mode gate (correctness
// / scope / security) is unchanged and does not include QA.
func cmdQAReview(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch qa-review prep|record|show <task-id> [flags]")
	}
	sub, id := args[0], args[1]
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	switch sub {
	case "prep":
		// The QA reviewer reads exactly the inputs trust prep materializes (diff.patch /
		// brief.md / validate.json / head.txt), so reuse it rather than duplicate the
		// materialization.
		dir, err := m.TrustPrep(id)
		if err != nil {
			return err
		}
		fmt.Printf("prepared qa-review inputs for %s in %s\n", id, dir)
		fmt.Printf("  run the QA reviewer (ttorch-reviewer-qa) over that dir, then: ttorch qa-review record %s\n", id)
		return nil
	case "record":
		fs := flag.NewFlagSet("qa-review record", flag.ContinueOnError)
		sha := fs.String("sha", "", "commit the review covers (default: the worker's current HEAD)")
		ttl := fs.Duration("ttl", 30*time.Minute, "how long the advisory result stays valid")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		v, err := m.QAReview(id, *sha, *ttl)
		if err != nil {
			return err
		}
		printQAVerdict(id, v)
		return nil
	case "show":
		v, ok := m.QAReviewShow(id)
		if !ok {
			fmt.Printf("no qa audit recorded for %s — run 'ttorch qa-review prep %s', review, then 'ttorch qa-review record %s'\n", id, id, id)
			return nil
		}
		printQAVerdict(id, v)
		return nil
	default:
		return errors.New("usage: ttorch qa-review prep|record|show <task-id> [flags]")
	}
}

// printQAVerdict surfaces an advisory QA verdict to the manager: a one-line headline making
// the advisory (non-blocking) nature explicit, then every finding (low/medium included), most
// severe first. A "block" verdict here surfaces a test-adequacy concern for the lead but never
// auto-blocks delivery.
func printQAVerdict(id string, v review.Verdict) {
	if v.Overall == review.Pass {
		fmt.Printf("qa audit for %s: PASS — advisory, does not block delivery\n", id)
	} else {
		fmt.Printf("qa audit for %s: BLOCK — advisory; surface to the lead (does not auto-block delivery)\n", id)
	}
	lines := review.Describe(v)
	if len(lines) == 0 {
		fmt.Println("  no qa findings")
		return
	}
	fmt.Printf("  %d finding(s):\n", len(lines))
	for _, l := range lines {
		fmt.Printf("    %s\n", l)
	}
}

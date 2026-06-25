package cli

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/nution101/ttorch/internal/review"
)

// cmdSecurityReview runs the standalone, advisory security-everywhere audit, which is
// available in EVERY delivery mode (not just trusted). It brackets the manager's
// dispatch of the ttorch-reviewer-security agent, mirroring `ttorch trust prep|record`
// but folding only the security dimension:
//
//	ttorch security-review prep <id>     materialize the reviewer's inputs (reuses trust prep)
//	  → manager runs the ttorch-reviewer-security agent, which writes security.json
//	ttorch security-review record <id>   fold security.json into an advisory verdict
//	ttorch security-review show <id>     show the latest advisory verdict
//
// The verdict is ADVISORY: it surfaces findings to the manager but never mints an
// approval, never touches the trust gate, and never blocks a merge. The trusted-mode
// gate (which already runs all three reviewers as a hard block) is unchanged.
func cmdSecurityReview(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch security-review prep|record|show <task-id> [flags]")
	}
	sub, id := args[0], args[1]
	switch sub {
	case "prep":
		// The security reviewer reads exactly the inputs trust prep materializes
		// (diff.patch / brief.md / validate.json / head.txt), so reuse it rather than
		// duplicate the materialization.
		dir, err := mgr().TrustPrep(id)
		if err != nil {
			return err
		}
		fmt.Printf("prepared security-review inputs for %s in %s\n", id, dir)
		fmt.Printf("  run the security reviewer (ttorch-reviewer-security) over that dir, then: ttorch security-review record %s\n", id)
		return nil
	case "record":
		fs := flag.NewFlagSet("security-review record", flag.ContinueOnError)
		sha := fs.String("sha", "", "commit the review covers (default: the worker's current HEAD)")
		ttl := fs.Duration("ttl", 30*time.Minute, "how long the advisory result stays valid")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		v, err := mgr().SecurityReview(id, *sha, *ttl)
		if err != nil {
			return err
		}
		printSecurityVerdict(id, v)
		return nil
	case "show":
		v, ok := mgr().SecurityReviewShow(id)
		if !ok {
			fmt.Printf("no security audit recorded for %s — run 'ttorch security-review prep %s', review, then 'ttorch security-review record %s'\n", id, id, id)
			return nil
		}
		printSecurityVerdict(id, v)
		return nil
	default:
		return errors.New("usage: ttorch security-review prep|record|show <task-id> [flags]")
	}
}

// printSecurityVerdict surfaces an advisory security verdict to the manager: a one-line
// headline making the advisory (non-blocking) nature explicit, then every finding
// (low/medium included), most severe first. A "block" verdict here surfaces a concern
// for the lead but, outside the trusted gate, does not auto-block delivery.
func printSecurityVerdict(id string, v review.Verdict) {
	if v.Overall == review.Pass {
		fmt.Printf("security audit for %s: PASS — advisory, does not block delivery\n", id)
	} else {
		fmt.Printf("security audit for %s: BLOCK — advisory; surface to the lead (does not auto-block delivery outside the trusted gate)\n", id)
	}
	lines := review.Describe(v)
	if len(lines) == 0 {
		fmt.Println("  no security findings")
		return
	}
	fmt.Printf("  %d finding(s):\n", len(lines))
	for _, l := range lines {
		fmt.Printf("    %s\n", l)
	}
}

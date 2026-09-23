package cli

import (
	"context"
	"flag"
	"os"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/watch"
)

// cmdInbox prints the manager's unread actionable updates and advances the watermark past
// them. It is what the manager runs when the scheduler's watch loop wakes it, and what it
// runs to catch up when the lead returns. Reading the inbox puts the manager back in the
// loop, so it clears the awaiting-lead backstop exactly as arming `ttorch watch` does;
// without that, a manager that stopped arming watch would stay marked as awaiting the lead
// and the scheduler would never wake it again.
func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := db.Open(paths.Default().StateDB())
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SetAwaitingLead(ctx, false); err != nil {
		return err
	}
	_, err = watch.ReadInbox(ctx, store, os.Stdout)
	return err
}

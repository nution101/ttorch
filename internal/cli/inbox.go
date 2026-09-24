package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/watch"
)

// refuseFromWorker returns an error when this process runs inside a worker's context (the
// $TTORCH_TASK_ID or .ttorch/task signal checkApproveCaller uses), for the commands that read
// or change the manager's own loop state: `ttorch inbox` and `ttorch watch` advance the
// manager's watermark and clear awaiting-lead, and `ttorch await-lead` sets or clears it. A
// worker running one of them would mark other tasks' updates read before the manager saw
// them, or re-enable the scheduler's wake while the lead is deciding. Like the approve guard,
// this stops the accidental and prompt-injected cases; it is not a boundary against a worker
// that deliberately strips its own identity.
func refuseFromWorker(command string) error {
	if signal := workerContextSignal(); signal != "" {
		return fmt.Errorf("refusing to run 'ttorch %s' from inside a worker context (%s): it reads and changes the manager's own state. Only the manager runs it, from the manager tab", command, signal)
	}
	return nil
}

// cmdInbox prints the manager's unread actionable updates and advances the watermark past
// them. It is what the manager runs when the scheduler's watch loop wakes it, and what it
// runs to catch up when the lead returns. Reading the inbox puts the manager back in the
// loop, so it clears the awaiting-lead backstop exactly as arming `ttorch watch` does;
// without that, a manager that stopped arming watch would stay marked as awaiting the lead
// and the scheduler would never wake it again. It refuses to run from a worker context
// before touching any state.
func cmdInbox(args []string) error {
	if err := refuseFromWorker("inbox"); err != nil {
		return err
	}
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

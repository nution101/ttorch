package watch

import (
	"context"
	"fmt"
	"io"

	"github.com/nution101/ttorch/internal/db"
)

// The manager's inbox is not a separate store: it is every actionable event above
// manager.watch_watermark, the same rows `ttorch watch` surfaces. Anything recorded there
// stays unread until a consumer advances the watermark past it, so a wake that is missed,
// a scheduler restart, or a manager that was busy loses nothing. `ttorch inbox` is the
// one command the manager runs when the scheduler wakes it.

// InboxResult reports what one ReadInbox call consumed.
type InboxResult struct {
	Since     int64      // the watermark the read started from
	Watermark int64      // the watermark afterwards (== Since when nothing was unread)
	Batch     []db.Event // the unread updates, one per entity, latest wins
}

// ReadInbox takes every unread actionable update, prints it to out in the watcher's
// format, and advances the watermark past it. The read and the advance are one
// transaction (db.ConsumeActionable), so racing an armed `ttorch watch` hands each update
// to exactly one of them, and running it again with nothing new prints an empty inbox and
// changes nothing.
func ReadInbox(ctx context.Context, store *db.Store, out io.Writer) (InboxResult, error) {
	c, err := store.ConsumeActionable(ctx, 0)
	if err != nil {
		return InboxResult{}, err
	}
	res := InboxResult{Since: c.Since, Watermark: c.Watermark}
	if len(c.Events) == 0 {
		fmt.Fprintf(out, "ttorch inbox: no unread updates (watermark #%d)\n", c.Watermark)
		fmt.Fprintf(out, "INBOX_WATERMARK=%d\n", c.Watermark)
		return res, nil
	}
	res.Batch = dedupeByEntity(c.Events)
	fmt.Fprintf(out, "ttorch inbox: %d unread update(s) since #%d (now #%d)\n", len(res.Batch), c.Since, c.Watermark)
	for _, e := range res.Batch {
		fmt.Fprintln(out, "  "+formatEventLine(e))
	}
	fmt.Fprintln(out, "next: ttorch tasks --status done,blocked,needs_input ; then gate / answer / surface")
	fmt.Fprintf(out, "INBOX_WATERMARK=%d\n", c.Watermark)
	return res, nil
}

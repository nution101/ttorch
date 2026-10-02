package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/nution101/ttorch/internal/board"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/peer"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/singleton"
)

// cmdSummary prints the coordinator's state in one pass: task counts, live workers, the
// decisions waiting on the lead (ids and ages, not their text), the open escalation count and
// highest open id, scheduler and manager health, and each repo's mode and free slots. --json
// prints it as a versioned object (peer.SchemaVersion) for scripts.
//
// The summary itself issues only reads, but it is not a read-only command. It opens the store
// through mgr() like every other command, and orchestrator.New runs pending migrations,
// imports legacy state and seeds default branches before anything is read, and the command
// then syncs approval escalations (db.SyncApprovalEscalations) so its escalation count agrees
// with `ttorch decisions`. Those writes are idempotent, so polling is harmless today; a caller
// that must not write (the peer serve verb) needs its own read-only open, and will count
// approvals nobody has synced yet as not open.
func cmdSummary(args []string) error {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the summary as one JSON object")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: ttorch summary [--json]")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if _, err := m.Store.SyncApprovalEscalations(context.Background()); err != nil {
		return err
	}
	src, err := summarySources(m)
	if err != nil {
		return err
	}
	sum, err := peer.Build(context.Background(), src, time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		return sum.WriteJSON(os.Stdout)
	}
	return sum.WriteText(os.Stdout)
}

// summarySources wires a summary to the reads the other status commands make: the board's
// snapshot from the same config `ttorch board` uses, free slots the way `ttorch status`
// counts them, the manager window the way the watcher checks it, and the daemon lock the way
// `ttorch scheduler status` checks it.
func summarySources(m *orchestrator.Manager) (peer.Sources, error) {
	b, err := board.New(boardConfig(m))
	if err != nil {
		return peer.Sources{}, err
	}
	live, err := m.Status()
	if err != nil {
		return peer.Sources{}, err
	}
	free := freeSlotsByRepo(m.Pool, live)
	return peer.Sources{
		Store: m.Store,
		Board: b,
		Mode:  projectinit.ReadMode,
		FreeSlots: func(repo string) int {
			if n, ok := free[repo]; ok {
				return n
			}
			return m.Pool.FreeSlots(nil) // no live task holds a slot in this repo
		},
		ManagerWindow: func() bool {
			return m.Backend.Available() && m.Backend.WindowExists(m.Session, "manager")
		},
		SchedulerRunning: func() bool { return singleton.Held(m.P.SchedulerPIDFile()) },
	}, nil
}

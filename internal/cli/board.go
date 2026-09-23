package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/board"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/scheduler"
)

// cmdBoard serves the local decisions-and-fleet page until interrupted. The URL it prints
// carries the session's access token, so it goes to stdout once and nowhere else; the
// board's own diagnostics (stderr) never contain it.
func cmdBoard(args []string) error {
	fs := flag.NewFlagSet("board", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: ttorch board")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	s, err := board.New(board.Config{
		Store: m.Store,
		Fleet: m,
		Mode:  projectinit.ReadMode,
		// Read-only: the board lists a task as awaiting approval when it holds no valid
		// approval. It never grants one.
		ApprovalValid:    func(id string) bool { return approval.Valid(m.P.ApprovalFile(id)) },
		SerializeOverlap: scheduler.SerializeOverlapFromEnv(),
		Log:              os.Stderr,
	})
	if err != nil {
		return err
	}
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	fmt.Printf("ttorch board is serving on 127.0.0.1 only. Open:\n\n  %s\n\n", s.URL())
	fmt.Println("The link carries this session's access token; do not share it. Ctrl-C stops the board.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Serve(ctx, ln)
}

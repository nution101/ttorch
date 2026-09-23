package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestCmdInbox_PrintsUnreadAndClearsAwaitingLead: `ttorch inbox` (dispatched through Main)
// prints the unread update, and clears the awaiting-lead backstop the way arming
// `ttorch watch` does, so a manager that stopped arming watch is still woken afterwards.
func TestCmdInbox_PrintsUnreadAndClearsAwaitingLead(t *testing.T) {
	path := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		proj, err := s.UpsertProject(ctx, "/repo/inbox", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTask(ctx, db.Task{ID: "inbox-t1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReportStatus(ctx, "inbox-t1", db.StatusDone, "worker:inbox-t1", "all green"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAwaitingLead(ctx, true); err != nil {
			t.Fatal(err)
		}
	})

	var code int
	out, _ := captureStdout(t, func() error {
		code = Main([]string{"inbox"})
		return nil
	})
	if code != 0 {
		t.Fatalf("ttorch inbox exit = %d, want 0 (output: %q)", code, out)
	}
	if !strings.Contains(out, "task=inbox-t1") || !strings.Contains(out, "all green") {
		t.Fatalf("inbox output missing the done update:\n%s", out)
	}

	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _, err := s.GetManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.AwaitingLead {
		t.Fatal("ttorch inbox must clear awaiting-lead, as arming ttorch watch does")
	}
}

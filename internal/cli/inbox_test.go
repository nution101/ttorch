package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// TestCmdInbox_PrintsUnreadAndClearsAwaitingLead: `ttorch inbox` (dispatched through Main)
// prints the unread update, and clears the awaiting-lead backstop the way arming
// `ttorch watch` does, so a manager that stopped arming watch is still woken afterwards.
func TestCmdInbox_PrintsUnreadAndClearsAwaitingLead(t *testing.T) {
	clearWorkerContext(t) // this suite may itself run inside a worker's worktree
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
	if !strings.Contains(out, `task="inbox-t1"`) || !strings.Contains(out, `worker text: "all green"`) {
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

// TestCmdSchedulerOnceWatch: `ttorch scheduler --once --watch` runs one watch-loop sweep. With
// an unread update and no manager window it records only and reports that no wake was typed.
// The tmux session is pinned to a name that cannot exist, so the sweep can never reach a real
// manager window on the machine running the tests, and the daemon's skill install is skipped so
// the test never runs npx.
func TestCmdSchedulerOnceWatch(t *testing.T) {
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-test-no-such-session-watch")
	t.Setenv("TTORCH_SKIP_SKILL_INSTALL", "1")
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		proj, err := s.UpsertProject(ctx, "/repo/watch", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTask(ctx, db.Task{ID: "watch-t1", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReportStatus(ctx, "watch-t1", db.StatusBlocked, "worker:watch-t1", "need a decision"); err != nil {
			t.Fatal(err)
		}
	})

	out, err := captureStdout(t, func() error {
		return cmdScheduler([]string{"--once", "--dispatch=false", "--watch"})
	})
	if err != nil {
		t.Fatalf("scheduler --once --watch: %v\n%s", err, out)
	}
	if !strings.Contains(out, "scheduler: watch: 1 unread update(s), woke manager: false") {
		t.Fatalf("expected one sweep reporting the unread update and no wake, got:\n%s", out)
	}
}

// seedUnreadAwaiting seeds one unread worker update and sets awaiting-lead, and returns the
// store path, the watermark before any read, and a reader for the state afterwards.
func seedUnreadAwaiting(t *testing.T) (string, func() db.Manager) {
	t.Helper()
	path := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		proj, err := s.UpsertProject(ctx, "/repo/guard", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTask(ctx, db.Task{ID: "other-task", ProjectID: proj.ID, Status: db.StatusActive, Kind: db.KindShip}, db.ActorManager); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReportStatus(ctx, "other-task", db.StatusNeedsInput, "worker:other-task", "which schema?"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAwaitingLead(ctx, true); err != nil {
			t.Fatal(err)
		}
	})
	return path, func() db.Manager {
		s, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		m, _, err := s.GetManager(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
}

// TestManagerCommandsRefuseWorkerContext: from a worker's context, `ttorch inbox`,
// `ttorch watch` and `ttorch await-lead` refuse before touching state. The manager's unread
// update stays unread (the watermark does not move) and awaiting-lead stays set, so a worker
// can neither hide another task's needs_input from the manager nor re-enable the scheduler's
// wake while the lead is deciding. Both identity signals are covered: $TTORCH_TASK_ID, and a
// .ttorch/task file above cwd with the variable unset.
func TestManagerCommandsRefuseWorkerContext(t *testing.T) {
	t.Setenv("TTORCH_TMUX_SESSION", "ttorch-test-no-such-session-guard") // never reach a real tmux session
	signals := map[string]func(t *testing.T){
		"env": func(t *testing.T) {
			clearWorkerContext(t)
			t.Setenv("TTORCH_TASK_ID", "w-guard")
		},
		"task file": func(t *testing.T) {
			clearWorkerContext(t)
			root := t.TempDir()
			writeTaskFile(t, root, "w-guard", filepath.Join(root, "state.db"))
			sub := filepath.Join(root, "internal", "deep")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(sub)
		},
	}
	commands := map[string]func() error{
		"inbox":              func() error { return cmdInbox(nil) },
		"watch":              func() error { return cmdWatch([]string{"--timeout", "1ms"}) },
		"await-lead --clear": func() error { return cmdAwaitLead([]string{"--clear"}) },
	}
	for sigName, enter := range signals {
		for cmdName, run := range commands {
			t.Run(sigName+"/"+cmdName, func(t *testing.T) {
				_, state := seedUnreadAwaiting(t)
				enter(t)
				before := state()

				out, err := captureStdout(t, run)
				if err == nil || !strings.Contains(err.Error(), "worker context") || !strings.Contains(err.Error(), "w-guard") {
					t.Fatalf("%s from a worker context: err=%v, want a refusal naming the worker signal", cmdName, err)
				}
				if strings.Contains(out, "which schema?") {
					t.Fatalf("%s printed the manager's updates to a worker:\n%s", cmdName, out)
				}
				after := state()
				if after.WatchWatermark != before.WatchWatermark {
					t.Fatalf("%s moved the watermark %d → %d from a worker context", cmdName, before.WatchWatermark, after.WatchWatermark)
				}
				if !after.AwaitingLead {
					t.Fatalf("%s cleared awaiting-lead from a worker context", cmdName)
				}
			})
		}
	}
}

// TestRenderWatchStandby: `ttorch status` names the holder while the watch loop stands by, and
// prints nothing otherwise.
func TestRenderWatchStandby(t *testing.T) {
	var b strings.Builder
	renderWatchStandby(&b, 4242, true)
	if !strings.Contains(b.String(), "daemon watch: standby, held by pid 4242") {
		t.Fatalf("standby line = %q", b.String())
	}
	b.Reset()
	renderWatchStandby(&b, 0, false)
	if b.String() != "" {
		t.Fatalf("printed %q when not standing by", b.String())
	}
}

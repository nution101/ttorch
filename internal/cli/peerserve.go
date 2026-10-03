package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nution101/ttorch/internal/buildinfo"
	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/peer"
)

const peerUsage = `usage: ttorch peer serve
  serve answers one control request from a parent coordinator. It runs as an ssh forced
  command (command="ttorch peer serve",restrict in authorized_keys): the verb comes from
  SSH_ORIGINAL_COMMAND and the request is one JSON object on stdin.`

// cmdPeer dispatches `ttorch peer`. serve is its only subcommand so far. It returns the exit
// status itself, because serve's status is part of its protocol: 0 for an answered request, 1
// for a refusal, whose JSON is on stdout either way.
func cmdPeer(args []string) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, peerUsage)
		return 2
	}
	if len(args) > 1 {
		// The forced command is exactly `ttorch peer serve`. A verb typed here would be a second
		// way in that the authorized_keys line does not describe.
		fmt.Fprintf(os.Stderr, "ttorch peer serve takes no arguments: the verb comes from SSH_ORIGINAL_COMMAND\n%s\n", peerUsage)
		return 2
	}
	return cmdPeerServe(os.Stdin, os.Stdout, os.Stderr)
}

// cmdPeerServe answers one request (internal/peer/serve.go) against this machine's ttorch home.
func cmdPeerServe(stdin io.Reader, stdout, stderr io.Writer) int {
	command := os.Getenv("SSH_ORIGINAL_COMMAND")
	// Read once. Nothing this process starts (ensure-up can bring up the tmux server, which every
	// later window inherits its environment from) needs the parent's command string.
	_ = os.Unsetenv("SSH_ORIGINAL_COMMAND")
	return peer.Serve(context.Background(), command, stdin, stdout, stderr, workerContextSignal(), peerHost(paths.Default()))
}

// peerHost wires the control verbs to this machine's store and fleet.
//
//   - version and summary read through db.OpenReadOnly: no migration, no legacy import, no
//     default-branch seed, no row written. The summary builds its Manager with NewWithStore for
//     the same reason, which is why it does not go through mgr().
//   - decisions, task-add, goal and answer open the store with db.Open, which migrates as every
//     command does, and nothing else: no legacy import and no seed.
//   - task-add runs addBacklogTask, the core of `ttorch task add`, brief lint included, as the
//     parent coordinator.
//   - ensure-up does what the lead's own `ttorch` does after a reboot, without a terminal.
func peerHost(p paths.Paths) peer.Host {
	return peer.Host{
		ReadStore: func() (*db.Store, error) { return db.OpenReadOnly(p.StateDB()) },
		Store:     func() (*db.Store, error) { return db.Open(p.StateDB()) },
		SummarySources: func(store *db.Store) (peer.Sources, error) {
			m, err := orchestrator.NewWithStore(p, store)
			if err != nil {
				return peer.Sources{}, err
			}
			return summarySources(m)
		},
		AddTask: func(ctx context.Context, store *db.Store, req peer.TaskAdd) (db.TaskAddResult, string, error) {
			return peerAddTask(ctx, store, p, req)
		},
		EnsureUp: func(ctx context.Context) (peer.EnsureUpResult, error) { return peerEnsureUp(ctx, p) },
		Version:  buildinfo.CurrentVersion(),
	}
}

// peerAddTask adds a task a parent handed over, through the same core as `ttorch task add`. The
// lint's report and notes are captured rather than printed, since stdout carries the response.
// A lint refusal is named brief_lint and carries the report.
func peerAddTask(ctx context.Context, store *db.Store, p paths.Paths, req peer.TaskAdd) (db.TaskAddResult, string, error) {
	var report bytes.Buffer
	res, err := addBacklogTask(ctx, store, p, taskAdd{
		ID: req.TaskID, ProjectID: req.ProjectID, Title: req.Title,
		Touches: strings.Join(req.Touches, ","), Brief: req.Brief, Effort: req.Effort, Model: req.Model,
		Actor: db.ActorParent, RequestID: req.RequestID,
	}, &report, &report)
	var le lintError
	if errors.As(err, &le) {
		return db.TaskAddResult{}, report.String(), &peer.Error{Code: peer.CodeBriefLint, Message: le.msg, Detail: report.String()}
	}
	return res, report.String(), err
}

// peerEnsureUp restores the manager and every worker window from saved state and starts the
// scheduler, as `ttorch` would after a reboot, with no terminal to attach. It opens the Manager
// with orchestrator.New, as `ttorch` does. It will not start a fresh manager: with no manager
// recorded there is nothing to restore, and a manager started from an ssh session would run in
// whatever directory that session started in.
func peerEnsureUp(ctx context.Context, p paths.Paths) (peer.EnsureUpResult, error) {
	m, err := orchestrator.New(p)
	if err != nil {
		return peer.EnsureUpResult{}, err
	}
	defer m.Close()
	if _, ok, err := m.Store.GetManager(ctx); err != nil {
		return peer.EnsureUpResult{}, err
	} else if !ok {
		return peer.EnsureUpResult{}, peer.Refuse(peer.CodeNoManager, "no manager session is recorded here, so there is nothing to restore; start one at this machine's terminal with ttorch")
	}
	// A control session has no screen to open worker tabs on (termtab.Open). Set for this
	// process, and so for a tmux server it starts.
	_ = os.Setenv("TTORCH_WORKER_TABS", "0")
	notes, err := m.Resume()
	if err != nil {
		return peer.EnsureUpResult{}, err
	}
	sched, err := m.StartScheduler()
	if err != nil {
		return peer.EnsureUpResult{Restored: notes}, err
	}
	return peer.EnsureUpResult{Restored: notes, Scheduler: sched}, nil
}

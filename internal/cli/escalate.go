package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// Escalations: the decisions a manager puts to the lead, kept in the store so they outlive
// the manager's scrollback (internal/db/escalation.go).
//
// Who may call what. escalate and answer are the manager's: the manager puts decisions to the
// lead, and relays the lead's answers back, since the lead talks only to the manager. Both
// refuse from a worker context (harness.WorkerContextSignal). decisions only reads (after
// syncing approval escalations) and may run anywhere.
//
// The worker-context refusal guards against accidents: a worker that runs one of these from
// its own shell by mistake, or because text it read told it to. It is not a security
// boundary. A process running as the lead's uid can unset $TTORCH_TASK_ID, run from outside
// its worktree, or write the database directly, so neither command can prove who called it.
// What bounds the damage is what the commands can do, not who may call them:
//
//   - escalate puts text in front of the lead. A worker already reaches its manager with
//     `ttorch report needs-input`/`blocked`, and the manager decides what goes to the lead; an
//     escalation that bypassed the manager is still capped at 2 KiB and escaped on every print.
//   - answer is recorded as relayed by the manager (the event's actor and answered_by), never
//     as the lead, because nothing verifies the lead typed it. It sends text to the manager and
//     closes the escalation. It mints no approval and passes no gate; an approval still needs
//     `ttorch approve` at the lead's own terminal. It does not ask for a terminal itself,
//     because the lead's answers arrive through the manager.

const escalateUsage = `usage: ttorch escalate --task <id> --kind approval|question -m "<text>"`

// cmdEscalate raises an escalation for the lead.
func cmdEscalate(args []string) error {
	if signal := workerContextSignal(); signal != "" {
		return fmt.Errorf("refusing to escalate from inside a worker context (%s): escalating is the manager's job. A worker asks its manager with 'ttorch report needs-input -m ...', and the manager decides what goes to the lead", signal)
	}
	fs := flag.NewFlagSet("escalate", flag.ContinueOnError)
	task := fs.String("task", "", "the task the decision is about (required)")
	kind := fs.String("kind", "", "approval (a done task awaiting the lead's approval) or question")
	msg := fs.String("m", "", "what the lead needs to decide (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *task == "" || strings.TrimSpace(*msg) == "" {
		return errors.New(escalateUsage)
	}
	if *kind != db.EscalationApproval && *kind != db.EscalationQuestion {
		return fmt.Errorf("escalate: --kind must be approval or question, got %q", *kind)
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	t, ok, err := m.Store.GetTask(ctx, *task)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("escalate: task %s not found", peer.SafeText(*task))
	}
	// An approval escalation stands for a done task waiting on the lead, and it is resolved
	// once the task leaves done (db.SyncApprovalEscalations); raising one for any other task
	// would be resolved on the next read.
	if *kind == db.EscalationApproval && t.Status != db.StatusDone {
		return fmt.Errorf("escalate: an approval escalation is for a done task; %s is %s", peer.SafeText(t.ID), t.Status)
	}
	if _, cut := db.CapText(*msg, db.MaxEscalationText); cut {
		fmt.Fprintf(os.Stderr, "ttorch: the message was cut to %d bytes\n", db.MaxEscalationText)
	}
	esc, err := m.Store.OpenEscalation(ctx, t.ID, *kind, *msg)
	if err != nil {
		return err
	}
	fmt.Printf("escalation #%d opened: %s for task %s (the lead sees it in ttorch decisions)\n", esc.ID, esc.Kind, peer.SafeText(esc.TaskID))
	return nil
}

// cmdDecisions lists the open escalations, after syncing approval escalations with the tasks
// (db.SyncApprovalEscalations).
func cmdDecisions(args []string) error {
	fs := flag.NewFlagSet("decisions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the open escalations as one JSON object")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: ttorch decisions [--json]")
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	ctx := context.Background()
	if _, err := m.Store.SyncApprovalEscalations(ctx); err != nil {
		return err
	}
	open, err := m.Store.ListEscalations(ctx, db.EscalationOpen)
	if err != nil {
		return err
	}
	d := peer.NewDecisionList(open, time.Now())
	if *asJSON {
		return d.WriteJSON(os.Stdout)
	}
	return d.WriteText(os.Stdout)
}

const answerUsage = `usage: ttorch answer <escalation-id> -m "<text>" [--request-id <id>]`

// cmdAnswer records the lead's answer to an open escalation, as relayed by the manager, and
// wakes the manager with it. The request id makes it idempotent: a repeat with the same id
// changes nothing and says so. Without --request-id a fresh one is minted and printed, so a
// retry can reuse it.
func cmdAnswer(args []string) error {
	if signal := workerContextSignal(); signal != "" {
		return fmt.Errorf("refusing to answer from inside a worker context (%s): answers are the lead's, relayed by the manager. Run it from the manager's shell, outside the worktree", signal)
	}
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(answerUsage)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("answer: %q is not an escalation id\n%s", args[0], answerUsage)
	}
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	msg := fs.String("m", "", "the answer (required)")
	requestID := fs.String("request-id", "", "an id that makes a retry of this answer a no-op (default: a fresh one)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || strings.TrimSpace(*msg) == "" {
		return errors.New(answerUsage)
	}
	if *requestID == "" {
		if *requestID, err = mintRequestID(); err != nil {
			return err
		}
	}
	m, err := mgr()
	if err != nil {
		return err
	}
	defer m.Close()
	if _, cut := db.CapText(*msg, db.MaxEscalationText); cut {
		fmt.Fprintf(os.Stderr, "ttorch: the answer was cut to %d bytes\n", db.MaxEscalationText)
	}
	res, err := m.Store.AnswerEscalation(context.Background(), id, *requestID, *msg, db.ActorManager)
	if err != nil {
		return err
	}
	if res.Replayed {
		fmt.Printf("escalation #%d was already answered by request %s; nothing changed (manager event #%d)\n", res.Escalation.ID, *requestID, res.EventID)
		return nil
	}
	fmt.Printf("escalation #%d: answer relayed by the manager recorded (request %s); the manager is woken by event #%d\n", res.Escalation.ID, *requestID, res.EventID)
	return nil
}

// mintRequestID returns a fresh request id for an answer typed at the command line.
func mintRequestID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("answer: minting a request id: %w", err)
	}
	return "cli-" + hex.EncodeToString(b[:]), nil
}

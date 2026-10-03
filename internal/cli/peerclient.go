package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/peer"
)

// The parent's peer commands: provisioning a peer, and using it through its control channel.
//
// Who may call what. add and adopt put a key in another machine's authorized_keys, so they are
// the lead's and run the same caller check as `ttorch approve` (checkLeadCaller): no worker
// context, and an interactive terminal, before anything else happens. Like that check, it guards
// against accidents and injected commands, and is not a boundary: the lead's ssh keys can reach
// the peer whatever ttorch refuses. The other commands use a peer already provisioned, the way a
// manager uses its worker pool, and talk to it only through the control key, which reaches only
// the control channel's verbs. Every one that sends the peer anything (status, decisions, answer,
// task-add, goal, repo add) refuses a worker context first (refuseWorkerContext); ls reads only
// this coordinator's store.

const peerClientUsage = `usage:
  ttorch peer add <name> <control-dest> [--approve-dest <dest>] [--remote-ttorch <path>]
  ttorch peer adopt <name> <control-dest> --force [--approve-dest <dest>] [--remote-ttorch <path>]
  ttorch peer ls
  ttorch peer status <name> [--json]
  ttorch peer decisions <name> [--since <id>] [--json]
  ttorch peer answer <name> <escalation-id> -m "<text>" [--request-id <id>]
  ttorch peer task-add <name> <task-id> --repo <path on the peer> (--brief-file <f> | --brief "...")
      [--title "..."] [--touches "a,b"] [--effort <level>] [--model <m>] [--request-id <id>]
  ttorch peer goal <name> -m "<text>" [--request-id <id>]
  ttorch peer repo add <name> <path on the peer> --origin <url>
  ttorch peer retire <name>

  <control-dest> is where the control key reaches the peer: [user@]host or
  ssh://[user@]host[:port]. No ssh_config is read on that channel, so it names the host itself.
  --approve-dest is the destination you use interactively (your ssh config applies); it
  defaults to <control-dest>. --remote-ttorch is ttorch on the peer, relative to its home
  (default .ttorch/bin/ttorch).`

// cmdPeerClient dispatches the parent's `ttorch peer` subcommands.
func cmdPeerClient(args []string) error {
	switch args[0] {
	case "add":
		return cmdPeerProvision(args[1:], false, os.Stdin)
	case "adopt":
		return cmdPeerProvision(args[1:], true, os.Stdin)
	case "ls":
		return cmdPeerLs(args[1:])
	case "status":
		return cmdPeerStatus(args[1:])
	case "decisions":
		return cmdPeerDecisions(args[1:])
	case "answer":
		return cmdPeerAnswer(args[1:])
	case "task-add":
		return cmdPeerTaskAdd(args[1:])
	case "goal":
		return cmdPeerGoal(args[1:])
	case "repo":
		if len(args) > 1 && args[1] == "add" {
			return cmdPeerRepoAdd(args[2:])
		}
	case "retire":
		return cmdPeerRetire(args[1:], os.Stdin)
	}
	return errors.New(peerClientUsage)
}

// peerStore opens this coordinator's store.
func peerStore() (*db.Store, error) { return db.Open(paths.Default().StateDB()) }

// refuseWorkerContext is the accident guard every command that sends a peer anything runs first,
// the one `ttorch answer` and `ttorch escalate` have: a worker that runs it from its own shell, by
// mistake or because text it read told it to, is stopped before ssh runs. Like those, it is not a
// boundary: a process running as the lead can unset $TTORCH_TASK_ID and leave its worktree.
func refuseWorkerContext(what string) error {
	if signal := workerContextSignal(); signal != "" {
		return fmt.Errorf("refusing to %s from inside a worker context (%s): a peer takes its work and answers from the manager. Run it from the manager's shell, outside the worktree", what, signal)
	}
	return nil
}

// usablePeer opens the store and finds name, which must be live or unreachable: a peer still
// provisioning has no key the channel admits yet, and a retired one has none at all. The client
// reaches it with the control key only.
func usablePeer(ctx context.Context, name string) (*db.Store, db.Peer, peer.Client, error) {
	if err := db.ValidPeerName(name); err != nil {
		return nil, db.Peer{}, peer.Client{}, err
	}
	store, err := peerStore()
	if err != nil {
		return nil, db.Peer{}, peer.Client{}, err
	}
	p, ok, err := store.GetPeer(ctx, name)
	switch {
	case err != nil:
	case !ok:
		err = fmt.Errorf("no peer is registered as %s (ttorch peer ls lists them)", name)
	case p.Status == db.PeerProvisioning:
		err = fmt.Errorf("peer %s is still provisioning; finish it with ttorch peer add %s %s", name, name, p.ControlDest)
	case p.Status == db.PeerRetired:
		err = fmt.Errorf("peer %s is retired", name)
	}
	if err != nil {
		store.Close()
		return nil, db.Peer{}, peer.Client{}, err
	}
	return store, p, peer.Client{SSH: peerSSH, Dest: p.ControlDest, Key: p.ControlKey, Timeout: peerCallTimeout}, nil
}

// peerCall makes one control call and records the outcome on the peer's row: the time of a
// success, or why it failed. summary, when set, is cached as the peer's display summary.
func peerCall(ctx context.Context, store *db.Store, p db.Peer, c peer.Client, verb string, req, result any) error {
	err := c.Call(ctx, verb, req, result)
	if err != nil {
		_ = store.RecordPeerError(ctx, p.Name, peer.Untrusted(err.Error()))
		return fmt.Errorf("peer %s: %w", p.Name, err)
	}
	_ = store.RecordPeerOK(ctx, p.Name, "")
	return nil
}

// ago is how long before now t was, in whole seconds, or "never" for a zero t.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t).Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

// cmdPeerLs lists the registry: each peer's status, destinations, what it last reported, what
// was delegated to it and the repositories it owns. It reads only this coordinator's store.
func cmdPeerLs(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: ttorch peer ls")
	}
	ctx := context.Background()
	store, err := peerStore()
	if err != nil {
		return err
	}
	defer store.Close()
	peers, err := store.ListPeers(ctx)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("no peers (add one with ttorch peer add <name> <control-dest>)")
		return nil
	}
	counts, err := store.CountDelegations(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, p := range peers {
		version := "-"
		if p.Version != "" {
			version = fmt.Sprintf("%s (protocol %d)", peer.Untrusted(p.Version), p.Protocol)
		}
		c := counts[p.Name]
		fmt.Printf("%s  %s  control %s  approve %s  ttorch %s  last ok %s  delegated %d task(s), %d goal(s)\n",
			p.Name, p.Status, peer.Untrusted(p.ControlDest), peer.Untrusted(p.ApproveDest), version, ago(now, p.LastOKAt), c.Tasks, c.Goals)
		repos, err := store.ListPeerRepos(ctx, p.Name)
		if err != nil {
			return err
		}
		for _, r := range repos {
			fmt.Printf("  repo %s  origin %s\n", peer.Untrusted(r.RemotePath), peer.Untrusted(r.OriginURL))
		}
		if p.LastError != "" {
			fmt.Printf("  last error: %s\n", peer.Untrusted(p.LastError))
		}
	}
	return nil
}

// cmdPeerStatus reads the peer's summary over the control channel and prints it, caching it on
// the peer's row for display. Every string in it came from the peer and was escaped on arrival.
func cmdPeerStatus(args []string) error {
	if err := refuseWorkerContext("read a peer's summary"); err != nil {
		return err
	}
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ttorch peer status <name> [--json]")
	}
	fs := flag.NewFlagSet("peer status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the peer's summary as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: ttorch peer status <name> [--json]")
	}
	ctx := context.Background()
	store, p, c, err := usablePeer(ctx, args[0])
	if err != nil {
		return err
	}
	defer store.Close()
	var sum peer.Summary
	if err := peerCall(ctx, store, p, c, peer.VerbSummary, nil, &sum); err != nil {
		return err
	}
	var cache bytes.Buffer
	if err := sum.WriteJSON(&cache); err == nil {
		_ = store.RecordPeerOK(ctx, p.Name, strings.TrimSpace(cache.String()))
	}
	if *asJSON {
		return sum.WriteJSON(os.Stdout)
	}
	fmt.Printf("peer %s (%s, control %s); its escalations: ttorch peer decisions %s\n", p.Name, p.Status, peer.Untrusted(p.ControlDest), p.Name)
	return sum.WriteText(os.Stdout)
}

// cmdPeerDecisions lists the peer's open escalations above --since. It does not move the cursor
// the scheduler's poll keeps (escalation_cursor); it only reads.
func cmdPeerDecisions(args []string) error {
	if err := refuseWorkerContext("read a peer's escalations"); err != nil {
		return err
	}
	const usage = "usage: ttorch peer decisions <name> [--since <id>] [--json]"
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("peer decisions", flag.ContinueOnError)
	since := fs.Int64("since", 0, "list only escalations with an id above this one")
	asJSON := fs.Bool("json", false, "print the peer's answer as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || *since < 0 {
		return errors.New(usage)
	}
	ctx := context.Background()
	store, p, c, err := usablePeer(ctx, args[0])
	if err != nil {
		return err
	}
	defer store.Close()
	var d peer.DecisionsResult
	if err := peerCall(ctx, store, p, c, peer.VerbDecisions, peer.DecisionsRequest{Since: *since}, &d); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	switch d.Open {
	case 0:
		fmt.Printf("peer %s: no open escalations\n", p.Name)
	default:
		fmt.Printf("peer %s: %d open escalation(s), highest #%d; %d above #%d\n", p.Name, d.Open, d.HighestOpenID, len(d.Escalations), d.Since)
		fmt.Printf("answer with: ttorch peer answer %s <id> -m \"...\" (recorded on the peer as relayed by its parent coordinator)\n", p.Name)
	}
	now := time.Now()
	for _, e := range d.Escalations {
		task := e.TaskID
		if task == "" {
			task = "-"
		}
		raised := "?"
		if e.AgeSeconds != nil {
			raised = (time.Duration(*e.AgeSeconds) * time.Second).String() + " ago"
		} else if !e.CreatedAt.IsZero() {
			raised = ago(now, e.CreatedAt)
		}
		fmt.Printf("#%d  %s  task %s  raised %s\n      %s\n", e.ID, e.Kind, task, raised, e.Body)
	}
	return nil
}

const peerAnswerUsage = `usage: ttorch peer answer <name> <escalation-id> -m "<text>" [--request-id <id>]`

// cmdPeerAnswer answers one of the peer's open escalations. The peer records it as relayed by
// its parent coordinator (actor parent), never as the lead: nothing here can show who typed it.
// It wakes the peer's manager with one event and grants nothing. Like `ttorch answer` it refuses a
// worker context, and a repeat with the same --request-id changes nothing.
func cmdPeerAnswer(args []string) error {
	if err := refuseWorkerContext("answer a peer's escalation"); err != nil {
		return err
	}
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return errors.New(peerAnswerUsage)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[1], "#"), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("peer answer: %q is not an escalation id\n%s", args[1], peerAnswerUsage)
	}
	fs := flag.NewFlagSet("peer answer", flag.ContinueOnError)
	msg := fs.String("m", "", "the answer (required)")
	requestID := fs.String("request-id", "", "an id that makes a retry of this answer a no-op (default: a fresh one)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || strings.TrimSpace(*msg) == "" {
		return errors.New(peerAnswerUsage)
	}
	if len(*msg) > db.MaxEscalationText {
		return fmt.Errorf("peer answer: the answer is %d bytes; the peer takes at most %d", len(*msg), db.MaxEscalationText)
	}
	if *requestID == "" {
		if *requestID, err = mintRequestID(); err != nil {
			return err
		}
	} else if err := db.ValidRequestID(*requestID); err != nil {
		return err
	}
	ctx := context.Background()
	store, p, c, err := usablePeer(ctx, args[0])
	if err != nil {
		return err
	}
	defer store.Close()
	self, err := store.GetCoordinator(ctx)
	if err != nil {
		return err
	}
	var res peer.AnswerResult
	err = peerCall(ctx, store, p, c, peer.VerbAnswer, peer.AnswerRequest{ParentID: self.CoordID, RequestID: *requestID, EscalationID: id, Text: *msg}, &res)
	if err != nil {
		return retryHint(err, *requestID)
	}
	if res.Replayed {
		fmt.Printf("peer %s escalation #%d was already answered by request %s; nothing changed (its manager's event #%d)\n", p.Name, res.EscalationID, *requestID, res.EventID)
		return nil
	}
	fmt.Printf("peer %s escalation #%d: answer recorded on the peer as relayed by this coordinator, its parent (request %s); the peer's manager is woken by event #%d\n", p.Name, res.EscalationID, *requestID, res.EventID)
	return nil
}

// retryHint adds, to a call that may or may not have reached the peer, the request id that makes
// a retry safe. A refusal the peer answered with needs no hint: the peer acted on nothing.
func retryHint(err error, requestID string) error {
	var re *peer.RemoteError
	if errors.As(err, &re) {
		return err
	}
	return fmt.Errorf("%w\nthe request may have reached the peer; retry with --request-id %s, which changes nothing if it did", err, requestID)
}

// cmdPeerTaskAdd hands the peer a briefed backlog task, which its scheduler dispatches like any
// other. The parent records the delegation first (request id, task, the brief's sha256), so after
// a restart it knows what it asked for; a refusal from the peer drops the record, since the peer
// acted on nothing. The brief lint runs on the peer, against the peer's checkout.
func cmdPeerTaskAdd(args []string) error {
	if err := refuseWorkerContext("hand a peer a task"); err != nil {
		return err
	}
	const usage = `usage: ttorch peer task-add <name> <task-id> --repo <path on the peer> (--brief-file <f> | --brief "...") [--title "..."] [--touches "a,b"] [--effort <level>] [--model <m>] [--request-id <id>]`
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New(usage)
	}
	name, taskID := args[0], args[1]
	if err := db.ValidateTaskID(taskID); err != nil {
		return fmt.Errorf("peer task-add: %w", err)
	}
	if len(taskID) > peer.MaxTaskID {
		return fmt.Errorf("peer task-add: the task id is %d bytes; the peer takes at most %d", len(taskID), peer.MaxTaskID)
	}
	fs := flag.NewFlagSet("peer task-add", flag.ContinueOnError)
	repo := fs.String("repo", "", "the repository's path on the peer, as registered there (required)")
	title := fs.String("title", "", "one-line task title")
	touches := fs.String("touches", "", "comma-separated files the task will touch")
	briefFile := fs.String("brief-file", "", "path to the task's brief")
	brief := fs.String("brief", "", "the task's brief, inline")
	effort := fs.String("effort", "", "reasoning effort to dispatch at")
	model := fs.String("model", "", "model to dispatch on")
	requestID := fs.String("request-id", "", "an id that makes a retry of this add a no-op (default: a fresh one)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || *repo == "" {
		return errors.New(usage)
	}
	text, err := resolveBrief("peer task-add", *brief, *briefFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("peer task-add: a brief is required; the peer's scheduler dispatches only briefed tasks")
	}
	var footprint []string
	for _, t := range strings.Split(*touches, ",") {
		if t = strings.TrimSpace(t); t != "" {
			footprint = append(footprint, t)
		}
	}
	sum := sha256.Sum256([]byte(text))
	req := peer.TaskAddRequest{RequestID: *requestID, TaskID: taskID, Repo: *repo, Title: *title, Touches: footprint, Brief: text, Effort: *effort, Model: *model}
	var res peer.TaskAddResult
	rid, err := delegate(name, db.Delegation{Kind: db.DelegationTask, RemoteTaskID: taskID, BriefSHA256: hex.EncodeToString(sum[:])}, *requestID,
		func(ctx context.Context, store *db.Store, p db.Peer, c peer.Client, parentID, rid string) error {
			req.ParentID, req.RequestID = parentID, rid
			return peerCall(ctx, store, p, c, peer.VerbTaskAdd, req, &res)
		})
	if err != nil {
		var re *peer.RemoteError
		if errors.As(err, &re) && re.Detail != "" {
			fmt.Fprintf(os.Stderr, "the peer's report: %s\n", re.Detail)
		}
		return err
	}
	if res.BriefSHA256 != hex.EncodeToString(sum[:]) {
		fmt.Fprintf(os.Stderr, "warning: the peer stored a brief with sha256 %s, not this one's %s\n", res.BriefSHA256, hex.EncodeToString(sum[:]))
	}
	state := "added"
	if res.Replayed {
		state = "was already added by this request; nothing changed"
	}
	fmt.Printf("peer %s task %s %s (status %s, brief stored: %v, request %s); its scheduler dispatches it\n", name, res.TaskID, state, res.Status, res.HasBrief, rid)
	return nil
}

// cmdPeerGoal hands the peer's manager plain-language work, the way the lead talks to a root
// manager. It is recorded as a delegation, with the sha256 of the text.
func cmdPeerGoal(args []string) error {
	if err := refuseWorkerContext("hand a peer a goal"); err != nil {
		return err
	}
	const usage = `usage: ttorch peer goal <name> -m "<text>" [--request-id <id>]`
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("peer goal", flag.ContinueOnError)
	msg := fs.String("m", "", "the goal (required)")
	requestID := fs.String("request-id", "", "an id that makes a retry of this goal a no-op (default: a fresh one)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || strings.TrimSpace(*msg) == "" {
		return errors.New(usage)
	}
	if len(*msg) > db.MaxEscalationText {
		return fmt.Errorf("peer goal: the goal is %d bytes; the peer takes at most %d", len(*msg), db.MaxEscalationText)
	}
	sum := sha256.Sum256([]byte(*msg))
	var res peer.GoalResult
	rid, err := delegate(args[0], db.Delegation{Kind: db.DelegationGoal, BriefSHA256: hex.EncodeToString(sum[:])}, *requestID,
		func(ctx context.Context, store *db.Store, p db.Peer, c peer.Client, parentID, rid string) error {
			return peerCall(ctx, store, p, c, peer.VerbGoal, peer.GoalRequest{ParentID: parentID, RequestID: rid, Text: *msg}, &res)
		})
	if err != nil {
		return err
	}
	if res.Replayed {
		fmt.Printf("peer %s already has this goal from request %s; nothing changed (its manager's event #%d)\n", args[0], rid, res.EventID)
		return nil
	}
	fmt.Printf("peer %s: goal handed to its manager as event #%d (request %s)\n", args[0], res.EventID, rid)
	return nil
}

// delegate records d for name under requestID (a fresh one if empty), runs call, and keeps or
// drops the record by the outcome: a refusal the peer answered with means it acted on nothing, so
// the record goes; anything else (success, a timeout, a transport failure) may have reached the
// peer, so it stays, and a failure says which request id makes a retry safe.
func delegate(name string, d db.Delegation, requestID string, call func(ctx context.Context, store *db.Store, p db.Peer, c peer.Client, parentID, requestID string) error) (string, error) {
	var err error
	if requestID == "" {
		if requestID, err = mintRequestID(); err != nil {
			return "", err
		}
	} else if err := db.ValidRequestID(requestID); err != nil {
		return "", err
	}
	ctx := context.Background()
	store, p, c, err := usablePeer(ctx, name)
	if err != nil {
		return "", err
	}
	defer store.Close()
	self, err := store.GetCoordinator(ctx)
	if err != nil {
		return "", err
	}
	d.RequestID, d.Peer = requestID, p.Name
	if _, _, err := store.RecordDelegation(ctx, d); err != nil {
		return "", err
	}
	if err := call(ctx, store, p, c, self.CoordID, requestID); err != nil {
		var re *peer.RemoteError
		if errors.As(err, &re) {
			_ = store.ForgetDelegation(ctx, requestID)
		}
		return "", retryHint(err, requestID)
	}
	return requestID, nil
}

// cmdPeerRepoAdd records that the peer owns a repository, after the peer's summary confirms it
// has a project at that path. One repository belongs to one coordinator (db.AddPeerRepo).
func cmdPeerRepoAdd(args []string) error {
	if err := refuseWorkerContext("record a peer's repository"); err != nil {
		return err
	}
	const usage = "usage: ttorch peer repo add <name> <path on the peer> --origin <url>"
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("peer repo add", flag.ContinueOnError)
	origin := fs.String("origin", "", "the repository's origin URL (required)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || *origin == "" {
		return errors.New(usage)
	}
	ctx := context.Background()
	store, p, c, err := usablePeer(ctx, args[0])
	if err != nil {
		return err
	}
	defer store.Close()
	var sum peer.Summary
	if err := peerCall(ctx, store, p, c, peer.VerbSummary, nil, &sum); err != nil {
		return err
	}
	found := false
	for _, r := range sum.Repos {
		found = found || r.Path == args[1]
	}
	if !found {
		return fmt.Errorf("peer %s has no project at %s; register it there with ttorch project add", p.Name, peer.Untrusted(args[1]))
	}
	r, err := store.AddPeerRepo(ctx, p.Name, args[1], *origin)
	if err != nil {
		return err
	}
	fmt.Printf("peer %s owns %s (origin %s)\n", r.Peer, peer.Untrusted(r.RemotePath), peer.Untrusted(r.OriginURL))
	return nil
}

// cmdPeerRetire stops using a peer: the row is kept, retired, and the control key is deleted, so
// nothing on this machine can use it again. The key's line stays in the peer's authorized_keys
// until the lead removes it; with the private half gone it admits no one. It is the lead's call,
// like add, and runs the same caller check.
func cmdPeerRetire(args []string, stdin *os.File) error {
	if err := checkLeadCaller("retire a peer", stdin); err != nil {
		return err
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ttorch peer retire <name>")
	}
	ctx := context.Background()
	store, err := peerStore()
	if err != nil {
		return err
	}
	defer store.Close()
	p, ok, err := store.GetPeer(ctx, args[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no peer is registered as %s", args[0])
	}
	if _, err := store.RetirePeer(ctx, p.Name); err != nil {
		return err
	}
	for _, f := range []string{p.ControlKey, p.ControlKey + ".pub"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("peer %s is retired, but its control key could not be deleted: %w", p.Name, err)
		}
	}
	_ = os.Remove(filepath.Dir(p.ControlKey))
	self, err := store.GetCoordinator(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("peer %s retired; its control key is deleted. On the peer, the authorized_keys line ending ttorch-peer-control:%s admits no key you hold and can be removed\n", p.Name, self.CoordID)
	return nil
}

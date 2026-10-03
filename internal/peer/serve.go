package peer

// The peer control channel: one request from a parent coordinator per process, run as an ssh
// forced command. The peer's authorized_keys pins the parent's control key to it,
//
//	command="ttorch peer serve",restrict ssh-ed25519 AAAA... ttorch-peer-control:<parent>
//
// so whatever the parent asks to run arrives in SSH_ORIGINAL_COMMAND and nothing else runs.
// Serve reads that string itself, without a shell: it must be exactly one verb from the list
// below, and anything else is refused with a named error before a byte of the body is read.
// The body is one JSON object on stdin, capped at MaxBody. The response is one JSON object on
// stdout that always carries the protocol version.
//
// What the channel can do is the verb list, and the list holds nothing that approves, merges,
// lands, gates or touches a worker. A verb that changes state takes a request id, and the
// peer_requests ledger makes a repeat of it return the first result and change nothing. The
// read verbs open the store read-only (db.OpenReadOnly), so a poll runs no migration and
// writes no row.
//
// Every byte of a request is treated as hostile, and so is every string a response carries:
// task ids a worker chose, repo paths, escalation text that may quote a worker. Each is escaped
// and capped where it is built, and the response then passes through sanitize, which escapes
// any string still carrying a non-printing rune and caps every string, keys included, at
// MaxText. A handler that forgets cannot put raw text on the wire.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
)

// ProtocolVersion is the control protocol's major version. Every response carries it, error
// responses included, and a parent refuses a version it does not speak rather than guess at
// fields.
const ProtocolVersion = 1

// MaxBody is the cap on a request body, the bound `ttorch stop-hook` already puts on its stdin.
const MaxBody = 1 << 20

// MaxCommand bounds SSH_ORIGINAL_COMMAND. The longest verb is nine bytes; a command longer than
// this is refused without being split.
const MaxCommand = 64

// MaxTaskID bounds the id of a task a parent adds. It equals MaxIDText, the cap on an id in a
// response, so a parent always gets back the id it chose uncut.
const MaxTaskID = MaxIDText

// Field bounds for a task-add request. A title is one line; a footprint entry is a path.
const (
	maxTitle      = 256
	maxTouches    = 256
	maxTouchBytes = 1024
	maxRepoPath   = 4096
)

// The verbs.
const (
	VerbVersion   = "version"
	VerbSummary   = "summary"
	VerbDecisions = "decisions"
	VerbTaskAdd   = "task-add"
	VerbGoal      = "goal"
	VerbAnswer    = "answer"
	VerbEnsureUp  = "ensure-up"
)

// verbs is every verb Serve answers, in the order the design lists them, and the handler for
// each. Nothing else reaches a handler. Left out on purpose: approve, merge-local, land, trust,
// spawn, send, peek and teardown, so the parent can neither approve, merge nor gate on a peer
// nor reach its workers; review-diff arrives with the approval relay. This file is in the
// ttorch-source tier of the gate's covered set (orchestrator.ttorchSourceFiles), so adding a
// verb is a gate change, and TestServeVerbList pins the list.
var verbs = []verb{
	{VerbVersion, serveVersion},
	{VerbSummary, serveSummary},
	{VerbDecisions, serveDecisions},
	{VerbTaskAdd, serveTaskAdd},
	{VerbGoal, serveGoal},
	{VerbAnswer, serveAnswer},
	{VerbEnsureUp, serveEnsureUp},
}

// verb is one entry of the verb list: its name and its handler, which decodes the body itself.
type verb struct {
	name  string
	serve func(ctx context.Context, h Host, body []byte) (any, error)
}

// Verbs returns the verb list, in order.
func Verbs() []string {
	out := make([]string, len(verbs))
	for i, v := range verbs {
		out[i] = v.name
	}
	return out
}

// Error codes. A refusal names one, so a parent can act on the kind without parsing the message.
const (
	CodeNoCommand     = "no_command"     // SSH_ORIGINAL_COMMAND is empty: an interactive login, or a run outside ssh
	CodeBadCommand    = "bad_command"    // over MaxCommand, or a verb followed by arguments
	CodeUnknownVerb   = "unknown_verb"   // not one of Verbs()
	CodeWorkerContext = "worker_context" // run from inside a worker's context
	CodeBodyTooLarge  = "body_too_large" // over MaxBody
	CodeBadBody       = "bad_body"       // not one JSON object, an unknown field, or trailing data
	CodeBadRequest    = "bad_request"    // a field missing, malformed or out of bounds
	CodeNotFound      = "not_found"      // no such project or escalation
	CodeConflict      = "conflict"       // a request id used for another request, a task id taken, an escalation no longer open
	CodeBriefLint     = "brief_lint"     // the brief lint refused the brief, or could not finish
	CodeUnavailable   = "unavailable"    // no state store, or one behind this binary's schema
	CodeNoManager     = "no_manager"     // ensure-up with no manager session recorded
	CodeInternal      = "internal"       // anything else
)

// Error is a refusal with a code. Message says what was refused; Detail, when set, carries
// supporting text such as the brief lint's report.
type Error struct {
	Code    string
	Message string
	Detail  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Refuse builds an *Error.
func Refuse(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Host is what the verbs act on. The CLI wires it (`ttorch peer serve`); a test wires fakes. A
// nil field answers as an internal error, never as a silent success.
type Host struct {
	// ReadStore opens the state store read-only, with no migration (db.OpenReadOnly). version
	// and summary use it, so a poll never writes the peer's store.
	ReadStore func() (*db.Store, error)
	// Store opens the state store for a verb that changes it.
	Store func() (*db.Store, error)
	// SummarySources wires a summary's reads (Build) to a store from ReadStore.
	SummarySources func(store *db.Store) (Sources, error)
	// AddTask creates a backlog task through the core `ttorch task add` uses, brief lint
	// included, and returns the lint's report. It refuses with an *Error where it can name one.
	AddTask func(ctx context.Context, store *db.Store, req TaskAdd) (db.TaskAddResult, string, error)
	// EnsureUp restores the manager and workers without a terminal, then starts the scheduler.
	EnsureUp func(ctx context.Context) (EnsureUpResult, error)
	// Version is the binary's version.
	Version string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (h Host) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

// Response is the one object Serve writes. Result is set when OK, Error when not.
type Response struct {
	Protocol int        `json:"protocol"`
	Verb     string     `json:"verb,omitempty"`
	OK       bool       `json:"ok"`
	Result   any        `json:"result,omitempty"`
	Error    *ErrorBody `json:"error,omitempty"`
}

// ErrorBody is a refusal as the wire carries it.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Serve answers one request: command is SSH_ORIGINAL_COMMAND, stdin the body. It writes one
// response to stdout and, for a refusal, one line to stderr, and returns the process exit
// status: 0 when the verb succeeded, 1 otherwise. workerSignal is the caller's worker-context
// signal (harness.WorkerContextSignal), "" outside one.
func Serve(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer, workerSignal string, h Host) int {
	resp := Response{Protocol: ProtocolVersion}
	result, verb, err := serve(ctx, command, stdin, workerSignal, h)
	if err == nil {
		resp.OK, resp.Verb, resp.Result = true, verb, result
	} else {
		resp.Verb = verb
		resp.Error = errorBody(err)
	}
	out, mErr := encode(resp)
	if mErr != nil {
		resp = Response{Protocol: ProtocolVersion, Verb: verb, Error: &ErrorBody{Code: CodeInternal, Message: "encoding the response failed"}}
		out, _ = encode(resp)
	}
	_, _ = stdout.Write(out)
	if resp.Error != nil {
		fmt.Fprintf(stderr, "ttorch peer serve: %s: %s\n", resp.Error.Code, resp.Error.Message)
		return 1
	}
	return 0
}

func serve(ctx context.Context, command string, stdin io.Reader, workerSignal string, h Host) (any, string, error) {
	v, err := parseCommand(command)
	if err != nil {
		return nil, "", err
	}
	// The same accident guard `ttorch answer` and `ttorch escalate` have. A worker could
	// otherwise hand its own manager a goal recorded as the parent's. A same-user process can
	// step around it, as it can around every check on one machine; what bounds the channel is
	// the verb list, not who runs it.
	if workerSignal != "" {
		return nil, v.name, Refuse(CodeWorkerContext, "the control channel is an ssh forced command and does not run inside a worker context (%s)", workerSignal)
	}
	body, err := readBody(stdin)
	if err != nil {
		return nil, v.name, err
	}
	result, err := v.serve(ctx, h, body)
	return result, v.name, err
}

// parseCommand finds the verb SSH_ORIGINAL_COMMAND names. The string is split on whitespace and
// never handed to a shell, so `summary; touch x` is a three-token command whose first token is
// not a verb. A verb takes no arguments: what it needs travels in the body.
func parseCommand(command string) (verb, error) {
	var zero verb
	if len(command) > MaxCommand {
		return zero, Refuse(CodeBadCommand, "the command is %d bytes, over the %d byte limit; send one verb (%s)", len(command), MaxCommand, strings.Join(Verbs(), ", "))
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return zero, Refuse(CodeNoCommand, "no command: this key runs one verb per call (%s)", strings.Join(Verbs(), ", "))
	}
	for _, v := range verbs {
		if fields[0] != v.name {
			continue
		}
		if len(fields) > 1 {
			return zero, Refuse(CodeBadCommand, "%s takes no arguments; a request travels as one JSON object on stdin", v.name)
		}
		return v, nil
	}
	return zero, Refuse(CodeUnknownVerb, "%q is not a verb this channel serves (%s)", fields[0], strings.Join(Verbs(), ", "))
}

// readBody reads the request body, at most MaxBody bytes. It reads one byte past the cap, which
// is what tells a body exactly at the cap from one over it.
func readBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil {
		return nil, Refuse(CodeBadBody, "reading the request: %v", err)
	}
	if len(b) > MaxBody {
		return nil, Refuse(CodeBodyTooLarge, "the request is over the %d byte limit", MaxBody)
	}
	return b, nil
}

// decode reads body into v: one JSON object, no field v does not name, nothing after it. An
// empty body reads as {}.
func decode(body []byte, v any) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		body = []byte("{}")
	}
	if body[0] != '{' {
		return Refuse(CodeBadBody, "the request must be one JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Refuse(CodeBadBody, "the request is not a valid object for this verb: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Refuse(CodeBadBody, "the request has data after its JSON object")
	}
	return nil
}

// errorBody is err as the wire carries it. A message is written by this package, often with
// hostile text already %q-quoted into it, or is an error from below that may hold raw text, so it
// takes sanitize's rule rather than SafeText's: escaped only if a non-printing rune is left, and
// capped either way. A quoted token is then not escaped a second time.
func errorBody(err error) *ErrorBody {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Message: err.Error()}
	}
	return &ErrorBody{Code: e.Code, Message: safeString(e.Message), Detail: safeString(e.Detail)}
}

// encode writes resp through sanitize, as one line of JSON and a newline.
func encode(resp Response) ([]byte, error) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	out, err := json.Marshal(sanitize(tree))
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// sanitize is the last pass over a response, decoded to its generic form. A string that still
// holds a non-printing rune was never escaped, so it is escaped now; every string, map keys
// included, is then capped at MaxText. A string escaped where it was built has no such rune and
// is left as it is, so nothing is escaped twice.
func sanitize(v any) any {
	switch x := v.(type) {
	case string:
		return safeString(x)
	case []any:
		for i := range x {
			x[i] = sanitize(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[safeString(k)] = sanitize(val)
		}
		return out
	default:
		return v
	}
}

func safeString(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 || !validUTF8(s) {
		return escapeCap(s, MaxText)
	}
	return capText(s, MaxText)
}

// capText cuts s to at most n bytes on a rune boundary, ending a cut value with truncated.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut, _ := db.CapText(s, n-len(truncated))
	return cut + truncated
}

func validUTF8(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// openErr names a store that could not be opened.
func openErr(err error) error {
	var behind *db.SchemaBehindError
	if errors.As(err, &behind) {
		return Refuse(CodeUnavailable, "%s", behind.Error())
	}
	return Refuse(CodeUnavailable, "the state store could not be opened: %v", err)
}

func readStore(h Host) (*db.Store, error) {
	if h.ReadStore == nil {
		return nil, Refuse(CodeInternal, "no read-only store is wired")
	}
	s, err := h.ReadStore()
	if err != nil {
		return nil, openErr(err)
	}
	return s, nil
}

func writeStore(h Host) (*db.Store, error) {
	if h.Store == nil {
		return nil, Refuse(CodeInternal, "no store is wired")
	}
	s, err := h.Store()
	if err != nil {
		return nil, openErr(err)
	}
	return s, nil
}

// --- version -----------------------------------------------------------------------------

// VersionResult is what `version` answers: the protocol, this binary's version, and the
// coordinator's name and role.
type VersionResult struct {
	Protocol    int             `json:"protocol"`
	Version     string          `json:"version"`
	Coordinator CoordinatorInfo `json:"coordinator"`
}

// CoordinatorInfo is the coordinator row as a parent sees it.
type CoordinatorInfo struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func serveVersion(ctx context.Context, h Host, body []byte) (any, error) {
	if err := decode(body, &struct{}{}); err != nil {
		return nil, err
	}
	store, err := readStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	c, err := store.GetCoordinator(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the coordinator row: %w", err)
	}
	return VersionResult{
		Protocol:    ProtocolVersion,
		Version:     SafeText(h.Version),
		Coordinator: CoordinatorInfo{Name: SafeText(c.Name), Role: SafeText(c.Role)},
	}, nil
}

// --- summary -----------------------------------------------------------------------------

func serveSummary(ctx context.Context, h Host, body []byte) (any, error) {
	if err := decode(body, &struct{}{}); err != nil {
		return nil, err
	}
	store, err := readStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if h.SummarySources == nil {
		return nil, Refuse(CodeInternal, "no summary sources are wired")
	}
	src, err := h.SummarySources(store)
	if err != nil {
		return nil, err
	}
	// The store is read-only, so approval escalations are not synced first (`ttorch summary`
	// syncs them): one nobody has listed yet is not counted as open. `decisions` syncs.
	return Build(ctx, src, h.now())
}

// --- decisions ---------------------------------------------------------------------------

// DecisionsRequest asks for the open escalations above Since, the highest escalation id the
// parent has already seen.
type DecisionsRequest struct {
	Since int64 `json:"since"`
}

// DecisionsResult is the open escalations with an id above Since, and the id of every open
// escalation, so a parent can drop the ones closed since its last call.
type DecisionsResult struct {
	Since         int64            `json:"since"`
	Open          int              `json:"open"`
	HighestOpenID int64            `json:"highest_open_id"`
	OpenIDs       []int64          `json:"open_ids"`
	Escalations   []EscalationItem `json:"escalations"`
}

func serveDecisions(ctx context.Context, h Host, body []byte) (any, error) {
	var req DecisionsRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.Since < 0 {
		return nil, Refuse(CodeBadRequest, "since must not be negative")
	}
	store, err := writeStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	// The sync is the one write: it resolves approval escalations whose task has left done and
	// opens one for a done task still waiting on approval, as `ttorch decisions` does.
	if _, err := store.SyncApprovalEscalations(ctx); err != nil {
		return nil, err
	}
	open, err := store.ListEscalations(ctx, db.EscalationOpen)
	if err != nil {
		return nil, err
	}
	list := NewDecisionList(open, h.now())
	out := DecisionsResult{Since: req.Since, Open: list.Open, HighestOpenID: list.HighestOpenID, OpenIDs: []int64{}, Escalations: []EscalationItem{}}
	for _, e := range list.Escalations {
		out.OpenIDs = append(out.OpenIDs, e.ID)
		if e.ID > req.Since {
			out.Escalations = append(out.Escalations, e)
		}
	}
	return out, nil
}

// --- task-add ----------------------------------------------------------------------------

// TaskAddRequest is a briefed backlog task a parent hands the peer. Repo is the repository's
// path on the peer, as registered there; Touches is its footprint.
type TaskAddRequest struct {
	RequestID string   `json:"request_id"`
	TaskID    string   `json:"task_id"`
	Repo      string   `json:"repo"`
	Title     string   `json:"title"`
	Touches   []string `json:"touches"`
	Brief     string   `json:"brief"`
	Effort    string   `json:"effort"`
	Model     string   `json:"model"`
}

// TaskAdd is a validated task-add request, with its repository resolved to a project.
type TaskAdd struct {
	RequestID string
	TaskID    string
	ProjectID int64
	Title     string
	Touches   []string
	Brief     string
	Effort    string
	Model     string
}

// TaskAddResult is what `task-add` answers: the task as created, or as the first request
// created it when Replayed. Lint is the brief lint's report, "" on a replay, which runs no lint.
type TaskAddResult struct {
	TaskID      string    `json:"task_id"`
	ProjectID   int64     `json:"project_id"`
	Status      string    `json:"status"`
	HasBrief    bool      `json:"has_brief"`
	BriefSHA256 string    `json:"brief_sha256"`
	EventID     int64     `json:"event_id"`
	CreatedAt   time.Time `json:"created_at"`
	Replayed    bool      `json:"replayed"`
	Lint        string    `json:"lint"`
}

func taskAddResult(r db.TaskAddResult, lint string) TaskAddResult {
	return TaskAddResult{
		TaskID: SafeID(r.TaskID), ProjectID: r.ProjectID, Status: SafeText(r.Status), HasBrief: r.HasBrief,
		BriefSHA256: SafeText(r.BriefSHA256), EventID: r.EventID, CreatedAt: r.CreatedAt.UTC(),
		Replayed: r.Replayed, Lint: SafeText(lint),
	}
}

func serveTaskAdd(ctx context.Context, h Host, body []byte) (any, error) {
	var req TaskAddRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTaskAdd(req); err != nil {
		return nil, err
	}
	store, err := writeStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	// A repeat is answered before the lint runs again: the lint reaches the network, and a
	// retry after a timeout must get the first result even if the remote is down now.
	stored, ok, err := store.StoredTaskAdd(ctx, req.RequestID)
	if err != nil {
		return nil, storeErr(err)
	}
	if ok {
		if stored.TaskID != req.TaskID {
			return nil, Refuse(CodeConflict, "request id %s already added task %s", req.RequestID, SafeID(stored.TaskID))
		}
		return taskAddResult(stored, ""), nil
	}
	proj, ok, err := store.GetProjectByRepo(ctx, req.Repo)
	if err != nil {
		return nil, err
	}
	if !ok || proj.Status == "archived" {
		return nil, Refuse(CodeNotFound, "no active project is registered here at %q", req.Repo)
	}
	if h.AddTask == nil {
		return nil, Refuse(CodeInternal, "no task add is wired")
	}
	res, lint, err := h.AddTask(ctx, store, TaskAdd{
		RequestID: req.RequestID, TaskID: req.TaskID, ProjectID: proj.ID, Title: req.Title,
		Touches: req.Touches, Brief: req.Brief, Effort: req.Effort, Model: req.Model,
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return taskAddResult(res, lint), nil
}

// checkTaskAdd refuses a task-add request whose fields are missing or out of bounds, before
// anything is opened.
func checkTaskAdd(req TaskAddRequest) error {
	if err := db.ValidRequestID(req.RequestID); err != nil {
		return Refuse(CodeBadRequest, "%v", err)
	}
	if err := db.ValidateTaskID(req.TaskID); err != nil {
		return Refuse(CodeBadRequest, "%v", err)
	}
	if len(req.TaskID) > MaxTaskID {
		return Refuse(CodeBadRequest, "task id is %d bytes, over the %d byte limit", len(req.TaskID), MaxTaskID)
	}
	if req.Repo == "" || len(req.Repo) > maxRepoPath {
		return Refuse(CodeBadRequest, "repo must be the registered path of a repository here, 1 to %d bytes", maxRepoPath)
	}
	if len(req.Title) > maxTitle || !printable(req.Title) {
		return Refuse(CodeBadRequest, "title must be one printable line of at most %d bytes", maxTitle)
	}
	if len(req.Touches) > maxTouches {
		return Refuse(CodeBadRequest, "touches has %d entries, over the %d limit", len(req.Touches), maxTouches)
	}
	for _, t := range req.Touches {
		// --touches is comma-separated, and the request goes through the same parsing, so a
		// comma inside one entry would quietly become two.
		if t == "" || len(t) > maxTouchBytes || !printable(t) || strings.Contains(t, ",") {
			return Refuse(CodeBadRequest, "each touches entry must be a printable path of 1 to %d bytes with no comma", maxTouchBytes)
		}
	}
	if strings.TrimSpace(req.Brief) == "" {
		return Refuse(CodeBadRequest, "brief is required: a task without one is never dispatched")
	}
	if req.Effort != "" && !harness.ValidEffort(req.Effort) {
		return Refuse(CodeBadRequest, "effort %q is not one of %s", req.Effort, strings.Join(harness.EffortLevels, "|"))
	}
	if req.Model != "" && !harness.ValidModel(req.Model) {
		return Refuse(CodeBadRequest, "model %q is not an alias (%s) or a full model id", req.Model, strings.Join(harness.ModelAliases, "|"))
	}
	return nil
}

// printable reports whether s is valid UTF-8 with every rune graphic: one line, nothing hidden.
func printable(s string) bool {
	return validUTF8(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) < 0
}

// storeErr names the store's refusals.
func storeErr(err error) error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, db.ErrRequestReused), errors.Is(err, db.ErrTaskExists), errors.Is(err, db.ErrEscalationNotOpen):
		return Refuse(CodeConflict, "%v", err)
	case errors.Is(err, db.ErrEscalationNotFound):
		return Refuse(CodeNotFound, "%v", err)
	}
	return err
}

// --- goal --------------------------------------------------------------------------------

// GoalRequest is plain-language work for the peer's manager, the way the lead talks to a root
// manager.
type GoalRequest struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

// GoalResult is the manager event that carries the goal.
type GoalResult struct {
	EventID  int64 `json:"event_id"`
	Replayed bool  `json:"replayed"`
}

func serveGoal(ctx context.Context, h Host, body []byte) (any, error) {
	var req GoalRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := db.ValidRequestID(req.RequestID); err != nil {
		return nil, Refuse(CodeBadRequest, "%v", err)
	}
	if err := checkText(req.Text); err != nil {
		return nil, err
	}
	store, err := writeStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	res, err := store.RecordGoal(ctx, req.RequestID, req.Text)
	if err != nil {
		return nil, storeErr(err)
	}
	return GoalResult{EventID: res.EventID, Replayed: res.Replayed}, nil
}

// checkText refuses empty text and text over the store's cap. The store would cut it; over this
// channel nobody would see the cut, so it is refused instead.
func checkText(text string) error {
	if strings.TrimSpace(text) == "" {
		return Refuse(CodeBadRequest, "text is required")
	}
	if len(text) > db.MaxEscalationText {
		return Refuse(CodeBadRequest, "text is %d bytes, over the %d byte limit", len(text), db.MaxEscalationText)
	}
	return nil
}

// --- answer ------------------------------------------------------------------------------

// AnswerRequest answers one open escalation.
type AnswerRequest struct {
	RequestID    string `json:"request_id"`
	EscalationID int64  `json:"escalation_id"`
	Text         string `json:"text"`
}

// AnswerResult is the escalation as the answer left it, and the manager event that carries the
// answer.
type AnswerResult struct {
	EscalationID int64     `json:"escalation_id"`
	Kind         string    `json:"kind"`
	TaskID       string    `json:"task_id"`
	Status       string    `json:"status"`
	AnsweredBy   string    `json:"answered_by"`
	ResolvedAt   time.Time `json:"resolved_at"`
	EventID      int64     `json:"event_id"`
	Replayed     bool      `json:"replayed"`
}

func serveAnswer(ctx context.Context, h Host, body []byte) (any, error) {
	var req AnswerRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := db.ValidRequestID(req.RequestID); err != nil {
		return nil, Refuse(CodeBadRequest, "%v", err)
	}
	if req.EscalationID <= 0 {
		return nil, Refuse(CodeBadRequest, "escalation_id must be a positive escalation id")
	}
	if err := checkText(req.Text); err != nil {
		return nil, err
	}
	store, err := writeStore(h)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	// Recorded as relayed by the parent coordinator: not the lead, whose words these may be but
	// nothing here can verify, and not the local manager, which never saw them.
	res, err := store.AnswerEscalation(ctx, req.EscalationID, req.RequestID, req.Text, db.ActorParent)
	if err != nil {
		return nil, storeErr(err)
	}
	e := res.Escalation
	return AnswerResult{
		EscalationID: e.ID, Kind: SafeText(e.Kind), TaskID: SafeID(e.TaskID), Status: SafeText(e.Status),
		AnsweredBy: SafeText(e.AnsweredBy), ResolvedAt: e.ResolvedAt.UTC(), EventID: res.EventID, Replayed: res.Replayed,
	}, nil
}

// --- ensure-up ---------------------------------------------------------------------------

// EnsureUpResult is what `ensure-up` did: the notes from restoring the manager and workers, and
// what starting the scheduler did (orchestrator.StartScheduler's outcome).
type EnsureUpResult struct {
	Restored  []string `json:"restored"`
	Scheduler string   `json:"scheduler"`
}

func serveEnsureUp(ctx context.Context, h Host, body []byte) (any, error) {
	if err := decode(body, &struct{}{}); err != nil {
		return nil, err
	}
	if h.EnsureUp == nil {
		return nil, Refuse(CodeInternal, "no ensure-up is wired")
	}
	res, err := h.EnsureUp(ctx)
	if err != nil {
		return nil, err
	}
	out := EnsureUpResult{Restored: []string{}, Scheduler: SafeText(res.Scheduler)}
	for _, n := range res.Restored {
		out.Restored = append(out.Restored, SafeText(n))
	}
	return out, nil
}

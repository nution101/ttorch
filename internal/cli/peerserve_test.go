package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/peer"
)

// These tests run `ttorch peer serve` as its own process, the way sshd runs a forced command:
// the verb in SSH_ORIGINAL_COMMAND, the body on stdin, the response on stdout and the status as
// the exit code. The process is this test binary re-entered through Main (see TestMain), which
// is the function cmd/ttorch's main calls, so what runs is the CLI's own dispatch with its own
// environment, working directory and file descriptors.

// serveHome is one peer account for the served process. account is the user's home directory
// and home the ttorch home, which the served process takes from peerAccount (set from
// testPeerHomeEnv and testPeerTtorchEnv in TestMain), never from the session's environment.
// peer.env in the ttorch home is the peer's own configuration. In production the ttorch home is
// <account>/.ttorch; here it is a separate temp dir, because the store's test guard refuses
// anything under $HOME/.ttorch and the served process sets HOME to the account.
type serveHome struct {
	account, home, db, dir string
	extraEnv               []string
}

// servedParent is the coordinator id of the parent a served home is provisioned under.
const servedParent = "0123456789abcdef0123456789abcdef"

// parentBody puts servedParent's id in a request body, as the parent's client does.
func parentBody(body string) string {
	return strings.Replace(body, "{", `{"parent_id":"`+servedParent+`",`, 1)
}

func newServeHome(t *testing.T) serveHome {
	t.Helper()
	account, home := t.TempDir(), t.TempDir()
	h := serveHome{account: account, home: home, db: filepath.Join(home, "state.db"), dir: t.TempDir()}
	s, err := db.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProvisionAsPeer(context.Background(), "served", servedParent, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.writePeerEnv(t,
		"PATH="+os.Getenv("PATH"),
		// A session name of its own, so the summary's window checks never read a real fleet.
		"TTORCH_TMUX_SESSION=ttorch-serve-test-"+filepath.Base(account),
		"TTORCH_SCHEDULER_AUTOSTART=0",
	)
	return h
}

// writePeerEnv replaces the peer's own configuration file with lines.
func (h serveHome) writePeerEnv(t *testing.T, lines ...string) {
	t.Helper()
	body := "# written by the test\n\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(h.home, "peer.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h serveHome) store(t *testing.T) *db.Store {
	t.Helper()
	return reopen(t, h.db)
}

func (h serveHome) paths() paths.Paths { return paths.Paths{Home: h.home} }

type served struct {
	code   int
	resp   peer.Response
	raw    string
	stderr string
}

// serveRun runs one request. The working directory is a temp dir, never this worktree, whose
// .ttorch/task would make the child a worker context.
func serveRun(t *testing.T, h serveHome, command, body string, args ...string) served {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"peer", "serve"}, args...)...)
	cmd.Dir = h.dir
	// HOME and PATH are what sshd would set; the served process replaces both (see
	// peerControlEnv), so the test's own values must not be what makes it work.
	cmd.Env = append([]string{
		runMainEnv + "=1",
		testPeerHomeEnv + "=" + h.account,
		testPeerTtorchEnv + "=" + h.home,
		"SSH_ORIGINAL_COMMAND=" + command,
		"HOME=" + h.account,
		"PATH=" + os.Getenv("PATH"),
	}, h.extraEnv...)
	cmd.Stdin = strings.NewReader(body)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running peer serve: %v", err)
	}
	s := served{code: code, raw: out.String(), stderr: errOut.String()}
	if len(args) == 0 {
		dec := json.NewDecoder(strings.NewReader(s.raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s.resp); err != nil {
			t.Fatalf("%q: stdout is not one response: %v\nstdout: %s\nstderr: %s", command, err, s.raw, s.stderr)
		}
		if s.resp.Protocol != peer.ProtocolVersion {
			t.Fatalf("%q: protocol = %d", command, s.resp.Protocol)
		}
	}
	return s
}

func (s served) refused(t *testing.T, label, code string) {
	t.Helper()
	if s.code != 1 || s.resp.OK || s.resp.Error == nil || s.resp.Error.Code != code {
		t.Errorf("%s: exit %d, response %s; want exit 1 refused with %s", label, s.code, s.raw, code)
	}
}

func (s served) result(t *testing.T, v any) {
	t.Helper()
	if s.code != 0 || !s.resp.OK {
		t.Fatalf("want success, got exit %d: %s\nstderr: %s", s.code, s.raw, s.stderr)
	}
	b, err := json.Marshal(s.resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("result does not decode as %T: %v\n%s", v, err, b)
	}
}

// TestPeerServeRefusesWhatIsNotAVerb: the gate's and the approval's verbs, and any shell, are
// refused with a named error, and a shell construct in the command runs nothing.
func TestPeerServeRefusesWhatIsNotAVerb(t *testing.T) {
	h := newServeHome(t)
	// The served process runs in h.dir, so a shell that ran `touch x` would create this file.
	// The relative name keeps each command under peer.MaxCommand; the last case is over it.
	marker := filepath.Join(h.dir, "x")
	cases := []struct{ command, code string }{
		{"approve t1", peer.CodeUnknownVerb},
		{"approve", peer.CodeUnknownVerb},
		{"merge-local t1", peer.CodeUnknownVerb},
		{"land --all", peer.CodeUnknownVerb},
		{"trust prep t1", peer.CodeUnknownVerb},
		{"trust record t1", peer.CodeUnknownVerb},
		{"sh -c 'touch x'", peer.CodeUnknownVerb},
		{"/bin/sh -c 'touch x'", peer.CodeUnknownVerb},
		{"ttorch approve t1", peer.CodeUnknownVerb},
		{"summary; touch x", peer.CodeUnknownVerb},
		{"summary;touch x", peer.CodeUnknownVerb},
		{"summary && touch x", peer.CodeBadCommand},
		{"summary | touch x", peer.CodeBadCommand},
		{"$(touch x)", peer.CodeUnknownVerb},
		{"`touch x`", peer.CodeUnknownVerb},
		{"sh -c 'touch " + marker + "'" + strings.Repeat(" ", peer.MaxCommand), peer.CodeBadCommand},
		{"", peer.CodeNoCommand},
	}
	for _, c := range cases {
		serveRun(t, h, c.command, `{}`).refused(t, c.command, c.code)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a command string ran a shell: %s exists (%v)", marker, err)
	}

	// The forced command is exactly `ttorch peer serve`; a verb typed after it is refused.
	if s := serveRun(t, h, "summary", "", "summary"); s.code != 2 || !strings.Contains(s.stderr, "takes no arguments") {
		t.Errorf("peer serve with an argument: exit %d, stderr %q", s.code, s.stderr)
	}
	// A worker context is refused, however the request looks.
	h.extraEnv = []string{"TTORCH_TASK_ID=t1"}
	serveRun(t, h, "goal", `{"request_id":"r1","text":"x"}`).refused(t, "worker context", peer.CodeWorkerContext)
}

// TestPeerServeRefusesAnOversizeBody: a 2 MiB body is refused before it is parsed.
func TestPeerServeRefusesAnOversizeBody(t *testing.T) {
	h := newServeHome(t)
	big := `{"request_id":"r1","text":"` + strings.Repeat("g", 2<<20) + `"}`
	serveRun(t, h, "goal", big).refused(t, "2 MiB goal", peer.CodeBodyTooLarge)
	serveRun(t, h, "summary", strings.Repeat(" ", 2<<20)).refused(t, "2 MiB of padding", peer.CodeBodyTooLarge)
	if evs := managerEventsIn(t, h.store(t)); len(evs) != 0 {
		t.Errorf("an oversize goal appended %d events", len(evs))
	}
}

func managerEventsIn(t *testing.T, s *db.Store) []db.Event {
	t.Helper()
	all, err := s.EventsSince(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Event
	for _, e := range all {
		if e.EntityType == db.EntityTypeManager {
			out = append(out, e)
		}
	}
	return out
}

func countIn(t *testing.T, s *db.Store, table string) int {
	t.Helper()
	tasks, err := s.ListTasks(context.Background(), db.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.EventsSince(context.Background(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	switch table {
	case "tasks":
		return len(tasks)
	case "events":
		return len(events)
	}
	t.Fatalf("countIn: unknown table %s", table)
	return 0
}

// TestPeerServeTaskAdd: a briefed task-add creates a pending row with has_brief set and the brief
// stored, recorded as the parent's, after the brief lint passed. A brief the lint refuses
// creates nothing. A repeated request id returns the first result and creates nothing.
func TestPeerServeTaskAdd(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	repo := lintRepo(t)
	s := h.store(t)
	if _, err := s.UpsertProject(ctx, repo, "fixture"); err != nil {
		t.Fatal(err)
	}

	req := func(requestID, taskID, brief string) string {
		b, err := json.Marshal(peer.TaskAddRequest{
			ParentID: servedParent, RequestID: requestID, TaskID: taskID, Repo: repo, Title: "from the parent",
			Touches: []string{"pkg/thing.go"}, Brief: brief, Effort: "high", Model: "opus",
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	bad := serveRun(t, h, "task-add", req("add-bad", "p-bad", defectiveBrief))
	bad.refused(t, "a defective brief", peer.CodeBriefLint)
	if bad.resp.Error != nil && !strings.Contains(bad.resp.Error.Detail, "violation") {
		t.Errorf("the lint refusal does not carry the lint's report: %+v", bad.resp.Error)
	}
	if _, ok, _ := s.GetTask(ctx, "p-bad"); ok {
		t.Fatal("a refused brief created a task")
	}
	if _, err := os.Stat(h.paths().BriefPath("p-bad")); !os.IsNotExist(err) {
		t.Fatalf("a refused brief was stored: %v", err)
	}

	first := serveRun(t, h, "task-add", req("add-1", "p-1", cleanBrief))
	var got peer.TaskAddResult
	first.result(t, &got)
	sum := sha256.Sum256([]byte(cleanBrief))
	if got.TaskID != "p-1" || got.Status != db.StatusPending || !got.HasBrief || got.Replayed ||
		got.BriefSHA256 != hex.EncodeToString(sum[:]) || !strings.Contains(got.Lint, "5 of 5 rules ran: passed") {
		t.Errorf("task-add result = %+v", got)
	}
	task := mustTask(t, s, "p-1")
	if !task.HasBrief || task.Status != db.StatusPending || task.CreatedBy != db.ActorParent ||
		task.Title != "from the parent" || task.Effort != "high" || task.Model != "opus" ||
		strings.Join(task.Footprint, ",") != "pkg/thing.go" {
		t.Errorf("task row = %+v", task)
	}
	if stored := mustReadFile(t, h.paths().BriefPath("p-1")); stored != cleanBrief {
		t.Errorf("stored brief = %q", stored)
	}

	tasks, events := countIn(t, s, "tasks"), countIn(t, s, "events")
	again := serveRun(t, h, "task-add", req("add-1", "p-1", cleanBrief+"\nchanged"))
	var replay peer.TaskAddResult
	again.result(t, &replay)
	if !replay.Replayed || replay.EventID != got.EventID || replay.BriefSHA256 != got.BriefSHA256 || replay.CreatedAt != got.CreatedAt {
		t.Errorf("repeat = %+v, want the first result %+v replayed", replay, got)
	}
	if n := countIn(t, s, "tasks"); n != tasks {
		t.Errorf("a repeat created %d tasks", n-tasks)
	}
	if n := countIn(t, s, "events"); n != events {
		t.Errorf("a repeat appended %d events", n-events)
	}
	if stored := mustReadFile(t, h.paths().BriefPath("p-1")); stored != cleanBrief {
		t.Errorf("a repeat rewrote the brief: %q", stored)
	}

	serveRun(t, h, "task-add", req("add-1", "p-other", cleanBrief)).refused(t, "request id reused for another task", peer.CodeConflict)
	serveRun(t, h, "task-add", req("add-2", "p-1", cleanBrief)).refused(t, "task id taken", peer.CodeConflict)
	if n := countIn(t, s, "tasks"); n != tasks {
		t.Errorf("the refusals created %d tasks", n-tasks)
	}
}

// serveFileState is a file's size, mtime and contents, or its absence.
type serveFileState struct {
	exists bool
	size   int64
	mtime  int64
	sum    [32]byte
}

func stateOfFile(t *testing.T, path string) serveFileState {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return serveFileState{}
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return serveFileState{exists: true, size: fi.Size(), mtime: fi.ModTime().UnixNano(), sum: sha256.Sum256(b)}
}

// TestPeerServeReadsWithoutWriting: summary and version leave the store's main file and its
// WAL byte for byte as they were, and against a store behind this binary's schema they are
// refused rather than migrating it. `ttorch summary` itself migrates, imports and seeds through
// mgr(); the served verbs must not.
func TestPeerServeReadsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	s, err := db.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.UpsertProject(ctx, "/repos/served", "served")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "t-served", ProjectID: proj.ID, Window: "w-served"}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportStatus(ctx, "t-served", db.StatusDone, "worker:t-served", "done"); err != nil {
		t.Fatal(err)
	}
	// A done task with an unsynced approval_required: `ttorch summary` would open an approval
	// escalation for it, which is a write.
	if _, err := s.AppendEvent(ctx, db.Event{EntityType: db.EntityTypeTask, EntityID: "t-served", Type: db.EventApprovalRequired, Actor: db.ActorSystem, Actionable: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	before, wal := stateOfFile(t, h.db), stateOfFile(t, h.db+"-wal")
	sum := serveRun(t, h, "summary", "")
	var got peer.Summary
	sum.result(t, &got)
	if got.SchemaVersion != peer.SchemaVersion || got.Tasks.ByStatus[db.StatusDone] != 1 || len(got.Repos) != 1 {
		t.Errorf("summary = %s", sum.raw)
	}
	var v peer.VersionResult
	serveRun(t, h, "version", "").result(t, &v)
	if v.Protocol != peer.ProtocolVersion || v.Coordinator.Role != db.CoordinatorPeer || v.Coordinator.ParentID != servedParent || v.Version == "" {
		t.Errorf("version = %+v", v)
	}
	if after := stateOfFile(t, h.db); after != before {
		t.Errorf("summary or version changed the main file: before %+v, after %+v", before, after)
	}
	if after := stateOfFile(t, h.db+"-wal"); wal.exists && after != wal || !wal.exists && after.size != 0 {
		t.Errorf("summary or version wrote to the WAL: before %+v, after %+v", wal, after)
	}
	r := reopen(t, h.db)
	if open, _, err := r.EscalationOpenCounts(ctx); err != nil || open != 0 {
		t.Errorf("open escalations after a served summary = %d (%v), want 0: the summary must not sync", open, err)
	}

	// A store behind the schema is refused, not migrated.
	behind := newServeHome(t)
	b, err := db.Open(behind.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.MigrateDown(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	before = stateOfFile(t, behind.db)
	serveRun(t, behind, "summary", "").refused(t, "summary of a store behind the schema", peer.CodeUnavailable)
	serveRun(t, behind, "version", "").refused(t, "version of a store behind the schema", peer.CodeUnavailable)
	if after := stateOfFile(t, behind.db); after != before {
		t.Errorf("a refused read changed the store: before %+v, after %+v", before, after)
	}
	if after := stateOfFile(t, behind.db+"-wal"); after.size != 0 {
		t.Errorf("a refused read wrote to the WAL: %+v", after)
	}
}

// TestPeerServeGoalAnswerDecisions drives the other mutating verbs through the binary: both are
// recorded as the parent coordinator's, never the lead's or the local manager's, and a repeat of
// either request id appends nothing.
func TestPeerServeGoalAnswerDecisions(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	s := h.store(t)
	proj, err := s.UpsertProject(ctx, "/repos/q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "t-q", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	esc, err := s.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "which one?")
	if err != nil {
		t.Fatal(err)
	}

	var d peer.DecisionsResult
	serveRun(t, h, "decisions", `{"since":0}`).result(t, &d)
	if d.Open != 1 || len(d.Escalations) != 1 || d.Escalations[0].Body != "which one?" {
		t.Errorf("decisions = %+v", d)
	}

	var g peer.GoalResult
	serveRun(t, h, "goal", parentBody(`{"request_id":"goal-1","text":"tidy the importer"}`)).result(t, &g)
	var a peer.AnswerResult
	answer := parentBody(`{"request_id":"ans-1","escalation_id":` + itoa(esc.ID) + `,"text":"the first one"}`)
	serveRun(t, h, "answer", answer).result(t, &a)
	if a.AnsweredBy != db.ActorParent || a.Status != db.EscalationAnswered {
		t.Errorf("answer = %+v", a)
	}
	var g2 peer.GoalResult
	serveRun(t, h, "goal", parentBody(`{"request_id":"goal-1","text":"tidy the importer"}`)).result(t, &g2)
	var a2 peer.AnswerResult
	serveRun(t, h, "answer", answer).result(t, &a2)
	if !g2.Replayed || g2.EventID != g.EventID || !a2.Replayed || a2.EventID != a.EventID {
		t.Errorf("repeats = %+v, %+v; want both replayed", g2, a2)
	}
	evs := managerEventsIn(t, s)
	if len(evs) != 2 {
		t.Fatalf("manager events = %+v, want one goal and one answer", evs)
	}
	for _, e := range evs {
		if e.Actor != db.ActorParent {
			t.Errorf("event %d (%s) recorded as %q, want %q", e.ID, e.Type, e.Actor, db.ActorParent)
		}
	}
}

// TestPeerServeRefusesAnotherParent: through the binary, a goal and an answer that name any
// coordinator but the one the peer's row records are refused and append nothing.
func TestPeerServeRefusesAnotherParent(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	s := h.store(t)
	proj, err := s.UpsertProject(ctx, "/repos/q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, db.Task{ID: "t-q", ProjectID: proj.ID}, db.ActorManager); err != nil {
		t.Fatal(err)
	}
	esc, err := s.OpenEscalation(ctx, "t-q", db.EscalationQuestion, "which one?")
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("e", 32)
	events := countIn(t, s, "events")
	serveRun(t, h, "goal", `{"parent_id":"`+other+`","request_id":"goal-1","text":"tidy"}`).refused(t, "another parent's goal", peer.CodeWrongParent)
	serveRun(t, h, "answer", `{"parent_id":"`+other+`","request_id":"ans-1","escalation_id":`+itoa(esc.ID)+`,"text":"x"}`).refused(t, "another parent's answer", peer.CodeWrongParent)
	if n := countIn(t, s, "events"); n != events {
		t.Errorf("another parent's requests appended %d events", n-events)
	}
	if e, _, _ := s.GetEscalation(ctx, esc.ID); e.Status != db.EscalationOpen {
		t.Errorf("another parent's answer changed the escalation: %+v", e)
	}
}

// TestPeerServeEnsureUpNeedsAManager: ensure-up restores what was there; with no manager
// recorded it starts nothing and says why.
func TestPeerServeEnsureUpNeedsAManager(t *testing.T) {
	h := newServeHome(t)
	serveRun(t, h, "ensure-up", "").refused(t, "ensure-up with no manager", peer.CodeNoManager)
}

// envDump reads a file `env` wrote: one KEY=VALUE per line.
func envDump(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestPeerServeEnsureUpPassesOnlyItsOwnEnvironment runs ensure-up from a session whose
// environment carries what an sshd AcceptEnv could let a client send: locale variables, ttorch
// path overrides, git variables, ssh session variables, a shell and a temp dir of its choosing.
// ensure-up starts tmux (the server every restored window and worker inherits from) and the
// scheduler daemon. A fake tmux on the peer's PATH and a fake installed ttorch record the
// environment each was started with, and neither may see any of it. The store and paths come
// from the account's home, so a session TTORCH_HOME or TTORCH_DB steers nothing.
func TestPeerServeEnsureUpPassesOnlyItsOwnEnvironment(t *testing.T) {
	ctx := context.Background()
	h := newServeHome(t)
	dumps := t.TempDir()

	fakeBin := t.TempDir()
	tmuxScript := "#!/bin/sh\nenv > \"" + dumps + "/tmux.$$.env\"\n" +
		"case \"$1\" in\n-V) echo 'tmux 3.5a' ;;\nhas-session) exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(tmuxScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// StartScheduler forks the installed binary, paths.Binary under the ttorch home.
	if err := os.MkdirAll(filepath.Join(h.home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := "#!/bin/sh\nenv > \"" + dumps + "/scheduler.env.tmp\" && mv \"" + dumps + "/scheduler.env.tmp\" \"" + dumps + "/scheduler.env\"\n"
	if err := os.WriteFile(filepath.Join(h.home, "bin", "ttorch"), []byte(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	session := "ttorch-serve-env-" + filepath.Base(h.account)
	peerPath := fakeBin + ":/usr/bin:/bin"
	h.writePeerEnv(t, "PATH="+peerPath, "TTORCH_TMUX_SESSION="+session, "TTORCH_MODEL=opus")
	if err := h.store(t).SetManager(ctx, db.Manager{Dir: h.dir, SessionID: "sid-1"}); err != nil {
		t.Fatal(err)
	}

	// The decoy is a second, complete ttorch home with no manager recorded: if the session's
	// TTORCH_HOME or TTORCH_DB were honoured, ensure-up would read it and refuse.
	decoy := newServeHome(t)
	evilShell := filepath.Join(dumps, "evil-shell")
	if err := os.WriteFile(evilShell, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := map[string]string{
		"LC_ALL":            "hostile-lc-all",
		"LC_CTYPE":          "hostile-lc-ctype",
		"LANG":              "hostile-lang",
		"TTORCH_HOME":       decoy.home,
		"TTORCH_DB":         decoy.db,
		"TTORCH_MODEL":      "hostile-model",
		"TTORCH_BACKEND":    "hostile-backend",
		"GIT_DIR":           filepath.Join(dumps, "hostile-git"),
		"GIT_CONFIG_GLOBAL": filepath.Join(dumps, "hostile-gitconfig"),
		"SSH_CONNECTION":    "198.51.100.7 50000 192.0.2.1 22",
		"SSH_CLIENT":        "198.51.100.7 50000 22",
		"SHELL":             evilShell,
		"TMPDIR":            "/tmp",
		"LD_PRELOAD":        filepath.Join(dumps, "hostile.so"),
		"HOSTILE_MARKER":    "1",
	}
	for k, v := range hostile {
		h.extraEnv = append(h.extraEnv, k+"="+v)
	}

	s := serveRun(t, h, "ensure-up", "")
	var res peer.EnsureUpResult
	s.result(t, &res)
	if res.Scheduler != "started" {
		t.Fatalf("ensure-up = %+v, want the scheduler started", res)
	}
	schedEnv := filepath.Join(dumps, "scheduler.env")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(schedEnv); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler daemon never recorded its environment")
		}
		time.Sleep(50 * time.Millisecond)
	}

	started := map[string]map[string]string{"scheduler daemon": envDump(t, schedEnv)}
	tmuxDumps, err := filepath.Glob(filepath.Join(dumps, "tmux.*.env"))
	if err != nil || len(tmuxDumps) == 0 {
		t.Fatalf("tmux was never started (%v)", err)
	}
	for _, d := range tmuxDumps {
		started["tmux "+filepath.Base(d)] = envDump(t, d)
	}
	for who, env := range started {
		for k, v := range hostile {
			if got, ok := env[k]; ok && (got == v || k == "HOSTILE_MARKER" || strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "SSH_") || strings.HasPrefix(k, "LC_") || k == "LD_PRELOAD") {
				t.Errorf("%s was started with the session's %s=%q", who, k, got)
			}
		}
		want := map[string]string{
			"HOME":                h.account,
			"PATH":                peerPath,
			"LANG":                peerLang,
			"TTORCH_TMUX_SESSION": session,
			"TTORCH_MODEL":        "opus",
			"TTORCH_WORKER_TABS":  "0",
		}
		for k, v := range want {
			if env[k] != v {
				t.Errorf("%s: %s = %q, want %q", who, k, env[k], v)
			}
		}
		if sh := env["SHELL"]; sh == evilShell || !strings.HasPrefix(sh, "/") {
			t.Errorf("%s: SHELL = %q, want a shell listed in /etc/shells", who, sh)
		}
	}
	// The launch command typed into the restored manager window reached the account's store:
	// the decoy has no manager, and ensure-up would have refused on it.
	if _, ok, _ := decoy.store(t).GetManager(ctx); ok {
		t.Fatal("setup: the decoy has a manager")
	}
}

// TestPeerEnvFile covers what the peer's own configuration may set: PATH and TTORCH_* settings,
// as written, and never the store's location, a worker's identity, or a non-printing value.
// Anything it refuses is refused whole, with the line named.
func TestPeerEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer.env")
	if got, err := readPeerEnv(path); err != nil || len(got) != 0 {
		t.Fatalf("a missing file = %v, %v; want nothing set", got, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("# peer settings\n\nPATH=/opt/x/bin:/usr/bin\nTTORCH_MODEL=opus\nTTORCH_TMUX_SESSION=peer b\n  # indented comment\n")
	got, err := readPeerEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"PATH": "/opt/x/bin:/usr/bin", "TTORCH_MODEL": "opus", "TTORCH_TMUX_SESSION": "peer b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("readPeerEnv = %v, want %v", got, want)
	}
	for _, bad := range []string{
		"TTORCH_HOME=/elsewhere", "TTORCH_DB=/elsewhere/state.db", "TTORCH_TASK_ID=t1",
		"TTORCH_WORKER_TABS=1", "TTORCH_CLAUDE_DIR=/x", "LC_ALL=C", "LD_PRELOAD=/x.so",
		"GIT_DIR=/x", "ttorch_model=opus", "TTORCH_=x", "TTORCH_MODEL", "TTORCH_MODEL=a\x1bb",
		"TTORCH_MODEL=a\rb", "export TTORCH_MODEL=opus",
	} {
		write("TTORCH_MODEL=opus\n" + bad + "\n")
		if got, err := readPeerEnv(path); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%q: readPeerEnv = %v, %v; want it refused at line 2", bad, got, err)
		}
	}
	write("PATH=" + strings.Repeat("p", maxPeerEnv))
	if _, err := readPeerEnv(path); err == nil {
		t.Error("an oversize peer.env was read")
	}
}

// TestPeerControlEnvKeepsOnlyWhatItVouchesFor runs peerControlEnv in this process (restoring the
// environment after) and checks the two session values it can keep are kept only when safe: a
// TMPDIR this account owns that no one else can write, and a SHELL /etc/shells lists.
func TestPeerControlEnvKeepsOnlyWhatItVouchesFor(t *testing.T) {
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(k, v)
			}
		}
	})
	account, ttorchHome := t.TempDir(), t.TempDir()
	orig := peerAccount
	t.Cleanup(func() { peerAccount = orig })
	peerAccount = func() (peerUser, error) { return peerUser{home: account, name: "p", ttorchHome: ttorchHome}, nil }

	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	var listed string
	if b, err := os.ReadFile("/etc/shells"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "/") {
				listed = l
				break
			}
		}
	}
	cases := []struct {
		label, shell, tmpdir, wantShell, wantTmp string
	}{
		{"private tmpdir, listed shell", listed, private, listed, private},
		{"world-writable tmpdir", listed, open, listed, ""},
		{"root's /tmp", listed, "/tmp", listed, ""},
		{"relative tmpdir", listed, "tmp", listed, ""},
		{"unlisted shell", "/tmp/evil-shell", private, "/bin/sh", private},
		{"relative shell", "sh", "", "/bin/sh", ""},
	}
	for _, c := range cases {
		if c.shell == "" && listed == "" {
			continue
		}
		os.Clearenv()
		_ = os.Setenv("SHELL", c.shell)
		_ = os.Setenv("TMPDIR", c.tmpdir)
		_ = os.Setenv("TTORCH_HOME", "/elsewhere")
		_ = os.Setenv("LC_ALL", "hostile")
		p, err := peerControlEnv()
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		if p.Home != ttorchHome || p.StateDB() != filepath.Join(ttorchHome, "state.db") {
			t.Errorf("%s: paths = %+v, want the account's ttorch home", c.label, p)
		}
		if got := os.Getenv("SHELL"); got != c.wantShell {
			t.Errorf("%s: SHELL = %q, want %q", c.label, got, c.wantShell)
		}
		if got := os.Getenv("TMPDIR"); got != c.wantTmp {
			t.Errorf("%s: TMPDIR = %q, want %q", c.label, got, c.wantTmp)
		}
		for _, k := range []string{"TTORCH_HOME", "LC_ALL"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Errorf("%s: %s=%q survived", c.label, k, v)
			}
		}
	}

	// A home that is not this account's directory is refused, and the session is gone anyway.
	peerAccount = func() (peerUser, error) { return peerUser{home: "/", name: "p", ttorchHome: ttorchHome}, nil }
	os.Clearenv()
	_ = os.Setenv("HOSTILE_MARKER", "1")
	if _, err := peerControlEnv(); err == nil && os.Getuid() != 0 {
		t.Error("a home owned by another account was accepted")
	}
	if _, ok := os.LookupEnv("HOSTILE_MARKER"); ok {
		t.Error("a refused environment kept the session's variables")
	}
}

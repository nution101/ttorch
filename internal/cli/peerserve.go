package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

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

// cmdPeerServe answers one request (internal/peer/serve.go) against this account's ttorch home.
//
// The first thing it does is replace its own environment (peerControlEnv), before a path is
// resolved or a process started. The session's environment is the client's to shape: whatever
// the peer's sshd AcceptEnv admits (LANG and LC_* on stock configs, anything if widened) arrives
// in it. ensure-up starts the tmux server every restored window and worker inherits from, and the
// scheduler daemon, so a variable left here would reach all of them, and TTORCH_HOME, TTORCH_DB,
// TTORCH_TMUX_SESSION or TTORCH_BACKEND would choose which store and session this process acts on.
func cmdPeerServe(stdin io.Reader, stdout, stderr io.Writer) int {
	command := os.Getenv("SSH_ORIGINAL_COMMAND")
	// The caller's context is judged on the environment it came with, before that is replaced.
	signal := workerContextSignal()
	p, err := peerControlEnv()
	host := peerHost(p)
	if err != nil {
		host = peer.Host{Unavailable: peer.Refuse(peer.CodeUnavailable, "the peer's control environment: %v", err)}
	}
	return peer.Serve(context.Background(), command, stdin, stdout, stderr, signal, host)
}

// peerUser is the account the control channel runs as: its home directory, its name, and its
// ttorch home (<home>/.ttorch).
type peerUser struct{ home, name, ttorchHome string }

// peerAccount looks the account up in the user database. It is a variable so a test that runs
// the served process can give it a temp home (cli_test.go TestMain).
var peerAccount = currentAccount

func currentAccount() (peerUser, error) {
	u, err := user.Current()
	if err != nil {
		return peerUser{}, fmt.Errorf("looking up this account: %w", err)
	}
	return peerUser{home: u.HomeDir, name: u.Username, ttorchHome: filepath.Join(u.HomeDir, ".ttorch")}, nil
}

// peerEnvFile is the peer's own configuration for the control channel, in the ttorch home. Only
// a process running as the peer's user writes it; the parent has no verb that can.
const peerEnvFile = "peer.env"

// maxPeerEnv bounds peer.env. It holds a handful of settings.
const maxPeerEnv = 64 << 10

// peerLang is the locale every process the channel starts gets, whatever the session asked for.
var peerLang = func() string {
	if runtime.GOOS == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}()

// peerEnvRefused are the TTORCH_* keys peer.env may not set: every path override (the store and
// the other paths come from the account's home; the validate cache holds the green results a
// trusted merge reuses; TTORCH_CLAUDE_JSON names the claude config ttorch writes folder trust
// into), a worker's identity, which is never the control channel's, and worker tabs, always off
// for a process with no screen. TestPeerEnvClassifiesEveryVariable fails on any TTORCH_* name
// ttorch reads that is neither here nor among the settings peer.env may hold.
var peerEnvRefused = map[string]bool{
	"TTORCH_HOME": true, "TTORCH_DB": true, "TTORCH_CLAUDE_DIR": true, "TTORCH_AGENTS_DIR": true,
	"TTORCH_BIN_DIR": true, "TTORCH_VALIDATE_CACHE_DIR": true, "TTORCH_CLAUDE_JSON": true,
	"TTORCH_TASK_ID": true, "TTORCH_TASK": true, "TTORCH_WORKER_TABS": true,
}

// peerControlEnv replaces this process's environment with one built only from the account and
// the peer's own configuration, and returns the paths to act on. Nothing in the session's
// environment survives except a SHELL that /etc/shells lists and a TMPDIR the account owns that
// nobody else can write:
//
//	HOME, USER, LOGNAME  from the user database (peerAccount), not the session
//	SHELL                the session's, if /etc/shells lists it; otherwise /bin/sh
//	PATH                 peer.env's PATH, otherwise a fixed default
//	TMPDIR               the session's, if it is a directory this account owns, writable by
//	                     no one else; otherwise unset
//	LANG                 fixed (peerLang); no LC_* at all
//	TTORCH_*             what peer.env sets, never the session's; TTORCH_WORKER_TABS=0
//
// The paths are the account's: <home>/.ttorch for the store, whatever TTORCH_HOME or TTORCH_DB
// said. The environment is replaced first, even when building it fails, so a refusal still runs
// with nothing of the session's.
//
// Where the user database cannot answer (a pure-Go build on a system whose account is not in
// /etc/passwd), os/user falls back to $HOME and $USER, which sshd sets from the account; only an
// AcceptEnv that admits HOME could change them, and the home must still be a directory this
// account owns.
func peerControlEnv() (paths.Paths, error) {
	session := map[string]string{"SHELL": os.Getenv("SHELL"), "TMPDIR": os.Getenv("TMPDIR")}
	os.Clearenv()
	u, err := peerAccount()
	if err != nil {
		return paths.Paths{}, err
	}
	if err := ownedDir(u.home, false); err != nil {
		return paths.Paths{}, fmt.Errorf("home directory %s: %w", u.home, err)
	}
	conf, err := readPeerEnv(filepath.Join(u.ttorchHome, peerEnvFile))
	if err != nil {
		return paths.Paths{}, err
	}
	env := map[string]string{
		"HOME": u.home, "USER": u.name, "LOGNAME": u.name, "LANG": peerLang,
		"SHELL": "/bin/sh", "PATH": defaultPeerPath(u), "TTORCH_WORKER_TABS": "0",
	}
	if listedShell(session["SHELL"]) {
		env["SHELL"] = session["SHELL"]
	}
	if t := session["TMPDIR"]; t != "" && ownedDir(t, true) == nil {
		env["TMPDIR"] = filepath.Clean(t)
	}
	for k, v := range conf {
		env[k] = v
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			return paths.Paths{}, err
		}
	}
	return paths.Paths{
		Home:     u.ttorchHome,
		Claude:   filepath.Join(u.home, ".claude"),
		Agents:   filepath.Join(u.home, ".agents"),
		LocalBin: filepath.Join(u.home, ".local", "bin"),
	}, nil
}

// defaultPeerPath is the PATH when peer.env sets none: the account's own bin directories, then
// the system's. Fixed, so a session PATH never chooses which tmux or claude runs.
func defaultPeerPath(u peerUser) string {
	return strings.Join([]string{
		filepath.Join(u.ttorchHome, "bin"), filepath.Join(u.home, ".local", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}, string(os.PathListSeparator))
}

// readPeerEnv reads peer.env: KEY=VALUE lines, with blank lines and # comments ignored. A key is
// PATH or TTORCH_ followed by capitals, digits and underscores, and not one of peerEnvRefused; a
// value is the rest of the line as written, printable, with no quoting or expansion. A missing
// file sets nothing. Anything else in it is refused, line named, rather than half-applied.
func readPeerEnv(path string) (map[string]string, error) {
	out := map[string]string{}
	b, err := readCapped(path, maxPeerEnv+1)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > maxPeerEnv {
		return nil, fmt.Errorf("%s is over %d bytes", path, maxPeerEnv)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("%s line %d: want KEY=VALUE", path, i+1)
		case !peerEnvKey(k):
			return nil, fmt.Errorf("%s line %d: %q is not a key it may set (PATH, or TTORCH_* other than the path and identity settings)", path, i+1, k)
		case !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0:
			return nil, fmt.Errorf("%s line %d: the value of %s holds a non-printing character", path, i+1, k)
		}
		out[k] = v
	}
	return out, nil
}

func peerEnvKey(k string) bool {
	if k == "PATH" {
		return true
	}
	rest, ok := strings.CutPrefix(k, "TTORCH_")
	if !ok || rest == "" || peerEnvRefused[k] {
		return false
	}
	for _, r := range rest {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// listedShell reports whether sh is an absolute path /etc/shells lists.
func listedShell(sh string) bool {
	if !filepath.IsAbs(sh) {
		return false
	}
	b, err := readCapped("/etc/shells", 64<<10)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == sh {
			return true
		}
	}
	return false
}

// ownedDir reports why dir is not an absolute directory owned by this process's user, or, with
// private set, why it is writable by its group or by others.
func ownedDir(dir string, private bool) error {
	if !filepath.IsAbs(dir) {
		return errors.New("not an absolute path")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a directory")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errors.New("not owned by this account")
	}
	if private && fi.Mode().Perm()&0o022 != 0 {
		return errors.New("writable by others")
	}
	return nil
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
	// TTORCH_WORKER_TABS=0 is already set (peerControlEnv): a control session has no screen to
	// open worker tabs on (termtab.Open), and a tmux server started here inherits it.
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

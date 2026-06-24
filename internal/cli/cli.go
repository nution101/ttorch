// Package cli implements ttorch's command dispatch. Milestone M0 ships the
// distribution surface (install / update / uninstall / doctor / version);
// orchestration verbs are stubbed with a clear "coming soon" message.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	ttorchembed "github.com/nution101/ttorch"
	"github.com/nution101/ttorch/internal/buildinfo"
	"github.com/nution101/ttorch/internal/doctor"
	"github.com/nution101/ttorch/internal/installer"
	"github.com/nution101/ttorch/internal/learnings"
	"github.com/nution101/ttorch/internal/manifest"
	"github.com/nution101/ttorch/internal/orchestrator"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/profile"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/review"
	"github.com/nution101/ttorch/internal/selfupdate"
	"github.com/nution101/ttorch/internal/skills"
	"github.com/nution101/ttorch/internal/supervisor"
	"github.com/nution101/ttorch/internal/validate"
	"github.com/nution101/ttorch/internal/wake"
	"github.com/nution101/ttorch/internal/worktree"
)

// repo is the GitHub slug releases are fetched from. Update when publishing.
const repo = "nution101/ttorch"

func assetName(tag string) string {
	return fmt.Sprintf("ttorch-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// Main runs ttorch and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		// Bare `ttorch` launches the manager session (auto-restoring saved state).
		return run(mgr().StartManager())
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(buildinfo.String())
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "doctor":
		return run(cmdDoctor(rest))
	case "install":
		return run(cmdInstall())
	case "update":
		return run(cmdUpdate(rest))
	case "uninstall":
		return run(cmdUninstall(rest))
	case "init":
		return run(cmdInit(rest))
	case "profile":
		return run(cmdProfile(rest))
	case "skills":
		return run(cmdSkills(rest))
	case "manager":
		return run(mgr().StartManager())
	case "resume":
		return run(cmdResume())
	case "reset":
		return run(cmdReset(rest))
	case "stop":
		return run(cmdStop())
	case "cc":
		return run(cmdCC(rest))
	case "spawn":
		return run(cmdSpawn(rest))
	case "status":
		return run(cmdStatus())
	case "peek":
		return run(cmdPeek(rest))
	case "send":
		return run(cmdSend(rest))
	case "teardown":
		return run(cmdTeardown(rest))
	case "daemon":
		return run(cmdDaemon(rest))
	case "supervise":
		return run(cmdSupervise())
	case "wake":
		return run(cmdWake(rest))
	case "wait":
		return run(cmdWait(rest))
	case "validate":
		return run(cmdValidate(rest))
	case "review-diff":
		return run(cmdReviewDiff(rest))
	case "approve":
		return run(cmdApprove(rest))
	case "trust":
		return run(cmdTrust(rest))
	case "merge-local":
		return run(cmdMergeLocal(rest))
	case "land":
		return run(cmdLand(rest))
	case "promote":
		return run(cmdPromote(rest))
	case "pr-check":
		return run(cmdPRCheck(rest))
	case "fleet-sync":
		return run(cmdFleetSync(rest))
	case "recovery":
		return run(cmdRecovery())
	case "learn":
		return run(cmdLearn(rest))
	case "learnings":
		return run(cmdLearnings(rest))
	case "worker", "skill":
		fmt.Fprintf(os.Stderr, "ttorch %s: not available yet — arrives in a later milestone.\n", cmd)
		return 3
	default:
		fmt.Fprintf(os.Stderr, "ttorch: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

func run(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "install missing tools without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return doctor.Run(os.Stdout, os.Stdin, *yes)
}

func cmdInstall() error {
	p := paths.Default()
	res, err := installer.Apply(ttorchembed.Content, p, buildinfo.CurrentVersion())
	if err != nil {
		return err
	}
	printResult(os.Stdout, res)
	fmt.Printf("Installed into %s and %s.\n", p.Claude, p.Agents)
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	contentOnly := fs.Bool("content-only", false, "re-apply embedded content without replacing the binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths.Default()

	if *contentOnly {
		return reapplyContent(p)
	}

	latest, err := selfupdate.Latest(repo, nil)
	if errors.Is(err, selfupdate.ErrNoReleases) {
		fmt.Println("No releases published yet; refreshing content from the current binary.")
		return reapplyContent(p)
	}
	if err != nil {
		return err
	}
	cur := buildinfo.CurrentVersion()
	if selfupdate.CompareVersions(latest, cur) <= 0 {
		fmt.Printf("Already up to date (%s).\n", cur)
		return reapplyContent(p)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Printf("Updating %s -> %s ...\n", cur, latest)
	cfg := selfupdate.Config{
		Repo:           repo,
		CurrentVersion: cur,
		ExecutablePath: exe,
		AssetName:      assetName(latest),
	}
	if err := cfg.Apply(latest); err != nil {
		return err
	}
	fmt.Println("Binary updated. Applying new content ...")
	// Re-exec the freshly written binary to apply its embedded content.
	c := exec.Command(exe, "install")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove ~/.ttorch state and data")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := installer.Uninstall(paths.Default(), *purge)
	if err != nil {
		return err
	}
	fmt.Printf("Removed %d managed file(s).\n", res.Report.Count(manifest.Retired))
	for _, f := range res.Report.Conflicts() {
		fmt.Printf("  kept: %s (%s)\n", f.Path, f.Note)
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Println("  " + n)
		}
	}
	return nil
}

func cmdSkills(args []string) error {
	if len(args) > 0 && args[0] == "install" {
		fs := flag.NewFlagSet("skills", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "install without prompting")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return skills.Install(os.Stdout, os.Stdin, *yes)
	}
	skills.List(os.Stdout)
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	mode := fs.String("mode", "pr", "delivery mode for this repo: pr | local | validated | trusted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	notes, err := projectinit.Init(dir, *mode)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	if p, err := profile.Apply(dir); err == nil {
		stack := p.Stack
		if stack == "" {
			stack = "unknown"
		}
		fmt.Printf("  wrote project profile (stack: %s) — commit AGENTS.md so workers pick it up\n", stack)
	}
	return nil
}

func cmdProfile(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	p, err := profile.Apply(dir)
	if err != nil {
		return err
	}
	stack := p.Stack
	if stack == "" {
		stack = "unknown"
	}
	fmt.Printf("Updated the project profile in %s/AGENTS.md (stack: %s).\n", dir, stack)
	if len(p.Exemplars) > 0 {
		fmt.Printf("  exemplars: %s\n", strings.Join(p.Exemplars, ", "))
	}
	fmt.Println("  Commit AGENTS.md so workers pick it up.")
	return nil
}

func mgr() *orchestrator.Manager { return orchestrator.New(paths.Default()) }

func cmdSpawn(args []string) error {
	// Task id and repo are the first two positionals; flags follow (the stdlib
	// flag parser stops at the first positional, so parse the remainder).
	if len(args) < 2 {
		return errors.New(`usage: ttorch spawn <task-id> <repo-path> [--scout] [--init] [--cmd "..."]`)
	}
	id, repo := args[0], args[1]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	scout := fs.Bool("scout", false, "investigation task: report only, no code changes")
	doInit := fs.Bool("init", false, "set the repo up for ttorch first (writes AGENTS.md block + CLAUDE.md symlink); plain spawn never modifies tracked files")
	raw := fs.String("cmd", "", "raw command to run instead of the default harness launch")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	m := mgr()
	if *doInit {
		notes, err := m.InitRepo(repo, "pr")
		if err != nil {
			return err
		}
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}
	t, err := m.Spawn(id, repo, *scout, *raw)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %s (%s) in window %s\n  worktree: %s\n", t.ID, t.Kind, t.Window, t.Worktree)
	return nil
}

func cmdStatus() error {
	m := mgr()
	tasks, err := m.Status()
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no active workers. dispatch with: ttorch spawn <task-id> <repo-path>")
		return nil
	}
	fmt.Printf("%-16s %-6s %-8s %-12s %s\n", "TASK", "KIND", "STATE", "WINDOW", "PROJECT")
	for _, t := range tasks {
		fmt.Printf("%-16s %-6s %-8s %-12s %s\n", t.ID, t.Kind, m.TaskState(t), t.Window, t.Project)
	}
	return nil
}

func cmdPeek(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch peek <task-id> [lines]")
	}
	lines := 40
	if len(args) > 1 {
		if n, err := strconv.Atoi(args[1]); err == nil {
			lines = n
		}
	}
	out, err := mgr().Peek(args[0], lines)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

const sendUsage = "usage: ttorch send <task-id> <text...> | <task-id> - | <task-id> --message-file <path>"

func cmdSend(args []string) error {
	if len(args) < 1 {
		return errors.New(sendUsage)
	}
	id := args[0]
	msg, err := resolveSendMessage(args[1:], os.Stdin, stdinIsTerminal())
	if err != nil {
		return err
	}
	// A send that resolves to nothing is a silent no-op waiting to happen: the
	// worker would get a bare Enter and the caller would believe a brief landed.
	// Fail loudly instead.
	if msg == "" {
		return errors.New("send: empty message — nothing to deliver")
	}
	if err := mgr().Send(id, msg); err != nil {
		return err
	}
	fmt.Printf("sent to %s\n", id)
	return nil
}

// resolveSendMessage determines the message body for `ttorch send` from the
// arguments that follow the task id. It supports three mutually exclusive
// sources, all of which carry the message as raw bytes that never reach a
// shell command line — so backticks, $(...), quotes, and angle brackets survive
// verbatim:
//
//   - inline:        send <id> <text...>            (joined with spaces; unchanged)
//   - stdin:         send <id> -                    (or no text at all, when piped)
//   - message file:  send <id> --message-file PATH  (also --message-file=PATH)
//
// Reading from stdin or a file is the safe path for arbitrary text: the bytes
// are passed to the worker untouched (tmux send-keys -l, never re-evaluated by a
// shell). For those two sources a single trailing newline run is trimmed so a
// plain `echo msg | ttorch send <id> -` does not deliver a spurious extra Enter;
// the inline form is left exactly as before. stdinIsTerminal guards the no-text
// case from blocking forever on an interactive terminal.
func resolveSendMessage(rest []string, stdin io.Reader, stdinIsTerminal bool) (string, error) {
	// --message-file <path> | --message-file=<path>
	if len(rest) > 0 && (rest[0] == "--message-file" || strings.HasPrefix(rest[0], "--message-file=")) {
		var path string
		var extra []string
		if eq := strings.IndexByte(rest[0], '='); eq >= 0 {
			path, extra = rest[0][eq+1:], rest[1:]
		} else {
			if len(rest) < 2 {
				return "", errors.New("send --message-file: missing <path>")
			}
			path, extra = rest[1], rest[2:]
		}
		if path == "" {
			return "", errors.New("send --message-file: empty <path>")
		}
		if len(extra) > 0 {
			return "", fmt.Errorf("send --message-file takes a single path; unexpected extra arguments: %v", extra)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("send: read message file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	// Explicit stdin (`-`), or no text given at all: read the whole of stdin.
	// With no text and an interactive terminal, fail loudly rather than hang.
	if (len(rest) == 1 && rest[0] == "-") || len(rest) == 0 {
		if len(rest) == 0 && stdinIsTerminal {
			return "", errors.New(sendUsage)
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("send: read message from stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	// Inline text — preserved byte-for-byte as before.
	return strings.Join(rest, " "), nil
}

// stdinIsTerminal reports whether ttorch's stdin is an interactive terminal (as
// opposed to a pipe or a redirected file). Used to decide whether a textless
// `ttorch send <id>` should read stdin or refuse rather than block on a TTY.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func cmdTeardown(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch teardown <task-id> [--force]")
	}
	id := args[0]
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	force := fs.Bool("force", false, "discard unlanded work")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	notes, err := mgr().Teardown(id, *force)
	if err != nil {
		return err
	}
	fmt.Printf("tore down %s\n", id)
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

func cmdCC(args []string) error {
	fs := flag.NewFlagSet("cc", flag.ContinueOnError)
	isolated := fs.Bool("isolated", false, "open in its own isolated worktree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return mgr().OpenCC(*isolated)
}

func cmdDaemon(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return supervisor.New(paths.Default()).Run(ctx)
	case "start":
		return daemonStart()
	case "stop":
		return daemonStop()
	case "status":
		return daemonStatus()
	default:
		return errors.New("usage: ttorch daemon run|start|stop|status")
	}
}

func cmdStop() error {
	_ = daemonStop() // stop the supervisor (prints its own line)
	notes, err := mgr().StopSession()
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return err
}

// cmdResume forces a rebuild of the manager window and every worker tab from
// saved state, then attaches the lead to the manager.
func cmdResume() error {
	m := mgr()
	notes, err := m.Resume()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return m.StartManager()
}

// cmdReset discards the saved session (manager record + task records) for a clean
// start. It confirms first unless --yes is given. Worktrees and branches are kept.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes && !confirm(os.Stdout, os.Stdin, "Discard the saved ttorch session (manager + task records)? Worktrees and branches are kept.") {
		fmt.Println("aborted")
		return nil
	}
	notes, err := mgr().Reset()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

// confirm prompts the user for a yes/no answer, defaulting to no.
func confirm(out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func cmdSupervise() error {
	if pid, ok := supervisor.Running(paths.Default()); ok {
		fmt.Printf("supervisor already running (pid %d)\n", pid)
		return nil
	}
	return daemonStart()
}

func cmdWake(args []string) error {
	if len(args) == 0 || args[0] != "drain" {
		return errors.New("usage: ttorch wake drain")
	}
	ws, err := (wake.Queue{Path: paths.Default().WakeQueue()}).Drain()
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		fmt.Println("no pending wakes")
		return nil
	}
	printWakes(ws)
	return nil
}

// printWakes renders drained wakes in the shared `wake drain` / `wait` format.
func printWakes(ws []wake.Wake) {
	fmt.Printf("%d wake(s):\n", len(ws))
	for _, w := range ws {
		key := w.Key
		if key == "" {
			key = "-"
		}
		fmt.Printf("  %-9s %-14s %s\n", w.Kind, key, w.Payload)
	}
}

// cmdWait blocks until a relevant supervision event is queued, then drains and
// prints it (same format as `wake drain`). With --task it returns only for that
// task's wakes; any other task's wakes that it drains while waiting are put back on
// the queue so they are never lost. It blocks efficiently via fsnotify on the state
// dir, with a periodic poll fallback, and is cancellable with Ctrl-C or --timeout.
func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	task := fs.String("task", "", "only return for wakes belonging to this task")
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = wait forever)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths.Default()
	q := wake.Queue{Path: p.WakeQueue()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	// Wakes for other tasks are drained but not consumed; return them to the queue
	// on the way out (normal return, timeout, or Ctrl-C) so no other task's wake is
	// dropped. Held in memory rather than re-appended each loop to avoid waking
	// ourselves on our own write.
	var held []wake.Wake
	defer func() {
		for _, w := range held {
			_ = q.Append(w.Kind, w.Key, w.Payload)
		}
	}()

	// fsnotify lets a queue write wake us instantly; if it can't start we degrade to
	// the poll ticker alone.
	var events chan fsnotify.Event
	var errs chan error
	if w, err := fsnotify.NewWatcher(); err == nil {
		defer w.Close()
		_ = os.MkdirAll(p.StateDir(), 0o755)
		if err := w.Add(p.StateDir()); err == nil {
			events = w.Events
			errs = w.Errors
		}
	}
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	for {
		ws, err := q.Drain()
		if err != nil {
			return err
		}
		matched, rest := wake.Filter(*task, ws)
		held = append(held, rest...)
		if len(matched) > 0 {
			printWakes(matched)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Println("no pending wakes")
			return nil
		case <-events:
		case <-errs:
		case <-poll.C:
		}
	}
}

func cmdValidate(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch validate <task-id>")
	}
	results, err := mgr().Validate(args[0])
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Println("no checks detected for this worktree (add .ttorch/validate.sh to define them)")
		return nil
	}
	failed := printResults(os.Stdout, results)
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(results))
	}
	fmt.Printf("all %d checks passed\n", len(results))
	return nil
}

// printResults renders check results in the shared PASS/FAIL format (reused by
// `validate` and `trust show`) and returns how many failed.
func printResults(w io.Writer, results []validate.Result) int {
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "  [%s] %s\n", status, r.Name)
		if !r.Passed && r.Output != "" {
			fmt.Fprintln(w, indentTail(r.Output, 15))
		}
	}
	return failed
}

// indentTail returns the last n lines of s, each indented for readability.
func indentTail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}

func cmdReviewDiff(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch review-diff <task-id> [--stat]")
	}
	id := args[0]
	fs := flag.NewFlagSet("review-diff", flag.ContinueOnError)
	stat := fs.Bool("stat", false, "summary (diffstat) only")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	out, err := mgr().ReviewDiff(id, *stat)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		fmt.Println("(no changes against the default branch)")
		return nil
	}
	fmt.Println(out)
	return nil
}

func cmdApprove(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch approve <task-id> [--ttl 10m]")
	}
	id := args[0]
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	ttl := fs.Duration("ttl", 10*time.Minute, "how long the approval stays valid")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := mgr().Approve(id, *ttl); err != nil {
		return err
	}
	fmt.Printf("approved %s for %s — now run: ttorch merge-local %s\n", id, *ttl, id)
	return nil
}

func cmdTrust(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch trust prep|record|show <task-id> [flags]")
	}
	sub, id := args[0], args[1]
	switch sub {
	case "prep":
		dir, err := mgr().TrustPrep(id)
		if err != nil {
			return err
		}
		fmt.Printf("prepared review inputs for %s in %s\n", id, dir)
		fmt.Printf("  run the three reviewers (%s), then: ttorch trust record %s\n",
			strings.Join([]string{"correctness", "scope", "security"}, " | "), id)
		return nil
	case "record":
		fs := flag.NewFlagSet("trust record", flag.ContinueOnError)
		sha := fs.String("sha", "", "commit the review covers (default: the worker's current HEAD)")
		ttl := fs.Duration("ttl", 30*time.Minute, "how long the verdict stays valid")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		v, err := mgr().TrustRecord(id, *sha, *ttl)
		if err != nil {
			return err
		}
		printResults(os.Stdout, review.ToResults(v))
		fmt.Printf("recorded %s verdict for %s (valid %s)\n", v.Overall, id, *ttl)
		return nil
	case "show":
		v, ok := mgr().TrustShow(id)
		if !ok {
			fmt.Printf("no valid verdict for %s — run 'ttorch trust record %s'\n", id, id)
			return nil
		}
		printResults(os.Stdout, review.ToResults(v))
		fmt.Printf("verdict: %s\n", v.Overall)
		return nil
	default:
		return errors.New("usage: ttorch trust prep|record|show <task-id> [flags]")
	}
}

func cmdMergeLocal(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch merge-local <task-id> [--require-verdict]")
	}
	id := args[0]
	fs := flag.NewFlagSet("merge-local", flag.ContinueOnError)
	requireVerdict := fs.Bool("require-verdict", false,
		"also require a passing adversarial-review verdict + a fresh green validate (implied by trusted mode)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	out, err := mgr().MergeLocal(id, *requireVerdict)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func cmdLand(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch land <task-id> [--require-verdict]")
	}
	id := args[0]
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	requireVerdict := fs.Bool("require-verdict", false,
		"also require a passing adversarial-review verdict + a fresh green validate (implied by trusted mode)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	out, err := mgr().Land(id, *requireVerdict)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func cmdPromote(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ttorch promote <task-id>")
	}
	if err := mgr().Promote(args[0]); err != nil {
		return err
	}
	fmt.Printf("promoted %s to a ship task\n", args[0])
	return nil
}

func cmdPRCheck(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: ttorch pr-check <task-id> <pr-url>")
	}
	if err := mgr().ArmPRCheck(args[0], args[1]); err != nil {
		return err
	}
	fmt.Printf("watching %s for merge of %s\n", args[0], args[1])
	return nil
}

func cmdFleetSync(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	notes, err := mgr().FleetSync(dir)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

func cmdRecovery() error {
	notes, err := mgr().Recovery()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	return nil
}

// resolveRepo picks the repo root for a learnings command: an explicit --repo, else
// the --task's project, else the repo containing the current directory.
func resolveRepo(repoFlag, taskFlag string) (string, error) {
	if repoFlag != "" {
		return worktree.RepoRoot(repoFlag)
	}
	if taskFlag != "" {
		t, err := mgr().Store.Load(taskFlag)
		if err != nil {
			return "", fmt.Errorf("unknown task %q", taskFlag)
		}
		if t.Project != "" {
			return t.Project, nil
		}
	}
	return worktree.RepoRoot(".")
}

func cmdLearn(args []string) error {
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository (default: current repo, or --task's repo)")
	task := fs.String("task", "", "attribute the lesson to a task (resolves its repo)")
	glob := fs.String("glob", "", "path scope, e.g. internal/api/**")
	pin := fs.Bool("pin", false, "always include this lesson (don't wait for recurrence)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return errors.New(`usage: ttorch learn [--task id] [--glob pat] [--pin] "<lesson>"`)
	}
	dir, err := resolveRepo(*repo, *task)
	if err != nil {
		return err
	}
	e, err := learnings.Apply(dir, text, *glob, *task, *pin)
	if err != nil {
		return err
	}
	status := "recorded"
	if e.Pinned || e.Count >= learnings.PromoteThreshold {
		status = "recorded + promoted to AGENTS.md"
	}
	fmt.Printf("%s (seen %dx): %s\n", status, e.Count, e.Text)
	return nil
}

func cmdLearnings(args []string) error {
	fs := flag.NewFlagSet("learnings", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := resolveRepo(*repo, "")
	if err != nil {
		return err
	}
	entries, err := (learnings.Store{Dir: dir}).Load()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no learnings recorded yet")
		return nil
	}
	promoted := map[string]bool{}
	for _, e := range learnings.Promoted(entries) {
		promoted[e.Text] = true
	}
	for _, e := range entries {
		tag := "ledger"
		if promoted[e.Text] {
			tag = "in AGENTS.md"
		}
		fmt.Printf("  [%dx · %s] %s\n", e.Count, tag, e.Text)
	}
	return nil
}

func daemonStart() error {
	p := paths.Default()
	pid, started, err := supervisor.Start(p)
	if err != nil {
		return err
	}
	if !started {
		fmt.Printf("supervisor already running (pid %d)\n", pid)
		return nil
	}
	fmt.Printf("supervisor started (pid %d); logging to %s\n", pid, p.DaemonLog())
	return nil
}

func daemonStop() error {
	p := paths.Default()
	pid, ok := supervisor.PID(p)
	if !ok || !supervisor.Alive(pid) {
		fmt.Println("supervisor not running")
		_ = os.Remove(p.PIDFile())
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	fmt.Printf("sent stop to supervisor (pid %d)\n", pid)
	return nil
}

func daemonStatus() error {
	p := paths.Default()
	pid, ok := supervisor.Running(p)
	if !ok {
		fmt.Println("supervisor not running")
		return nil
	}
	fmt.Printf("supervisor running (pid %d)\n", pid)
	if fi, err := os.Stat(p.Beacon()); err == nil {
		fmt.Printf("  last beat: %s ago\n", time.Since(fi.ModTime()).Round(time.Second))
	}
	return nil
}

func reapplyContent(p paths.Paths) error {
	res, err := installer.Apply(ttorchembed.Content, p, buildinfo.CurrentVersion())
	if err != nil {
		return err
	}
	printResult(os.Stdout, res)
	return nil
}

func printResult(w io.Writer, res *installer.Result) {
	r := res.Report
	fmt.Fprintf(w, "Content: %d added, %d updated, %d unchanged, %d retired.\n",
		r.Count(manifest.Added), r.Count(manifest.Updated), r.Count(manifest.Unchanged), r.Count(manifest.Retired))
	if c := r.Conflicts(); len(c) > 0 {
		fmt.Fprintf(w, "%d file(s) need your attention (your changes were preserved):\n", len(c))
		for _, f := range c {
			fmt.Fprintf(w, "  - %s: %s\n", f.Path, f.Note)
		}
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Fprintln(w, "  "+n)
		}
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ttorch — manage a team of Claude Code agents

Usage: ttorch [command] [flags]   (bare 'ttorch' launches/restores the manager session)

Team:
  (bare) ttorch           start/attach the manager (one persistent session). If a
                          saved session exists (after stop/reboot/upgrade) it rebuilds
                          the manager + all worker tabs, each resumed to its prior
                          conversation; otherwise a new manager starts in this folder
  resume                  force a rebuild of the manager + all worker tabs, then attach
  reset [--yes]           discard the saved session for a clean start (keeps worktrees)
  stop                    stop the manager session + supervisor (resumable: run 'ttorch')
  cc [--isolated]         open a Claude session attached to the team
  spawn <id> <repo>       start a worker on a task in an isolated worktree
                          (read-only w.r.t. the repo's tracked files)
    --scout                 investigation only (report, no code changes)
    --init                  set the repo up for ttorch first (AGENTS.md block +
                            CLAUDE.md symlink); otherwise spawn never writes them
    --cmd "..."             run a raw command instead of the default harness
  status                  list active workers
  peek <id> [lines]       read recent output from a worker
  send <id> <text...>     type a message into a worker (delivered verbatim)
    send <id> -             read the message body from stdin (safe for any chars)
    send <id> --message-file <path>   read the message body from a file
  teardown <id> [--force] finish a worker (refuses to discard unlanded work)

Supervision:
  supervise               ensure the background supervisor is running
  daemon run|start|stop|status   manage the supervisor process
  wake drain              print and clear pending supervision events
  wait [--task id]        block until the next supervision event, then print it
    --timeout d             give up after this long (0 = wait forever)

Delivery:
  validate <id>               run the repo's build/test/lint checks on a worker
  review-diff <id> [--stat]   show a worker's changes vs the default branch
  approve <id> [--ttl 10m]    grant a time-boxed approval (run by the lead)
  trust prep|record|show <id> prep/record/show the adversarial-review verdict
  merge-local <id> [--require-verdict]
                              fast-forward the local default branch (needs approval;
                              --require-verdict also gates on a passing verdict + validate)
  land <id> [--require-verdict]
                              one safe atomic delivery: fetch, rebase onto the current
                              default (abort on conflict), re-validate, integrate per the
                              repo's delivery mode honoring the existing gates, verify the
                              landed tip matches the validated commit, and fast-forward
                              the local default
  promote <id>                turn a scout task into a ship task
  pr-check <id> <url>         watch a PR and wake when it merges
  fleet-sync [dir]            refresh local default from origin; prune gone branches
  recovery                    reconcile tracked tasks against live windows
  learn [--task id] "<msg>"   record a durable repo lesson (auto-promotes when recurring)
  learnings                   list recorded lessons

Setup:
  install                 install/update managed skills, agents, and guidance
  update [--content-only] self-update the binary, then re-apply content
  uninstall [--purge]     remove managed files (keeps files you edited)
  doctor [--yes]          check/install tmux, git, gh, claude
  skills [install]        list / install recommended agent skills (e.g. axi)
  init [--mode m]         set up a repo's AGENTS.md + CLAUDE.md + delivery mode
  profile [dir]           derive the repo's stack/commands/conventions into AGENTS.md
  version | help          version / this message

Coming later: native-Windows polish
`)
}

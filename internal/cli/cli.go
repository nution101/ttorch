// Package cli implements orcha's command dispatch. Milestone M0 ships the
// distribution surface (install / update / uninstall / doctor / version);
// orchestration verbs are stubbed with a clear "coming soon" message.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	orchaembed "github.com/nution101/orcha"
	"github.com/nution101/orcha/internal/buildinfo"
	"github.com/nution101/orcha/internal/doctor"
	"github.com/nution101/orcha/internal/installer"
	"github.com/nution101/orcha/internal/manifest"
	"github.com/nution101/orcha/internal/orchestrator"
	"github.com/nution101/orcha/internal/paths"
	"github.com/nution101/orcha/internal/projectinit"
	"github.com/nution101/orcha/internal/selfupdate"
)

// repo is the GitHub slug releases are fetched from. Update when publishing.
const repo = "nution101/orcha"

func assetName(tag string) string {
	return fmt.Sprintf("orcha-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// Main runs orcha and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		// Bare `orcha` launches the manager session.
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
	case "manager":
		return run(mgr().StartManager())
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
	case "worker", "supervise", "daemon", "skill", "review-diff", "merge-local":
		fmt.Fprintf(os.Stderr, "orcha %s: not available yet — arrives in a later milestone.\n", cmd)
		return 3
	default:
		fmt.Fprintf(os.Stderr, "orcha: unknown command %q\n\n", cmd)
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
	res, err := installer.Apply(orchaembed.Content, p, buildinfo.CurrentVersion())
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
	purge := fs.Bool("purge", false, "also remove ~/.orcha state and data")
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

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	mode := fs.String("mode", "pr", "delivery mode for this repo: pr | local | validated")
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
	return nil
}

func mgr() *orchestrator.Manager { return orchestrator.New(paths.Default()) }

func cmdSpawn(args []string) error {
	// Task id and repo are the first two positionals; flags follow (the stdlib
	// flag parser stops at the first positional, so parse the remainder).
	if len(args) < 2 {
		return errors.New(`usage: orcha spawn <task-id> <repo-path> [--scout] [--cmd "..."]`)
	}
	id, repo := args[0], args[1]
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	scout := fs.Bool("scout", false, "investigation task: report only, no code changes")
	raw := fs.String("cmd", "", "raw command to run instead of the default harness launch")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	t, err := mgr().Spawn(id, repo, *scout, *raw)
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
		fmt.Println("no active workers. dispatch with: orcha spawn <task-id> <repo-path>")
		return nil
	}
	fmt.Printf("%-16s %-6s %-8s %-12s %s\n", "TASK", "KIND", "STATE", "WINDOW", "PROJECT")
	for _, t := range tasks {
		st := "gone"
		if m.Live(t) {
			st = "running"
		}
		fmt.Printf("%-16s %-6s %-8s %-12s %s\n", t.ID, t.Kind, st, t.Window, t.Project)
	}
	return nil
}

func cmdPeek(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orcha peek <task-id> [lines]")
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

func cmdSend(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: orcha send <task-id> <text...>")
	}
	if err := mgr().Send(args[0], strings.Join(args[1:], " ")); err != nil {
		return err
	}
	fmt.Printf("sent to %s\n", args[0])
	return nil
}

func cmdTeardown(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orcha teardown <task-id> [--force]")
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

func reapplyContent(p paths.Paths) error {
	res, err := installer.Apply(orchaembed.Content, p, buildinfo.CurrentVersion())
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
	fmt.Fprint(w, `orcha — manage a team of Claude Code agents

Usage: orcha [command] [flags]   (bare 'orcha' launches the manager session)

Team:
  (bare) orcha            launch the manager session and attach
  cc [--isolated]         open a Claude session attached to the team
  spawn <id> <repo>       start a worker on a task in an isolated worktree
    --scout                 investigation only (report, no code changes)
    --cmd "..."             run a raw command instead of the default harness
  status                  list active workers
  peek <id> [lines]       read recent output from a worker
  send <id> <text...>     type a message into a worker
  teardown <id> [--force] finish a worker (refuses to discard unlanded work)

Setup:
  install                 install/update managed skills, agents, and guidance
  update [--content-only] self-update the binary, then re-apply content
  uninstall [--purge]     remove managed files (keeps files you edited)
  doctor [--yes]          check/install tmux, git, gh, claude
  init [--mode m]         set up a repo's AGENTS.md + CLAUDE.md + delivery mode
  version | help          version / this message

Coming later: supervise, daemon, skill, review-diff, merge-local
`)
}

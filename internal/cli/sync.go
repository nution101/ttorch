package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
)

const syncUsage = `usage: ttorch sync

Run by a worker in its clone (TTORCH_WORKER_CLONES). Moves the clone's origin/<default> to
the base the gate lands on, read from the lead's repository, so 'git rebase origin/<default>'
in the clone rebases onto it. Nothing in the lead's repository is written.`

// cmdSync is the worker's way to refresh its base. In a linked worktree, origin/<default> is
// the lead's own remote-tracking ref, which the manager's fetch keeps current. In a clone it
// is a snapshot taken at provisioning, so after the default branch moves the worker runs
// `ttorch sync` and then rebases.
//
// The task comes from the caller's worker identity ($TTORCH_TASK_ID or a .ttorch/task file
// above the cwd), and its clone and the lead's repository from that task's DB row. Being in a
// worker context is not proof the caller is the worker: the lead can run this from inside a
// clone. So every git command sync runs is hardened against the clone's configuration (see
// syncGitEnv and syncClone), rather than trusting who ran it.
func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), syncUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(syncUsage)
	}
	callerID, fileDB := callerIdentity()
	if callerID == "" {
		return errors.New("sync: run this as a worker, from its clone; it refreshes that clone's origin/<default>")
	}
	store, err := db.Open(resolveDBPath(fileDB))
	if err != nil {
		return err
	}
	defer store.Close()
	t, ok, err := store.GetTask(context.Background(), callerID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sync: no task %q in the DB", callerID)
	}
	return syncClone(t.Project, t.Worktree, os.Stdout)
}

// syncGitEnv is the environment for every git command sync runs: the caller's environment
// with each GIT_ variable dropped, so no GIT_DIR, GIT_CONFIG_PARAMETERS or alternate object
// directory from the caller steers it, and with no system or global config, either of which
// a worker can write. GIT_ALLOW_PROTOCOL=file refuses every transport but a local path,
// overriding any protocol.*.allow a repository's config sets, so a url.*.insteadOf in the
// clone cannot turn the fetch into an ext:: command or an ssh call. GIT_NO_LAZY_FETCH=1 stops
// git fetching a missing object through a promisor remote the clone's config names, which
// would run that remote's upload-pack program.
func syncGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// syncGit runs git with syncGitEnv and returns its trimmed output.
func syncGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = syncGitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// syncFetchFlags are the -c overrides on the fetch that runs in the clone. A command-line -c
// outranks the clone's own config, and each one names a program the clone's config could
// otherwise have git run during a fetch: hooks (reference-transaction runs on every ref
// update), the fsmonitor (queried when the fetch starts), the alternate-refs command (run
// against the borrowed object store) and auto gc or maintenance.
var syncFetchFlags = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.alternateRefsCommand=true",
	"-c", "protocol.ext.allow=never",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// syncClone fetches the lead repository's current landing base into the clone's
// refs/remotes/origin/<default>. Every git command but the fetch is a read of the lead's
// repository. The fetch runs in the clone and names the lead's repository as its source, so
// the lead's side of it is an upload-pack, which reads and does not write.
//
// It does not fetch origin in the lead's repository first: that would write the lead's refs
// from a worker's process. The base is as fresh as the manager's last fetch, which is the
// same base the land gate rebases onto after its own fetch, or older.
//
// Nothing it prints names the lead's repository path; the worker needs only the ref and sha.
func syncClone(repo, workdir string, out io.Writer) error {
	if !harness.IsCloneWorkdir(workdir) {
		return fmt.Errorf("sync: %s is not a clone working directory. In a linked worktree origin/<default> is already the lead's ref; rebase onto it directly", workdir)
	}
	def := syncDefaultBranch(repo)
	dst := "refs/remotes/origin/" + def
	if _, err := syncGit("check-ref-format", dst); err != nil {
		return fmt.Errorf("sync: the lead's default branch %q does not make a valid ref name", def)
	}
	baseRef, sha, err := syncBase(repo, def)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	args := append([]string{"-C", workdir}, syncFetchFlags...)
	args = append(args, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		"--no-auto-gc", "--no-auto-maintenance", "--upload-pack=git-upload-pack",
		"--", repo, "+"+sha+":"+dst)
	if msg, err := syncGit(args...); err != nil {
		return fmt.Errorf("sync: fetching %s from the lead's repository: %v: %s", sha, err, strings.ReplaceAll(msg, repo, "<lead repository>"))
	}
	fmt.Fprintf(out, "origin/%s -> %s (the lead's %s). Rebase with: git rebase origin/%s\n", def, sha, baseRef, def)
	return nil
}

// syncDefaultBranch names the lead repository's default branch the way worktree.DefaultBranch
// does (origin/HEAD, then main or master, then the checked-out branch), with sync's hardened
// git.
func syncDefaultBranch(repo string) string {
	if out, err := syncGit("-C", repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if syncRefExists(repo, "refs/heads/"+b) {
			return b
		}
	}
	if out, err := syncGit("-C", repo, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		return out
	}
	return "main"
}

func syncRefExists(repo, ref string) bool {
	_, err := syncGit("-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// syncBase picks the same base the land gate does (orchestrator's landBase): origin/<default>
// when it exists and the local default is an ancestor of it, else the local default. The refs
// are fully qualified so a tag named like the default branch cannot stand in for it.
func syncBase(repo, def string) (ref, sha string, err error) {
	local := "refs/heads/" + def
	if !syncRefExists(repo, local) {
		return "", "", fmt.Errorf("the lead's repository has no branch %s", def)
	}
	localSha, err := syncGit("-C", repo, "rev-parse", "--verify", local+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("could not resolve the lead's %s", def)
	}
	ref, sha = def, localSha
	origin := "refs/remotes/origin/" + def
	if _, err := syncGit("-C", repo, "remote", "get-url", "origin"); err == nil && syncRefExists(repo, origin) {
		originSha, err := syncGit("-C", repo, "rev-parse", "--verify", origin+"^{commit}")
		if err != nil {
			return "", "", fmt.Errorf("could not resolve the lead's origin/%s", def)
		}
		if _, err := syncGit("-C", repo, "merge-base", "--is-ancestor", localSha, originSha); err == nil {
			ref, sha = "origin/"+def, originSha
		}
	}
	return ref, sha, nil
}

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
	"github.com/nution101/ttorch/internal/worktree"
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
// Only a worker may run it, for its own task: the process runs git in the clone, and the
// manager never runs git in a worker's clone. The task's clone and the lead's repository come
// from the DB row, not from the caller.
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

// syncClone fetches the lead repository's current landing base into the clone's
// refs/remotes/origin/<default>. Every git command but the fetch is a read of the lead's
// repository. The fetch runs in the clone and names the lead's repository as its source, so
// the lead's side of it is an upload-pack, which reads and does not write.
//
// It does not fetch origin in the lead's repository first: that would write the lead's refs
// from a worker's process. The base is as fresh as the manager's last fetch, which is the
// same base the land gate rebases onto after its own fetch, or older.
func syncClone(repo, workdir string, out io.Writer) error {
	if !harness.IsCloneWorkdir(workdir) {
		return fmt.Errorf("sync: %s is not a clone working directory. In a linked worktree origin/<default> is already the lead's ref; rebase onto it directly", workdir)
	}
	def := worktree.DefaultBranch(repo)
	dst := "refs/remotes/origin/" + def
	if err := exec.Command("git", "check-ref-format", dst).Run(); err != nil {
		return fmt.Errorf("sync: the lead's default branch %q does not make a valid ref name", def)
	}
	baseRef, sha, err := syncBase(repo, def)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	cmd := exec.Command("git", "-C", workdir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "--", repo, "+"+sha+":"+dst)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sync: fetching %s from %s: %v: %s", sha, repo, err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(out, "origin/%s -> %s (%s in %s). Rebase with: git rebase origin/%s\n", def, sha, baseRef, repo, def)
	return nil
}

// syncBase picks the same base the land gate does (orchestrator's landBase): origin/<default>
// when it exists and the local default is an ancestor of it, else the local default. The refs
// are fully qualified so a tag named like the default branch cannot stand in for it.
func syncBase(repo, def string) (ref, sha string, err error) {
	local := "refs/heads/" + def
	if !worktree.RefExists(repo, local) {
		return "", "", fmt.Errorf("the lead's repository %s has no branch %s", repo, def)
	}
	localSha, err := worktree.ResolveRef(repo, local+"^{commit}")
	if err != nil {
		return "", "", err
	}
	ref, sha = def, localSha
	origin := "refs/remotes/origin/" + def
	if worktree.RemoteExists(repo, "origin") && worktree.RefExists(repo, origin) {
		originSha, err := worktree.ResolveRef(repo, origin+"^{commit}")
		if err != nil {
			return "", "", err
		}
		if worktree.IsAncestor(repo, localSha, originSha) {
			ref, sha = "origin/"+def, originSha
		}
	}
	return ref, sha, nil
}

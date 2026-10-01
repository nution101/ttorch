package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/proc"
)

// CloneRefs is the ref namespace in the lead's repository that holds what ttorch keeps for a
// task's clone: refs/ttorch/clones/<task>/<sha> for each commit imported from it, beside the
// other refs the task's clone needs held there. Every name under it is built from an id that
// passed ValidTaskID and, for an import, a sha that passed ValidObjectID.
const CloneRefs = "refs/ttorch/clones/"

// ValidTaskID reports whether id may name a task's directory under CloneRefs. It is an
// allowlist, ^[A-Za-z0-9][A-Za-z0-9._-]*$, plus the two refname rules that pattern still
// lets through: no ".." anywhere and no ".lock" at the end. The leading character must be
// alphanumeric because git check-ref-format accepts a component that starts with "-", which
// a command line can read as an option, so the allowlist does the work and git's own check
// on the ref is the second one.
func ValidTaskID(id string) bool {
	if id == "" || strings.Contains(id, "..") || strings.HasSuffix(id, ".lock") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// ValidObjectID reports whether s is a full SHA-1 or SHA-256 object id in lowercase hex.
func ValidObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// importTimeout bounds one ImportCommit, the fetch and the checks around it together. The
// fetch starts an upload-pack in the worker's clone, and a clone that never answers must not
// hold the gate.
const importTimeout = 2 * time.Minute

// ImportCommit brings the commit sha from the worker's clone at clone into the lead's
// repository repo, as the ref CloneRefs/<task>/<sha>, and returns that ref. Once a worker
// has its clone, this is the only git command ttorch runs against the clone's repository,
// and it runs in repo: the fetch starts a git upload-pack in the clone, as the lead, which
// reads objects and serves the one sha. No ref name or value comes from the clone, because
// the source of the refspec is a sha the caller chose (from ObserveHead).
//
// The exit code is not the answer. A commit whose history crosses a shallow boundary is
// refused with only a warning and exit 0, so the ref is read back; a blob, tree or tag
// imports without complaint, so the ref must peel to the commit sha itself, and a ref that
// does not is deleted. A sha the clone does not have fails with "not our ref". The
// receiving side hashes every object it is sent, so the bytes stored under sha are bytes
// that hash to it whatever the clone's object files hold; that is why the clone's content
// is not a trust question, only which sha is reviewed and merged.
//
// When the ref already names the commit, nothing is fetched. task and sha are checked with
// ValidTaskID and ValidObjectID, and clone must be an absolute clean path, before any git
// command runs. The whole call runs under importTimeout as well as ctx.
func ImportCommit(ctx context.Context, repo, clone, task, sha string) (string, error) {
	if repo == "" {
		return "", errors.New("import: no repository to import into")
	}
	if !ValidTaskID(task) {
		return "", fmt.Errorf("import: invalid task id %q", task)
	}
	if !ValidObjectID(sha) {
		return "", fmt.Errorf("import: invalid object id %q", sha)
	}
	if !filepath.IsAbs(clone) || filepath.Clean(clone) != clone || strings.ContainsRune(clone, 0) {
		return "", fmt.Errorf("import: clone path %q is not absolute and clean", clone)
	}
	ref := CloneRefs + task + "/" + sha

	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	hooks, err := os.MkdirTemp("", "ttorch-import-hooks-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(hooks)
	run := func(args ...string) (string, string, error) {
		return importGit(ctx, hooks, nil, append([]string{"-C", repo}, args...)...)
	}
	resolve := func(name string) (string, bool) {
		id, _, err := run("rev-parse", "--verify", "--quiet", name)
		return id, err == nil
	}

	if id, ok := resolve(ref + "^{commit}"); ok && id == sha {
		return ref, nil
	}
	_, stderr, err := run(
		"fetch", "--quiet",
		"--no-tags",               // a tag the worker made on a fetched object would follow it in
		"--no-write-fetch-head",   // FETCH_HEAD would record the clone's path in repo
		"--no-recurse-submodules", // nothing but the named commit's objects
		"--no-auto-gc", "--no-auto-maintenance",
		// No --update-shallow, ever: with it a shallow clone makes repo shallow. No "+": the
		// name holds the sha, so the ref existing with another value is corruption.
		"--", clone, sha+":"+ref,
	)
	from := escapeForTerminal(clone)
	if err != nil {
		return "", fmt.Errorf("import %s from %s: %w", sha, from, err)
	}
	id, ok := resolve(ref)
	switch {
	case !ok:
		return "", fmt.Errorf("import %s from %s: the fetch left no ref %s: %s", sha, from, ref, escapeForTerminal(strings.TrimSpace(stderr)))
	case id != sha:
		return "", fmt.Errorf("import %s from %s: %s names %s", sha, from, ref, id)
	}
	if peeled, ok := resolve(ref + "^{commit}"); !ok || peeled != sha {
		typ, _, _ := run("cat-file", "-t", sha)
		if _, _, err := run("update-ref", "-d", ref, sha); err != nil {
			return "", fmt.Errorf("import %s from %s: it is a %s, not a commit, and its ref could not be deleted: %w", sha, from, typ, err)
		}
		return "", fmt.Errorf("import %s from %s: it is a %s, not a commit", sha, from, typ)
	}
	return ref, nil
}

// importConfig is the configuration every git command of the import runs with, on top of
// repo's own. protocol.version=2 serves any object the clone has; version 0 refuses a sha no
// ref advertises, such as the parent of the tip. transfer.fsckObjects makes git check every
// object it receives, which refuses a tree holding an entry named .git and leaves no ref and
// no object behind. The empty hooks directory keeps repo's own hooks (reference-transaction
// runs on the import's ref update) from running. gc.auto and maintenance.auto, with the
// fetch's --no-auto-gc and --no-auto-maintenance, keep the import from repacking repo.
//
// These settings reach the git commands run in repo and their children, and not the clone's
// upload-pack: git drops command-line config when it starts the local transport's
// upload-pack. So core.fsmonitor, uploadpack.packObjectsHook and core.alternateRefsCommand
// are set for the receiving side only, and what keeps upload-pack from running a program is
// importEnv. upload-pack takes packObjectsHook only from global, system or command-line
// config, and ran none of the programs the clone's own config named.
func importConfig(hooks string) []string {
	return []string{
		"-c", "protocol.version=2",
		"-c", "transfer.fsckObjects=true",
		"-c", "core.hooksPath=" + hooks,
		"-c", "core.fsmonitor=false",
		"-c", "uploadpack.packObjectsHook=",
		"-c", "core.alternateRefsCommand=",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
}

// importEnv is the environment every git command of the import runs with, the clone's
// upload-pack included. None of the caller's GIT_* variables, so the import means the same
// whoever runs it: a GIT_DIR would point it at another repository, and a
// GIT_OBJECT_DIRECTORY would write the commit where repo does not read it, with the import
// still reporting success. No global or system config: the import needs neither, the worker
// could have edited the lead's global file, and upload-pack reads it, where a global
// uploadpack.packObjectsHook would run. No prompt, so an import can never wait on a
// terminal.
func importEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

// importGit runs one git command of the import under ctx with importConfig and importEnv,
// in its own process group, so a deadline ends whatever it forked, upload-pack included. It
// returns stdout trimmed and stderr as written. The error text is escaped for the terminal,
// as git()'s is, because git's stderr carries bytes from the clone.
func importGit(ctx context.Context, hooks string, stdin io.Reader, args ...string) (string, string, error) {
	cmd := proc.Command(ctx, "git", append(importConfig(hooks), args...)...)
	cmd.Env = importEnv()
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := proc.Run(cmd)
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%w)", err, ctx.Err())
		}
		return out, stderr.String(), fmt.Errorf("git %s: %w: %s", escapeForTerminal(strings.Join(args, " ")), err, escapeForTerminal(strings.TrimSpace(stderr.String())))
	}
	return out, stderr.String(), nil
}

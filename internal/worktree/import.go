package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
// command runs. Before it fetches, it refuses a git without the CVE-2024-32004 fix
// (checkImportGit) and confirms clone is a real directory, and it hands the source to git as
// a file:// URL. The whole call runs under importTimeout as well as ctx.
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
	hooks, done, err := emptyHooksDir()
	if err != nil {
		return "", err
	}
	defer done()
	run := func(args ...string) (string, string, error) {
		return importGit(ctx, hooks, nil, append([]string{"-C", repo}, args...)...)
	}
	resolve := func(name string) (string, bool) {
		id, _, err := run("rev-parse", "--verify", "--quiet", name)
		return id, err == nil
	}

	from := escapeForTerminal(clone)
	if id, ok := resolve(ref + "^{commit}"); ok && id == sha {
		return ref, nil
	}
	if err := checkImportGit(ctx, hooks); err != nil {
		return "", err
	}
	// Immediately before the fetch, confirm the source is a real directory. Lstat, not Stat,
	// so a symlink at the path is rejected rather than followed, as ObserveHead does. A
	// regular file here would otherwise be read as a git bundle, a transport that honours a
	// worker-written file; passing the source as a file:// URL already refuses the bundle
	// route, and this refuses it a step earlier with a clearer message.
	if fi, err := os.Lstat(clone); err != nil {
		return "", fmt.Errorf("import %s: clone path %s: %w", sha, from, err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("import %s: clone path %s is not a directory (mode %s)", sha, from, fi.Mode().Type())
	}
	_, stderr, err := run(
		"fetch", "--quiet",
		"--no-tags",               // a tag the worker made on a fetched object would follow it in
		"--no-write-fetch-head",   // FETCH_HEAD would record the clone's path in repo
		"--no-recurse-submodules", // nothing but the named commit's objects
		"--no-auto-gc", "--no-auto-maintenance",
		// --upload-pack fixes the program the local fetch starts in the clone. repo's own
		// config can set remote."file://<clone>".uploadpack to any command, which
		// GIT_ALLOW_PROTOCOL=file does not stop because it governs the transport, not the
		// upload-pack binary. The command-line value beats that config key. git-upload-pack
		// (not an absolute path) resolves on repo's PATH, which is the lead's, not the worker's.
		"--upload-pack=git-upload-pack",
		// No --update-shallow, ever: with it a shallow clone makes repo shallow. No "+": the
		// name holds the sha, so the ref existing with another value is corruption. The source
		// is a file:// URL, never a bare path, so git takes the file transport and never the
		// bundle route a bare path to a file would.
		"--", "file://"+clone, sha+":"+ref,
	)
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

// cveFixedPatch is the lowest patch release in each 2.x series that carries the fix for
// CVE-2024-32004, the arbitrary-code-execution bug where the clone's upload-pack lazy-fetches
// a missing object through a promisor remote named in the clone's own config and runs the
// program that config points at. git shipped the fix on 2024-05-14 across the maintained
// series at once; each of these releases names CVE-2024-32004 in its release notes (verified
// against Documentation/RelNotes/<v>.adoc):
//
//	2.39.4  2.40.2  2.41.1  2.42.2  2.43.4  2.44.1  2.45.1
//
// A 2.x.y git is fixed when y >= cveFixedPatch[x] for its series, or when the series is newer
// than every entry (2.46 and up), or when a series sits between two entries and so was never
// vulnerable in a release we would see. Below 2.39 there is no fixed release, so those are
// refused outright. This floor also clears every earlier requirement the import had:
// GIT_CONFIG_GLOBAL (2.32), --no-write-fetch-head and --no-auto-maintenance (2.29). And
// GIT_NO_LAZY_FETCH, importEnv's belt-and-suspenders against the same bug, is honoured by
// every git at or above this floor (it is in promisor-remote.c as of 2.39.4).
//
// This reads the version STRING, not the actual behaviour, so it refuses a distro git that
// carries the fix backported onto an older string: Ubuntu 24.04's git 2.43.0, for instance,
// has the CVE-2024-32004 fix but reports 2.43.0, below the 2.43.4 the table wants. That is a
// deliberate false refusal, not a flag to override: a version string is the only thing the
// import can read, and loosening it to admit one distro's backport would admit every real
// pre-fix 2.43.0 too. The remedy is to run a git whose version string is at or above the
// floor (upstream, or a distro package that bumped the string), which doctor's floor message
// also says.
var cveFixedPatch = map[int]int{39: 4, 40: 2, 41: 1, 42: 2, 43: 4, 44: 1, 45: 1}

// gitFixesCVE202432004 reports whether a 2.x.y git carries the CVE-2024-32004 fix.
func gitFixesCVE202432004(major, minor, patch int) bool {
	if major > 2 {
		return true
	}
	if major < 2 || minor < 39 {
		return false
	}
	if minor >= 46 {
		return true
	}
	min, known := cveFixedPatch[minor]
	if !known {
		// A series with no entry (none exist between 39 and 45 today); treat as unfixed so a
		// future gap fails closed rather than open.
		return false
	}
	return patch >= min
}

// checkImportGit refuses a git that does not carry the CVE-2024-32004 fix, or whose version
// cannot be read. On an unfixed git the clone's upload-pack would run a worker-chosen program
// during the import, which no flag of the fetch can stop.
func checkImportGit(ctx context.Context, hooks string) error {
	out, _, err := importGit(ctx, hooks, nil, "version")
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	major, minor, patch, ok := parseGitVersion(out)
	if !ok {
		return fmt.Errorf("import: cannot read the git version from %q", escapeForTerminal(out))
	}
	if !gitFixesCVE202432004(major, minor, patch) {
		return fmt.Errorf("import: git %d.%d.%d does not carry the fix for CVE-2024-32004; "+
			"the import needs 2.45.1, or the backport for its series (2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4, 2.44.1)",
			major, minor, patch)
	}
	return nil
}

// parseGitVersion reads the major, minor and patch numbers from `git version` output, such as
// "git version 2.50.1", "git version 2.50.1 (vendor build 155)" or
// "git version 2.45.0.rc1.17.gabcdef0" (patch 0). A missing patch component reads as 0.
func parseGitVersion(out string) (major, minor, patch int, ok bool) {
	v, found := strings.CutPrefix(strings.TrimSpace(out), "git version ")
	if !found {
		return 0, 0, 0, false
	}
	v, _, _ = strings.Cut(v, " ")
	parts := strings.SplitN(v, ".", 4)
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return 0, 0, 0, false
	}
	if len(parts) >= 3 {
		// The patch component may carry an rc or build suffix ("0.rc1.17.gabcdef0"); take its
		// leading digits, and treat no leading digit as 0 rather than failing the whole parse.
		patch = leadingInt(parts[2])
	}
	return major, minor, patch, true
}

// leadingInt returns the integer the leading decimal digits of s spell, or 0 when s has none.
func leadingInt(s string) int {
	n := 0
	for ; n < len(s) && s[n] >= '0' && s[n] <= '9'; n++ {
	}
	if n == 0 {
		return 0
	}
	v, err := strconv.Atoi(s[:n])
	if err != nil {
		return 0
	}
	return v
}

// importConfig is the configuration every git command of the import runs with, on top of
// repo's own. protocol.version=2 serves any object the clone has; version 0 refuses a sha no
// ref advertises, such as the parent of the tip. The empty hooks directory keeps repo's own
// hooks (reference-transaction runs on the import's ref update) from running. gc.auto and
// maintenance.auto, with the fetch's --no-auto-gc and --no-auto-maintenance, keep the import
// from repacking repo.
//
// The fsck settings make git check every object it receives and refuse a hostile tree,
// leaving no ref and no object behind. fetch.fsckObjects and transfer.fsckObjects are both
// turned on because repo's OWN config can turn the check off even though -c outranks a config
// file: a key set in repo beats the general key it specialises, so fetch.fsckObjects=false in
// repo defeats -c transfer.fsckObjects=true (fetch's key wins over transfer's). The specific
// severities are pinned to error for the same reason, because repo could set any of them to
// ignore: hasDotgit (a tree entry named .git), hasDotdot (an entry named ..), and
// gitmodulesUrl / gitmodulesPath / gitmodulesSymlink (a .gitmodules whose url or path reads
// as an option, or that is itself a symlink). Each was shown importing its hostile object
// before it was pinned.
//
// What -c cannot fix is fetch.fsck.skipList: it names a file of object ids to exempt, it
// ACCUMULATES across config sources, and -c fetch.fsck.skipList= does not clear an entry repo
// already set. So a determined repo config can still exempt a specific object from fsck. That
// is a known residual, not closed here; the backstop is the floor behind this: the gate never
// checks out or follows a submodule from an imported commit except through AddDetached and
// validate, which refuse a path collision, and submodule repos are out of scope for clones.
//
// These settings reach the git commands run in repo and their children, and not the clone's
// upload-pack: git drops command-line config when it starts the local transport's
// upload-pack. So core.fsmonitor, uploadpack.packObjectsHook and core.alternateRefsCommand
// are set for the receiving side only, and what keeps upload-pack from running a program is
// importEnv together with the fetch's --upload-pack (which fixes the program the local fetch
// starts in the clone, against repo's remote.<url>.uploadpack).
func importConfig(hooks string) []string {
	return []string{
		"-c", "protocol.version=2",
		"-c", "transfer.fsckObjects=true",
		"-c", "fetch.fsckObjects=true",
		"-c", "fetch.fsck.hasDotgit=error",
		"-c", "fetch.fsck.hasDotdot=error",
		"-c", "fetch.fsck.gitmodulesUrl=error",
		"-c", "fetch.fsck.gitmodulesPath=error",
		"-c", "fetch.fsck.gitmodulesSymlink=error",
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
// still reporting success. Stripping them all also means an ambient GIT_NO_LAZY_FETCH=0 or
// GIT_ALLOW_PROTOCOL=all cannot override the two this function then sets.
//
// It sets, beyond the originals:
//   - GIT_NO_LAZY_FETCH=1 forces the clone's upload-pack not to lazy-fetch a missing object
//     through a promisor remote the worker named, which is the CVE-2024-32004 code path. The
//     version floor refuses a git that lacks the fix; this enforces it even on a fixed git,
//     whose default a worker's clone config could otherwise flip. Measured: with the value 0
//     the clone's upload-pack ran the worker's promisor program; with 1 it did not.
//   - GIT_ALLOW_PROTOCOL=file restricts git to the file transport, so url.<ext::…>.insteadOf
//     plus protocol.ext.allow=always in repo cannot rewrite the clone source into an ext::
//     command and run it. Measured: that rewrite ran a worker-chosen command without this,
//     and was refused as "transport 'ext' not allowed" with it. The import only ever speaks
//     to a local path, so file is the only transport it needs.
//
// No global or system config: the import needs neither, the worker could have edited the
// lead's global file, and upload-pack reads it, where a global uploadpack.packObjectsHook
// would run. No prompt, so an import can never wait on a terminal.
func importEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=file",
	)
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

// DropImports deletes every ref under CloneRefs/<task>/ in repo: the task's imported commits
// and anything else its clone keeps there. Once a task is done with, this leaves the objects
// to repo's ordinary gc. One update-ref transaction deletes them all or none, under the
// import's settings, so none of repo's hooks run. task is checked with ValidTaskID before any
// git command runs; a "*" would otherwise match every task's refs.
func DropImports(ctx context.Context, repo, task string) error {
	if repo == "" {
		return errors.New("drop imports: no repository")
	}
	if !ValidTaskID(task) {
		return fmt.Errorf("drop imports: invalid task id %q", task)
	}
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	hooks, done, err := emptyHooksDir()
	if err != nil {
		return err
	}
	defer done()

	dir := CloneRefs + task + "/"
	out, _, err := importGit(ctx, hooks, nil, "-C", repo, "for-each-ref", "--format=%(refname)", dir)
	if err != nil {
		return fmt.Errorf("drop imports for %s: %w", task, err)
	}
	if out == "" {
		return nil
	}
	var in strings.Builder
	for _, ref := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ref, dir) {
			return fmt.Errorf("drop imports for %s: listing %s returned %q", task, dir, escapeForTerminal(ref))
		}
		in.WriteString("delete " + ref + "\n")
	}
	if _, _, err := importGit(ctx, hooks, strings.NewReader(in.String()), "-C", repo, "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("drop imports for %s: %w", task, err)
	}
	return nil
}

// emptyHooksDir makes the empty directory the import points core.hooksPath at, and returns
// it with a func that removes it.
func emptyHooksDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "ttorch-import-hooks-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

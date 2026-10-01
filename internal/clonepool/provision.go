package clonepool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nution101/ttorch/internal/worktree"
)

// seededConfigKeys is the allowlist of main's local config keys a clone receives by value.
// A linked worktree reads main's config because it is the same file; a clone has its own,
// so the keys that decide who a commit is by and what a checkout writes are copied. It is
// an allowlist, not a denylist: aliases, diff and merge drivers, credential helpers,
// includes and everything else stay behind, and a key joins this list when a real
// repository needs it. The platform keys are written before the checkout because they
// change the bytes it writes.
var seededConfigKeys = []string{
	"user.name",
	"user.email",
	"user.signingkey",
	"commit.gpgsign",
	"core.autocrlf",
	"core.eol",
	"core.filemode",
	"core.ignorecase",
	"core.symlinks",
	"core.precomposeunicode",
}

// ErrUnsupportedRepo marks a repository a clone cannot be provisioned for: one that uses
// LFS, is a partial clone, or has submodules. Each needs state copied that the allowlist
// does not carry (LFS storage, the promisor remote, submodule gitdirs), so the spawn is
// refused with a message rather than handed a clone that half works.
var ErrUnsupportedRepo = errors.New("worker clones do not support this repository")

// source is what provisioning reads from main, resolved once per acquire.
type source struct {
	repo   string // main's top level, as the caller recorded it
	common string // main's common git dir, absolute; its objects back the clone
	def    string // the default branch name
	base   string // the commit a new clone starts from
}

// warnf reports a non-fatal provisioning problem on stderr. A package var so tests can
// capture it.
var warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ttorch: "+format+"\n", args...)
}

// resolveSource refreshes origin in main when it has one and resolves the base a new clone
// starts from: refs/remotes/origin/<def>, else refs/heads/<def>, else HEAD. The names are
// fully qualified so a tag named like the default branch cannot stand in for it. A failed
// fetch warns and falls back to the last-known base, as the worktree pool does.
func resolveSource(repo string) (source, error) {
	common, err := git("-C", repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return source{}, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	common = filepath.Clean(common)

	def := defaultBranch(repo)
	if def == "" || def == "HEAD" {
		return source{}, fmt.Errorf("cannot provision a clone of %s: no default branch resolves", repo)
	}
	if _, err := git("check-ref-format", "refs/heads/"+def); err != nil {
		return source{}, fmt.Errorf("cannot provision a clone of %s: default branch %q is not a valid branch name", repo, def)
	}

	if worktree.RemoteExists(repo, "origin") {
		if err := worktree.Fetch(repo); err != nil {
			warnf("could not fetch origin in %s: %v; basing on the last-known default, which may be behind origin", repo, err)
		}
	}
	for _, ref := range []string{"refs/remotes/origin/" + def, "refs/heads/" + def, "HEAD"} {
		if sha, err := git("-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && isObjectID(sha) {
			return source{repo: repo, common: common, def: def, base: sha}, nil
		}
	}
	return source{}, fmt.Errorf("cannot provision a clone of %s: no base commit resolves", repo)
}

// defaultBranch returns main's default branch name: what refs/remotes/origin/HEAD points
// at, else main or master, else the current branch, else "". It reads full ref names.
// worktree.DefaultBranch asks for --short, and git shortens refs/remotes/origin/main to
// remotes/origin/main once a tag refs/tags/origin/main exists, so it returns a name that
// is not a branch; a worktree survives that only because the bare-name lookup after it
// resolves the same string back to the remote-tracking ref.
func defaultBranch(repo string) string {
	if out, err := git("-C", repo, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(out, "refs/remotes/origin/"); ok && b != "" {
			return b
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git("-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	if out, err := git("-C", repo, "symbolic-ref", "-q", "HEAD"); err == nil {
		if b, ok := strings.CutPrefix(out, "refs/heads/"); ok {
			return b
		}
	}
	return ""
}

// refuseUnsupported returns ErrUnsupportedRepo when main is a partial clone, or when the
// base tree uses submodules or LFS. It reads main and the base commit only.
func refuseUnsupported(src source) error {
	refuse := func(what, evidence string) error {
		return fmt.Errorf("%w: %s uses %s (%s). Worker clones do not support LFS, partial-clone or submodule repositories; spawn with --workdir worktree, or unset TTORCH_WORKER_CLONES",
			ErrUnsupportedRepo, src.repo, what, evidence)
	}

	if v, _ := git("-C", src.repo, "config", "--get", "extensions.partialClone"); v != "" {
		return refuse("a partial clone", "extensions.partialClone = "+v)
	}
	if out, _ := git("-C", src.repo, "config", "--type=bool", "--get-regexp", `^remote\..*\.promisor$`); out != "" {
		for _, line := range strings.Split(out, "\n") {
			if key, val, ok := strings.Cut(line, " "); ok && val == "true" {
				return refuse("a partial clone", key+" = true")
			}
		}
	}
	if m, _ := filepath.Glob(filepath.Join(src.common, "objects", "pack", "*.promisor")); len(m) > 0 {
		return refuse("a partial clone", "promisor pack "+filepath.Base(m[0]))
	}

	out, err := gitOut("-C", src.repo, "ls-tree", "-r", "-z", "--full-tree", src.base)
	if err != nil {
		return fmt.Errorf("cannot read the base tree of %s: %w", src.repo, err)
	}
	var attrs []string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		// "<mode> SP <type> SP <oid> TAB <path>"; with -z the path is never quoted.
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return fmt.Errorf("cannot read the base tree of %s: unparseable record %q", src.repo, rec)
		}
		switch {
		case strings.HasPrefix(meta, "160000 "):
			return refuse("submodules", "gitlink "+path)
		case path == ".gitmodules":
			return refuse("submodules", ".gitmodules")
		case path == ".lfsconfig":
			return refuse("LFS", ".lfsconfig")
		case filepath.Base(path) == ".gitattributes" && strings.HasPrefix(meta, "100"):
			attrs = append(attrs, path)
		}
	}
	if len(attrs) > 0 {
		blobs, err := worktree.CatBlobs(src.repo, src.base, attrs)
		if err != nil {
			return fmt.Errorf("cannot read the base .gitattributes of %s: %w", src.repo, err)
		}
		for _, p := range attrs {
			if strings.Contains(string(blobs[p]), "filter=lfs") {
				return refuse("LFS", "filter=lfs in "+p)
			}
		}
	}
	return nil
}

// pinRef is the ref in main that keeps a task's base reachable while its clone borrows
// main's objects through the alternates file, so a gc in main cannot prune history the
// clone still reads.
func pinRef(taskID string) string { return "refs/ttorch/clones/" + taskID + "/base" }

// provision builds a private repository at slot, which must not exist, for taskID: a git
// init whose object store borrows main's through an alternates file, with no remote that
// points at main, with config and hooks seeded from the allowlist, main's tags, and the
// task branch ttorch/<taskID> checked out at the base.
//
// Every git command here runs in main or in the directory this call just created, so the
// only repository config git reads is main's or the one written here. The checkout runs
// with an empty hooks path and the hooks are copied after it, so provisioning never runs
// a hook.
func provision(src source, slot, taskID string) error {
	tmpl, err := os.MkdirTemp("", "ttorch-clone-template-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpl)

	// init, not clone: clone always records the source as a remote, and a remote naming
	// main is a push route into it. Building the repository by hand means main's path is
	// never in the clone's config. The empty template skips the sample hooks.
	if _, err := git("init", "-q", "--template="+tmpl, "--initial-branch="+src.def, "--", slot); err != nil {
		return err
	}
	alt := filepath.Join(slot, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(alt, []byte(filepath.Join(src.common, "objects")+"\n"), 0o644); err != nil {
		return err
	}
	originRef := "refs/remotes/origin/" + src.def
	if _, err := git("-C", slot, "update-ref", originRef, src.base); err != nil {
		return err
	}
	if _, err := git("-C", slot, "symbolic-ref", "refs/remotes/origin/HEAD", originRef); err != nil {
		return err
	}
	if url := originURL(src); url != "" {
		if _, err := git("-C", slot, "config", "--", "remote.origin.url", url); err != nil {
			return err
		}
		if _, err := git("-C", slot, "config", "--", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return err
		}
	}
	if err := seedConfig(src, slot); err != nil {
		return err
	}
	if _, err := git("-C", slot, "-c", "core.hooksPath="+tmpl, "checkout", "-q", "-B", "ttorch/"+taskID, src.base); err != nil {
		return err
	}
	if err := seedHooks(src, slot); err != nil {
		return err
	}
	return seedTags(src, slot)
}

// originURL returns main's origin URL as the clone should record it, or "" when main has
// no origin or its origin is main itself. The raw config value is read, not `remote
// get-url`, so an insteadOf rewrite in the lead's config is applied at use, as it is in
// main. A relative local path is made absolute against main, where it was written.
func originURL(src source) string {
	url, err := git("-C", src.repo, "config", "--get", "remote.origin.url")
	if err != nil || url == "" {
		return ""
	}
	local := strings.TrimPrefix(url, "file://")
	if strings.Contains(local, "://") || scpLike(local) {
		return url
	}
	if !filepath.IsAbs(local) {
		local = filepath.Join(src.repo, local)
		url = local
	}
	if samePath(local, src.repo) || samePath(local, src.common) {
		return ""
	}
	return url
}

// scpLike reports git's scp-style remote form, host:path, which has a colon before any
// slash.
func scpLike(u string) bool {
	colon := strings.IndexByte(u, ':')
	slash := strings.IndexByte(u, '/')
	return colon > 0 && (slash < 0 || colon < slash)
}

// samePath reports whether a and b name the same directory once symlinks are resolved.
func samePath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// seedConfig copies the allowlisted keys from main's local config into the clone's.
func seedConfig(src source, slot string) error {
	for _, key := range seededConfigKeys {
		val, err := git("-C", src.repo, "config", "--local", "--get", key)
		if err != nil {
			continue // unset in main
		}
		if _, err := git("-C", slot, "config", "--", key, val); err != nil {
			return err
		}
	}
	return nil
}

// seedHooks copies main's hooks into the clone, so a worker's commits run the same local
// hooks a worktree's do today. The .sample files are skipped. core.hooksPath is copied
// only when it is relative, so it resolves inside the clone's own checkout and never
// names a directory in main.
func seedHooks(src source, slot string) error {
	from := filepath.Join(src.common, "hooks")
	entries, err := os.ReadDir(from)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	to := filepath.Join(slot, ".git", "hooks")
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sample") {
			continue
		}
		fi, err := os.Stat(filepath.Join(from, e.Name()))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	if hp, err := git("-C", src.repo, "config", "--local", "--get", "core.hooksPath"); err == nil && hp != "" &&
		!filepath.IsAbs(hp) && !strings.HasPrefix(hp, "~") {
		if _, err := git("-C", slot, "config", "--", "core.hooksPath", hp); err != nil {
			return err
		}
	}
	return nil
}

// seedTags writes main's tags into the clone's packed-refs in one file write, so `git
// describe` and version stamping work in a build. They are the clone's own afterwards: a
// tag made in the clone does not reach main.
func seedTags(src source, slot string) error {
	out, err := gitOut("-C", src.repo, "for-each-ref", "--format=%(objectname) %(refname)", "refs/tags")
	if err != nil {
		return err
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !isObjectID(oid) || !strings.HasPrefix(name, "refs/tags/") || strings.ContainsAny(name, " \t") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Slice(lines, func(i, j int) bool {
		_, a, _ := strings.Cut(lines[i], " ")
		_, b, _ := strings.Cut(lines[j], " ")
		return a < b
	})
	// No peeled trait and no ^ lines: git peels an annotated tag on demand from the
	// object, which the clone reads through the alternates file.
	var b strings.Builder
	b.WriteString("# pack-refs with: sorted \n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	dst := filepath.Join(slot, ".git", "packed-refs")
	tmp := dst + ".ttorch-tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

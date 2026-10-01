package worktree

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestValidTaskID: a task id becomes one component of a ref under CloneRefs, so the
// validator must refuse everything git would refuse there, and an id that reads as an
// option, before any git command sees it. Every id it accepts is checked against git's own
// rule as well, which is the second check.
func TestValidTaskID(t *testing.T) {
	sha := strings.Repeat("a", 40)
	accept := []string{
		"a", "A", "9", "TTORCH-CLONES-IMPORT", "cc-150405-ab12", "scout-1", "a.b", "a_b",
		"a-", "a.", "x.lockx", "lock", "a.-b", "v1.2.3",
	}
	reject := []string{
		"", ".a", "-x", "_a", "a..b", "a.lock", "x.y.lock", "..", ".", "a b", "a/b", "a/../b",
		`a\b`, "a:b", "a~b", "a^b", "a?b", "a*b", "a[b", "a@{b", "@", "é", "a\x00b",
		"a\nb", "a\tb", "a\x7f",
	}
	for _, id := range accept {
		if !ValidTaskID(id) {
			t.Errorf("ValidTaskID(%q) = false, want true", id)
		}
	}
	for _, id := range reject {
		if ValidTaskID(id) {
			t.Errorf("ValidTaskID(%q) = true, want false", id)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, id := range accept {
		ref := CloneRefs + id + "/" + sha
		if out, err := exec.Command("git", "check-ref-format", ref).CombinedOutput(); err != nil {
			t.Errorf("ValidTaskID accepts %q but git refuses %s: %v %s", id, ref, err, out)
		}
	}
}

func TestValidObjectID(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("0123456789abcdef", 2) + "01234567": true,
		strings.Repeat("0123456789abcdef", 4):              true,
		strings.Repeat("A", 40):                            false,
		strings.Repeat("a", 39):                            false,
		strings.Repeat("a", 41):                            false,
		strings.Repeat("a", 63):                            false,
		strings.Repeat("g", 40):                            false,
		strings.Repeat("a", 39) + "\n":                     false,
		"-" + strings.Repeat("a", 39):                      false,
		"":                                                 false,
		"HEAD":                                             false,
	} {
		if got := ValidObjectID(s); got != want {
			t.Errorf("ValidObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}

// realGitPath is the git found on PATH before any test puts a shim in front of it.
var (
	realGitOnce sync.Once
	realGitPath string
)

func realGit(t *testing.T) string {
	t.Helper()
	realGitOnce.Do(func() { realGitPath, _ = exec.LookPath("git") })
	if realGitPath == "" {
		t.Skip("git not installed")
	}
	return realGitPath
}

// skipIfGitBelowFloor skips a test that needs ImportCommit to run a real fetch when the host's
// git does not carry the CVE-2024-32004 fix, because ImportCommit refuses such a git
// (checkImportGit) before it ever fetches. The gate's build host runs an older git than this
// Mac, so without this those tests would fail (or pass for the wrong reason) there rather than
// on the behaviour they check. The refusal itself stays proven by the version-stub test, which
// does not call this. A distro git with the fix backported onto an old version string skips
// here too, for the same reason the import refuses it: the version string is what is read.
func skipIfGitBelowFloor(t *testing.T) {
	t.Helper()
	out, err := exec.Command(realGit(t), "version").Output()
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	major, minor, patch, ok := parseGitVersion(strings.TrimSpace(string(out)))
	if !ok {
		t.Fatalf("cannot parse host git version %q", strings.TrimSpace(string(out)))
	}
	if !gitFixesCVE202432004(major, minor, patch) {
		t.Skipf("host git %d.%d.%d is below the CVE-2024-32004 floor (2.45.1, or a per-series "+
			"backport 2.39.4/2.40.2/2.41.1/2.42.2/2.43.4/2.44.1); ImportCommit refuses it, so this "+
			"real-import test cannot run here", major, minor, patch)
	}
}

// fixtureEnv is the environment fixture git commands run with: none of the test process's
// GIT_* variables, no global or system config, and a fixed identity. A test that hands the
// import a hostile environment does not change how its fixture is built.
func fixtureEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
}

// fgit runs git -C dir with args for a fixture, feeding it stdin, and returns its trimmed
// output.
func fgit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(realGit(t), append([]string{"-C", dir}, args...)...)
	cmd.Env = fixtureEnv()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fgitOK reports whether git -C dir with args succeeds.
func fgitOK(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command(realGit(t), append([]string{"-C", dir}, args...)...)
	cmd.Env = fixtureEnv()
	return cmd.Run() == nil
}

func commitFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fgit(t, dir, "", "add", "--", name)
	fgit(t, dir, "", "commit", "-q", "-m", name)
	return fgit(t, dir, "", "rev-parse", "HEAD")
}

// cloneFixture is the lead's repository and a worker's clone of it, provisioned the way a
// clone slot is: an empty-template init whose object store borrows main's through an
// alternates file, origin/main pinned at main's commit, and no remote pointing at main.
type cloneFixture struct {
	main, clone string
	base        string // main's commit, where the clone starts
	parent, tip string // the worker's two commits on ttorch/t1
}

func newCloneFixture(t *testing.T) cloneFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := cloneFixture{main: filepath.Join(root, "main"), clone: filepath.Join(root, "clone")}
	fgit(t, root, "", "init", "-q", "-b", "main", f.main)
	f.base = commitFile(t, f.main, "a.txt", "a\n")

	template := filepath.Join(root, "template")
	if err := os.Mkdir(template, 0o755); err != nil {
		t.Fatal(err)
	}
	fgit(t, root, "", "init", "-q", "--template="+template, "-b", "main", f.clone)
	common := fgit(t, f.main, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
	info := filepath.Join(f.clone, ".git", "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(common, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fgit(t, f.clone, "", "update-ref", "refs/remotes/origin/main", f.base)
	fgit(t, f.clone, "", "checkout", "-q", "-B", "ttorch/t1", f.base)
	f.parent = commitFile(t, f.clone, "one.txt", "one\n")
	f.tip = commitFile(t, f.clone, "two.txt", "two\n")
	return f
}

// refsOf lists every ref in repo as "<name> <id>", sorted.
func refsOf(t *testing.T, repo string) []string {
	t.Helper()
	out := fgit(t, repo, "", "for-each-ref", "--format=%(refname) %(objectname)")
	if out == "" {
		return nil
	}
	refs := strings.Split(out, "\n")
	slices.Sort(refs)
	return refs
}

func assertNoRef(t *testing.T, repo, ref string) {
	t.Helper()
	if fgitOK(t, repo, "rev-parse", "--verify", "--quiet", ref) {
		t.Fatalf("%s exists in %s after a refused import", ref, repo)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// unsetEnv removes key from the environment for the rest of the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		t.Setenv(key, v) // registers the restore
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// installGitShim puts a git on PATH that appends one line per call to a log, then runs body,
// then hands its arguments to the real git. It returns the log's path. Each line is the
// call's working directory and arguments, separated by \x1f.
func installGitShim(t *testing.T, body string) string {
	t.Helper()
	real := realGit(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf '%s' \"$PWD\"; for a in \"$@\"; do printf '\\037%s' \"$a\"; done; printf '\\n'; } >> '" + log + "'\n" +
		body + "\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// readCalls returns the shim log's calls, each as working directory then arguments.
func readCalls(t *testing.T, log string) [][]string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line != "" {
			calls = append(calls, strings.Split(line, "\x1f"))
		}
	}
	return calls
}

// TestImportCommit_ImportsOneRefAndNothingElse: a clone whose worker tagged "main", moved
// its own main branch, tagged the tip and pointed origin somewhere hostile imports as one ref
// in main and nothing else: no tag, no FETCH_HEAD, no config change, no shallow file. The sha
// imported is the one ObserveHead reads from the clone's files, and importing it again
// changes nothing.
func TestImportCommit_ImportsOneRefAndNothingElse(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	fgit(t, f.clone, "", "tag", "main")
	fgit(t, f.clone, "", "update-ref", "refs/heads/main", f.tip)
	fgit(t, f.clone, "", "tag", "-a", "evil-tag", "-m", "x", f.tip)
	fgit(t, f.clone, "", "config", "remote.origin.url", "https://evil.invalid/x.git")
	gitdir := filepath.Join(f.main, ".git")
	refsBefore := refsOf(t, f.main)
	configBefore, err := os.ReadFile(filepath.Join(gitdir, "config"))
	if err != nil {
		t.Fatal(err)
	}

	head, ok := ObserveHead(f.clone, f.main)
	if !ok || head != f.tip {
		t.Fatalf("ObserveHead(clone) = %q, %v; the clone's tip is %q", head, ok, f.tip)
	}
	ref, err := ImportCommit(context.Background(), f.main, f.clone, "t1", head)
	if err != nil {
		t.Fatal(err)
	}
	if want := CloneRefs + "t1/" + f.tip; ref != want {
		t.Fatalf("ImportCommit returned %q, want %q", ref, want)
	}
	want := slices.Sorted(slices.Values(append(slices.Clone(refsBefore), ref+" "+f.tip)))
	if got := refsOf(t, f.main); !slices.Equal(got, want) {
		t.Fatalf("main's refs after the import:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if typ := fgit(t, f.main, "", "cat-file", "-t", f.tip); typ != "commit" {
		t.Fatalf("main holds %s as a %s", f.tip, typ)
	}
	for _, name := range []string{"FETCH_HEAD", "shallow"} {
		if exists(filepath.Join(gitdir, name)) {
			t.Errorf("the import wrote %s in main", name)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(gitdir, "config")); string(after) != string(configBefore) {
		t.Errorf("the import changed main's config:\n%s", after)
	}

	again, err := ImportCommit(context.Background(), f.main, f.clone, "t1", head)
	if err != nil || again != ref {
		t.Fatalf("importing again = %q, %v; want %q", again, err, ref)
	}
	if got := refsOf(t, f.main); !slices.Equal(got, want) {
		t.Fatalf("importing again changed main's refs: %q", got)
	}
}

// TestImportCommit_ServesAnyCommitTheCloneHas: a sha no ref in the clone advertises, the
// tip's parent or a commit the worker orphaned, imports even when the lead's repository is
// set to protocol version 0, which refuses an unadvertised sha.
func TestImportCommit_ServesAnyCommitTheCloneHas(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	fgit(t, f.main, "", "config", "protocol.version", "0")
	fgit(t, f.clone, "", "checkout", "-q", "-b", "scratch")
	orphan := commitFile(t, f.clone, "three.txt", "three\n")
	fgit(t, f.clone, "", "checkout", "-q", "ttorch/t1")
	fgit(t, f.clone, "", "branch", "-q", "-D", "scratch")
	for _, c := range []struct{ name, sha string }{{"the tip's parent", f.parent}, {"an orphaned commit", orphan}} {
		if _, err := ImportCommit(context.Background(), f.main, f.clone, "t1", c.sha); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// TestImportCommit_RefusesADotGitTreeEntry: a commit whose tree holds an entry named .git is
// refused by the receiving side's object check and leaves no ref and no object in main.
func TestImportCommit_RefusesADotGitTreeEntry(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	blob := fgit(t, f.clone, "x\n", "hash-object", "-w", "--stdin")
	raw, err := hex.DecodeString(blob)
	if err != nil {
		t.Fatal(err)
	}
	tree := fgit(t, f.clone, "100644 .git\x00"+string(raw), "hash-object", "-t", "tree", "-w", "--stdin", "--literally")
	bad := fgit(t, f.clone, "", "commit-tree", tree, "-m", "bad")

	_, err = ImportCommit(context.Background(), f.main, f.clone, "t1", bad)
	if err == nil {
		t.Fatal("imported a commit whose tree has a .git entry")
	}
	t.Logf("refused: %v", err)
	assertNoRef(t, f.main, CloneRefs+"t1/"+bad)
	for _, sha := range []string{bad, tree, blob} {
		if fgitOK(t, f.main, "cat-file", "-e", sha) {
			t.Errorf("main holds %s after the refused import", sha)
		}
	}
}

// TestImportCommit_RefusesASwappedObject: a clone whose object file for one blob holds
// another blob's bytes serves those bytes under the first name. The receiving side hashes
// what it is sent and refuses the commit, and no ref is left.
func TestImportCommit_RefusesASwappedObject(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	blobA := fgit(t, f.clone, "", "rev-parse", f.tip+":two.txt")
	blobB := fgit(t, f.clone, "BBB\n", "hash-object", "-w", "--stdin")
	obj := func(sha string) string { return filepath.Join(f.clone, ".git", "objects", sha[:2], sha[2:]) }
	b, err := os.ReadFile(obj(blobB))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(obj(blobA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj(blobA), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fgit(t, f.clone, "", "cat-file", "-p", blobA); got != "BBB" {
		t.Fatalf("the swap did not take: the clone serves %q under %s", got, blobA)
	}

	_, err = ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
	if err == nil {
		t.Fatal("imported a commit whose blob the clone serves with the wrong bytes")
	}
	t.Logf("refused: %v", err)
	assertNoRef(t, f.main, CloneRefs+"t1/"+f.tip)
}

// TestImportCommit_RefusesAShallowBoundary: a commit whose history crosses a shallow
// boundary the worker made in its clone is refused by git with a warning and exit 0, so the
// import has to read its ref back to fail, and the refusal carries git's warning so whoever
// adjudicates it can see why. main must not become shallow.
func TestImportCommit_RefusesAShallowBoundary(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	up := filepath.Join(filepath.Dir(f.main), "unrelated")
	fgit(t, filepath.Dir(up), "", "init", "-q", "-b", "main", up)
	for _, n := range []string{"u1", "u2", "u3"} {
		commitFile(t, up, n, n+"\n")
	}
	fgit(t, f.clone, "", "fetch", "-q", "--depth=1", "file://"+up, "main")
	fgit(t, f.clone, "", "checkout", "-q", "--detach", "FETCH_HEAD")
	fgit(t, f.clone, "", "commit", "-q", "--allow-empty", "-m", "work")
	work := fgit(t, f.clone, "", "rev-parse", "HEAD")
	if !exists(filepath.Join(f.clone, ".git", "shallow")) {
		t.Fatal("the clone is not shallow; the fixture is wrong")
	}

	_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", work)
	if err == nil {
		t.Fatal("imported a commit across a shallow boundary")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "no ref") || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("the refusal does not say the fetch left no ref because of the shallow boundary: %v", err)
	}
	assertNoRef(t, f.main, CloneRefs+"t1/"+work)
	if exists(filepath.Join(f.main, ".git", "shallow")) {
		t.Fatal("the import made main shallow")
	}
}

// TestImportCommit_RefusesANonCommit: git imports a blob, a tree or an annotated tag without
// complaint, so the import checks that its ref peels to the commit sha itself and deletes
// the ref when it does not. A tag peels to the commit it names, which is not the sha asked
// for.
func TestImportCommit_RefusesANonCommit(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	fgit(t, f.clone, "", "tag", "-a", "v1", "-m", "x", f.tip)
	for _, c := range []struct{ name, sha string }{
		{"blob", fgit(t, f.clone, "just a blob\n", "hash-object", "-w", "--stdin")},
		{"tree", fgit(t, f.clone, "", "rev-parse", f.tip+"^{tree}")},
		{"annotated tag", fgit(t, f.clone, "", "rev-parse", "refs/tags/v1")},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", c.sha)
			if err == nil {
				t.Fatalf("imported a %s as a commit", c.name)
			}
			t.Logf("refused: %v", err)
			assertNoRef(t, f.main, CloneRefs+"t1/"+c.sha)
		})
	}
}

// TestImportCommit_UnknownShaFailsClosed: a sha the clone does not have, as a stale
// observation would be, fails and leaves no ref.
func TestImportCommit_UnknownShaFailsClosed(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	missing := strings.Repeat("0123456789", 4)
	if _, err := ImportCommit(context.Background(), f.main, f.clone, "t1", missing); err == nil {
		t.Fatal("imported a sha the clone does not have")
	}
	assertNoRef(t, f.main, CloneRefs+"t1/"+missing)
}

// TestImportCommit_RefusesMalformedInputBeforeGit: a malformed task id, sha or clone path is
// refused before any git command runs.
func TestImportCommit_RefusesMalformedInputBeforeGit(t *testing.T) {
	f := newCloneFixture(t)
	log := installGitShim(t, "")
	for _, c := range []struct{ name, repo, clone, task, sha string }{
		{"task id with ..", f.main, f.clone, "a..b", f.tip},
		{"task id that reads as an option", f.main, f.clone, "-x", f.tip},
		{"task id with a slash", f.main, f.clone, "a/b", f.tip},
		{"task id ending in .lock", f.main, f.clone, "t.lock", f.tip},
		{"empty task id", f.main, f.clone, "", f.tip},
		{"abbreviated sha", f.main, f.clone, "t1", f.tip[:12]},
		{"uppercase sha", f.main, f.clone, "t1", strings.ToUpper(f.tip)},
		{"sha carrying a refspec", f.main, f.clone, "t1", f.tip + ":refs/heads/main"},
		{"a ref name, not a sha", f.main, f.clone, "t1", "HEAD"},
		{"relative clone path", f.main, "clone", "t1", f.tip},
		{"clone path that is not clean", f.main, f.clone + "/../clone", "t1", f.tip},
		{"clone path that reads as an option", f.main, "--upload-pack=touch /tmp/x", "t1", f.tip},
		{"no repository", "", f.clone, "t1", f.tip},
	} {
		if _, err := ImportCommit(context.Background(), c.repo, c.clone, c.task, c.sha); err == nil {
			t.Errorf("%s: imported", c.name)
		}
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("malformed input reached git:\n%q", calls)
	}
}

// TestImportCommit_RunsNoProgramItWasNotAskedTo: no program runs during an import that the
// clone's config or hooks name, that main's own hooks name, or that the lead's global config
// names. main's reference-transaction hook runs on the import's ref update unless the import
// points core.hooksPath away from it. A global uploadpack.packObjectsHook is honoured by the
// clone's upload-pack, which reads global config and does not see the -c settings the fetch
// is given, so only the import's own GIT_CONFIG_GLOBAL keeps it from running.
func TestImportCommit_RunsNoProgramItWasNotAskedTo(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	type sentinel struct {
		script string
		ran    func() bool
	}
	newSentinel := func() sentinel { s, ran := runsNoProgram(t); return sentinel{s, ran} }
	plantHooks := func(dir, script string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{
			"reference-transaction", "post-checkout", "post-merge", "pre-auto-gc", "post-rewrite",
			"post-index-change", "pre-receive", "update", "post-receive", "post-update",
			"push-to-checkout", "proc-receive", "fsmonitor-watchman", "pre-commit", "commit-msg",
		} {
			body := "#!/bin/sh\n'" + script + "'\nexit 0\n"
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	sentinels := map[string]sentinel{}
	// The clone's config and hooks. Set last: from here on no fixture command runs in the
	// clone, since its own git would run them.
	for _, key := range []string{"core.fsmonitor", "uploadpack.packObjectsHook", "core.alternateRefsCommand", "diff.external", "core.sshCommand"} {
		s := newSentinel()
		sentinels["the clone's "+key] = s
		fgit(t, f.clone, "", "config", key, s.script)
	}
	clonePath := newSentinel()
	sentinels["the clone's core.hooksPath hooks"] = clonePath
	hooksDir := filepath.Join(t.TempDir(), "clone-hooks")
	plantHooks(hooksDir, clonePath.script)
	fgit(t, f.clone, "", "config", "core.hooksPath", hooksDir)
	cloneHooks := newSentinel()
	sentinels["the clone's .git/hooks"] = cloneHooks
	plantHooks(filepath.Join(f.clone, ".git", "hooks"), cloneHooks.script)

	mainHooks := newSentinel()
	sentinels["main's .git/hooks"] = mainHooks
	plantHooks(filepath.Join(f.main, ".git", "hooks"), mainHooks.script)

	global := newSentinel()
	sentinels["the lead's global uploadpack.packObjectsHook"] = global
	cfg := "[uploadpack]\n\tpackObjectsHook = " + global.script + "\n"
	home, xdg := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	unsetEnv(t, "GIT_CONFIG_GLOBAL")

	_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range sentinels {
			if !yield(k) {
				return
			}
		}
	})
	for _, name := range names {
		if sentinels[name].ran() {
			t.Errorf("the import ran the program %s names", name)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestImportCommit_GivesUpOnACloneThatNeverAnswers: a fetch that does not finish, here a git
// that forks a child and waits on it, ends at the caller's deadline together with what it
// forked, rather than whenever the child exits.
func TestImportCommit_GivesUpOnACloneThatNeverAnswers(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	installGitShim(t, `for a in "$@"; do if [ "$a" = fetch ]; then sleep 30 & wait; exit 1; fi; done`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ImportCommit(ctx, f.main, f.clone, "t1", f.tip)
	took := time.Since(start)
	if err == nil {
		t.Fatal("a fetch that never finished imported")
	}
	if took > 15*time.Second {
		t.Fatalf("the import took %v to give up after a 300ms deadline", took)
	}
	t.Logf("gave up after %v: %v", took.Round(time.Millisecond), err)
}

// TestImportCommit_TouchesTheCloneOnlyThroughTheFetch: observing a clone's head starts no
// git process, and the import runs every git command in main. No call has -C, --git-dir or
// a working directory in the clone, and the clone's path appears only as the fetch's source
// after "--". An import whose ref is already in place does not fetch at all.
func TestImportCommit_TouchesTheCloneOnlyThroughTheFetch(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	log := installGitShim(t, "")
	head, ok := ObserveHead(f.clone, f.main)
	if !ok {
		t.Fatal("ObserveHead could not read the clone's head")
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("ObserveHead started git:\n%q", calls)
	}
	if _, err := ImportCommit(context.Background(), f.main, f.clone, "t1", head); err != nil {
		t.Fatal(err)
	}
	isFetch := func(args []string) bool { return slices.Contains(args, "fetch") }
	// The fetch names the clone only as a file:// URL after "--", never a bare path (which git
	// would read as a possible bundle) and never through --git-dir/--work-tree.
	fetchURL := "file://" + f.clone
	fetches := 0
	for _, call := range readCalls(t, log) {
		cwd, args := call[0], call[1:]
		if within(f.clone, cwd) {
			t.Errorf("git ran with its working directory in the clone: %q", call)
		}
		target := ""
		if i := slices.Index(args, "-C"); i >= 0 && i+1 < len(args) {
			target = args[i+1]
		}
		if target != f.main && !slices.Contains(args, "version") {
			t.Errorf("git ran against %q, not main: %q", target, args)
		}
		for i, a := range args {
			fetchSource := isFetch(args) && i > 0 && args[i-1] == "--" && a == fetchURL
			if (strings.Contains(a, f.clone) || strings.HasPrefix(a, "--git-dir") || strings.HasPrefix(a, "--work-tree")) && !fetchSource {
				t.Errorf("git was handed the clone other than as the fetch's file:// source: %q", args)
			}
		}
		if isFetch(args) {
			fetches++
			if i := slices.Index(args, "--"); i < 0 || i+1 >= len(args) || args[i+1] != fetchURL {
				t.Errorf("the fetch source is not the clone's file:// URL: %q", args)
			}
		}
	}
	if fetches != 1 {
		t.Errorf("%d fetches, want 1", fetches)
	}

	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCommit(context.Background(), f.main, f.clone, "t1", head); err != nil {
		t.Fatal(err)
	}
	for _, call := range readCalls(t, log) {
		if isFetch(call[1:]) {
			t.Errorf("an import already in place fetched again: %q", call)
		}
	}
}

// TestImportCommit_StartsNoRepackInMain: a fetch ends with an automatic gc in the receiving
// repository when its threshold is met. The import turns that off, so with gc.auto at 1, gc
// in the foreground and two loose objects in objects/17 (the directory gc samples), the
// loose objects are still there afterwards.
func TestImportCommit_StartsNoRepackInMain(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	var loose []string
	for i := 0; len(loose) < 2; i++ {
		body := fmt.Sprintf("filler %d\n", i)
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(body), body)))
		if sum[0] != 0x17 {
			continue
		}
		name := fmt.Sprintf("f%d.txt", i)
		commitFile(t, f.main, name, body)
		id := hex.EncodeToString(sum[:])
		loose = append(loose, filepath.Join(f.main, ".git", "objects", id[:2], id[2:]))
	}
	for _, p := range loose {
		if !exists(p) {
			t.Fatalf("%s is not loose; the fixture is wrong", p)
		}
	}
	// Set after the commits above, which would otherwise run the gc themselves.
	fgit(t, f.main, "", "config", "gc.auto", "1")
	fgit(t, f.main, "", "config", "gc.autoDetach", "false")
	fgit(t, f.main, "", "config", "maintenance.autoDetach", "false")

	if _, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip); err != nil {
		t.Fatal(err)
	}
	for _, p := range loose {
		if !exists(p) {
			t.Errorf("the import repacked main: %s is gone", p)
		}
	}
}

// TestImportCommit_IgnoresTheCallersGitEnvironment: the import means the same thing whoever
// runs it. A GIT_DIR in the caller's environment would point it at another repository, and a
// GIT_OBJECT_DIRECTORY would write the commit somewhere main does not read, while the import
// still reported success.
func TestImportCommit_IgnoresTheCallersGitEnvironment(t *testing.T) {
	skipIfGitBelowFloor(t)
	for _, c := range []struct {
		name string
		env  func(t *testing.T, f cloneFixture) (key, value string)
	}{
		{"GIT_DIR", func(t *testing.T, f cloneFixture) (string, string) {
			other := filepath.Join(t.TempDir(), "other")
			fgit(t, filepath.Dir(other), "", "init", "-q", other)
			return "GIT_DIR", filepath.Join(other, ".git")
		}},
		{"GIT_OBJECT_DIRECTORY", func(t *testing.T, f cloneFixture) (string, string) {
			return "GIT_OBJECT_DIRECTORY", t.TempDir()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCloneFixture(t)
			key, value := c.env(t, f)
			t.Setenv(key, value)
			ref, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
			if err != nil {
				t.Fatal(err)
			}
			if got := fgit(t, f.main, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}"); got != f.tip {
				t.Fatalf("%s in main = %q, want %q", ref, got, f.tip)
			}
			if !fgitOK(t, f.main, "cat-file", "-e", f.tip) {
				t.Fatalf("main does not hold %s after the import", f.tip)
			}
		})
	}
}

// TestImportCommit_RefusesGitWithoutTheCVEFix: on a git without the CVE-2024-32004 fix the
// clone's upload-pack can lazy-fetch through a worker-named promisor and run its program, which
// no flag of the fetch stops. The import refuses before it fetches on such a git, or one whose
// version it cannot read, and runs on a fixed one. The boundary is per-series: a backport is
// fixed, the release just below it is not.
func TestImportCommit_RefusesGitWithoutTheCVEFix(t *testing.T) {
	for _, c := range []struct {
		version string
		ok      bool
	}{
		{"git version 2.45.1", true},
		{"git version 2.45.0", false},
		{"git version 2.44.1", true},
		{"git version 2.44.0", false},
		{"git version 2.43.4", true},
		{"git version 2.43.3", false},
		{"git version 2.39.4", true},
		{"git version 2.39.3", false},
		{"git version 2.38.5", false},
		{"git version 2.50.1 (vendor build 155)", true},
		{"git version 3.0.0", true},
		{"git version 2.31.0", false},
		{"git version garbage", false},
	} {
		t.Run(c.version, func(t *testing.T) {
			f := newCloneFixture(t)
			log := installGitShim(t, `for a in "$@"; do if [ "$a" = version ]; then echo '`+c.version+`'; exit 0; fi; done`)
			_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
			if c.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("imported on a git without the CVE-2024-32004 fix")
			}
			t.Logf("refused: %v", err)
			for _, call := range readCalls(t, log) {
				if slices.Contains(call, "fetch") {
					t.Fatalf("fetched before refusing the version: %q", call)
				}
			}
			assertNoRef(t, f.main, CloneRefs+"t1/"+f.tip)
		})
	}
}

func TestParseGitVersion(t *testing.T) {
	for _, c := range []struct {
		out                 string
		major, minor, patch int
		ok                  bool
	}{
		{"git version 2.50.1 (vendor build 155)", 2, 50, 1, true},
		{"git version 2.45.0.rc1.17.gabcdef0", 2, 45, 0, true},
		{"git version 2.39.4", 2, 39, 4, true},
		{"git version 2.32.0\n", 2, 32, 0, true},
		{"git version 2.43", 2, 43, 0, true}, // missing patch reads as 0
		{"git version 2", 0, 0, 0, false},
		{"git version x.y.z", 0, 0, 0, false},
		{"git version -1.40.0", 0, 0, 0, false},
		{"version 2.50.1", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	} {
		major, minor, patch, ok := parseGitVersion(c.out)
		if major != c.major || minor != c.minor || patch != c.patch || ok != c.ok {
			t.Errorf("parseGitVersion(%q) = %d, %d, %d, %v; want %d, %d, %d, %v",
				c.out, major, minor, patch, ok, c.major, c.minor, c.patch, c.ok)
		}
	}
}

func TestGitFixesCVE202432004(t *testing.T) {
	for _, c := range []struct {
		major, minor, patch int
		want                bool
	}{
		{2, 45, 1, true}, {2, 45, 0, false}, {2, 45, 2, true},
		{2, 44, 1, true}, {2, 44, 0, false},
		{2, 43, 4, true}, {2, 43, 3, false},
		{2, 42, 2, true}, {2, 42, 1, false},
		{2, 41, 1, true}, {2, 41, 0, false},
		{2, 40, 2, true}, {2, 40, 1, false},
		{2, 39, 4, true}, {2, 39, 3, false},
		{2, 38, 9, false}, {2, 46, 0, true}, {2, 50, 1, true},
		{3, 0, 0, true}, {1, 99, 9, false},
	} {
		if got := gitFixesCVE202432004(c.major, c.minor, c.patch); got != c.want {
			t.Errorf("gitFixesCVE202432004(%d,%d,%d) = %v, want %v", c.major, c.minor, c.patch, got, c.want)
		}
	}
}

// TestImportGitOK: the exported check accepts exactly what gitFixesCVE202432004 accepts, read
// from `git version` output, and reports output it cannot read as unreadable.
func TestImportGitOK(t *testing.T) {
	for _, c := range []struct {
		out          string
		ok, readable bool
	}{
		{"git version 2.50.1 (vendor build 155)", true, true},
		{"git version 2.45.1", true, true},
		{"git version 2.45.0", false, true},
		{"git version 2.45.0.rc1.17.gabcdef0", false, true},
		{"git version 2.44.1", true, true},
		{"git version 2.43.4", true, true},
		{"git version 2.43.0", false, true},
		{"git version 2.39.4", true, true},
		{"git version 2.38.9", false, true},
		{"git version 2.32.0\n", false, true},
		{"git version 2.46.0.windows.1", true, true},
		{"git version x.y.z", false, false},
		{"", false, false},
	} {
		ok, readable := ImportGitOK(c.out)
		if ok != c.ok || readable != c.readable {
			t.Errorf("ImportGitOK(%q) = %v, %v; want %v, %v", c.out, ok, readable, c.ok, c.readable)
		}
	}
}

// TestImportGitFloor: the floor text is built from the same table the check reads, newest
// series first and every backport after it.
func TestImportGitFloor(t *testing.T) {
	want := "2.45.1, or the backport for its series (2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4, 2.44.1)"
	if got := ImportGitFloor(); got != want {
		t.Errorf("ImportGitFloor() = %q, want %q", got, want)
	}
}

// TestDropImports_DeletesOnlyThatTasksRefs: dropping a task's imports deletes every ref under
// its directory in CloneRefs, imports and the other refs kept there alike, and nothing of a
// task whose id merely starts the same way, nor any ref outside the namespace. main's hooks do
// not run, a second drop is a no-op, and the commit can be imported again afterwards.
func TestDropImports_DeletesOnlyThatTasksRefs(t *testing.T) {
	skipIfGitBelowFloor(t)
	f := newCloneFixture(t)
	ctx := context.Background()
	for _, c := range []struct{ task, sha string }{{"t1", f.tip}, {"t1", f.parent}, {"t10", f.tip}, {"t1x", f.tip}} {
		if _, err := ImportCommit(ctx, f.main, f.clone, c.task, c.sha); err != nil {
			t.Fatal(err)
		}
	}
	fgit(t, f.main, "", "update-ref", CloneRefs+"t1/base", f.base)
	fgit(t, f.main, "", "update-ref", "refs/ttorch/discarded/t1-"+f.base[:12], f.base)
	keep := []string{}
	for _, r := range refsOf(t, f.main) {
		if !strings.HasPrefix(r, CloneRefs+"t1/") {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(refsOf(t, f.main)) {
		t.Fatal("the fixture has no refs under t1/ to drop")
	}
	hooks, ran := runsNoProgram(t)
	hookFile := filepath.Join(f.main, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hookFile, []byte("#!/bin/sh\n'"+hooks+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DropImports(ctx, f.main, "t1"); err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, f.main); !slices.Equal(got, keep) {
		t.Fatalf("main's refs after dropping t1:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(keep, "\n  "))
	}
	if ran() {
		t.Error("dropping the imports ran main's reference-transaction hook")
	}
	if err := DropImports(ctx, f.main, "t1"); err != nil {
		t.Fatalf("a second drop: %v", err)
	}
	if err := os.Remove(hookFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCommit(ctx, f.main, f.clone, "t1", f.tip); err != nil {
		t.Fatalf("importing again after the drop: %v", err)
	}
}

// TestDropImports_RefusesMalformedTaskIDBeforeGit: a task id that is not one ref component
// never reaches git. "*" in particular would match every task's refs.
func TestDropImports_RefusesMalformedTaskIDBeforeGit(t *testing.T) {
	f := newCloneFixture(t)
	log := installGitShim(t, "")
	for _, task := range []string{"", "*", "t*", "a..b", "-x", "t1/..", "a/b", "t1/"} {
		if err := DropImports(context.Background(), f.main, task); err == nil {
			t.Errorf("DropImports(%q) succeeded", task)
		}
	}
	if err := DropImports(context.Background(), "", "t1"); err == nil {
		t.Error("DropImports with no repository succeeded")
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("a malformed task id reached git:\n%q", calls)
	}
}

// TestImportEnv_DisablesLazyFetch: importEnv sets GIT_NO_LAZY_FETCH=1, and because it strips
// every GIT_* first, a hostile GIT_NO_LAZY_FETCH=0 in the caller's environment cannot override
// it, with no duplicate entry for resolution to depend on. On this git the behavioural promisor
// test cannot show this line red (git's internal default already blocks the fetch), so this
// direct assertion is what guards it; an earlier commit split dropped it and a removal of the
// line went uncaught.
func TestImportEnv_DisablesLazyFetch(t *testing.T) {
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	t.Setenv("GIT_DIR", "/somewhere/else")
	last, count := "", 0
	for _, kv := range importEnv() {
		if v, ok := strings.CutPrefix(kv, "GIT_NO_LAZY_FETCH="); ok {
			last, count = v, count+1
		}
		if strings.HasPrefix(kv, "GIT_DIR=") {
			t.Error("importEnv kept the caller's GIT_DIR")
		}
	}
	if last != "1" || count != 1 {
		t.Errorf("importEnv GIT_NO_LAZY_FETCH: value %q, count %d; want \"1\", 1", last, count)
	}
}

// TestImportEnv_RestrictsProtocol: importEnv sets GIT_ALLOW_PROTOCOL=file, and because it
// strips every GIT_* first, a hostile GIT_ALLOW_PROTOCOL=all in the caller's environment
// cannot override it, with no duplicate entry for resolution to depend on.
func TestImportEnv_RestrictsProtocol(t *testing.T) {
	t.Setenv("GIT_ALLOW_PROTOCOL", "all")
	last, count := "", 0
	for _, kv := range importEnv() {
		if v, ok := strings.CutPrefix(kv, "GIT_ALLOW_PROTOCOL="); ok {
			last, count = v, count+1
		}
	}
	if last != "file" || count != 1 {
		t.Errorf("importEnv GIT_ALLOW_PROTOCOL: value %q, count %d; want \"file\", 1", last, count)
	}
}

// promisorClone builds main, and a clone that (a) borrows main's objects by alternates so it
// genuinely lacks an object that lives only in an upstream, (b) has that upstream configured
// as a promisor remote whose upload-pack is a marker script, and (c) carries a commit whose
// tree references the absent object. Importing that commit is what would make the clone's
// upload-pack lazy-fetch through the marker. It returns main, the clone, the bad commit, and a
// func reporting whether the marker ran.
func promisorClone(t *testing.T) (main, clone, bad string, markerRan func() bool) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(root, "up")
	fgit(t, root, "", "init", "-q", "-b", "main", up)
	commitFile(t, up, "secret", "secret\n")
	blob := fgit(t, up, "", "rev-parse", "HEAD:secret")

	marker := filepath.Join(root, "ran")
	script := filepath.Join(root, "up.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexec git upload-pack \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	main = filepath.Join(root, "main")
	fgit(t, root, "", "init", "-q", "-b", "main", main)
	base := commitFile(t, main, "a.txt", "a\n")

	clone = filepath.Join(root, "clone")
	fgit(t, root, "", "init", "-q", "-b", "main", clone)
	common := fgit(t, main, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
	info := filepath.Join(clone, ".git", "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(common, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fgit(t, clone, "", "update-ref", "refs/remotes/origin/main", base)
	fgit(t, clone, "", "checkout", "-q", "-B", "ttorch/t1", base)
	tree := fgit(t, clone, "100644 blob "+blob+"\tsecret\n", "mktree", "--missing")
	bad = fgit(t, clone, "", "commit-tree", tree, "-p", base, "-m", "references a promised blob the clone lacks")
	fgit(t, clone, "", "update-ref", "refs/heads/ttorch/t1", bad)
	fgit(t, clone, "", "config", "core.repositoryformatversion", "1")
	fgit(t, clone, "", "config", "extensions.partialClone", "origin")
	fgit(t, clone, "", "config", "remote.origin.promisor", "true")
	fgit(t, clone, "", "config", "remote.origin.partialclonefilter", "blob:none")
	fgit(t, clone, "", "config", "remote.origin.url", "file://"+up)
	fgit(t, clone, "", "config", "remote.origin.uploadpack", script)

	// Confirm the blob is absent WITHOUT triggering a lazy fetch (which would run the marker
	// and cache the blob): GIT_NO_LAZY_FETCH=1 makes cat-file report absence instead of fetching.
	probe := exec.Command(realGit(t), "-C", clone, "cat-file", "-e", blob)
	probe.Env = append(fixtureEnv(), "GIT_NO_LAZY_FETCH=1")
	if probe.Run() == nil {
		t.Skip("the clone already has the promised blob; cannot stage a lazy fetch")
	}
	return main, clone, bad, func() bool { _, err := os.Stat(marker); return err == nil }
}

// TestImportCommit_RunsNoPromisorProgram: importing a commit that references an object the
// clone lacks must not run the worker's promisor program, even when the ambient environment
// re-enables lazy fetch (as GIT_NO_LAZY_FETCH=0 does on a fixed git). The control shows the
// same clone does run the marker when the fetch is let to lazy-fetch, so the test is not
// vacuous.
func TestImportCommit_RunsNoPromisorProgram(t *testing.T) {
	skipIfGitBelowFloor(t)
	// Control: a fetch allowed to lazy-fetch runs the worker's program.
	t.Run("control: the attack is real", func(t *testing.T) {
		main, clone, bad, ran := promisorClone(t)
		cmd := exec.Command(realGit(t), "-C", main, "-c", "protocol.version=2",
			"fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", clone, bad+":refs/ttorch/clones/t1/"+bad)
		cmd.Env = append(fixtureEnv(), "GIT_NO_LAZY_FETCH=0")
		_ = cmd.Run()
		if !ran() {
			t.Skip("git did not lazy-fetch even with GIT_NO_LAZY_FETCH=0; cannot demonstrate the attack on this git")
		}
	})

	main, clone, bad, ran := promisorClone(t)
	t.Setenv("GIT_NO_LAZY_FETCH", "0") // the hostile ambient value importEnv must override
	_, err := ImportCommit(context.Background(), main, clone, "t1", bad)
	if ran() {
		t.Fatal("the import ran the clone's promisor program")
	}
	if err == nil {
		t.Fatal("importing a commit referencing an object the clone lacks should fail closed")
	}
	t.Logf("refused: %v", err)
	assertNoRef(t, main, CloneRefs+"t1/"+bad)
}

// dotGitTreeCommit commits, in the clone, a tree holding an entry literally named .git, which
// transfer.fsckObjects refuses. It returns the commit sha.
func dotGitTreeCommit(t *testing.T, clone string) string {
	t.Helper()
	blob := fgit(t, clone, "x\n", "hash-object", "-w", "--stdin")
	raw, err := hex.DecodeString(blob)
	if err != nil {
		t.Fatal(err)
	}
	tree := fgit(t, clone, "100644 .git\x00"+string(raw), "hash-object", "-t", "tree", "-w", "--stdin", "--literally")
	return fgit(t, clone, "", "commit-tree", tree, "-m", "dotgit")
}

// fsckHostileCommit commits, in the clone, an object that trips the named fsck check, and
// returns the commit sha. The checks map to the severities importConfig pins.
func fsckHostileCommit(t *testing.T, clone, check string) string {
	t.Helper()
	switch check {
	case "hasDotgit":
		return dotGitTreeCommit(t, clone)
	case "hasDotdot":
		blob := fgit(t, clone, "x\n", "hash-object", "-w", "--stdin")
		tree := fgit(t, clone, "100644 blob "+blob+"\t..\n", "mktree")
		return fgit(t, clone, "", "commit-tree", tree, "-m", "dotdot")
	case "gitmodulesUrl":
		return gitmodulesCommit(t, clone, "[submodule \"x\"]\n\tpath = x\n\turl = -u./payload\n")
	case "gitmodulesPath":
		return gitmodulesCommit(t, clone, "[submodule \"x\"]\n\tpath = -evil\n\turl = ./ok\n")
	case "gitmodulesSymlink":
		blob := fgit(t, clone, "/etc/passwd", "hash-object", "-w", "--stdin")
		tree := fgit(t, clone, "120000 blob "+blob+"\t.gitmodules\n", "mktree")
		return fgit(t, clone, "", "commit-tree", tree, "-m", "gmsymlink")
	}
	t.Fatalf("unknown fsck check %q", check)
	return ""
}

// gitmodulesCommit commits a tree whose .gitmodules blob is body.
func gitmodulesCommit(t *testing.T, clone, body string) string {
	t.Helper()
	blob := fgit(t, clone, body, "hash-object", "-w", "--stdin")
	tree := fgit(t, clone, "100644 blob "+blob+"\t.gitmodules\n", "mktree")
	return fgit(t, clone, "", "commit-tree", tree, "-m", "gitmodules")
}

// TestImportCommit_FsckPinnedAgainstMainConfig: repo's own config must not be able to turn off
// the object check. fetch.fsckObjects=false beats -c transfer.fsckObjects=true, and each
// fetch.fsck.<check>=ignore turns off that one check even with fsck on; the import pins
// fetch.fsckObjects and every severity it relies on, so the matching hostile object is refused
// with any of them set in repo.
func TestImportCommit_FsckPinnedAgainstMainConfig(t *testing.T) {
	skipIfGitBelowFloor(t)
	for _, c := range []struct {
		name, key, value, check string
	}{
		{"fetch.fsckObjects=false", "fetch.fsckObjects", "false", "hasDotgit"},
		{"transfer.fsckObjects=false", "transfer.fsckObjects", "false", "hasDotgit"},
		{"fetch.fsck.hasDotgit=ignore", "fetch.fsck.hasDotgit", "ignore", "hasDotgit"},
		{"fetch.fsck.hasDotdot=ignore", "fetch.fsck.hasDotdot", "ignore", "hasDotdot"},
		{"fetch.fsck.gitmodulesUrl=ignore", "fetch.fsck.gitmodulesUrl", "ignore", "gitmodulesUrl"},
		{"fetch.fsck.gitmodulesPath=ignore", "fetch.fsck.gitmodulesPath", "ignore", "gitmodulesPath"},
		{"fetch.fsck.gitmodulesSymlink=ignore", "fetch.fsck.gitmodulesSymlink", "ignore", "gitmodulesSymlink"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCloneFixture(t)
			fgit(t, f.main, "", "config", c.key, c.value)
			bad := fsckHostileCommit(t, f.clone, c.check)
			_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", bad)
			if err == nil {
				t.Fatalf("imported a %s object with %s set in main", c.check, c.name)
			}
			t.Logf("refused: %v", err)
			assertNoRef(t, f.main, CloneRefs+"t1/"+bad)
			if fgitOK(t, f.main, "cat-file", "-e", bad) {
				t.Errorf("main holds %s after the refused import", bad)
			}
		})
	}
}

// TestImportCommit_RefusesExtProtocolRewrite: repo config that rewrites the clone source into
// an ext:: URL and allows the ext transport turns a fetch into command execution. The import
// sets GIT_ALLOW_PROTOCOL=file, so the ext helper never runs. The control shows the rewrite
// does run it when the protocol is not restricted.
func TestImportCommit_RefusesExtProtocolRewrite(t *testing.T) {
	skipIfGitBelowFloor(t)
	setup := func(t *testing.T) (f cloneFixture, ran func() bool) {
		t.Helper()
		f = newCloneFixture(t)
		dir := filepath.Dir(f.main)
		marker := filepath.Join(dir, "ran-ext")
		helper := filepath.Join(dir, "ext.sh")
		if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch '"+marker+"'\nexec git upload-pack .\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		fgit(t, f.main, "", "config", "protocol.ext.allow", "always")
		fgit(t, f.main, "", "config", "url.ext::"+helper+" .insteadOf", "file://"+f.clone)
		fgit(t, f.main, "", "config", "--add", "url.ext::"+helper+" .insteadOf", f.clone)
		return f, func() bool { _, err := os.Stat(marker); return err == nil }
	}

	t.Run("control: the rewrite runs a command", func(t *testing.T) {
		f, ran := setup(t)
		cmd := exec.Command(realGit(t), "-C", f.main, "-c", "protocol.version=2", "-c", "protocol.ext.allow=always",
			"fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", f.clone, f.tip+":refs/ttorch/clones/t1/"+f.tip)
		cmd.Env = fixtureEnv() // no GIT_ALLOW_PROTOCOL restriction
		_ = cmd.Run()
		if !ran() {
			t.Skip("git did not run the ext helper; cannot demonstrate the attack on this git")
		}
	})

	f, ran := setup(t)
	_, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
	if ran() {
		t.Fatal("the import ran the ext:: helper the repo config named")
	}
	if err == nil {
		t.Fatal("the import should fail when repo rewrites the source to an ext transport")
	}
	t.Logf("refused: %v", err)
	assertNoRef(t, f.main, CloneRefs+"t1/"+f.tip)
}

// TestImportCommit_RefusesABundleAtThePath: a regular file at the clone path would be read as a
// git bundle, a transport that honours a worker-written file. The import rejects a path that is
// not a real directory before it fetches, and passes the source as a file:// URL, which never
// takes the bundle route. The control shows a bare-path fetch does consume the bundle.
func TestImportCommit_RefusesABundleAtThePath(t *testing.T) {
	skipIfGitBelowFloor(t)
	build := func(t *testing.T) (main, bundle, ref, want string) {
		t.Helper()
		f := newCloneFixture(t)
		bundle = filepath.Join(filepath.Dir(f.main), "clonefile")
		fgit(t, f.clone, "", "bundle", "create", bundle, "refs/heads/ttorch/t1")
		return f.main, bundle, "refs/heads/ttorch/t1:refs/ttorch/clones/t1/x", f.tip
	}

	t.Run("control: a bare path consumes the bundle", func(t *testing.T) {
		main, bundle, ref, want := build(t)
		cmd := exec.Command(realGit(t), "-C", main, "fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", bundle, ref)
		cmd.Env = fixtureEnv()
		if err := cmd.Run(); err != nil {
			t.Skipf("git did not consume the bundle on this git: %v", err)
		}
		if got := fgit(t, main, "", "rev-parse", "--verify", "--quiet", "refs/ttorch/clones/t1/x"); got != want {
			t.Skipf("bundle route did not produce the ref; cannot demonstrate on this git")
		}
	})

	main, bundle, _, _ := build(t)
	// Point the import at the bundle file in place of a clone directory. The sha only has to
	// pass the argument checks; the Lstat refuses the file before any fetch.
	_, err := ImportCommit(context.Background(), main, bundle, "t1", strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("the import treated a bundle file as a clone")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the refusal does not name the real reason: %v", err)
	}
	assertNoRef(t, main, CloneRefs+"t1/"+strings.Repeat("a", 40))
}

// TestImportCommit_RefusesUploadPackConfigOverride: repo's own config can set
// remote."file://<clone>".uploadpack to any command, which runs when the local fetch starts
// the upload-pack for that URL; GIT_ALLOW_PROTOCOL=file does not stop it, because it governs
// the transport, not the upload-pack binary. The import passes --upload-pack=git-upload-pack,
// which the command line wins over the config key, so the command never runs. The control
// shows the same config runs the command when --upload-pack is not passed.
func TestImportCommit_RefusesUploadPackConfigOverride(t *testing.T) {
	skipIfGitBelowFloor(t)
	setup := func(t *testing.T) (f cloneFixture, ran func() bool) {
		t.Helper()
		f = newCloneFixture(t)
		marker := filepath.Join(filepath.Dir(f.main), "ran-up")
		script := filepath.Join(filepath.Dir(f.main), "up.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexec git-upload-pack \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		fgit(t, f.main, "", "config", "remote.file://"+f.clone+".uploadpack", script)
		return f, func() bool { _, err := os.Stat(marker); return err == nil }
	}

	t.Run("control: the config key runs a command", func(t *testing.T) {
		f, ran := setup(t)
		cmd := exec.Command(realGit(t), "-C", f.main, "-c", "protocol.version=2",
			"fetch", "-q", "--no-tags", "--no-write-fetch-head", "--", "file://"+f.clone, f.tip+":refs/ttorch/clones/t1/"+f.tip)
		cmd.Env = append(fixtureEnv(), "GIT_ALLOW_PROTOCOL=file") // no --upload-pack
		_ = cmd.Run()
		if !ran() {
			t.Skip("git did not run the configured upload-pack; cannot demonstrate the attack on this git")
		}
	})

	f, ran := setup(t)
	ref, err := ImportCommit(context.Background(), f.main, f.clone, "t1", f.tip)
	if ran() {
		t.Fatal("the import ran the upload-pack command repo config named")
	}
	if err != nil {
		t.Fatalf("the import should still succeed with the config key present: %v", err)
	}
	if got := fgit(t, f.main, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}"); got != f.tip {
		t.Fatalf("%s in main = %q, want %q", ref, got, f.tip)
	}
}

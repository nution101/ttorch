package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/db"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// writeTree writes files (relative path → contents) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeadIdentity_Layouts(t *testing.T) {
	// Paths in file bodies may use @WT@ for the worktree's real path. Linked-worktree
	// cases name the project as root/main.
	cases := []struct {
		name   string
		files  map[string]string
		want   string
		wantOK bool
	}{
		{"symref to a loose ref", map[string]string{
			".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": idA + "\n",
		}, idA, true},
		{"packed-refs only", map[string]string{
			".git/HEAD":        "ref: refs/heads/main\n",
			".git/packed-refs": "# pack-refs with: peeled fully-peeled sorted \n" + idB + " refs/heads/other\n" + idA + " refs/heads/main\n^" + idB + "\n",
		}, idA, true},
		{"loose ref wins over packed-refs", map[string]string{
			".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": idB + "\n",
			".git/packed-refs": idA + " refs/heads/main\n",
		}, idB, true},
		{"detached HEAD", map[string]string{".git/HEAD": idA + "\n"}, idA, true},
		{"sha-256 detached HEAD", map[string]string{".git/HEAD": strings.Repeat("c", 64) + "\n"}, strings.Repeat("c", 64), true},
		{".git file to a linked-worktree gitdir with commondir", map[string]string{
			".git":                                "gitdir: ../main/.git/worktrees/wt\n",
			"../main/.git/worktrees/wt/HEAD":      "ref: refs/heads/feature\n",
			"../main/.git/worktrees/wt/commondir": "../..\n",
			"../main/.git/worktrees/wt/gitdir":    "@WT@/.git\n",
			"../main/.git/refs/heads/feature":     idB + "\n",
		}, idB, true},
		{"linked worktree, branch only in the common packed-refs", map[string]string{
			".git":                                "gitdir: ../main/.git/worktrees/wt\n",
			"../main/.git/worktrees/wt/HEAD":      "ref: refs/heads/feature\n",
			"../main/.git/worktrees/wt/commondir": "../..\n",
			"../main/.git/worktrees/wt/gitdir":    "@WT@/.git\n",
			"../main/.git/packed-refs":            idA + " refs/heads/feature\n",
		}, idA, true},
		{"linked worktree with a relative back-link", map[string]string{
			".git":                                "gitdir: ../main/.git/worktrees/wt\n",
			"../main/.git/worktrees/wt/HEAD":      idA + "\n",
			"../main/.git/worktrees/wt/commondir": "../..\n",
			"../main/.git/worktrees/wt/gitdir":    "../../../../wt/.git\n",
		}, idA, true},
		{"gitdir inside the worktree itself", map[string]string{
			".git": "gitdir: meta/git\n", "meta/git/HEAD": idA + "\n",
		}, idA, true},
		{"oversized HEAD", map[string]string{".git/HEAD": idA + strings.Repeat("\n", maxSmallGitFile)}, "", false},
		{"ref escaping the gitdir", map[string]string{
			".git/HEAD": "ref: refs/../../outside\n", "outside": idA + "\n",
		}, "", false},
		{"ref not under refs/", map[string]string{".git/HEAD": "ref: HEAD\n"}, "", false},
		{"ref name that is not clean", map[string]string{
			".git/HEAD": "ref: refs/heads/./main\n", ".git/refs/heads/main": idA + "\n",
		}, "", false},
		{"second symref level", map[string]string{
			".git/HEAD": "ref: refs/heads/a\n", ".git/refs/heads/a": "ref: refs/heads/b\n", ".git/refs/heads/b": idA + "\n",
		}, "", false},
		{"garbage in HEAD", map[string]string{".git/HEAD": "not a ref\n"}, "", false},
		{"abbreviated id", map[string]string{".git/HEAD": idA[:12] + "\n"}, "", false},
		{"missing HEAD", map[string]string{".git/config": ""}, "", false},
		{"missing ref and no packed-refs", map[string]string{".git/HEAD": "ref: refs/heads/main\n"}, "", false},
		{".git file without gitdir:", map[string]string{".git": "nonsense\n"}, "", false},
		{".git file to a missing gitdir", map[string]string{".git": "gitdir: ../nowhere\n"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			wt := filepath.Join(root, "wt")
			if err := os.MkdirAll(wt, 0o755); err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(wt)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{}
			for k, v := range c.files {
				files[k] = strings.ReplaceAll(v, "@WT@", real)
			}
			writeTree(t, wt, files)
			got, ok := headIdentity(wt, filepath.Join(root, "main"))
			if ok != c.wantOK || got != c.want {
				t.Fatalf("headIdentity = %q, %v; want %q, %v", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestHeadIdentity_EmptyAndMissingWorktree(t *testing.T) {
	if _, ok := headIdentity("", ""); ok {
		t.Fatal("an empty worktree path must read as unknown")
	}
	if _, ok := headIdentity(filepath.Join(t.TempDir(), "absent"), ""); ok {
		t.Fatal("a missing worktree must read as unknown")
	}
}

// TestHeadIdentity_RefusesSpecialFiles: a FIFO in place of HEAD must be refused without
// blocking the watcher, and a symlinked HEAD or .git must be refused rather than followed.
func TestHeadIdentity_RefusesSpecialFiles(t *testing.T) {
	t.Run("fifo HEAD", func(t *testing.T) {
		wt := t.TempDir()
		writeTree(t, wt, map[string]string{".git/refs/heads/main": idA + "\n"})
		if err := syscall.Mkfifo(filepath.Join(wt, ".git", "HEAD"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		if _, ok := headIdentity(wt, ""); ok {
			t.Fatal("a FIFO HEAD must read as unknown")
		}
	})
	t.Run("fifo HEAD through a gitdir: line", func(t *testing.T) {
		wt := t.TempDir()
		writeTree(t, wt, map[string]string{".git": "gitdir: meta/git\n", "meta/git/refs/heads/main": idA + "\n"})
		if err := syscall.Mkfifo(filepath.Join(wt, "meta", "git", "HEAD"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		if _, ok := headIdentity(wt, ""); ok {
			t.Fatal("a FIFO HEAD behind gitdir: must read as unknown")
		}
	})
	t.Run("symlinked HEAD", func(t *testing.T) {
		wt := t.TempDir()
		writeTree(t, wt, map[string]string{".git/refs/heads/main": idA + "\n", "elsewhere": idA + "\n"})
		if err := os.Symlink(filepath.Join(wt, "elsewhere"), filepath.Join(wt, ".git", "HEAD")); err != nil {
			t.Fatal(err)
		}
		if _, ok := headIdentity(wt, ""); ok {
			t.Fatal("a symlinked HEAD must read as unknown")
		}
	})
	t.Run("symlinked .git", func(t *testing.T) {
		wt, real := t.TempDir(), t.TempDir()
		writeTree(t, real, map[string]string{"HEAD": idA + "\n"})
		if err := os.Symlink(real, filepath.Join(wt, ".git")); err != nil {
			t.Fatal(err)
		}
		if _, ok := headIdentity(wt, ""); ok {
			t.Fatal("a symlinked .git must read as unknown")
		}
	})
}

// TestHeadIdentity_ConfinesGitDir: the git dir a .git file names, and its commondir, must
// be inside the worktree or be this worktree's admin entry under the project's common git
// dir. Anywhere else reads as unknown, even when a valid HEAD is waiting there.
func TestHeadIdentity_ConfinesGitDir(t *testing.T) {
	// layout builds root/main (the project, with a valid admin entry for root/wt) and
	// root/wt, then applies files under root/wt; it returns the worktree and project.
	layout := func(t *testing.T, files map[string]string) (string, string) {
		t.Helper()
		root := t.TempDir()
		wt, main := filepath.Join(root, "wt"), filepath.Join(root, "main")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(wt)
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, main, map[string]string{
			".git/HEAD":                      idB + "\n",
			".git/worktrees/wt/HEAD":         idA + "\n",
			".git/worktrees/wt/commondir":    "../..\n",
			".git/worktrees/wt/gitdir":       real + "/.git\n",
			".git/worktrees/other/HEAD":      idA + "\n",
			".git/worktrees/other/gitdir":    filepath.Join(root, "elsewhere", ".git") + "\n",
			".git/worktrees/other/commondir": "../..\n",
		})
		writeTree(t, root, map[string]string{"outside/HEAD": idA + "\n", "outside/refs/heads/main": idA + "\n"})
		writeTree(t, wt, files)
		return wt, main
	}
	cases := []struct {
		name  string
		files map[string]string
		setup func(t *testing.T, wt string)
	}{
		{"gitdir: /etc", map[string]string{".git": "gitdir: /etc\n"}, nil},
		{"gitdir: a directory outside the worktree", map[string]string{".git": "gitdir: ../outside\n"}, nil},
		{"gitdir: the project's common git dir", map[string]string{".git": "gitdir: ../main/.git\n"}, nil},
		{"gitdir: another worktree's admin entry", map[string]string{".git": "gitdir: ../main/.git/worktrees/other\n"}, nil},
		{"gitdir: inside the worktree through a symlink out of it", map[string]string{".git": "gitdir: meta\n"},
			func(t *testing.T, wt string) {
				if err := os.Symlink(filepath.Join(wt, "..", "outside"), filepath.Join(wt, "meta")); err != nil {
					t.Fatal(err)
				}
			}},
		{"commondir outside the worktree", map[string]string{
			".git": "gitdir: meta/git\n", "meta/git/HEAD": "ref: refs/heads/main\n", "meta/git/commondir": "../../../outside\n",
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wt, main := layout(t, c.files)
			if c.setup != nil {
				c.setup(t, wt)
			}
			if id, ok := headIdentity(wt, main); ok {
				t.Fatalf("headIdentity = %q, want unknown", id)
			}
		})
	}
	t.Run("worktree path swapped for a symlink", func(t *testing.T) {
		wt, main := layout(t, map[string]string{".git/HEAD": idA + "\n"})
		moved := wt + ".moved"
		if err := os.Rename(wt, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, wt); err != nil {
			t.Fatal(err)
		}
		if id, ok := headIdentity(wt, main); ok {
			t.Fatalf("headIdentity = %q, want unknown", id)
		}
	})
	t.Run("the worktree's own admin entry still reads", func(t *testing.T) {
		wt, main := layout(t, map[string]string{".git": "gitdir: ../main/.git/worktrees/wt\n"})
		if id, ok := headIdentity(wt, main); !ok || id != idA {
			t.Fatalf("headIdentity = %q, %v; want %q", id, ok, idA)
		}
	})
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestHeadIdentity_AgreesWithGit: on real repositories (a main checkout, a linked
// worktree, and both again after pack-refs) the file read matches `git rev-parse HEAD`.
func TestHeadIdentity_AgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	main := filepath.Join(t.TempDir(), "main")
	gitIn(t, filepath.Dir(main), "init", "-q", "-b", "main", main)
	gitIn(t, main, "commit", "-q", "--allow-empty", "-m", "one")
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	gitIn(t, linked, "commit", "-q", "--allow-empty", "-m", "two")

	check := func(stage string) {
		t.Helper()
		for _, dir := range []string{main, linked} {
			want := gitIn(t, dir, "rev-parse", "HEAD")
			got, ok := headIdentity(dir, main)
			if !ok || got != want {
				t.Fatalf("%s, %s: headIdentity = %q, %v; git says %q", stage, filepath.Base(dir), got, ok, want)
			}
		}
	}
	check("loose refs")
	for _, dir := range []string{main, linked} {
		want, err := filepath.EvalSymlinks(gitIn(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		if err != nil {
			t.Fatal(err)
		}
		if got := commonGitDir(dir); got != want {
			t.Fatalf("%s: commonGitDir = %q; git says %q", filepath.Base(dir), got, want)
		}
	}
	if id, ok := headIdentity(linked, ""); ok {
		t.Fatalf("a linked worktree with no recorded project read as %q; its git dir is outside the worktree", id)
	}
	if id, ok := headIdentity(linked, filepath.Join(t.TempDir(), "other")); ok {
		t.Fatalf("a linked worktree read as %q under a project it was not cut from", id)
	}
	gitIn(t, main, "pack-refs", "--all")
	check("packed refs")
	gitIn(t, linked, "checkout", "-q", "--detach")
	check("detached linked worktree")
}

// runsNoProgram builds a marker-writing script and returns its path plus a func reporting
// whether it ever ran.
func runsNoProgram(t *testing.T) (script string, ran func() bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	script = filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, func() bool { _, err := os.Stat(marker); return err == nil }
}

// TestHeadRead_SignatureConfigRunsNoProgram: with log.showSignature on and gpg.program set
// to a script, a signed HEAD commit must not make the HEAD read run that script. The
// round-1 reader (`git log`) ran it.
func TestHeadRead_SignatureConfigRunsNoProgram(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	script, ran := runsNoProgram(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "config", "log.showSignature", "true")
	gitIn(t, repo, "config", "gpg.program", script)
	hash := func(typ, body string) string {
		t.Helper()
		cmd := exec.Command("git", "-C", repo, "hash-object", "-t", typ, "-w", "--stdin")
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := hash("tree", "")
	sha := hash("commit", "tree "+tree+"\nauthor A <a@example.invalid> 1767225600 +0000\n"+
		"committer C <c@example.invalid> 1767225600 +0000\n"+
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n AAAA\n -----END PGP SIGNATURE-----\n\nsigned\n")
	gitIn(t, repo, "update-ref", "HEAD", sha)

	got, ok := headIdentity(repo, "")
	if ran() {
		t.Fatal("reading HEAD ran the repository's configured gpg.program")
	}
	if !ok || got != sha {
		t.Fatalf("headIdentity = %q, %v; want %q", got, ok, sha)
	}
}

// TestHeadRead_PromisorFetchRunsNoProgram: a partial-clone repo whose promisor remote's
// uploadpack is a script, with HEAD naming a missing object, must not make the HEAD read
// run that script. The round-2 reader (`git cat-file commit HEAD`) lazily fetched the
// object through it.
func TestHeadRead_PromisorFetchRunsNoProgram(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	script, ran := runsNoProgram(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "config", "core.repositoryformatversion", "1")
	gitIn(t, repo, "config", "extensions.partialClone", "origin")
	gitIn(t, repo, "config", "remote.origin.url", repo)
	gitIn(t, repo, "config", "remote.origin.promisor", "true")
	gitIn(t, repo, "config", "remote.origin.uploadpack", script)
	missing := strings.Repeat("1", 40)
	writeTree(t, repo, map[string]string{".git/refs/heads/main": missing + "\n"})

	got, ok := headIdentity(repo, "")
	if ran() {
		t.Fatal("reading HEAD ran the promisor remote's configured uploadpack")
	}
	if !ok || got != missing {
		t.Fatalf("headIdentity = %q, %v; want %q (the id is read, the object is never needed)", got, ok, missing)
	}
}

func TestValidRefName(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/main":        true,
		"refs/heads/feat/x":      true,
		"refs/../../outside":     false,
		"refs/heads/../../x":     false,
		"refs/heads/./main":      false,
		"refs/heads//main":       false,
		"refs/heads/main/":       false,
		"HEAD":                   false,
		"/refs/heads/main":       false,
		"refs/heads/a\\b":        false,
		"refs/heads/nul\x00byte": false,
	} {
		if got := validRefName(ref); got != want {
			t.Errorf("validRefName(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestJoinWithin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "git")
	for ref, want := range map[string]bool{
		"refs/heads/main":    true,
		"refs/../../outside": false,
		"../sibling":         false,
		"..":                 false,
		"refs/../HEAD":       true, // cleans to base/HEAD, still inside
	} {
		if _, got := joinWithin(base, ref); got != want {
			t.Errorf("joinWithin(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestBoundedHead_SlowOpenTimesOutWithoutPileUp: a HEAD open that never returns (an
// automount or FUSE path behind gitdir:) must read as unknown once the deadline passes,
// and must not start a second read for the task while the first is still hanging.
func TestBoundedHead_SlowOpenTimesOutWithoutPileUp(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var opens atomic.Int32
	orig := openGitFile
	t.Cleanup(func() { openGitFile = orig })
	t.Cleanup(unblock) // runs first: let any hung read finish
	openGitFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "HEAD" {
			opens.Add(1)
			<-release
		}
		return orig(path)
	}

	wt := t.TempDir()
	writeTree(t, wt, map[string]string{".git": "gitdir: meta/git\n", "meta/git/HEAD": idA + "\n"})
	b := newBoundedHead(50*time.Millisecond, headReader.read)
	task := db.Task{ID: "slow", Worktree: wt}

	read := func(stage string) (string, bool) {
		t.Helper()
		type res struct {
			id string
			ok bool
		}
		done := make(chan res, 1)
		go func() { id, ok := b.identity(task); done <- res{id, ok} }()
		select {
		case r := <-done:
			return r.id, r.ok
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the HEAD read blocked past its deadline", stage)
			return "", false
		}
	}

	if id, ok := read("hung open"); ok {
		t.Fatalf("hung open: got %q, want unknown", id)
	}
	if id, ok := read("second sweep"); ok {
		t.Fatalf("second sweep: got %q, want unknown", id)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("started %d reads while the first still hung, want 1", n)
	}

	unblock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if id, ok := read("after release"); ok {
			if id != idA {
				t.Fatalf("after release: got %q, want %q", id, idA)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task's slot was never freed after the hung read returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHeadIdentity_ReftableReadsAsUnknown: a repository whose config sets
// extensions.refStorage to reftable keeps HEAD in the reftable stack, and the HEAD and
// refs files there are stubs git ignores. The read must be unknown rather than trust a
// planted HEAD file that disagrees with git.
func TestHeadIdentity_ReftableReadsAsUnknown(t *testing.T) {
	for name, c := range map[string]struct {
		config string
		wantOK bool
	}{
		"refStorage = reftable":           {"[extensions]\n\trefStorage = reftable\n", false},
		"mixed case, quoted":              {"[Extensions]\n\tRefStorage = \"reftable\"\n", false},
		"on the header line, commented":   {"[core]\n\tbare = false\n[extensions] refstorage=reftable ; set by init\n", false},
		"refStorage = files":              {"[extensions]\n\trefStorage = files\n", true},
		"reftable under a subsection":     {"[extensions \"x\"]\n\trefStorage = reftable\n", true},
		"reftable as another key's value": {"[extensions]\n\tobjectFormat = sha1\n[core]\n\trefStorage = reftable\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			wt := t.TempDir()
			writeTree(t, wt, map[string]string{".git/config": c.config, ".git/HEAD": idA + "\n"})
			if _, ok := headIdentity(wt, ""); ok != c.wantOK {
				t.Fatalf("headIdentity ok = %v, want %v", ok, c.wantOK)
			}
		})
	}
	t.Run("linked worktree, reftable set in the common config", func(t *testing.T) {
		root := t.TempDir()
		wt, main := filepath.Join(root, "wt"), filepath.Join(root, "main")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(wt)
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, main, map[string]string{
			".git/config":                 "[extensions]\n\trefstorage = reftable\n",
			".git/worktrees/wt/HEAD":      idA + "\n",
			".git/worktrees/wt/commondir": "../..\n",
			".git/worktrees/wt/gitdir":    real + "/.git\n",
		})
		writeTree(t, wt, map[string]string{".git": "gitdir: ../main/.git/worktrees/wt\n"})
		if id, ok := headIdentity(wt, main); ok {
			t.Fatalf("headIdentity = %q, want unknown", id)
		}
	})
	t.Run("config that cannot be read", func(t *testing.T) {
		wt := t.TempDir()
		writeTree(t, wt, map[string]string{".git/HEAD": idA + "\n"})
		if err := syscall.Mkfifo(filepath.Join(wt, ".git", "config"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		if id, ok := headIdentity(wt, ""); ok {
			t.Fatalf("headIdentity = %q with an unreadable config, want unknown", id)
		}
	})
	t.Run("repository made by git", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		repo := filepath.Join(t.TempDir(), "r")
		cmd := exec.Command("git", "init", "-q", "--ref-format=reftable", repo)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git cannot create a reftable repository: %v\n%s", err, out)
		}
		gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
		if gitIn(t, repo, "rev-parse", "HEAD") == "" {
			t.Fatal("git could not resolve HEAD")
		}
		writeTree(t, repo, map[string]string{".git/HEAD": idA + "\n"}) // a stub git ignores
		if id, ok := headIdentity(repo, repo); ok {
			t.Fatalf("headIdentity = %q in a reftable repository, want unknown", id)
		}
	})
}

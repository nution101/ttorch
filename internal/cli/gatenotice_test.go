package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/projectinit"
)

// noticeRepo builds a git repository on main with a ttorch block for mode. policy, when not
// empty, goes under the delivery-mode line. commit decides whether AGENTS.md is committed.
func noticeRepo(t *testing.T, mode, policy string, commit bool) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "base")
	if _, err := projectinit.Init(repo, mode); err != nil {
		t.Fatal(err)
	}
	if policy != "" {
		p := filepath.Join(repo, "AGENTS.md")
		b, _ := os.ReadFile(p)
		line := "- delivery-mode: " + mode + "\n"
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), line, line+policy+"\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if commit {
		run("add", "-A")
		run("commit", "-q", "-m", "ttorch block")
	}
	return repo
}

// TestGateChangeApprovalNotices: `ttorch update` and `ttorch doctor` print one line for each
// trusted project whose default branch has no gate-change-approval line, since the default
// flip removed the approval for exactly those. A project that sets the line either way, is
// not trusted, or whose default branch keeps the approval anyway is not listed.
func TestGateChangeApprovalNotices(t *testing.T) {
	unset := noticeRepo(t, "trusted", "", true)
	required := noticeRepo(t, "trusted", "- gate-change-approval: required", true)
	off := noticeRepo(t, "trusted", "- gate-change-approval: off", true)
	malformed := noticeRepo(t, "trusted", "* gate-change-approval: required", true)
	prMode := noticeRepo(t, "pr", "", true)
	uncommitted := noticeRepo(t, "trusted", "", false)
	gone := filepath.Join(t.TempDir(), "gone")
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		for _, r := range []string{unset, required, off, malformed, prMode, uncommitted, gone} {
			p, err := s.UpsertProject(ctx, r, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectDefaultBranch(ctx, p.ID, "main", ""); err != nil {
				t.Fatal(err)
			}
		}
	})
	var out bytes.Buffer
	printGateNotices(&out, paths.Default(), false)
	want := "gate-change approval is now off by default for " + unset + "; add '- gate-change-approval: required' to its AGENTS.md to keep it\n"
	if out.String() != want {
		t.Fatalf("notices:\n%s\nwant exactly:\n%s", out.String(), want)
	}
}

// TestGateChangeApprovalNotices_NoStateDB: on a machine with no ttorch state there is nothing
// to announce, and printing the notices must not create the database.
func TestGateChangeApprovalNotices_NoStateDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("TTORCH_DB", dbPath)
	var out bytes.Buffer
	printGateNotices(&out, paths.Default(), false)
	if out.Len() != 0 {
		t.Fatalf("want no notices without a state database, got %q", out.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("printing the notices must not create %s (stat err %v)", dbPath, err)
	}
}

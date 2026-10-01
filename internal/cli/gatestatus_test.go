package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/worktree"
)

func gitRev(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", rev).Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// doctorGateLines is what `ttorch doctor` prints after its tool checks.
func doctorGateLines() string {
	var out bytes.Buffer
	printGateNotices(&out, paths.Default(), true)
	return out.String()
}

// TestDoctorPrintsWhatTheGateReads: `ttorch doctor` prints, on every run, for each trusted
// project, the recorded default branch and the commit it is at, the last landed commit, and the
// URL origin resolves to. It warns when origin no longer matches the URL recorded with the branch
// (a rewritten remote.origin.url, or an insteadOf rule that redirects it) and when the branch no
// longer contains the last landed commit. A project that is not trusted gets no status, and
// `ttorch update` prints none.
func TestDoctorPrintsWhatTheGateReads(t *testing.T) {
	clearWorkerContext(t)
	repo := noticeRepo(t, "trusted", "- gate-change-approval: required", true)
	prMode := noticeRepo(t, "pr", "", true)
	const recorded = "https://example.com/team/repo.git"
	gitInRepo(t, repo, "remote", "add", "origin", recorded)
	tip := gitRev(t, repo, "refs/heads/main")
	parent := gitRev(t, repo, "refs/heads/main~1")
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		for _, r := range []string{repo, prMode} {
			p, err := s.UpsertProject(ctx, r, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FillProjectDefaultBranch(ctx, p.ID, "main", worktree.OriginURL(r), false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.SetProjectLastLanded(ctx, repo, tip); err != nil {
			t.Fatal(err)
		}
	})

	first := doctorGateLines()
	for _, want := range []string{
		"trust gate for " + repo + " (project 1):\n",
		"  default branch: main at " + tip + "\n",
		"  last landed: " + tip + "\n",
		"  origin: " + recorded + "\n",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("doctor output is missing %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "warning:") {
		t.Fatalf("nothing has changed since the land, so there is nothing to warn about:\n%s", first)
	}
	if strings.Contains(first, "trust gate for "+prMode) {
		t.Fatalf("a project that is not trusted gets no status:\n%s", first)
	}
	if again := doctorGateLines(); again != first {
		t.Fatalf("doctor must print the status on every run; second run:\n%s\nfirst:\n%s", again, first)
	}
	var update bytes.Buffer
	printGateNotices(&update, paths.Default(), false)
	if strings.Contains(update.String(), "trust gate for") {
		t.Fatalf("update prints no status:\n%s", update.String())
	}

	const elsewhere = "https://example.com/elsewhere/repo.git"
	originWarning := "warning: origin is " + elsewhere + ", but it was " + recorded + " when the default branch was recorded"
	gitInRepo(t, repo, "remote", "set-url", "origin", elsewhere)
	if out := doctorGateLines(); !strings.Contains(out, "  origin: "+elsewhere+"\n") || !strings.Contains(out, originWarning) {
		t.Fatalf("a rewritten remote.origin.url must be printed and flagged:\n%s", out)
	}
	gitInRepo(t, repo, "remote", "set-url", "origin", recorded)

	gitInRepo(t, repo, "config", "url.https://example.com/elsewhere/.insteadOf", "https://example.com/team/")
	if out := doctorGateLines(); !strings.Contains(out, originWarning) {
		t.Fatalf("an insteadOf rule that redirects origin must be flagged:\n%s", out)
	}
	gitInRepo(t, repo, "config", "--unset", "url.https://example.com/elsewhere/.insteadOf")

	gitInRepo(t, repo, "update-ref", "refs/heads/main", parent)
	out := doctorGateLines()
	if !strings.Contains(out, "  default branch: main at "+parent+"\n") || !strings.Contains(out, "which does not contain "+tip[:12]) {
		t.Fatalf("a branch moved off the last landed commit must be printed and flagged:\n%s", out)
	}
	if strings.Contains(out, "warning: origin") {
		t.Fatalf("origin is back to the recorded URL:\n%s", out)
	}
}

// TestDoctorGateStatus_EscapesTheOriginURL: the origin URL comes from git config a worker can
// write, so control characters in it are shown escaped rather than sent to the lead's terminal,
// where they could rewrite or hide the line.
func TestDoctorGateStatus_EscapesTheOriginURL(t *testing.T) {
	clearWorkerContext(t)
	repo := noticeRepo(t, "trusted", "- gate-change-approval: required", true)
	withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, err := s.UpsertProject(ctx, repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.FillProjectDefaultBranch(ctx, p.ID, "main", "", false); err != nil {
			t.Fatal(err)
		}
	})
	gitInRepo(t, repo, "remote", "add", "origin", "https://example.com/\x1b[2Krepo.git")
	out := doctorGateLines()
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("doctor printed a raw escape character:\n%q", out)
	}
	if !strings.Contains(out, `origin: https://example.com/\e[2Krepo.git`) || !strings.Contains(out, "but it was none when the default branch was recorded") {
		t.Fatalf("the URL must be shown escaped and flagged against the recorded none:\n%q", out)
	}
}

// TestDoctorGateStatus_FromAWorkerContext: doctor run in a worker's pane still prints the status,
// and only reads.
func TestDoctorGateStatus_FromAWorkerContext(t *testing.T) {
	repo := noticeRepo(t, "trusted", "- gate-change-approval: required", true)
	dbPath := withSeedDB(t, func(ctx context.Context, s *db.Store) {
		p, err := s.UpsertProject(ctx, repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.FillProjectDefaultBranch(ctx, p.ID, "main", "", true); err != nil {
			t.Fatal(err)
		}
	})
	clearWorkerContext(t)
	t.Setenv("TTORCH_TASK_ID", "w1")
	out := doctorGateLines()
	if !strings.Contains(out, "  default branch: main at "+gitRev(t, repo, "refs/heads/main")) {
		t.Fatalf("doctor from a worker context must still print the status:\n%s", out)
	}
	if p, _, _ := reopen(t, dbPath).GetProjectByRepo(context.Background(), repo); p.DefaultBranchSeed != db.DefaultBranchSeedNotice {
		t.Fatalf("doctor from a worker context cleared the notice: %+v", p)
	}
}

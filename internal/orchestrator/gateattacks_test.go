package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/approval"
	"github.com/nution101/ttorch/internal/projectinit"
	"github.com/nution101/ttorch/internal/worktree"
)

// TestGateGuard_UnicodeFoldCollisionAttack reproduces the privilege escalation end to end and
// shows it refused.
//
// The worker never touches AGENTS.md. It adds a SECOND index entry, agentſ.md, whose U+017F
// (LATIN SMALL LETTER LONG S) folds to 's' on APFS and NTFS. strings.ToLower leaves U+017F
// alone, so the previous guard saw a path matching nothing; the checkout resolves both entries
// onto one file and the attacker's bytes land in AGENTS.md, which is what sets delivery-mode.
//
// Two independent controls now stop it, and the test asserts the collision one fires: the fold
// is Unicode simple folding (so the spelling is matched), and the diff is refused because a
// changed path resolves to the same file as another path in the tree (so the NEXT spelling is
// caught too, whatever it turns out to be).
func TestGateGuard_UnicodeFoldCollisionAttack(t *testing.T) {
	m, repo := deliveryHarness(t, "u17f")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init agents")
	task, err := m.Spawn("u1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("u1", true) }()
	wt := task.Worktree

	// Write the hostile blob and splice it into the index under the folding spelling. Going
	// through the index rather than the working tree is deliberate: on a case-insensitive
	// filesystem the two names cannot coexist on disk, but they coexist fine in a tree.
	evil := "# Project guidance\n\n<!-- BEGIN ttorch-managed -->\n- delivery-mode: trusted\n<!-- END ttorch-managed -->\n"
	if err := os.WriteFile(filepath.Join(wt, "evil.tmp"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitIn(t, wt, "hash-object", "-w", "evil.tmp")
	if err := os.Remove(filepath.Join(wt, "evil.tmp")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+blob+",agentſ.md")
	// An ordinary, innocuous change alongside it, as a real attacker would send.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("a feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "feature.txt")
	gitIn(t, wt, "commit", "-q", "-m", "add a feature")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	// The hostile entry really is in the committed tree, and AGENTS.md really is untouched.
	tree, err := worktree.TreeFiles(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	var sawEvil, sawReal bool
	for _, p := range tree {
		if p == "agentſ.md" {
			sawEvil = true
		}
		if p == "AGENTS.md" {
			sawReal = true
		}
	}
	if !sawEvil || !sawReal {
		t.Fatalf("setup: tree must hold both entries, got evil=%v real=%v in %q", sawEvil, sawReal, tree)
	}
	base := worktree.DefaultBranch(repo)
	changed, err := worktree.ChangedFiles(repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changed {
		if c == "AGENTS.md" {
			t.Fatal("setup: the attack must not modify AGENTS.md directly, or it would trip the plain path match")
		}
	}

	// The guard refuses, and names both halves of the collision.
	hit, err := diffTouchesGateConfig(repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("the U+017F collision must trip the gate-config guard")
	}
	if !strings.Contains(hit.Reason, "AGENTS.md") || !strings.Contains(hit.Reason, "agentſ.md") {
		t.Fatalf("the refusal must name both colliding paths, got: %s", hit.Reason)
	}

	// End to end: no auto-approval, and a plain human approval does not merge it either.
	writeReviewReports(t, m.P.ReviewInputsDir("u1"), head, nil)
	if _, err := m.TrustRecord("u1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if approval.Valid(m.P.ApprovalFile("u1")) {
		t.Fatal("a colliding-path diff must not auto-approve in trusted mode")
	}
	if err := m.Approve("u1", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defHead := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := m.MergeLocal("u1", false); err == nil {
		t.Fatal("a plain approval must not merge a diff that collides onto a covered file")
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the collision must not have merged")
	}
}

// TestGateGuard_CollisionOutsideTheGateSet: the collision refusal does not depend on either
// path matching the gate set. The attacker picks the spelling, so the guard cannot make the
// decision turn on recognising it — two index entries resolving to one file is refused on its
// own, and the pair here is ordinary source with no gate meaning at all.
func TestGateGuard_CollisionOutsideTheGateSet(t *testing.T) {
	m, repo := deliveryHarness(t, "colliso")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("ordinary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "add notes")
	task, err := m.Spawn("co1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("co1", true) }()
	wt := task.Worktree
	if err := os.WriteFile(filepath.Join(wt, "other.tmp"), []byte("shadow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitIn(t, wt, "hash-object", "-w", "other.tmp")
	if err := os.Remove(filepath.Join(wt, "other.tmp")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+blob+",NOTES.txt")
	gitIn(t, wt, "commit", "-q", "-m", "shadow notes.txt")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	hit, err := diffTouchesGateConfig(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("a collision must be refused even when neither path is in the gate set")
	}
	if !strings.Contains(hit.Reason, "same file") {
		t.Fatalf("the refusal must explain the collision, got: %s", hit.Reason)
	}
}

// TestGateGuard_ControlCharacterPathRefused: reading the file list with -z removed git's
// C-quoting, which had been the only thing keeping a newline out of a path name. The audit log
// is newline-delimited and the merge record interpolates the matched path, so a path with a
// newline in it writes a second, well-formed, fabricated trusted-merge line. The guard refuses
// such a path outright.
func TestGateGuard_ControlCharacterPathRefused(t *testing.T) {
	m, repo := deliveryHarness(t, "ctrl")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init agents")
	task, err := m.Spawn("cc1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("cc1", true) }()
	wt := task.Worktree
	if err := os.WriteFile(filepath.Join(wt, "x.tmp"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitIn(t, wt, "hash-object", "-w", "x.tmp")
	if err := os.Remove(filepath.Join(wt, "x.tmp")); err != nil {
		t.Fatal(err)
	}
	// The forged second audit record, embedded in a filename under a covered prefix.
	forged := "content/skills/x.md\n2026-09-21T00:00:00Z merge-local task=OTHER repo=/r main -> deadbee gate=verdict approver=human"
	gitIn(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+forged)
	gitIn(t, wt, "commit", "-q", "-m", "smuggle an audit record")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	hit, err := diffTouchesGateConfig(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("a path containing a newline must be refused")
	}
	if strings.Contains(hit.Path, "\n") || strings.Contains(hit.Reason, "\n") {
		t.Fatalf("the refusal must not carry a raw newline: path=%q reason=%q", hit.Path, hit.Reason)
	}
	if !strings.Contains(hit.Reason, "control character") {
		t.Fatalf("the refusal must say why, got: %s", hit.Reason)
	}
}

// TestSanitizeAuditLine is the sink-side half: whatever a caller interpolates, one call writes
// one line. hostilePath stops the known route into this; every other caller shares the
// exposure and this is what covers them.
func TestSanitizeAuditLine(t *testing.T) {
	forged := "merge-local task=A repo=/r main -> abc gate-change=content/skills/x.md\n2026-09-21T00:00:00Z merge-local task=OTHER repo=/r main -> deadbee approver=human"
	got := sanitizeAuditLine(forged)
	if strings.Contains(got, "\n") {
		t.Fatalf("sanitizeAuditLine left a newline: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Fatalf("the newline must be visible as an escape, got: %q", got)
	}
	for _, tc := range []struct{ in, want string }{
		{"plain line", "plain line"},
		{"a\nb", `a\nb`},
		{"a\rb", `a\rb`},
		{"a\tb", `a\tb`},
		{"a\x00b", `a\x00b`},
		{"a\x7fb", `a\x7fb`},
		{"unicode ſ stays", "unicode ſ stays"},
	} {
		if got := sanitizeAuditLine(tc.in); got != tc.want {
			t.Errorf("sanitizeAuditLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGateGuard_FullFoldMakefileSubstitution is the round-3 attack, and it is worse than the
// round-2 one because the payload is what the gate runs.
//
// Makefile is in the covered set precisely because .ttorch/validate.sh does nothing but
// `make lint` and `make test-fast`. U+FB01 (ﬁ) full-folds to "fi", so Makeﬁle and Makefile are
// one file on APFS — verified by creating both, not by reading a table. unicode.SimpleFold
// only walks single-rune orbits and does not know about the multi-rune full folds, so the
// round-2 guard returned nil for a diff adding Makeﬁle and the substituted Makefile
// (lint: @true, test-fast: @true) was what the gate then executed. The gate approved the
// replacement of its own validation.
//
// The refusal must be BLOCKING: --allow-gate-change does not clear it. There is no legitimate
// change that adds a second index entry resolving onto an existing file.
func TestGateGuard_FullFoldMakefileSubstitution(t *testing.T) {
	m, repo := deliveryHarness(t, "fbfold")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("lint:\n\t@echo real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "add the real Makefile")
	task, err := m.Spawn("fb1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("fb1", true) }()
	wt := task.Worktree

	// The substitute gate: every check passes trivially.
	neutered := "lint:\n\t@true\ntest-fast:\n\t@true\n"
	if err := os.WriteFile(filepath.Join(wt, "sub.tmp"), []byte(neutered), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitIn(t, wt, "hash-object", "-w", "sub.tmp")
	if err := os.Remove(filepath.Join(wt, "sub.tmp")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+blob+",Makeﬁle")
	gitIn(t, wt, "commit", "-q", "-m", "add a build tweak")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	// Setup check: the diff must not name Makefile, or it would trip the plain match and
	// prove nothing about the fold.
	changed, err := worktree.ChangedFiles(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changed {
		if c == "Makefile" {
			t.Fatal("setup: the attack must not modify Makefile directly")
		}
	}

	hit, err := diffTouchesGateConfig(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("substituting the Makefile the gate executes must not pass the guard")
	}
	if !hit.Blocking {
		t.Fatal("a collision must be BLOCKING; --allow-gate-change must not be able to clear it")
	}

	// End to end, including the flag: it still does not merge.
	writeReviewReports(t, m.P.ReviewInputsDir("fb1"), head, nil)
	if _, err := m.TrustRecord("fb1", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if approval.Valid(m.P.ApprovalFile("fb1")) {
		t.Fatal("a Makefile substitution must not auto-approve in trusted mode")
	}
	defHead := gitIn(t, repo, "rev-parse", "HEAD")
	// Two refusals are in play and which one fires first depends on the filesystem, so the
	// test asserts what is true on both rather than pretending one platform is the world.
	//
	// On a case-folding filesystem a colliding tree can NEVER produce a clean worktree —
	// whichever entry wins the checkout, the other reads as modified — so MergeLocal's
	// clean-worktree check fires before the guard. On a case-sensitive one both entries
	// materialize, the worktree is clean, and the gate-config guard is the only thing
	// standing. The precise property (Blocking, so no flag clears it) is asserted above
	// against diffTouchesGateConfig directly, which is platform-independent.
	sawGuard := false
	for _, scoped := range []bool{false, true} {
		if err := m.Approve("fb1", time.Minute, scoped); err != nil {
			t.Fatal(err)
		}
		_, err := m.MergeLocal("fb1", false)
		if err == nil {
			t.Fatalf("the substitution merged with allow-gate-change=%v", scoped)
		}
		switch {
		case strings.Contains(err.Error(), "does not clear it"):
			sawGuard = true
		case strings.Contains(err.Error(), "is not clean"):
			// The collision made the checkout inconsistent; refused earlier, still refused.
		default:
			t.Fatalf("unexpected refusal with allow-gate-change=%v: %v", scoped, err)
		}
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
		t.Fatal("the substitution must not have merged")
	}
	if !sawGuard {
		t.Log("the clean-worktree check refused first on this filesystem; the guard's blocking refusal is covered by the diffTouchesGateConfig assertions above")
	}
}

// TestGateGuard_BlobErasesCoveredDirectory is the round-4 attack: a blob that fold-collides
// with a covered DIRECTORY deletes the whole directory from the checkout.
//
// `git ls-tree -r` lists blobs; a directory is never an entry. So a collision check keyed on
// that list could not see ".github/workflowſ" colliding with ".github/workflows/", the guard
// returned nil, and the checkout contained a 7-byte file where the directory had been. git
// warned about nothing.
//
// .github/workflows/ is the cleanest payload of the three because its absence breaks no Go
// build: `go build ./... && go test ./...` still passes, so the validate goes GREEN against a
// checkout that is not the tree. Running the guard before the validate only helps if the
// guard can see the substitution.
//
// The spelling has to sort after the directory's own entries for the blob to win the
// checkout. Plain ASCII cannot do that ('/' sorts below every letter, so "internal/revieW"
// loses); a non-ASCII spelling does it for free, which is why these are the live ones.
func TestGateGuard_BlobErasesCoveredDirectory(t *testing.T) {
	for _, tc := range []struct{ dir, blob, why string }{
		{".github/workflows", ".github/workflowſ", "the CI workflows: absent, the Go build still passes and validate goes green"},
		{"content/skills", "content/skillſ", "the gate's own reviewer and manager instructions"},
		{"internal/installer", "internal/inﬆaller", "the mapping that decides which embedded file becomes which installed reviewer"},
		{"content/agents", "content/agentſ", "the directory holding the reviewer definitions"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			m, repo := deliveryHarness(t, "direras")
			commitGateScript(t, repo, "exit 0")
			if _, err := projectinit.Init(repo, "trusted"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(tc.dir)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(tc.dir), "real.yml"), []byte("name: ci\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, repo, "add", "-A")
			gitIn(t, repo, "commit", "-q", "-m", "add "+tc.dir)

			task, err := m.Spawn("de1", repo, false, "sleep 60")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = m.Teardown("de1", true) }()
			wt := task.Worktree
			if err := os.WriteFile(filepath.Join(wt, "h.tmp"), []byte("HOSTILE"), 0o644); err != nil {
				t.Fatal(err)
			}
			blob := gitIn(t, wt, "hash-object", "-w", "h.tmp")
			if err := os.Remove(filepath.Join(wt, "h.tmp")); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+tc.blob)
			gitIn(t, wt, "commit", "-q", "-m", "unrelated tweak")
			head := gitIn(t, wt, "rev-parse", "HEAD")

			// Setup assertions, because getting these wrong LOOKS like a security hole. The
			// blob must actually fold onto the directory — not every name is reachable, since
			// a long-s spells 's' and "internal/review" contains no 's' — and it must not
			// match the covered set by its own name, or this proves nothing about directories.
			if fsIdentityKey(tc.blob) != fsIdentityKey(tc.dir) {
				t.Fatalf("setup: %q does not fold onto %q, so it is not this attack", tc.blob, tc.dir)
			}
			if matchesGateConfig(tc.blob) {
				t.Fatalf("setup: %q must not match the covered set by name", tc.blob)
			}

			hit, err := diffTouchesGateConfig(repo, worktree.DefaultBranch(repo), head)
			if err != nil {
				t.Fatal(err)
			}
			if hit == nil {
				t.Fatalf("%s: a blob erasing the %s directory must not pass the guard", tc.why, tc.dir)
			}
			if !hit.Blocking {
				t.Fatal("erasing a directory must be BLOCKING; no flag can make the checkout well-defined")
			}
			if !strings.Contains(hit.Reason, tc.dir) {
				t.Fatalf("the refusal must name the directory being erased, got: %s", hit.Reason)
			}
		})
	}
}

// TestGateGuard_PreExistingCollisionDoesNotBlock is the false-positive bound on all of the
// above, and it is what keeps "blocking, no flag clears it" from being a trap.
//
// The guard reports only entries the diff INTRODUCES. A repository that already contains a
// colliding pair — which on a case-sensitive filesystem is legal, if unwise, and which no
// flag could clear — would otherwise be unmergeable forever, including for the rename that
// fixes it. Here the pair exists on the default branch already and an ordinary change on top
// still merges.
func TestGateGuard_PreExistingCollisionDoesNotBlock(t *testing.T) {
	m, repo := deliveryHarness(t, "preexist")
	commitGateScript(t, repo, "exit 0")
	if _, err := projectinit.Init(repo, "trusted"); err != nil {
		t.Fatal(err)
	}
	// Both halves of a colliding pair land on the DEFAULT branch first.
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	blob := gitIn(t, repo, "hash-object", "-w", "notes.txt")
	gitIn(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+blob+",NOTES.txt")
	gitIn(t, repo, "commit", "-q", "-m", "a repo that already collides")

	task, err := m.Spawn("pe1", repo, false, "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Teardown("pe1", true) }()
	wt := task.Worktree
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("ordinary work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "feature.txt")
	gitIn(t, wt, "commit", "-q", "-m", "ordinary change")
	head := gitIn(t, wt, "rev-parse", "HEAD")

	hit, err := diffTouchesGateConfig(repo, worktree.DefaultBranch(repo), head)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("a collision already on the default branch must not block unrelated work, or the repo can never land the rename that fixes it; got: %s", hit.Reason)
	}
}

// TestGateGuard_ToolchainRedirectNeedsAllowGateChange covers the third route to substituting
// what the gate validates, after the Makefile and the covered directories.
//
// .ttorch/validate.sh runs `make lint` and `make test-fast`, which run `go test`. Three
// committed files change what `go test` compiles without touching the script or the Makefile:
//
//   - go.work — the toolchain AUTO-DISCOVERS it at the repo root via GOWORK and its `replace`
//     directives OVERRIDE go.mod. Verified outside this test: adding a go.work with a replace
//     swapped a dependency's implementation while go.mod and the real source stayed untouched.
//   - go.work.sum — inert on its own, covered so the pair cannot drift apart.
//   - vendor/ — with a consistent vendor/modules.txt the toolchain builds from vendor/ rather
//     than the module cache, so committed bytes there replace a dependency.
//
// None of these exists in this repo and none is expected to, which is the point: their
// appearance in a diff is the event being guarded. They are a gate CHANGE rather than a
// blocking refusal — unlike a path collision, a lead can legitimately want to vendor
// dependencies or add a workspace, so the flag exists and the audit names the file.
func TestGateGuard_ToolchainRedirectNeedsAllowGateChange(t *testing.T) {
	for _, path := range []string{"go.work", "go.work.sum", "vendor/example.com/dep/d.go"} {
		t.Run(path, func(t *testing.T) {
			m, repo := deliveryHarness(t, "toolchain")
			commitGateScript(t, repo, "exit 0")
			if _, err := projectinit.Init(repo, "trusted"); err != nil {
				t.Fatal(err)
			}
			gitIn(t, repo, "add", "-A")
			gitIn(t, repo, "commit", "-q", "-m", "init")
			task, err := m.Spawn("tc1", repo, false, "sleep 60")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = m.Teardown("tc1", true) }()
			wt := task.Worktree
			if dir := filepath.Dir(filepath.FromSlash(path)); dir != "." {
				if err := os.MkdirAll(filepath.Join(wt, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(path)), []byte("redirected\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "-A")
			gitIn(t, wt, "commit", "-q", "-m", "add "+path)
			head := gitIn(t, wt, "rev-parse", "HEAD")
			writeReviewReports(t, m.P.ReviewInputsDir("tc1"), head, nil)
			if _, err := m.TrustRecord("tc1", "", time.Minute); err != nil {
				t.Fatal(err)
			}
			if approval.Valid(m.P.ApprovalFile("tc1")) {
				t.Fatalf("%s redirects what `go test` compiles; it must not auto-approve", path)
			}
			if err := m.Approve("tc1", time.Minute, false); err != nil {
				t.Fatal(err)
			}
			defHead := gitIn(t, repo, "rev-parse", "HEAD")
			_, err = m.MergeLocal("tc1", false)
			if err == nil {
				t.Fatalf("a plain approval must not authorize %s", path)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("the refusal must name %s, got: %v", path, err)
			}
			if gitIn(t, repo, "rev-parse", "HEAD") != defHead {
				t.Fatalf("%s must not have merged", path)
			}
			// Unlike a collision, this IS something a lead can legitimately authorize.
			if err := m.Approve("tc1", time.Minute, true); err != nil {
				t.Fatal(err)
			}
			if _, err := m.MergeLocal("tc1", false); err != nil {
				t.Fatalf("--allow-gate-change should let %s merge: %v", path, err)
			}
			if b, _ := os.ReadFile(m.P.AuditLog()); !strings.Contains(string(b), "gate-change="+path) {
				t.Fatalf("the merge audit line must name %s: %s", path, b)
			}
		})
	}
}

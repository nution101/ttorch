package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/validate"
)

// greenGateResults is a minimal GREEN result set: one passing check. stagedGreen treats it as
// green (len > 0, no failures), mirroring the fresh gateGreen decision — so a cache hit that
// returns it reproduces the identical (green, results) outcome a real run would.
var greenGateResults = []validate.Result{{Name: "gate", Passed: true, Output: "ok"}}

// redGateResults is a minimal RED result set: one failing check. A red tree is never cached.
var redGateResults = []validate.Result{{Name: "gate", Passed: false, Output: "boom"}}

// installGateStub replaces the runGateOnCommitted seam with fn (which supplies the (green,
// results, err) a real suite run would) and returns a pointer to the live invocation count, so
// a test can prove exactly how many times the REAL suite actually ran. Everything ELSE that
// validateCommitted does — resolving the default-branch gate definition, hashing the git tree,
// keying, and reading/writing the on-disk cache — still runs for real; only the (minutes-long)
// suite execution is stubbed and counted. The original is restored on cleanup, and the package
// tests run serially, so the swap never races another test.
func installGateStub(t *testing.T, fn func() (bool, []validate.Result, error)) *int {
	t.Helper()
	calls := 0
	orig := runGateOnCommitted
	runGateOnCommitted = func(repo, sha string, def gateDefinition) (bool, []validate.Result, error) {
		calls++
		return fn()
	}
	t.Cleanup(func() { runGateOnCommitted = orig })
	return &calls
}

// cacheEntries lists the published (non-temp) cache files under the configured cache dir, so a
// test can assert a green was persisted or a red/no-script result was NOT. A missing dir counts
// as zero entries.
func cacheEntries(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("TTORCH_VALIDATE_CACHE_DIR")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// cacheRepo builds a real git repo whose DEFAULT BRANCH defines a passing gate script, and
// redirects the validate cache into a temp dir. It returns the repo and its current HEAD (a
// commit whose tree the gate would validate). No tmux/Spawn: it exercises validateCommitted's
// real gate-resolution, tree-hashing, and cache I/O directly, so it runs in the fast lane.
func cacheRepo(t *testing.T) (repo, head string) {
	t.Helper()
	repo = newRepoMain(t)
	commitGateScript(t, repo, "exit 0")
	t.Setenv("TTORCH_VALIDATE_CACHE_DIR", t.TempDir())
	return repo, gitIn(t, repo, "rev-parse", "HEAD")
}

// TestValidateCache_Hit proves the core optimization: validating the SAME tree under the SAME
// gate twice runs the real suite exactly ONCE — the second call is served from the
// content-addressed cache and reproduces the identical green decision.
func TestValidateCache_Hit(t *testing.T) {
	repo, head := cacheRepo(t)
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return true, greenGateResults, nil })

	green, results, err := validateCommitted(repo, head)
	if err != nil || !green || len(results) != 1 {
		t.Fatalf("first validate: green=%v results=%v err=%v", green, results, err)
	}
	if *calls != 1 {
		t.Fatalf("first validate must run the real suite once, ran %d times", *calls)
	}
	if got := cacheEntries(t); len(got) != 1 {
		t.Fatalf("a green result must be cached (want 1 entry), got %v", got)
	}

	green, results, err = validateCommitted(repo, head)
	if err != nil || !green || len(results) != 1 || !results[0].Passed {
		t.Fatalf("second validate must return the cached green: green=%v results=%v err=%v", green, results, err)
	}
	if *calls != 1 {
		t.Fatalf("second validate of an identical tree+gate must be a cache HIT (no re-run), but the suite ran %d times", *calls)
	}
}

// TestValidateCache_TreeMiss proves a DIFFERENT tree content re-runs the suite: the tree hash
// is part of the key, so a new commit (new tree) never reuses the prior tree's cached result.
func TestValidateCache_TreeMiss(t *testing.T) {
	repo, head1 := cacheRepo(t)
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return true, greenGateResults, nil })

	if _, _, err := validateCommitted(repo, head1); err != nil {
		t.Fatalf("validate head1: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("head1 should run once, ran %d", *calls)
	}

	head2 := commitFeature(t, repo, "feature.txt", "different content\n")
	if head2 == head1 {
		t.Fatal("the second commit must produce a new head/tree")
	}
	green, _, err := validateCommitted(repo, head2)
	if err != nil || !green {
		t.Fatalf("validate head2: green=%v err=%v", green, err)
	}
	if *calls != 2 {
		t.Fatalf("a different tree must MISS and re-run the suite (want 2 runs), got %d", *calls)
	}
}

// TestValidateCache_GateMiss proves the gate DEFINITION is part of the key: validating the SAME
// commit (an unchanged tree) after the default-branch .ttorch/validate.sh changed re-runs the
// suite, because an identical tree can legitimately validate differently under a different gate.
func TestValidateCache_GateMiss(t *testing.T) {
	repo, head := cacheRepo(t) // default-branch gate v1 ("exit 0")
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return true, greenGateResults, nil })

	if _, _, err := validateCommitted(repo, head); err != nil {
		t.Fatalf("validate under gate v1: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("first validate should run once, ran %d", *calls)
	}

	// Advance the default branch's gate script. head's TREE is unchanged, but the resolved gate
	// definition now differs, so its cache key differs.
	commitGateScript(t, repo, "exit 0 # v2")
	green, _, err := validateCommitted(repo, head)
	if err != nil || !green {
		t.Fatalf("validate under gate v2: green=%v err=%v", green, err)
	}
	if *calls != 2 {
		t.Fatalf("a changed default-branch gate must MISS and re-run the suite (want 2 runs) even for the same tree, got %d", *calls)
	}
}

// TestValidateCache_NoScriptNeverCached proves the ecosystem-detection fallback is NEVER cached:
// with no default-branch .ttorch/validate.sh, the gate's checks depend on the worker's own
// checkout and are not a stable, content-hashable authority, so every call runs fresh and
// nothing is written to the cache.
func TestValidateCache_NoScriptNeverCached(t *testing.T) {
	repo := newRepoMain(t) // no default-branch .ttorch/validate.sh
	t.Setenv("TTORCH_VALIDATE_CACHE_DIR", t.TempDir())
	head := gitIn(t, repo, "rev-parse", "HEAD")
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return true, greenGateResults, nil })

	for i := 1; i <= 2; i++ {
		if _, _, err := validateCommitted(repo, head); err != nil {
			t.Fatalf("validate call %d: %v", i, err)
		}
		if *calls != i {
			t.Fatalf("no default-branch script ⇒ every call must run fresh; after call %d the suite ran %d times", i, *calls)
		}
	}
	if got := cacheEntries(t); len(got) != 0 {
		t.Fatalf("the detection fallback must NEVER be cached, but found entries: %v", got)
	}
}

// TestValidateCache_RedNotCached proves a RED (failing) result is never written to the cache: it
// is being iterated on, and caching it risks pinning a flaky failure — so a later call re-runs.
func TestValidateCache_RedNotCached(t *testing.T) {
	repo, head := cacheRepo(t)
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return false, redGateResults, nil })

	green, results, err := validateCommitted(repo, head)
	if err != nil || green || len(results) != 1 {
		t.Fatalf("first validate should be red: green=%v results=%v err=%v", green, results, err)
	}
	if got := cacheEntries(t); len(got) != 0 {
		t.Fatalf("a red result must NOT be cached, found: %v", got)
	}

	green, _, err = validateCommitted(repo, head)
	if err != nil || green {
		t.Fatalf("second validate should still be red: green=%v err=%v", green, err)
	}
	if *calls != 2 {
		t.Fatalf("an uncached red must re-run on the next call (want 2 runs), got %d", *calls)
	}
}

// TestValidateCache_FailClosedOnCorrupt proves the read path is fail-closed: a corrupt/truncated
// cache entry is treated as a MISS, so validation still runs the real suite and returns the
// correct green decision — a garbage entry can never block or poison validation.
func TestValidateCache_FailClosedOnCorrupt(t *testing.T) {
	repo, head := cacheRepo(t)
	calls := installGateStub(t, func() (bool, []validate.Result, error) { return true, greenGateResults, nil })

	if _, _, err := validateCommitted(repo, head); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	entries := cacheEntries(t)
	if len(entries) != 1 {
		t.Fatalf("expected one cached entry to corrupt, got %v", entries)
	}
	if err := os.WriteFile(entries[0], []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	green, results, err := validateCommitted(repo, head)
	if err != nil || !green || len(results) != 1 {
		t.Fatalf("a corrupt entry must fail closed to a fresh, correct run: green=%v results=%v err=%v", green, results, err)
	}
	if *calls != 2 {
		t.Fatalf("a corrupt entry must be a MISS that re-runs the suite (want 2 runs), got %d", *calls)
	}
}

// TestValidateCacheKey_BindsTreeAndGate proves the SAFETY INVARIANT at the key level,
// hermetically: the key is stable for an identical (tree, gate script) pair, and changes when
// EITHER the tree OR the gate script changes. A cache hit therefore corresponds only to a
// byte-identical tree validated by an identical gate — it can never serve a result for a
// different tree or a different set of checks.
func TestValidateCacheKey_BindsTreeAndGate(t *testing.T) {
	const (
		treeA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		treeB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		scriptA = "exit 0\n"
		scriptB = "make test\n"
	)
	base := validateCacheKey(treeA, scriptA)
	if validateCacheKey(treeA, scriptA) != base {
		t.Fatal("key must be stable for an identical tree+gate")
	}
	if validateCacheKey(treeB, scriptA) == base {
		t.Fatal("a different tree must yield a different key")
	}
	if validateCacheKey(treeA, scriptB) == base {
		t.Fatal("a different gate script must yield a different key")
	}
	if validateCacheKey(treeB, scriptB) == base {
		t.Fatal("a different tree AND gate must yield a different key")
	}
}

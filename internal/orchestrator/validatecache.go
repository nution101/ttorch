package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/validate"
)

// validateCacheKey derives the content-addressed cache key for a trust-gate validate result:
// a SHA-256 over (git TREE hash + "\n" + gateDefIdentity), where gateDefIdentity is itself a
// SHA-256 of the default-branch .ttorch/validate.sh text. Folding BOTH into the key is THE
// SAFETY INVARIANT: a cached result is served ONLY for a BYTE-IDENTICAL tree validated by an
// IDENTICAL gate script.
//
// The git tree hash is a cryptographic content identity (the same discipline as
// review.DiffID), so a matching key provably corresponds to the exact file content the checks
// would see — the cache can never serve a result computed for a different tree or a different
// set of checks. An identical tree can legitimately re-validate differently only if the gate
// script changed, and that flips gateDefIdentity, so the key changes and the old entry is not
// reused. This is strictly a performance optimization UNDER validateCommitted: it changes only
// WHETHER the suite re-runs, never WHETHER a commit is authorized. The trust gate's authority —
// the commit-pinned passing verdict, the single-use approval consumed at the fast-forward, the
// HEAD-unchanged brackets in MergeLocal — is untouched.
func validateCacheKey(treeHash, gateScript string) string {
	idSum := sha256.Sum256([]byte(gateScript))
	gateDefIdentity := hex.EncodeToString(idSum[:])
	keySum := sha256.Sum256([]byte(treeHash + "\n" + gateDefIdentity))
	return hex.EncodeToString(keySum[:])
}

// validateCacheFile is the on-disk path of the entry for key: the hex key + ".json" under the
// configured cache dir (paths.ValidateCacheDir, TTORCH_VALIDATE_CACHE_DIR-overridable).
func validateCacheFile(key string) string {
	return filepath.Join(paths.Default().ValidateCacheDir(), key+".json")
}

// loadValidateCache returns the cached []validate.Result for key, or ok=false on any miss. It
// is FAIL-CLOSED by construction: a missing, unreadable, truncated, or otherwise unparseable
// entry (including one a concurrent writer is mid-publish on) is a MISS, so the caller re-runs
// the real suite — never an error that blocks validation. Only GREEN result sets are ever
// stored (storeValidateCache), so a hit is always green.
func loadValidateCache(key string) ([]validate.Result, bool) {
	raw, err := os.ReadFile(validateCacheFile(key))
	if err != nil {
		return nil, false
	}
	var results []validate.Result
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, false // corrupt/truncated entry ⇒ miss (fail closed → run fresh)
	}
	return results, true
}

// storeValidateCache writes results as the cache entry for key. The write is ATOMIC — a temp
// file in the SAME dir followed by os.Rename — because LandSet runs many validateCommitted
// concurrently and a reader must never observe a half-written entry; rename gives an
// all-or-nothing publish. It is best-effort: a failed write is swallowed (the result was
// already produced for the caller), so an unwritable cache only forfeits the optimization,
// never fails validation. validateCommitted passes only GREEN results here, so a red/flaky
// failure is never pinned.
func storeValidateCache(key string, results []validate.Result) {
	dir := paths.Default().ValidateCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, "."+key+"-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, validateCacheFile(key)); err != nil {
		os.Remove(tmpName)
	}
}

// processValidate memoizes the GREEN validate results THIS PROCESS actually produced, keyed by
// the same content key as the on-disk cache. It is the trust gate's only reusable green.
//
// The on-disk cache cannot be that, and the reason is not that its key is weak — the key is a
// git tree hash, which is a cryptographic content identity, and the invariant it proves (a hit
// is served only for a byte-identical tree under an identical gate script) is true. The reason
// is that the invariant addresses the wrong adversary. It proves the cache cannot serve a
// result computed for a DIFFERENT tree. It says nothing about an entry that was never computed
// at all, and both inputs to the key — the worker's own tree hash and the default-branch
// .ttorch/validate.sh — are readable by the worker, so any process running as the lead can
// write a green entry for its own tree. Against the diff channel (hostile content committed to
// the repo) the on-disk cache is fine; against a live session running as the lead it is an
// unauthenticated green.
//
// Authenticating entries with a per-install secret was considered and rejected rather than
// shipped as a fix: the secret is readable by the same uid as the process it would be
// defending against, so it would read as a control in the changelog and be one nowhere. What
// is left that a local process cannot forge is provenance in memory: an entry is reusable as
// an AUTHORITY only if this process ran the suite and observed the green itself. The on-disk
// cache keeps its job as a performance record for REPORTING — the reviewers' validate.json and
// any status output — where a forged green misleads a reader but authorizes nothing.
//
// Entries hold only the DECISION SHAPE, never the checks' output. A validate.Result carries
// Output — the whole CombinedOutput() of the check — and the on-disk cache, which stores the
// same values under the same key, runs to gigabytes on this repo's own history (168 entries,
// 3.3 GB, largest 74 MB). A process-global map with no eviction in a scheduler that stays up
// for days would hold that resident. Nothing needs it to: the authority decision is
// stagedGreen, which reads Passed, and the no-checks hard block reads len(). Output stays in
// the on-disk record, which is read for reporting and for the reviewers' validate.json.
//
// What is left is one small struct per check per distinct tree the daemon touches, so the map
// is left unbounded.
var (
	processValidateMu sync.Mutex
	processValidate   = map[string][]validate.Result{}
)

// loadProcessValidate returns the green results this process produced for key, if any.
func loadProcessValidate(key string) ([]validate.Result, bool) {
	processValidateMu.Lock()
	defer processValidateMu.Unlock()
	results, ok := processValidate[key]
	return results, ok
}

// storeProcessValidate records a green this process produced, stripped to the decision shape
// by memoShape. Only runAndRecordGate calls it, and only after a real suite run, so a cache
// HIT can never become a memoized authority.
func storeProcessValidate(key string, results []validate.Result) {
	processValidateMu.Lock()
	defer processValidateMu.Unlock()
	processValidate[key] = memoShape(results)
}

// memoShape copies results with the checks' output dropped. It preserves the per-check
// identity and pass/fail that stagedGreen and the no-checks block read, and nothing else, so
// a reused memo reproduces the identical decision while holding none of the log. It copies
// rather than mutating, because the caller still returns the full results to its own caller.
func memoShape(results []validate.Result) []validate.Result {
	if results == nil {
		return nil
	}
	out := make([]validate.Result, len(results))
	for i, r := range results {
		r.Output = ""
		out[i] = r
	}
	return out
}

// resetProcessValidate drops every memoized green. It exists for the tests: the fixtures build
// byte-identical trees under identical gate scripts, so they share a content key and would
// otherwise serve each other's results across test boundaries.
func resetProcessValidate() {
	processValidateMu.Lock()
	defer processValidateMu.Unlock()
	processValidate = map[string][]validate.Result{}
}

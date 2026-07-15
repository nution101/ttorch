package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

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

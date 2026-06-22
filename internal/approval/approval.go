// Package approval implements the human-approval token that gates merges.
//
// `orcha approve <id>` grants a short-lived token; `orcha merge-local <id>`
// consumes it. This makes approval a distinct, time-boxed, auditable step rather
// than an implicit side effect of the manager running a command. (It is defense
// in depth + an audit trail, not an unbreakable barrier against a fully
// compromised manager, which would require a secret the manager cannot read.)
package approval

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Grant writes an approval token valid for ttl.
func Grant(path string, ttl time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	expires := time.Now().Add(ttl).UnixNano()
	return os.WriteFile(path, []byte(strconv.FormatInt(expires, 10)+"\n"), 0o600)
}

// Valid reports whether a non-expired approval token exists (without consuming it).
func Valid(path string) bool {
	exp, ok := readExpiry(path)
	return ok && time.Now().UnixNano() < exp
}

// Consume validates and removes the token, returning whether it was valid. A
// stale (expired) token is also removed.
func Consume(path string) bool {
	exp, ok := readExpiry(path)
	if !ok {
		return false
	}
	_ = os.Remove(path)
	return time.Now().UnixNano() < exp
}

func readExpiry(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return exp, true
}

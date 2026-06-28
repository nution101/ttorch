// Package approval implements the human-approval token that gates merges.
//
// `ttorch approve <id>` grants a short-lived token bound to the reviewed commit;
// `ttorch merge-local <id>` consumes it and refuses if the worker's HEAD has moved
// since approval. This makes approval a distinct, time-boxed, commit-bound step
// rather than an implicit side effect of the manager running a command. (It is
// defense in depth + an audit trail, not an unbreakable barrier against a fully
// compromised manager, which would require a secret the manager cannot read.)
package approval

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Grant writes a token valid for ttl, bound to opaque data (the reviewed commit
// sha) that Consume returns.
func Grant(path string, ttl time.Duration, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	expires := time.Now().Add(ttl).UnixNano()
	body := strconv.FormatInt(expires, 10) + "\n" + data + "\n"
	return os.WriteFile(path, []byte(body), 0o600)
}

// Valid reports whether a non-expired token exists (without consuming it).
func Valid(path string) bool {
	exp, _, ok := read(path)
	return ok && time.Now().UnixNano() < exp
}

// Data returns the bound data of a non-expired token without consuming it, so a caller
// can inspect a token's provenance (e.g. who minted it) before deciding whether the
// token may be consumed at all. It mirrors Valid but also yields the data.
func Data(path string) (string, bool) {
	exp, data, ok := read(path)
	if !ok || time.Now().UnixNano() >= exp {
		return "", false
	}
	return data, true
}

// Remaining returns how long a non-expired token has left to live, without consuming
// it. ok is false when the token is absent or already expired. The land carry-forward
// re-pins an auto-approval to the rebased commit while preserving its ORIGINAL lifetime
// (rather than extending it), so it reads the remaining time here and re-grants with it.
func Remaining(path string) (time.Duration, bool) {
	exp, _, ok := read(path)
	if !ok {
		return 0, false
	}
	d := time.Duration(exp - time.Now().UnixNano())
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// Consume removes the token and returns its bound data and whether it was valid.
// A stale (expired) token is also removed.
func Consume(path string) (data string, ok bool) {
	exp, data, present := read(path)
	if !present {
		return "", false
	}
	_ = os.Remove(path)
	if time.Now().UnixNano() >= exp {
		return "", false
	}
	return data, true
}

func read(path string) (expires int64, data string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	lines := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 2)
	exp, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return 0, "", false
	}
	if len(lines) > 1 {
		data = lines[1]
	}
	return exp, data, true
}

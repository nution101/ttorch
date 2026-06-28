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

// Repin rebinds a still-valid token to newData while preserving its ORIGINAL absolute
// expiry, so a carry-forward never resets the clock or extends the grant. It is the land
// carry-forward primitive: a clean, disjoint rebase moves the reviewed commit to a new sha,
// and the approval token must follow it to the rebased commit — same provenance, same
// remaining lifetime — when (and only when) the reviewed content is byte-identical.
//
// The re-check of the token's validity and the write are done here together against the same
// read, so a token that lapsed in a caller's read→re-pin window is reported as moved==false
// and NOTHING is written (the caller can then carry nothing and fail closed). moved==true
// means the token was rebound to newData. Either way the gate is never bypassed: the merge
// re-validates the consumed token's sha against the commit it actually fast-forwards.
func Repin(path, newData string) (moved bool, err error) {
	exp, _, present := read(path)
	if !present || time.Now().UnixNano() >= exp {
		return false, nil
	}
	body := strconv.FormatInt(exp, 10) + "\n" + newData + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return false, err
	}
	return true, nil
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

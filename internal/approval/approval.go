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

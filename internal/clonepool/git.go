package clonepool

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// git runs a git command and returns its combined output, trimmed. The error text quotes
// the arguments and the output, so control bytes in a path or in git's stderr reach the
// lead's terminal as visible escapes rather than as sequences the terminal obeys. The
// returned value is left verbatim for the caller to parse.
func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", quoteArgs(args), err, strconv.Quote(s))
	}
	return s, nil
}

// gitOut runs a git command and returns its stdout verbatim, for output parsed by
// separator, where folding stderr in or trimming would corrupt the records.
func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", quoteArgs(args), err, strconv.Quote(strings.TrimSpace(errBuf.String())))
	}
	return string(out), nil
}

func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = strconv.Quote(a)
	}
	return strings.Join(q, " ")
}

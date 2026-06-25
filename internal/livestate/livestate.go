// Package livestate holds the small, pure heuristics that classify a worker's
// live state from an observable tmux pane capture. Keeping them in a neutral leaf
// package lets the supervisor's stale-detection, `ttorch status` (DeriveState),
// and the watcher share one definition so they never disagree, without any of
// those callers importing one another.
package livestate

import "strings"

// Busy reports whether a captured pane shows a harness busy indicator (the worker
// is mid-turn). It is the shared heuristic behind the supervisor's stale-detection
// and `ttorch status`, so the two never disagree.
func Busy(pane string) bool {
	low := strings.ToLower(pane)
	for _, m := range []string{"esc to interrupt", "working…", "working...", "thinking", "generating"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

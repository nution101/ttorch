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

// Stalled reports whether a captured pane shows the harness's mid-stream API-stall
// error ("API Error: Response stalled mid-stream"): the model stream died and the
// worker is now sitting idle at the prompt, recoverable by a single "continue" rather
// than any human intervention. Matched case-insensitively on the distinctive phrase
// (with and without the hyphen) so minor wording around it still hits. A stalled pane
// carries no Busy marker, so the watcher's auto-resume reaches it only on an otherwise-
// idle worker — a genuinely working one is never mistaken for stalled. Kept beside Busy
// so every caller shares one definition of "stalled".
func Stalled(pane string) bool {
	low := strings.ToLower(pane)
	for _, m := range []string{"stalled mid-stream", "stalled mid stream"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

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

// Stalled reports whether a captured pane shows one of the harness's recoverable
// mid-stream API-stall errors — the model stream or the underlying connection died
// part-way through a response, leaving the worker sitting idle at the prompt, recoverable
// by a single "continue" rather than any human intervention. It recognizes the known
// phrasings ("API Error: Response stalled mid-stream" and "API Error: Connection closed
// mid-response"), each matched case-insensitively on its distinctive fragment (with and
// without the hyphen) so the surrounding "API Error:" prefix or a trailing "(retrying)"
// still hits — while a non-stall API error (e.g. a rate limit or auth failure) does not,
// so a worker that is merely showing a transient, self-retrying error is never nudged.
// A stalled pane carries no Busy marker, so the watcher's auto-resume reaches it only on
// an otherwise-idle worker — a genuinely working one is never mistaken for stalled. Kept
// beside Busy so every caller shares one definition of "stalled".
func Stalled(pane string) bool {
	low := strings.ToLower(pane)
	for _, m := range []string{
		"stalled mid-stream", "stalled mid stream", // API Error: Response stalled mid-stream
		"closed mid-response", "closed mid response", // API Error: Connection closed mid-response
	} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

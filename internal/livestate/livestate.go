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
	for _, m := range []string{"esc to interrupt", "working…", "working...", "thinking", "generating", "compacting"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// Idle reports whether a captured pane shows a worker sitting at the Claude Code input
// prompt with its turn ENDED — the positive complement to Busy. A pane is idle when it
// is NOT Busy AND shows the input-prompt caret: the "> " the harness renders inside its
// input box (e.g. `│ > Try "edit this file"`, or a bare `> ` line). Requiring the caret —
// rather than treating EVERY not-busy pane as idle — is what keeps a nudge from ever being
// injected into a pane that is not actually waiting for input: a crashed shell prompt
// (`$ `), a half-rendered frame, or a screen with no input box at all is not idle. The
// caret is matched only at the START of a line once box-border glyphs and indentation are
// stripped, so a ">" buried inside ordinary output never counts. A Busy pane is never idle
// (a worker mid-turn is filtered first), so a genuinely working worker is never mistaken
// for idle. Kept beside Busy/Stalled so the supervisor's idle-nudge and any future caller
// share one definition of "idle at the prompt."
func Idle(pane string) bool {
	if Busy(pane) {
		return false
	}
	for _, line := range strings.Split(pane, "\n") {
		// Strip the input box's left/right border glyphs and surrounding whitespace so the
		// caret that opens the prompt line is exposed; the caret itself (">") is never in
		// the cutset, so "> Try …" survives as "> Try …".
		s := strings.TrimSpace(strings.Trim(line, " \t│┃┆┇┊┋╎╏║|"))
		if s == ">" || strings.HasPrefix(s, "> ") {
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

// apiStallMarkers is the small, documented, maintainable set of pane fragments that mark one
// of the harness's RECOVERABLE mid-stream stalls — the model stream or the underlying
// connection died part-way through a response, or a stream watchdog fired — leaving the
// session idle at the prompt with its turn ended, recoverable by a single "continue". Each is
// matched case-insensitively on its distinctive fragment (with and without the hyphen) so the
// surrounding "API Error:" prefix or a trailing "(retrying)" still hits. It is deliberately a
// SUPERSET of Stalled's two phrasings (Stalled is the watcher's existing looser check; this
// list adds the generic "response stalled", "stream disconnected", and the stream-watchdog
// timeout phrasing). It deliberately does NOT include a bare "API Error": that substring
// subsumes rate-limit (429), auth (401), and request-timeout failures that a "continue" cannot
// fix — those are non-recoverable and left for a human/the manager, never auto-nudged. Add a
// phrasing here only when it is genuinely a recoverable mid-stream stall.
var apiStallMarkers = []string{
	"stalled mid-stream", "stalled mid stream", // API Error: Response stalled mid-stream
	"response stalled",                           // generic "Response stalled" phrasing
	"closed mid-response", "closed mid response", // API Error: Connection closed mid-response
	"stream disconnected", // the stream dropped mid-response
	"stream watchdog",     // the stream-watchdog timeout phrasing
}

// APIStalled reports whether a captured pane shows the session sitting IDLE at the input
// prompt with a recoverable API-stall error (apiStallMarkers) as its LAST significant output —
// the precise signal the daemon's stall-recovery pass acts on (a single "continue" revives
// it), for BOTH worker windows and the manager window (the same harness, the same pane shape).
// It is the stricter, position-aware sibling of Stalled: it is true ONLY when
//
//   - the turn has ENDED at the prompt — Idle(pane): NOT Busy AND the input caret is present —
//     so a working/streaming or half-rendered pane is never treated as stalled, and a nudge is
//     never injected into a session that has not actually returned to the prompt; and
//   - the LAST significant line — scanning bottom-up past blank lines and the harness's input-
//     box furniture (borders, the caret line, the shortcut hint) — contains an api-stall marker.
//
// Requiring the stall to be the LAST significant output is what distinguishes a session truly
// wedged at the prompt by a stall from one that hit a transient stall, RECOVERED, and produced
// real output afterward (the stall is then buried above newer activity → NOT APIStalled, so it
// falls to the plain idle path, never this one). This is the explicit COMPLEMENT to a plain
// idle prompt (Idle && no error), which the scheduler's H2 idle-nudge owns; the two act on
// disjoint pane states so a session is nudged by exactly one path.
//
// One accepted ambiguity (shared with Idle): the harness's input placeholder line "> Try …"
// is indistinguishable from a markdown blockquote "> …" in real output, so both count as
// caret furniture. A session whose final real output line is a blockquote, sitting below an
// older stall, can therefore read as APIStalled. The cost is at most one extra "continue" to
// an otherwise-idle session (which the idle path would nudge anyway) — bounded and benign.
func APIStalled(pane string) bool {
	if !Idle(pane) {
		return false
	}
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		// Strip the input box's left/right border glyphs and surrounding whitespace, mirroring
		// Idle, so the caret and any boxed content are exposed for the furniture/marker checks.
		stripped := strings.TrimSpace(strings.Trim(lines[i], " \t│┃┆┇┊┋╎╏║|"))
		if isPromptFurniture(stripped) {
			continue
		}
		// First significant line from the bottom: it is a stall iff it carries a marker.
		low := strings.ToLower(stripped)
		for _, m := range apiStallMarkers {
			if strings.Contains(low, m) {
				return true
			}
		}
		return false
	}
	return false
}

// isPromptFurniture reports whether a border-stripped line is part of the harness's input-box
// chrome rather than real session output: a blank line, the input caret/placeholder line, a
// pure box-drawing border, or the keyboard-shortcut hint. APIStalled's bottom-up scan skips
// these so the "last significant line" it tests is real output (the stall marker), never the
// prompt furniture that always renders below it.
func isPromptFurniture(stripped string) bool {
	if stripped == "" {
		return true
	}
	if stripped == ">" || strings.HasPrefix(stripped, "> ") {
		return true // the input caret / placeholder line (see APIStalled's accepted ambiguity)
	}
	if strings.Contains(strings.ToLower(stripped), "for shortcuts") {
		return true // e.g. "? for shortcuts"
	}
	// A pure box-drawing border (only frame glyphs remain after stripping the border cutset).
	if strings.Trim(stripped, "─━┄┅┈┉╌╍╭╮╰╯╴╵╶╷ ") == "" {
		return true
	}
	return false
}

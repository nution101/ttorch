package brieflint

import (
	"regexp"
	"strings"
)

// Signal matching — how a brief is judged to have SAID something, without pinning it to a
// phrasing.
//
// The prototype this replaces matched a closed set of accepted full phrasings ("verify this
// count yourself and report your number"). That is brittle in exactly the way that matters:
// a hedge that said the same thing in different words failed the check, so the linter
// punished good briefs and taught its users to parrot a magic sentence.
//
// Instead, a requirement is decomposed into independent WORD-STEM signals, each an open set
// of stems rather than one phrasing, and matched:
//
//   - case-insensitively and on word boundaries, with an explicit `*` suffix for the stems
//     that are meant to match a family ("verif*" covers verify / verified / verification);
//   - order-free — "report the real number once you have counted" and "count them, then
//     report the real number" satisfy the same pair;
//   - sentence-scoped where co-occurrence carries the meaning (a reporting verb beside a
//     word for the number), brief-scoped where it does not (one hedge covers the brief).
//
// A stem list is cheap to extend and each entry is independently sufficient, so an
// unanticipated wording costs a one-word addition instead of a whole new phrasing. The
// trade is deliberate: this admits the occasional brief that carries the words without the
// intent, and in exchange it stops rejecting briefs that carry the intent in other words.
// For a lint that gates a human's own writing before dispatch, a false accept costs one
// unhedged count; a false reject costs the author a round trip and teaches them to distrust
// the linter.

// stemRe builds a case-insensitive alternation over stems, anchored on word boundaries.
// A stem ending in `*` matches any continuation (a word family); every other stem must
// match a whole word, so "pr" cannot fire inside "problem". Whitespace inside a stem
// matches any run of whitespace, so a phrase that wraps across lines still matches.
func stemRe(stems ...string) *regexp.Regexp {
	parts := make([]string, len(stems))
	for i, s := range stems {
		prefix := strings.HasSuffix(s, "*")
		esc := regexp.QuoteMeta(strings.TrimSuffix(s, "*"))
		esc = strings.ReplaceAll(esc, " ", `\s+`)
		if !prefix {
			esc += `\b`
		}
		parts[i] = esc
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(parts, "|") + `)`)
}

// verifySignal marks text that tells the reader not to trust a stated number: a verifying
// or re-measuring verb, or an explicit admission that the figure may be wrong.
var verifySignal = stemRe(
	"verif*", "confirm*", "check*", "re-check*", "recheck*", "re-count*", "recount*",
	"count them", "count it", "count the", "counting", "measure*", "establish*",
	"determin*", "validat*", "audit*", "do not trust", "don't trust", "dont trust",
	"may be wrong", "might be wrong", "could be wrong", "may be off", "might be off",
	"may be stale", "not authoritative", "indicativ*", "approximat*", "as a hint",
	"yourself", "your own",
)

// reportSignal marks a reporting verb — the second half of the hedge: the worker must not
// only re-check, it must tell the reader what it found.
var reportSignal = stemRe(
	"report*", "state*", "tell*", "say", "says", "give", "gives", "surface*", "note",
	"notes", "record*", "list", "lists", "come back with", "let me know", "what you find",
	"what you found",
)

// numberSignal marks a word for the thing being reported. Paired with reportSignal in the
// same sentence, it is what distinguishes "report the real number" from "report your
// progress".
var numberSignal = stemRe(
	"number*", "count", "counts", "total*", "figure", "figures", "tally", "how many",
	"actual", "real", "true", "finding", "findings", "result", "results",
)

// hedged reports whether the brief hedges its hard counts: somewhere it tells the reader to
// verify the figure, AND somewhere a reporting verb sits in the same sentence as a word for
// the number being reported. The hedge is brief-scoped deliberately — one hedge at the top
// legitimately covers every count below it, and demanding one per count would only push
// authors to repeat themselves.
func hedged(b *brief) bool {
	if !verifySignal.MatchString(b.raw) {
		return false
	}
	for _, s := range sentences(b.raw) {
		if reportSignal.MatchString(s.text) && numberSignal.MatchString(s.text) {
			return true
		}
	}
	return false
}

// boundSignal marks a prohibition that names what it protects or how far it reaches, rather
// than banning an action outright: a target ("to main", "into the product branch"), a bound
// ("until I have reviewed"), or the specific artifact banned ("no PR").
var boundSignal = stemRe(
	"until", "before", "unless", "without my", "without the lead", "to main", "to master",
	"to the default", "to the product", "to any product", "to origin", "into main",
	"into master", "into the default", "into the product", "on main", "on master",
	"on the default", "on the product", "product branch", "shared branch", "release branch",
	"pull request", "pull requests", "pr", "prs", "remote branch", "base branch",
)

// endStateSignal marks a statement of what the worker IS allowed to do, or where the work
// is meant to stop. A prohibition without one leaves the worker no legal way to finish,
// which is how a blanket "do NOT push" stalls a worker mid-task.
var endStateSignal = stemRe(
	"allowed", "permitted", "you may", "you can", "end state", "leave the", "leave it",
	"leave your", "stop after", "stop when", "stop once", "hand back", "hand it back",
	"hand off", "commit locally", "local commit", "local commits", "commit to your",
	"commit on your", "report the sha", "report back", "i will tell you", "i'll tell you",
	"wait for", "ask me", "tell me when", "when i say", "on my go", "expected end",
	"deliverable", "done when",
)

// createSignal marks a sentence that asks for something to be CREATED. A brief may
// legitimately cite a path that does not exist yet ("write dev/report.md"), and reporting
// that as a missing file would fail every brief that asks for a new file. A citation in
// such a sentence is exempted from the existence check, and the exemption is reported.
var createSignal = stemRe(
	"write", "writes", "writing", "create*", "add", "adds", "adding", "new file",
	"introduce*", "generate*", "produce*", "scaffold*", "author", "populate*",
)

// span is a slice of the brief with its byte offset, so a match can be reported with the
// line it came from.
type span struct {
	text string
	off  int
}

// sentences splits text into sentence-ish spans, preserving offsets. A newline always ends
// a span — a brief is Markdown, where a bullet or a heading is a sentence even without a
// full stop — and so does sentence punctuation, but ONLY when whitespace or the end of the
// text follows it. That proviso is what keeps "internal/cli/cli.go:40" in one piece: a
// splitter that broke on every dot and colon would cut a cited path in half, both in the
// quoted text and in the path extraction that reads these spans.
func sentences(text string) []span {
	var out []span
	emit := func(s string, off int) {
		if strings.TrimSpace(s) != "" {
			out = append(out, span{text: s, off: off})
		}
	}
	pos := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
		case '.', '!', '?', ';', ':':
			if i+1 < len(text) && !isSpace(text[i+1]) {
				continue
			}
		default:
			continue
		}
		emit(text[pos:i], pos)
		pos = i + 1
	}
	emit(text[pos:], pos)
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// markerPrefix is a leading Markdown bullet, heading or block-quote marker: the markers are
// only stripped when whitespace follows, so emphasis around the author's own words
// ("**do not push**") survives into the quote.
var markerPrefix = regexp.MustCompile(`^(?:[-*+>#]+\s+)+`)

// trim renders a span for quoting: collapsed whitespace with any leading Markdown bullet or
// heading marker removed, so the quote reads as the sentence the author wrote.
func (s span) trim() string {
	t := strings.Join(strings.Fields(s.text), " ")
	return markerPrefix.ReplaceAllString(t, "")
}

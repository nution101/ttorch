package brieflint

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
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

// distrustSignal marks text saying the figure itself is not to be trusted.
var distrustSignal = stemRe(
	"may be wrong", "might be wrong", "could be wrong", "may be off", "might be off",
	"may be stale", "might be stale", "may be out of date", "not authoritative",
	"do not trust", "don't trust", "dont trust", "indicativ*", "approximat*", "rough*",
	"estimate*", "unverified", "not exact", "may not be exact", "as a hint", "may be more",
	"may be fewer", "may be less", "could be more", "could be fewer", "i may have miscounted",
	"miscount*", "from memory", "off the top of my head",
)

// recountSignal marks an instruction to establish the number again rather than take it as
// given. Bare "check" and bare "verify" are deliberately absent: "check the build" is not a
// hedge, and admitting words that common is what let boilerplate satisfy this rule.
var recountSignal = stemRe(
	"recount*", "re-count*", "count them", "count it", "count the", "count yourself",
	"count again", "count for yourself", "verify the count", "verify the number",
	"verify the total", "verify the figure", "verify that count", "verify that number",
	"check the count", "check the number", "check the total", "check the figure",
	"confirm the count", "confirm the number", "confirm the total", "establish the",
	"determine the actual", "determine the real", "determine the true", "your own count",
	"your own number", "your own total", "your own tally", "find the real",
	"find the actual", "find the true",
)

// hedgedBlocks returns the blocks carrying hedge wording: something saying the figure is not
// to be trusted, or telling the reader to establish it again.
//
// Precomputed for the whole brief, because the question is asked once per hard count and the
// answer depends only on the block. Asking it by rescanning every span per count made a
// 36 KB brief of nothing but counts take 19s.
func hedgedBlocks(sents []span) map[int]bool {
	out := map[int]bool{}
	for _, s := range sents {
		if out[s.blk] {
			continue
		}
		if distrustSignal.MatchString(s.text) || recountSignal.MatchString(s.text) {
			out[s.blk] = true
		}
	}
	return out
}

// hedgedAt reports whether a hard count in block blk carries a hedge attached to it: one in
// its own block, or in the block after it.
//
// Reach is structural rather than a sentence budget. Counting sentences meant an aside
// between the count and its hedge ("Note: I grepped for the bare name") spent the budget on
// the aside, and the brief was reported as unhedged although a careful author had hedged it
// in the next breath.
//
// Block scope is also why this needs no stop-unit narrowing, which is a decision rather than
// an omission: a block is wider than a sentence either way, so splitting a merged span
// cannot change which blocks are in reach.
func hedgedAt(hedged map[int]bool, blk int) bool {
	return hedged[blk] || hedged[blk+1]
}

// reportsTheNumber reports whether some sentence pairs a reporting verb with a number word.
// Per sentence, and a merged span carries more than one: "note the results. 21 files are
// stale" is two sentences and pairs nothing.
func reportsTheNumber(b *brief) bool {
	for _, s := range sentences(b.raw) {
		for _, u := range s.units() {
			part := s.text[u[0]:u[1]]
			if reportSignal.MatchString(part) && numberSignal.MatchString(part) {
				return true
			}
		}
	}
	return false
}

// clauseSplit marks the end of a clause inside a sentence. A comma or semicolon is the only
// punctuation left that separates clauses, now that neither ends a sentence.
var clauseSplit = regexp.MustCompile(`[,;]`)

// leadingConjunction is the "and"/"or"/"then" that opens a continuation clause. It is
// stripped before asking whether a clause LEADS with a bound.
var leadingConjunction = regexp.MustCompile(`(?i)^\s*(?:and|or|then|but|so)\s+`)

// clauses splits a sentence into comma-delimited clause ranges.
func clauses(text string) [][2]int {
	var out [][2]int
	start := 0
	for _, m := range clauseSplit.FindAllStringIndex(text, -1) {
		out = append(out, [2]int{start, m[0]})
		start = m[1]
	}
	return append(out, [2]int{start, len(text)})
}

// prohibitionHit is one ban found in a span: where it sits, which sentence of the span
// carries it, and whether a bound in reach qualifies it.
type prohibitionHit struct {
	loc     [2]int // the ban, relative to the span
	unit    [2]int // the sentence carrying it, relative to the span
	bounded bool
}

// prohibitionHits finds every ban in a span and decides each one's bounding, in one pass
// over the span.
//
// The bound has to be in the ban's own clause, or in a clause at one end of its sentence
// that opens with the bound and carries no ban of its own ("Until the gate is green, do not
// push", "do not push, and do not merge, until the gate is green").
//
// Sentence scope was what soft-wrap joining widened: a blanket prohibition and an unrelated
// later clause became one sentence, so "and before you begin, read the notes" bounded "do
// not commit anything at all".
//
// One pass matters as much as the scoping. The first version asked each ban its own
// question, and each question rescanned the span from byte 0 to find the ban's sentence and
// that sentence's clauses. That is O(bans x span bytes), and a brief is untrusted input:
// 8000 bans in one block took 69s where the version before it took 0.13s. Each clause is
// now scanned a fixed number of times and every ban reads the result.
func prohibitionHits(ctx context.Context, sent span) ([]prohibitionHit, bool) {
	text := sent.text
	var out []prohibitionHit
	scanned := 0
	for _, u := range sent.units() {
		unit := text[u[0]:u[1]]
		cls := clauses(unit)
		bound := make([]bool, len(cls))
		bans := make([][][2]int, len(cls))
		for i, c := range cls {
			// A clause is the finest unit of work here: one bound scan and one ban scan
			// over it, both linear. The budget is checked between clauses, so the work
			// that cannot be interrupted is one clause of a brief whose size is capped.
			if scanned%budgetCheckEvery == 0 && ctx.Err() != nil {
				return out, false
			}
			scanned++
			part := unit[c[0]:c[1]]
			bound[i] = boundSignal.MatchString(part)
			for _, m := range prohibitionRe.FindAllStringIndex(part, -1) {
				bans[i] = append(bans[i], [2]int{u[0] + c[0] + m[0], u[0] + c[0] + m[1]})
			}
		}
		// A bound at either end of the sentence, opening its clause and banning nothing,
		// qualifies every ban in that sentence.
		edge := false
		for _, i := range []int{0, len(cls) - 1} {
			if !bound[i] || len(bans[i]) > 0 {
				continue
			}
			if leadsWithBound(unit[cls[i][0]:cls[i][1]]) {
				edge = true
			}
		}
		for i := range cls {
			for _, loc := range bans[i] {
				out = append(out, prohibitionHit{loc: loc, unit: u, bounded: bound[i] || edge})
			}
		}
	}
	return out, true
}

// leadsWithBound reports whether a clause opens with its bound, which is what a qualifier
// attached to the whole sentence looks like.
func leadsWithBound(part string) bool {
	part = leadingConjunction.ReplaceAllString(strings.TrimSpace(part), "")
	loc := boundSignal.FindStringIndex(part)
	return loc != nil && loc[0] == 0
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

// createSignal marks a verb asking for something to be CREATED. A brief may legitimately
// cite a path that does not exist yet ("write dev/report.md"), and reporting that as a
// missing file would fail every brief that asks for a new file.
//
// The verb has to govern the citation, not merely share a sentence with it: see
// governedByCreate. Sentence scope exempted "The regression at internal/nope/ghost.go:9999
// came from adding the cache", where the create verb is retrospective and the path is
// fabricated, which is precisely the defect this rule exists to catch.
var createSignal = stemRe(
	"write", "writes", "writing", "create*", "add", "adds", "adding", "new file",
	"introduce*", "generate*", "produce*", "scaffold*", "author", "populate*",
)

// span is a slice of the brief with its byte offset, so a match can be reported with the
// line it came from, and the index of the block it belongs to.
type span struct {
	text string
	off  int
	blk  int
	// stops are the offsets, within text, of the full stops the splitter merged. Computed
	// once when the span is built, because every rule that asks "which sentence is this
	// position in" asks it once per item it found, and computing the answer per question
	// made two rules quadratic in the brief's size, in consecutive commits.
	stops []int
}

// unitAround returns the sentence of this span containing pos: a walk over the precomputed
// boundaries, never a scan of the text.
func (s span) unitAround(pos int) [2]int {
	lo, hi := 0, len(s.text)
	for _, st := range s.stops {
		if st > pos {
			hi = st
			break
		}
		lo = st + 1
	}
	return [2]int{lo, hi}
}

// units returns this span's sentences, in order.
func (s span) units() [][2]int {
	out := make([][2]int, 0, len(s.stops)+1)
	start := 0
	for _, st := range s.stops {
		out = append(out, [2]int{start, st})
		start = st + 1
	}
	return append(out, [2]int{start, len(s.text)})
}

// blockMarker opens a Markdown block: a bullet, a heading, a quote, a table row, a fence,
// or a numbered or lettered list item. Its trailing space is part of the match, so the
// scanner can start after it.
var blockMarker = regexp.MustCompile("^[ \t]*(?:```|\\||(?:[-*+>]+|#{1,6}|[0-9]{1,3}[.)]|[A-Za-z][.)])[ \t]+)")

// operandAbbrevs are words whose trailing dot never ends a sentence, because what follows is
// the abbreviation's operand and may be capitalised or a digit: "approx. 21 occurrences",
// "Fig. 3", "e.g. Foo". Dotted initialisms (e.g., i.e., U.S.) are recognised by shape and
// treated the same way.
var operandAbbrevs = map[string]bool{
	"approx": true, "cf": true, "vs": true, "resp": true, "fig": true, "incl": true,
	"excl": true, "eg": true, "ie": true,
}

// clauseAbbrevs are words whose trailing dot MAY end a sentence. "etc. See the note" is two
// sentences; "etc. and then fix it" is one. The lookahead decides, exactly as it does for an
// ordinary period.
var clauseAbbrevs = map[string]bool{"etc": true, "al": true, "viz": true}

// blockRange is one Markdown block: a paragraph, a list item, a heading. Soft-wrapped lines
// belong to the block they continue.
type blockRange struct{ start, end int }

// blocks groups text into Markdown blocks. A blank line ends a block, and a line opening
// with a block marker starts one; any other line continues the block above it.
//
// Joining soft-wrapped lines is the point. An earlier version ended a span at every newline,
// which cut the wrapped sentences of an ordinary brief into fragments and pushed a hedge out
// of reach of the count it hedged.
func blocks(text string) []blockRange {
	var out []blockRange
	cur := blockRange{-1, -1}
	flush := func() {
		if cur.start >= 0 {
			out = append(out, cur)
			cur = blockRange{-1, -1}
		}
	}
	for off := 0; off <= len(text); {
		end := len(text)
		next := len(text) + 1
		if nl := strings.IndexByte(text[off:], '\n'); nl >= 0 {
			end = off + nl
			next = end + 1
		}
		line := text[off:end]
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case cur.start < 0 || blockMarker.MatchString(line):
			flush()
			cur = blockRange{off, end}
		default:
			cur.end = end
		}
		off = next
	}
	flush()
	return out
}

// sentences splits text into sentence-ish spans, preserving offsets and block membership.
//
// Only . ! and ? end a sentence, and only when whitespace or the end of the block follows
// AND the next thing along can begin one. Three kinds of ordinary prose broke the simpler
// rule this replaced:
//
//   - A cited path. "internal/cli/cli.go:40" must stay in one piece, both in the quoted text
//     and in the path extraction that reads these spans. That is why a terminator needs
//     whitespace after it, and why a colon is not a terminator at all.
//   - An abbreviation. "Add e.g. internal/new/thing.go" split at "e.g." and left the create
//     verb in an earlier fragment, so a brief asking for a new file was reported as citing a
//     missing one.
//   - A list marker. "1. There are 21 occurrences" split into "1" and the rest, which spent
//     the hedge lookahead on markup.
func sentences(text string) []span {
	var out []span
	emit := func(s string, off, blk int) {
		for len(s) > 0 && isSpace(s[0]) {
			s = s[1:]
			off++
		}
		if strings.TrimSpace(s) != "" {
			out = append(out, span{text: s, off: off, blk: blk, stops: sentenceStops(s)})
		}
	}
	for bi, b := range blocks(text) {
		bt := text[b.start:b.end]
		pos := len(blockMarker.FindString(bt))
		for i := pos; i < len(bt); i++ {
			if c := bt[i]; c != '.' && c != '!' && c != '?' {
				continue
			}
			next, ok := endsSentence(bt, i)
			if !ok {
				continue
			}
			emit(bt[pos:i], b.start+pos, bi)
			pos = next
			i = next - 1
		}
		emit(bt[pos:], b.start+pos, bi)
	}
	return out
}

// endsSentence reports whether the terminator at i closes a sentence rather than sitting
// inside a path, an abbreviation or a decimal. It also returns the index the next sentence
// starts from, which is past any closing punctuation belonging to this one.
//
// One rule, applied on every path: after any closing quote or bracket, whitespace or the end
// of the block must follow, and the next thing along must be able to begin a sentence. An
// earlier version skipped that lookahead on two branches and both MERGED sentences that
// should have split. "Add the fix, etc. See <path>" and `the note "done." See <path>` each
// became one sentence, which handed the create verb in the first sentence an exemption over
// a citation in the second. Over-splitting cost false positives in round 2; skipping the
// lookahead to cure that cost a false negative on the very rule the splitting protects.
func endsSentence(s string, i int) (int, bool) {
	j := i + 1
	for j < len(s) {
		n := closerLen(s, j)
		if n == 0 {
			break
		}
		j += n
	}
	if j < len(s) && !isSpace(s[j]) {
		return 0, false
	}
	// An operand abbreviation is never a boundary, whatever follows it.
	if s[i] == '.' && abbrevKind(s, i) == abbrevOperand {
		return 0, false
	}
	k := j
	for k < len(s) && isSpace(s[k]) {
		k++
	}
	if k >= len(s) {
		return j, true
	}
	return j, startsSentence(s[k])
}

// closerLen returns the width of the closing punctuation at j, or 0. A terminator can sit
// inside a quotation or a parenthesis, and the mark that closes it belongs to the sentence
// being closed.
func closerLen(s string, j int) int {
	switch s[j] {
	case '"', '\'', ')', ']', '}', '`':
		return 1
	}
	for _, q := range []string{"\u201d", "\u2019", "\u00bb"} {
		if strings.HasPrefix(s[j:], q) {
			return len(q)
		}
	}
	return 0
}

// startsSentence reports whether c can open a sentence: a capital, a digit, or the opening
// punctuation a sentence can start with.
func startsSentence(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("\"'(*_[`", c) >= 0
}

type abbrevClass int

const (
	abbrevNone abbrevClass = iota
	// abbrevOperand: the dot is always internal, because the operand follows it.
	abbrevOperand
	// abbrevClause: the dot may end a sentence, so the lookahead decides.
	abbrevClause
)

// abbrevKind classifies the dot at position dot by the word in front of it.
func abbrevKind(s string, dot int) abbrevClass {
	j := dot
	for j > 0 && isLetter(s[j-1]) {
		j--
	}
	w := strings.ToLower(s[j:dot])
	switch {
	case w == "":
		return abbrevNone
	case len(w) == 1:
		// A single letter already preceded by a dot is a dotted initialism: e.g., i.e., U.S.
		if j > 0 && s[j-1] == '.' {
			return abbrevOperand
		}
		return abbrevNone
	case operandAbbrevs[w]:
		return abbrevOperand
	case clauseAbbrevs[w]:
		return abbrevClause
	}
	return abbrevNone
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// markerPrefix is a leading Markdown bullet, heading or block-quote marker: the markers are
// only stripped when whitespace follows, so emphasis around the author's own words
// ("**do not push**") survives into the quote.
var markerPrefix = regexp.MustCompile(`^(?:[-*+>#]+\s+)+`)

// maxQuoteBytes caps a quoted sentence. A sentence is normally one line; a brief is
// untrusted input, and one of them was 93 KB of a single sentence, which a finding then
// printed in full into a report a human reads. Same treatment as a remote's stderr.
const maxQuoteBytes = 200

// trim renders a span for quoting: collapsed whitespace with any leading Markdown bullet or
// heading marker removed, so the quote reads as the sentence the author wrote, and capped so
// one sentence cannot fill a report.
func (s span) trim() string {
	t := strings.Join(strings.Fields(s.text), " ")
	t = markerPrefix.ReplaceAllString(t, "")
	if len(t) <= maxQuoteBytes {
		return t
	}
	cut := maxQuoteBytes
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	return t[:cut] + "..."
}

// createWindow is how many words may sit between a create verb and the path it governs. A
// verb further away than this is talking about something else in the sentence.
const createWindow = 6

// beForms make a create verb passive. "is generated by <path>" reads as an assertion that
// the path already exists, and the participle is the same word the active form uses.
var beForms = map[string]bool{
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"being": true, "gets": true, "got": true,
}

// sourceWords mark the path as where something comes FROM rather than what is being made.
// "Add the case from <path>", "Author the fix using <path>": the object being created is the
// case or the fix, and the path is a source that has to exist. Destination words (to, at,
// in, into) are deliberately absent, because "write your findings to dev/out.md" is a
// genuine create.
var sourceWords = map[string]bool{
	"from": true, "using": true, "by": true, "per": true, "like": true, "copied": true,
	"copying": true, "mirroring": true, "matching": true, "against": true, "similar": true,
	"based": true, "shown": true, "described": true,
}

// sentenceStops returns the offsets of the full stops inside text that read as ending a
// sentence: a dot followed by whitespace that is not part of an operand abbreviation.
//
// The splitter deliberately keeps these inside one span, because the word after them is
// lowercase and a lowercase word is usually a continuation rather than a new sentence. That
// is the right call for segmenting and the wrong one for any rule that then asks a question
// about "this sentence", so every such rule narrows its scope with these offsets. Without
// that, the same brief passes or fails on capitalisation alone.
func sentenceStops(text string) []int {
	var out []int
	for i := 0; i < len(text); i++ {
		if text[i] != '.' || i+1 >= len(text) || !isSpace(text[i+1]) {
			continue
		}
		if abbrevKind(text, i) != abbrevOperand {
			out = append(out, i)
		}
	}
	return out
}

// crossesAStop reports whether a full stop separates the two ends of text.
func crossesAStop(text string) bool { return len(sentenceStops(text)) > 0 }

// createVerbs returns the create-verb matches in a sentence. Computed once per sentence and
// passed to governedByCreate for every citation in it: scanning the prefix per citation is
// what made rule 2 take six minutes on a brief of 24,000 path-shaped tokens.
func createVerbs(sent string) [][]int {
	return createSignal.FindAllStringIndex(sent, -1)
}

// governedByCreate reports whether a create verb governs the citation starting at pathStart.
// verbs are the sentence's create-verb matches, from createVerbs.
//
// The verb must precede the path and sit within createWindow words of it, must not be
// passive ("the regression is generated by <path>" asserts the path exists), must not have
// the path as its source rather than its object ("add the case from <path>"), and must not
// be separated from the path by a full stop the splitter merged.
//
// Everything here reads a bounded neighbourhood of the citation. Nothing walks the sentence
// from the start, because a sentence can hold thousands of citations.
func governedByCreate(sent string, verbs [][]int, pathStart int) bool {
	last := lastVerbBefore(verbs, pathStart)
	if last == nil {
		return false
	}
	gap := sent[last[1]:pathStart]
	if countWordsUpTo(gap, createWindow+1) > createWindow {
		return false
	}
	// A verb cannot govern across a full stop, even one the splitter merged. The splitter
	// keeps "shim. e.g. <path>" in one sentence because a lowercase word follows the stop,
	// which is right for segmenting and wrong for reaching: "I already created the shim"
	// says nothing about the path in the clause after it.
	if crossesAStop(gap) {
		return false
	}
	// Passive voice: "the regression IS GENERATED by <path>" asserts the path exists.
	if beForms[wordBefore(sent, last[0])] {
		return false
	}
	// The word immediately before the path says whether the path is the thing being made
	// or the thing it is being made from.
	return !sourceWords[wordBefore(sent, pathStart)]
}

// lastVerbBefore returns the last verb match ending at or before pos, by binary search.
func lastVerbBefore(verbs [][]int, pos int) []int {
	lo, hi := 0, len(verbs)
	for lo < hi {
		mid := (lo + hi) / 2
		if verbs[mid][1] <= pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return nil
	}
	return verbs[lo-1]
}

// countWordsUpTo counts the words in s, giving up once the count passes max. The caller only
// cares whether a gap is short, and the gap can be the rest of a very long sentence.
func countWordsUpTo(s string, max int) int {
	n, in := 0, false
	for i := 0; i < len(s); i++ {
		if isSpace(s[i]) {
			in = false
			continue
		}
		if !in {
			in = true
			n++
			if n > max {
				return n
			}
		}
	}
	return n
}

// wordBefore returns the word ending immediately before pos, lowercased and stripped of
// punctuation. It reads backwards over one word, never from the start of the sentence.
func wordBefore(s string, pos int) string {
	end := pos
	for end > 0 && isSpace(s[end-1]) {
		end--
	}
	start := end
	for start > 0 && !isSpace(s[start-1]) {
		start--
	}
	return strings.ToLower(strings.Trim(s[start:end], ".,;:()[]\"'`"))
}

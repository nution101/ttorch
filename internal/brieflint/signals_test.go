package brieflint

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A stem without a `*` must match a whole word: "pr" in the prohibition bound list must not
// fire inside "problem", or every brief mentioning a problem would read as bounded.
func TestStemReWholeWordVsFamily(t *testing.T) {
	whole := stemRe("pr", "note")
	for _, s := range []string{"no PR until Friday", "a pr is fine", "note the sha"} {
		if !whole.MatchString(s) {
			t.Fatalf("%q: want a match", s)
		}
	}
	for _, s := range []string{"a problem appeared", "prepare the branch", "notebook"} {
		if whole.MatchString(s) {
			t.Fatalf("%q: must not match a whole-word stem", s)
		}
	}
	family := stemRe("verif*")
	for _, s := range []string{"verify it", "verified", "verification steps"} {
		if !family.MatchString(s) {
			t.Fatalf("%q: want a family match", s)
		}
	}
}

// Whitespace in a stem matches any run of whitespace, so a phrase that wraps across lines
// still matches.
func TestStemRePhraseAcrossLines(t *testing.T) {
	re := stemRe("report the sha")
	if !re.MatchString("please report\n   the sha when done") {
		t.Fatal("a wrapped phrase must still match")
	}
}

func TestSentencesKeepOffsets(t *testing.T) {
	text := "First thing. Second thing\nwrapped on.\n\n- A bullet."
	for _, s := range sentences(text) {
		if text[s.off:s.off+len(s.text)] != s.text {
			t.Fatalf("offset %d does not point at %q", s.off, s.text)
		}
	}
}

// The splitter is measured against prose shaped like a real brief, because the two false
// positives it was rewritten for were both reachable only from ordinary writing: a numbered
// list, an aside introduced with a colon, and "e.g.".
func TestSentencesSplitsRealProse(t *testing.T) {
	cases := map[string]struct {
		text string
		want []string
	}{
		"a numbered list keeps its marker with its text": {
			"1. There are 21 occurrences of the old helper.\n2. Verify the count yourself.",
			[]string{"There are 21 occurrences of the old helper", "Verify the count yourself"},
		},
		"a colon introduces, it does not terminate": {
			"Note: I grepped for the bare name.",
			[]string{"Note: I grepped for the bare name"},
		},
		"an abbreviation is not a sentence end": {
			"Add e.g. internal/new/thing.go as a new config file.",
			[]string{"Add e.g. internal/new/thing.go as a new config file"},
		},
		"a soft-wrapped line is one sentence": {
			"Fix the regression at internal/cli/cli.go:40 before you\ntouch anything else.",
			[]string{"Fix the regression at internal/cli/cli.go:40 before you touch anything else"},
		},
		"a cited path survives intact": {
			"See internal/cli/cli.go:40. Then stop.",
			[]string{"See internal/cli/cli.go:40", "Then stop"},
		},
		"an abbreviation before a new clause splits": {
			"Add the fix, etc. See internal/ghost/nonexistent.go:9999 for context.",
			[]string{"Add the fix, etc", "See internal/ghost/nonexistent.go:9999 for context"},
		},
		"a quote-terminated sentence splits": {
			"Write the note \"done.\" See internal/ghost/nonexistent.go:9999 for context.",
			[]string{"Write the note \"done", "See internal/ghost/nonexistent.go:9999 for context"},
		},
		"a parenthesis-terminated sentence splits": {
			"Add the helper (see round 3.) See internal/ghost/nonexistent.go:9999 too.",
			[]string{"Add the helper (see round 3", "See internal/ghost/nonexistent.go:9999 too"},
		},
		"an operand abbreviation before a digit does not split": {
			"There are approx. 21 occurrences left.",
			[]string{"There are approx. 21 occurrences left"},
		},
		"a decimal is not a boundary": {
			"The budget is 45.5 seconds in practice.",
			[]string{"The budget is 45.5 seconds in practice"},
		},
		"a version string is not a boundary": {
			"Upgrade to go1.26.5 before you start.",
			[]string{"Upgrade to go1.26.5 before you start"},
		},
		"an ellipsis mid-sentence is not a boundary": {
			"Wait for the gate... then land it.",
			[]string{"Wait for the gate... then land it"},
		},
		"a parenthetical is not a boundary": {
			"Fix it (the second one) before you push.",
			[]string{"Fix it (the second one) before you push"},
		},
		"a heading is its own span": {
			"## Rule 3\n\nThere are 21 occurrences.",
			[]string{"Rule 3", "There are 21 occurrences"},
		},
		"a bullet list is one span per bullet": {
			"- Do not push.\n- Do not open a PR.",
			[]string{"Do not push", "Do not open a PR"},
		},
	}
	for name, tc := range cases {
		var got []string
		for _, s := range sentences(tc.text) {
			got = append(got, s.trim())
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: want %d spans %q, got %d: %q", name, len(tc.want), tc.want, len(got), got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: span %d = %q, want %q", name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestHedgedAtRequiresAttachment(t *testing.T) {
	cases := map[string]struct {
		brief string
		want  bool
	}{
		"same sentence": {
			"There are 21 occurrences, give or take, so count them yourself.", true},
		"next sentence": {
			"There are 21 occurrences. That figure may be wrong.", true},
		"later in the same paragraph": {
			"There are 21 occurrences. Fix them. That figure may be wrong.", true},
		"across an aside, in the next list item": {
			"1. There are 21 occurrences of the old helper. Note: I grepped for the bare\n   name, not the qualified one.\n2. Verify the count yourself before you start.", true},
		"two blocks away": {
			"There are 21 occurrences.\n\nFix them all.\n\nThat figure may be wrong.", false},
		"the reviewer's boilerplate": {
			"There are 21 occurrences of the old helper. Fix them. Check the build afterwards and note the results.", false},
		"bare check nearby is not a hedge": {
			"There are 21 occurrences. Check the build afterwards.", false},
		"no hedge at all": {
			"There are 21 occurrences of the old helper. Fix them.", false},
	}
	for name, tc := range cases {
		sents := sentences(tc.brief)
		counts, _ := hardCounts(context.Background(), sents)
		if len(counts) != 1 {
			t.Errorf("%s: fixture must state exactly one hard count, got %d", name, len(counts))
			continue
		}
		if got := hedgedAt(hedgedBlocks(sents), sents[counts[0].sent].blk); got != tc.want {
			t.Errorf("%s: hedgedAt = %v, want %v for %q", name, got, tc.want, tc.brief)
		}
	}
}

// The attached half must say the figure is suspect or ask for it again. Mentioning results
// is not that, which is the distinction the boilerplate above turned on.
func TestDistrustAndRecountSignals(t *testing.T) {
	for _, s := range []string{
		"that count may be wrong", "my tally is indicative only", "roughly 21",
		"I counted them approximately", "do not trust it", "count them yourself",
		"verify the number before you start", "establish the true total",
		"report your own count",
	} {
		if !distrustSignal.MatchString(s) && !recountSignal.MatchString(s) {
			t.Fatalf("%q: want a hedge signal", s)
		}
	}
	for _, s := range []string{
		"check the build afterwards", "note the results", "list the findings",
		"verify the tests pass", "report back when done", "count on me",
	} {
		if distrustSignal.MatchString(s) || recountSignal.MatchString(s) {
			t.Fatalf("%q: must not read as a hedge", s)
		}
	}
}

// reportsTheNumber stays brief-scoped, and still needs the reporting verb and the word for
// the number in one sentence.
func TestReportsTheNumber(t *testing.T) {
	if !reportsTheNumber(parse("Report the real total you find.")) {
		t.Fatal("want the report instruction recognised")
	}
	if reportsTheNumber(parse("Report your progress.\nThe number of packages is large.")) {
		t.Fatal("signals in unrelated sentences must not count")
	}
}

// A create verb exempts only the path it governs, and only when it comes first.
func TestGovernedByCreate(t *testing.T) {
	cases := map[string]struct {
		sent string
		path string
		want bool
	}{
		"write, adjacent":      {"Write dev/report/EVIDENCE.md with the results", "dev/report/EVIDENCE.md", true},
		"add, a few words off": {"Add a test to internal/x_test.go", "internal/x_test.go", true},
		"verb after the path":  {"The regression at internal/nope/ghost.go:9999 came from adding the cache", "internal/nope/ghost.go:9999", false},
		"verb governs another": {"Add the cache layer, then fix the regression at internal/deep/thing.go:40", "internal/deep/thing.go:40", false},
		"no verb":              {"Fix the bug in internal/x/y.go", "internal/x/y.go", false},
	}
	for name, tc := range cases {
		at := indexOfPath(t, tc.sent, tc.path)
		if got := governedByCreate(tc.sent, createVerbs(tc.sent), at); got != tc.want {
			t.Fatalf("%s: governedByCreate = %v, want %v", name, got, tc.want)
		}
	}
}

func indexOfPath(t *testing.T, sent, path string) int {
	t.Helper()
	i := strings.Index(sent, path)
	if i < 0 {
		t.Fatalf("fixture %q does not contain %q", sent, path)
	}
	return i
}

func TestLineAt(t *testing.T) {
	b := parse("one\ntwo\nthree")
	for _, tc := range []struct {
		off, want int
	}{{0, 1}, {3, 1}, {4, 2}, {7, 2}, {8, 3}, {12, 3}} {
		if got := b.lineAt(tc.off); got != tc.want {
			t.Fatalf("lineAt(%d) = %d, want %d", tc.off, got, tc.want)
		}
	}
}

// A dot or colon inside a cited path must not end a sentence: a splitter that broke on
// every one of them would cut "internal/cli/cli.go:40" in half, both in the quoted text and
// in the path extraction that reads these spans.
func TestSentencesKeepCitedPathsIntact(t *testing.T) {
	got := sentences("Fix internal/cli/cli.go:40 today. Then docs/a.md:7 tomorrow")
	if len(got) != 2 {
		t.Fatalf("want 2 sentences, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].text, "internal/cli/cli.go:40") {
		t.Fatalf("first sentence lost the citation: %q", got[0].text)
	}
	if !strings.Contains(got[1].text, "docs/a.md:7") {
		t.Fatalf("second sentence lost the citation: %q", got[1].text)
	}
}

// The exemption is for a path the brief asks to CREATE. Three shapes that read as claims
// about an existing file had it anyway: passive voice, and a verb whose real object is
// something else with the path as its source.
func TestCreateExemptionNeedsAnActiveVerbGoverningThePath(t *testing.T) {
	cases := map[string]struct {
		sent string
		path string
		want bool
	}{
		"plain create":              {"Write dev/report/EVIDENCE.md: the rule set as implemented.", "dev/report/EVIDENCE.md", true},
		"create after e.g.":         {"Add e.g. internal/new/thing.go as a new config file.", "internal/new/thing.go", true},
		"create with destination":   {"Write your findings to dev/notes/out.md when you are done.", "dev/notes/out.md", true},
		"passive":                   {"The regression is generated by internal/x/phantom.go, per the report.", "internal/x/phantom.go", false},
		"path is the source":        {"Add the case from internal/x/ghost.go:9999 to the suite.", "internal/x/ghost.go:9999", false},
		"verb governs another noun": {"Author the fix using internal/x/a.go:4000 as the template.", "internal/x/a.go:4000", false},
		"retrospective":             {"The regression at internal/nope/ghost.go:9999 came from adding the cache.", "internal/nope/ghost.go:9999", false},
	}
	for name, tc := range cases {
		i := strings.Index(tc.sent, tc.path)
		if i < 0 {
			t.Fatalf("%s: fixture does not contain %q", name, tc.path)
		}
		if got := governedByCreate(tc.sent, createVerbs(tc.sent), i); got != tc.want {
			t.Errorf("%s: governedByCreate = %v, want %v for %q", name, got, tc.want, tc.sent)
		}
	}
}

// The splitter has now been wrong in both directions on this branch: it over-split ordinary
// prose, and the cure made it merge sentences that a create verb then reached across. Both
// directions are pinned here, together, because each earlier round was tested only against
// its own finding.
func TestSplitterHoldsBothDirections(t *testing.T) {
	// Merging: a create verb must not reach a citation in a following sentence.
	for _, tc := range []string{
		"Add the fix, etc. See internal/ghost/nonexistent.go:9999 for context.",
		"Write the note \"done.\" See internal/ghost/nonexistent.go:9999 for context.",
	} {
		if _, exempt := firstCitationExempt(t, tc); exempt {
			t.Errorf("a create verb reached across a sentence boundary in %q", tc)
		}
	}
	// Over-splitting: the round-2 false positives must stay fixed.
	if _, exempt := firstCitationExempt(t, "Add e.g. internal/new/thing.go as a new config file."); !exempt {
		t.Error("an abbreviation must not cut a create verb off the path it governs")
	}
	list := "1. There are 21 occurrences of the old helper. Note: I grepped for the bare\n   name.\n2. Verify the count yourself before you start."
	sents := sentences(list)
	counts, _ := hardCounts(context.Background(), sents)
	if len(counts) != 1 {
		t.Fatalf("want one hard count, got %d", len(counts))
	}
	if !hedgedAt(hedgedBlocks(sents), sents[counts[0].sent].blk) {
		t.Error("a hedge in the next list item must still reach the count")
	}
}

// firstCitationExempt reports the first cited path in text and whether it earned the create
// exemption, the way citations() asks.
func firstCitationExempt(t *testing.T, text string) (string, bool) {
	t.Helper()
	for _, s := range sentences(text) {
		for _, m := range candidateRe.FindAllStringIndex(s.text, -1) {
			if c, ok := citationOf(s.text[m[0]:m[1]]); ok && strings.Contains(c.path, "/") {
				return c.raw, governedByCreate(s.text, createVerbs(s.text), m[0])
			}
		}
	}
	t.Fatalf("no citation found in %q", text)
	return "", false
}

// A bound has to plausibly govern the ban it is credited to. Soft-wrap joining made a whole
// paragraph one sentence, so an unrelated later clause bounded a blanket prohibition; the
// cure must not reject the ordinary shapes where a bound genuinely trails or leads several
// bans.
func TestBoundMustGovernTheBanItBounds(t *testing.T) {
	cases := map[string]bool{
		"Do not push to main.":                                    true,
		"Do not push until the gate is green.":                    true,
		"Do not push anything at all until the gate is green.":    true,
		"Until the gate is green, do not push.":                   true,
		"Do not push, and do not merge, until the gate is green.": true,
		"Do not merge without my approval.":                       true,
		"Do not open a PR.":                                       true,
		"Do not push, do not merge and do not commit anything at all, and before you begin, read the notes.": false,
		"Do not push, do not merge and do not commit anything at all.":                                       false,
		"Never commit.":                  false,
		"Do not push. Before you begin.": false,
	}
	for text, want := range cases {
		got := bareBans(text) == 0
		if got != want {
			t.Errorf("bounded = %v, want %v for %q", got, want, text)
		}
	}
}

// A create verb must not reach across a full stop, including one the splitter deliberately
// merged because a lowercase word followed it.
func TestCreateVerbDoesNotReachAcrossAFullStop(t *testing.T) {
	cases := map[string]bool{
		"I already created the shim. e.g. internal/x/ghost.go is the one to change.": false,
		"Add e.g. internal/new/thing.go as a new config file.":                       true,
		"There are approx. 21 of them. Write dev/notes/out.md when you are done.":    true,
		"Write dev/report/EVIDENCE.md: the rule set as implemented.":                 true,
	}
	for text, want := range cases {
		if _, got := firstCitationExempt(t, text); got != want {
			t.Errorf("exempt = %v, want %v for %q", got, want, text)
		}
	}
}

// Every rule that asks a question about one sentence must ask it of the sentences the
// splitter merged, not of the merged span. These are the consumers that need the narrowing;
// hedgedAt is block-scoped and deliberately does not.
func TestWithinSentenceScopesRespectAMergedStop(t *testing.T) {
	t.Run("a bound does not cross a stop", func(t *testing.T) {
		const text = "do not push to main. do not commit anything at all."
		bare := bareBans(text)
		if bare != 1 {
			t.Errorf("want the second ban unbounded, got %d bare ban(s) in %q", bare, text)
		}
	})

	t.Run("a create verb does not cross a stop", func(t *testing.T) {
		if _, exempt := firstCitationExempt(t, "i already created the shim. internal/x/ghost.go is the one to change."); exempt {
			t.Error("a create verb reached a path in the next sentence")
		}
	})

	t.Run("a reporting verb does not pair across a stop", func(t *testing.T) {
		// "let me know" carries no number word and "the total is stale" carries no
		// reporting verb, so the pair exists only if the stop between them is ignored.
		if reportsTheNumber(&brief{raw: "let me know how it goes. the total is stale."}) {
			t.Error("a reporting verb paired with a number word in the next sentence")
		}
		if !reportsTheNumber(&brief{raw: "report the number you actually find."}) {
			t.Error("a reporting verb and its number in one sentence must still pair")
		}
	})

	t.Run("a hedge still reaches across a stop, by block scope", func(t *testing.T) {
		sents := sentences("there are 21 occurrences. that figure may be wrong.")
		counts, _ := hardCounts(context.Background(), sents)
		if len(counts) != 1 {
			t.Fatalf("want one hard count, got %d", len(counts))
		}
		if !hedgedAt(hedgedBlocks(sents), sents[counts[0].sent].blk) {
			t.Error("hedgedAt is block-scoped and must not be narrowed to the sentence")
		}
	})
}

// bareBans counts the prohibitions in text that no bound in reach qualifies.
func bareBans(text string) int {
	n := 0
	for _, s := range sentences(text) {
		hits, _ := prohibitionHits(context.Background(), s)
		for _, hit := range hits {
			if !hit.bounded {
				n++
			}
		}
	}
	return n
}

// unitAround is asked once per item a rule finds, and spans merge whenever the next
// sentence starts lowercase, so one brief can be a single span holding every sentence in
// it. Walking the stop list from index 0 answered the kth question in k steps. Measured
// end to end on one block of "on 21 uses. ", best of three, walk against search:
//
//	  131,072 B    10,922 sentences   0.123s   0.099s
//	  262,144 B    21,845             0.189s   0.137s
//	  524,288 B    43,690             0.455s   0.220s
//	1,048,000 B    87,333             1.430s   0.411s
//
// The walk's exponent over the last doubling is 1.65. The search's is 0.90.
//
// The budget here is an assertion about complexity, not a performance target: n questions
// by a walk is n*n/2 steps, which cannot finish inside it, and by a search is n*log2(n),
// which finishes in a fraction of it.
func TestUnitAroundIsFlatInTheNumberOfSentences(t *testing.T) {
	const n = 400_000
	var sb strings.Builder
	sb.Grow(2 * n)
	stops := make([]int, 0, n)
	for i := 0; i < n; i++ {
		sb.WriteString("a.")
		stops = append(stops, 2*i+1)
	}
	s := span{text: sb.String(), stops: stops}

	const budget = 2 * time.Second
	start := time.Now()
	for i := 0; i < n; i++ {
		if u := s.unitAround(2 * i); u[0] != 2*i || u[1] != 2*i+1 {
			t.Fatalf("sentence %d: got %v, want [%d %d]", i, u, 2*i, 2*i+1)
		}
	}
	if elapsed := time.Since(start); elapsed > budget {
		t.Fatalf("%d questions took %s; a walk from index 0 is %d steps and does not fit in %s",
			n, elapsed, n*n/2, budget)
	}
}

// The search has to answer exactly what the walk answered. The interesting positions are
// the boundaries: the byte of a stop, the byte after it, offset 0, and one past the end.
func TestUnitAroundMatchesALinearWalk(t *testing.T) {
	walk := func(s span, pos int) [2]int {
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
	texts := []string{
		"", "a", "a.", "a. ", "a. b", "a. b.", "one. two. three.",
		". ", ". . ", "a.. b", "no stop here", "trailing. ",
	}
	for _, text := range texts {
		s := span{text: text, stops: sentenceStops(text)}
		for pos := 0; pos <= len(text); pos++ {
			if got, want := s.unitAround(pos), walk(s, pos); got != want {
				t.Errorf("%q stops %v pos %d: search %v, walk %v", text, s.stops, pos, got, want)
			}
		}
	}
}

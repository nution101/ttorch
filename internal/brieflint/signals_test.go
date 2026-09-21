package brieflint

import (
	"strings"
	"testing"
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
		counts := hardCounts(sents)
		if len(counts) != 1 {
			t.Errorf("%s: fixture must state exactly one hard count, got %d", name, len(counts))
			continue
		}
		if got := hedgedAt(sents, counts[0]); got != tc.want {
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
		if got := governedByCreate(tc.sent, at); got != tc.want {
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

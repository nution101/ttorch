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
	text := "first thing. second thing\nthird thing"
	got := sentences(text)
	if len(got) != 3 {
		t.Fatalf("want 3 sentences, got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if text[s.off:s.off+len(s.text)] != s.text {
			t.Fatalf("offset %d does not point at %q", s.off, s.text)
		}
	}
}

func TestSpanTrimStripsMarkdown(t *testing.T) {
	s := span{text: "  - **do not push**   to\n   main "}
	if got, want := s.trim(), "**do not push** to main"; got != want {
		t.Fatalf("trim() = %q, want %q", got, want)
	}
}

// hedged needs BOTH halves: an admission that the figure may be wrong, and an instruction
// to report the number found. Either alone is not a hedge.
func TestHedgedNeedsBothSignals(t *testing.T) {
	both := "The count may be wrong. Report the real total you find."
	if !hedged(parse(both)) {
		t.Fatalf("%q: want hedged", both)
	}
	for _, s := range []string{
		"The count may be wrong.",
		"Report your progress when you are done.",
		"Fix the 21 occurrences.",
	} {
		if hedged(parse(s)) {
			t.Fatalf("%q: must not count as hedged", s)
		}
	}
}

// The reporting verb and the word for the number must share a sentence: "report" in one
// sentence and "number" in an unrelated one is not an instruction to report the number.
func TestHedgedRequiresSameSentence(t *testing.T) {
	split := "Verify the count. Report your progress.\nThe number of packages is large."
	if hedged(parse(split)) {
		t.Fatal("signals in unrelated sentences must not satisfy the hedge")
	}
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

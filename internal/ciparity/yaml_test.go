package ciparity

import "testing"

func topMapping(t *testing.T, data string) *mapping {
	t.Helper()
	m, ok := parseYAML(data).(*mapping)
	if !ok {
		t.Fatalf("expected a top-level mapping, got %T", parseYAML(data))
	}
	return m
}

func TestParseYAML_ScalarsCommentsQuotes(t *testing.T) {
	const doc = `name: CI        # trailing comment
# a full-line comment
quoted: "hello world"
single: 'it''s here'
plain: go test ./...
flow: [a, b, c]
`
	m := topMapping(t, doc)
	cases := map[string]string{
		"name":   "CI",
		"quoted": "hello world",
		"single": "it's here",
		"plain":  "go test ./...",
		"flow":   "[a, b, c]", // flow collections are preserved verbatim as opaque scalars
	}
	for k, want := range cases {
		v, ok := m.get(k)
		if !ok {
			t.Fatalf("missing key %q", k)
		}
		if s, _ := v.(string); s != want {
			t.Fatalf("%s = %q, want %q", k, s, want)
		}
	}
}

func TestParseYAML_HashInsideValueIsNotAComment(t *testing.T) {
	// A '#' not preceded by whitespace is part of the value, not a comment.
	m := topMapping(t, "run: echo c#1\n")
	if v, _ := m.get("run"); v != "echo c#1" {
		t.Fatalf("run = %q, want %q", v, "echo c#1")
	}
}

func TestBlockScalar_LiteralClip(t *testing.T) {
	const doc = `script: |
  line one
  line two
next: done
`
	m := topMapping(t, doc)
	if v, _ := m.get("script"); v != "line one\nline two\n" {
		t.Fatalf("clip literal = %q", v)
	}
	if v, _ := m.get("next"); v != "done" {
		t.Fatalf("sibling after block = %q, want done", v)
	}
}

func TestBlockScalar_Strip(t *testing.T) {
	const doc = `script: |-
  only line
after: x
`
	m := topMapping(t, doc)
	if v, _ := m.get("script"); v != "only line" {
		t.Fatalf("strip literal = %q, want %q", v, "only line")
	}
}

func TestBlockScalar_PreservesInnerIndentAndBlanks(t *testing.T) {
	const doc = `script: |
  if true; then
    echo nested

  fi
end: 1
`
	m := topMapping(t, doc)
	want := "if true; then\n  echo nested\n\nfi\n"
	if v, _ := m.get("script"); v != want {
		t.Fatalf("nested-indent literal = %q, want %q", v, want)
	}
}

func TestBlockScalar_Folded(t *testing.T) {
	const doc = `text: >
  one two
  three
end: 1
`
	m := topMapping(t, doc)
	if v, _ := m.get("text"); v != "one two three\n" {
		t.Fatalf("folded = %q, want %q", v, "one two three\n")
	}
}

func TestKeyColon_IgnoresColonInValueAndQuotes(t *testing.T) {
	m := topMapping(t, "image: node:18\n")
	if v, _ := m.get("image"); v != "node:18" {
		t.Fatalf("image = %q, want node:18", v)
	}
}

func TestParseYAML_NestedSequenceOfMappings(t *testing.T) {
	const doc = `jobs:
  build:
    steps:
      - name: a
        run: echo a
      - uses: actions/checkout@v4
`
	m := topMapping(t, doc)
	jobs, _ := m.get("jobs")
	jm, ok := jobs.(*mapping)
	if !ok {
		t.Fatalf("jobs is %T, want mapping", jobs)
	}
	build, _ := jm.get("build")
	bm := build.(*mapping)
	stepsNode, _ := bm.get("steps")
	steps, ok := stepsNode.([]node)
	if !ok || len(steps) != 2 {
		t.Fatalf("steps = %#v, want a 2-element sequence", stepsNode)
	}
	first := steps[0].(*mapping)
	if v, _ := first.get("name"); v != "a" {
		t.Fatalf("first step name = %v, want a", v)
	}
	if v, _ := first.get("run"); v != "echo a" {
		t.Fatalf("first step run = %v, want echo a", v)
	}
	second := steps[1].(*mapping)
	if v, _ := second.get("uses"); v != "actions/checkout@v4" {
		t.Fatalf("second step uses = %v", v)
	}
}

func TestBlockIndicator(t *testing.T) {
	for _, in := range []string{"|", ">", "|-", "|+", "|2", ">-", "| # comment"} {
		if _, ok := blockIndicator(in); !ok {
			t.Fatalf("blockIndicator(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"value", "| inline text", "x|", ""} {
		if _, ok := blockIndicator(in); ok {
			t.Fatalf("blockIndicator(%q) = true, want false", in)
		}
	}
}

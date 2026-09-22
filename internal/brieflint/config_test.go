package brieflint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadWith(t *testing.T, body string) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(dir)
}

func TestLoadConfigNoFile(t *testing.T) {
	c := LoadConfig(t.TempDir())
	if c.Source != "" || c.Standards != nil || c.StandardsEmpty || len(c.Disabled) != 0 {
		t.Fatalf("a project with no %s must configure nothing: %+v", configFile, c)
	}
}

func TestLoadConfigNoRepo(t *testing.T) {
	if c := LoadConfig(""); c.Source != "" || c.Standards != nil {
		t.Fatalf("no repository means no configuration: %+v", c)
	}
}

func TestLoadConfigStandardsPointers(t *testing.T) {
	c := loadWith(t, "- brief-standards: docs/standards/, CONTRIBUTING.md\n")
	if len(c.Standards) != 2 || c.Standards[0] != "docs/standards/" || c.Standards[1] != "CONTRIBUTING.md" {
		t.Fatalf("want both pointers, got %+v", c.Standards)
	}
	if c.StandardsEmpty {
		t.Fatal("a declaration with values is not empty")
	}
	if c.Source == "" {
		t.Fatal("the report needs the file the configuration came from")
	}
}

func TestLoadConfigStandardsRepeatedKey(t *testing.T) {
	c := loadWith(t, "- brief-standards: a/x.md\ntext\n- brief-standards: b/y.md\n")
	if len(c.Standards) != 2 {
		t.Fatalf("a repeated key accumulates: %+v", c.Standards)
	}
}

// A key with no value is a broken declaration, not "no declaration" and not a pass.
func TestLoadConfigStandardsEmptyValue(t *testing.T) {
	for _, body := range []string{"- brief-standards:\n", "- brief-standards:   \n", "- brief-standards: ,,\n"} {
		c := loadWith(t, body)
		if !c.StandardsEmpty || len(c.Standards) != 0 {
			t.Fatalf("%q: want an empty declaration, got %+v", body, c)
		}
	}
}

// A project that declared the key twice, once with a value, has a usable value.
func TestLoadConfigStandardsEmptyThenValued(t *testing.T) {
	c := loadWith(t, "- brief-standards:\n- brief-standards: docs/x.md\n")
	if c.StandardsEmpty || len(c.Standards) != 1 {
		t.Fatalf("want the valued declaration to win: %+v", c)
	}
}

func TestLoadConfigDisable(t *testing.T) {
	c := loadWith(t, "- brief-lint-disable: hard-counts, PROHIBITION, not-a-rule\n")
	if !c.Disabled[RuleHardCounts] || !c.Disabled[RuleProhibition] {
		t.Fatalf("want both named rules disabled (case-insensitively): %+v", c.Disabled)
	}
	if len(c.UnknownDisabled) != 1 || c.UnknownDisabled[0] != "not-a-rule" {
		t.Fatalf("an unknown rule must be surfaced, not ignored: %+v", c.UnknownDisabled)
	}
	if c.Disabled[RuleTargetBranch] {
		t.Fatal("only the named rules are disabled")
	}
}

// The configuration the reader sees must say what the rules were judged against.
func TestConfigDescribe(t *testing.T) {
	cases := map[string]Config{
		"brief-standards: none declared":            {},
		"brief-standards: declared but EMPTY":       {StandardsEmpty: true},
		`brief-standards: 1 declared ("docs/x.md")`: {Standards: []string{"docs/x.md"}},
		"disabled: target-branch, hard-counts":      {Disabled: map[RuleID]bool{RuleHardCounts: true, RuleTargetBranch: true}},
	}
	for want, c := range cases {
		if got := c.describe(); !strings.Contains(got, want) {
			t.Fatalf("describe() = %q, want it to mention %q", got, want)
		}
	}
}

// An AGENTS.md past the cap is refused, not truncated and not read. It belongs to the
// repository under review, so its size is chosen by whoever committed it, and a 400 MiB one
// took a single run to 859 MB of resident memory.
//
// What makes this a correctness question rather than a memory one: the zero Config is a
// valid "this project declares nothing" state, which is pass-shaped. A file the linter
// refused to read must never resolve to it.
func TestLoadConfigRefusesAnOversizeFile(t *testing.T) {
	declared := "- brief-standards: docs/STANDARDS.md\n- brief-lint-disable: prohibition\n"
	body := declared + strings.Repeat("filler line that declares nothing\n", (maxConfigBytes/34)+64)
	if len(body) <= maxConfigBytes {
		t.Fatalf("the fixture must exceed the cap: %d bytes vs %d", len(body), maxConfigBytes)
	}
	c := loadWith(t, body)
	if !c.Oversize {
		t.Fatalf("a %d byte %s must be refused, got %+v", len(body), configFile, c)
	}
	if c.Size != int64(len(body)) {
		t.Errorf("the report needs the file's real size: got %d, want %d", c.Size, len(body))
	}
	// The declarations are on the FIRST two lines, so a truncating read would have found
	// them. Neither may be applied.
	if c.Standards != nil || c.StandardsEmpty {
		t.Errorf("a refused file declares no pointer: %+v", c)
	}
	if len(c.Disabled) != 0 || len(c.UnknownDisabled) != 0 {
		t.Errorf("a refused file disables nothing: %+v", c)
	}
	if !strings.Contains(c.describe(), "not read") {
		t.Errorf("the report header must say the file was not read, got %q", c.describe())
	}
}

// A file exactly at the cap is read. The cap is a limit, not a margin.
func TestLoadConfigReadsAFileAtTheCap(t *testing.T) {
	declared := "- brief-standards: docs/STANDARDS.md\n"
	body := declared + strings.Repeat("x", maxConfigBytes-len(declared))
	if len(body) != maxConfigBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(body), maxConfigBytes)
	}
	c := loadWith(t, body)
	if c.Oversize {
		t.Fatalf("a file exactly at the cap must be read, got %+v", c)
	}
	if len(c.Standards) != 1 || c.Standards[0] != "docs/STANDARDS.md" {
		t.Fatalf("want the declared pointer, got %+v", c.Standards)
	}
}

// The pointer list comes out of the repository's AGENTS.md, so the project chooses its
// length. The report header renders it on every run, a clean pass included, and 16,000
// pointers came to 896 KB of header.
func TestConfigDescribeCapsThePointerList(t *testing.T) {
	var c Config
	for i := 0; i < 16_000; i++ {
		c.Standards = append(c.Standards, fmt.Sprintf("docs/standard-%d.md", i))
	}
	got := c.describe()
	if len(got) > 1_000 {
		t.Fatalf("the header is %d bytes for %d pointers; it must be bounded", len(got), len(c.Standards))
	}
	if !strings.Contains(got, "16000 declared") {
		t.Errorf("the header must still say how many were declared, got %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("and %d more", 16_000-maxShownPointers)) {
		t.Errorf("the header must count what it did not print, got %q", got)
	}
	if strings.Contains(got, "docs/standard-15999.md") {
		t.Errorf("the header must not render the whole list, got %d bytes", len(got))
	}
}

// A single pointer is untrusted text too: a project can declare one a megabyte long.
func TestConfigDescribeClipsALongPointer(t *testing.T) {
	c := Config{Standards: []string{strings.Repeat("a", 50_000)}}
	if got := c.describe(); len(got) > 1_000 {
		t.Fatalf("a %d byte pointer rendered %d bytes of header", 50_000, len(got))
	}
}

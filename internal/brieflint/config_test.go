package brieflint

import (
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
		"brief-standards: none declared":       {},
		"brief-standards: declared but EMPTY":  {StandardsEmpty: true},
		`brief-standards: "docs/x.md"`:         {Standards: []string{"docs/x.md"}},
		"disabled: target-branch, hard-counts": {Disabled: map[RuleID]bool{RuleHardCounts: true, RuleTargetBranch: true}},
	}
	for want, c := range cases {
		if got := c.describe(); !strings.Contains(got, want) {
			t.Fatalf("describe() = %q, want it to mention %q", got, want)
		}
	}
}

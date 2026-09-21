package brieflint

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-project configuration lives in the repository's AGENTS.md, as `- key: value` lines,
// beside the delivery-mode and auto-mint-max-age policy lines the gates already read. The
// lines are honored anywhere in the file (not only inside the ttorch-managed block) so a
// developer can place them where `ttorch init` will not regenerate over them. ttorch only
// ever READS them; they are human-authored policy.
const (
	// standardsKey declares the pointer a brief must cite to satisfy RuleStandards — the
	// path, document or phrase where this project keeps the standards it expects a worker
	// to follow. Repeat the key, or comma-separate, to accept any of several pointers.
	//
	// This exists because the prototype hardcoded a grep for ONE project's standards
	// corpus, so a brief for any other repository failed the rule no matter how correctly
	// it stated its standards. The expectation must belong to the project.
	standardsKey = "- brief-standards:"
	// disableKey lists rule ids this project does not want enforced. A legitimately
	// unusual brief sometimes needs one off; the override is always echoed in the report
	// so it is visible rather than silent.
	disableKey = "- brief-lint-disable:"
)

// configFile is the file the keys are read from, relative to the repository root.
const configFile = "AGENTS.md"

// Config is a project's brief-lint configuration.
type Config struct {
	// Standards holds the pointers a brief may cite to satisfy RuleStandards. Nil means the
	// project declares none, which is a supported state: RuleStandards then falls back to
	// requiring that the brief point at standards SOMEHOW (see checkStandards) instead of
	// demanding a fixed path no project agreed to.
	Standards []string
	// StandardsEmpty records a declared-but-valueless `- brief-standards:` line. It is NOT
	// the same as declaring none: a project that names the key has opted in, so an empty
	// value is a broken declaration and RuleStandards reports it as unevaluable. An empty
	// value must never be a way to satisfy the rule.
	StandardsEmpty bool
	// Disabled is the set of rules this project turned off.
	Disabled map[RuleID]bool
	// UnknownDisabled holds disable-list entries that name no rule — a configuration typo,
	// surfaced rather than ignored, because the rule the author meant to disable is still
	// running.
	UnknownDisabled []string
	// Source is the file the configuration was read from, for the report. Empty when no
	// configuration file was found.
	Source string
}

// LoadConfig reads the brief-lint configuration for the repository at dir. A missing or
// unreadable AGENTS.md is not an error: it yields the zero Config, which is a valid
// "project declares nothing" state. dir may be empty, meaning no repository is available.
func LoadConfig(dir string) Config {
	var c Config
	if dir == "" {
		return c
	}
	path := filepath.Join(dir, configFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	c.Source = path
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, standardsKey); ok {
			vals := splitList(rest)
			if len(vals) == 0 {
				c.StandardsEmpty = true
				continue
			}
			c.Standards = append(c.Standards, vals...)
		}
		if rest, ok := strings.CutPrefix(line, disableKey); ok {
			for _, v := range splitList(rest) {
				id := RuleID(strings.ToLower(v))
				if !ValidRule(id) {
					c.UnknownDisabled = append(c.UnknownDisabled, v)
					continue
				}
				if c.Disabled == nil {
					c.Disabled = map[RuleID]bool{}
				}
				c.Disabled[id] = true
			}
		}
	}
	// A project that declared the standards key twice — once with a value, once empty —
	// has a usable value, so the empty line is not a broken declaration.
	if len(c.Standards) > 0 {
		c.StandardsEmpty = false
	}
	return c
}

// quoteAll renders values read out of AGENTS.md for the terminal. That file is part of the
// repository under review and a worker can commit it, so its contents are untrusted text on
// a path a human reads: quoting keeps a value carrying control bytes or newlines from
// writing its own line in the report. Brief-derived quotes already go through %q; this is
// the config side of the same rule.
func quoteAll(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = strconv.Quote(v)
	}
	return out
}

// splitList parses a comma-separated value, dropping blank entries so `a,,b` and a trailing
// comma are not mistaken for empty pointers.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// describe renders the configuration for the report header, so the reader can see which
// expectation a rule was judged against.
func (c Config) describe() string {
	var parts []string
	switch {
	case len(c.Standards) > 0:
		parts = append(parts, "brief-standards: "+strings.Join(quoteAll(c.Standards), ", "))
	case c.StandardsEmpty:
		parts = append(parts, "brief-standards: declared but EMPTY")
	default:
		parts = append(parts, "brief-standards: none declared")
	}
	if len(c.Disabled) > 0 {
		var ids []string
		for _, id := range Rules() { // stable order
			if c.Disabled[id] {
				ids = append(ids, string(id))
			}
		}
		parts = append(parts, "disabled: "+strings.Join(ids, ", "))
	}
	return strings.Join(parts, "; ")
}

// disableSource names where a disable came from, for the visible override note.
func (c Config) disableSource() string {
	if c.Source == "" {
		return disableKey
	}
	return c.Source + " " + disableKey
}

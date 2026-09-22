package brieflint

import (
	"fmt"
	"io"
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

// maxConfigBytes caps the configuration file one run will read. AGENTS.md belongs to the
// repository under review and anyone who can commit to it chooses its size, so it is
// untrusted input like the brief and like a cited blob, and both of those are capped
// (MaxBriefBytes, maxBlobBytes). This read was not: the whole file was pulled into memory
// and split into lines, and a 400 MiB AGENTS.md took one `ttorch brief-lint` to 859 MB of
// resident memory, measured with /usr/bin/time -l.
//
// A real AGENTS.md is a few KB. The cap matches MaxBriefBytes because both bound a
// human-written file in the same repository, and one number is easier to remember than two.
const maxConfigBytes = MaxBriefBytes

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
	// Oversize records a configuration file past maxConfigBytes. Nothing in it was read, so
	// what the project declares is unknown, and that is NOT the same as declaring nothing:
	// the zero Config is a pass-shaped state, and a file the linter refused to read must not
	// resolve to one. RuleStandards reports unevaluable instead of falling back to its
	// generic signal, no disable is applied (a rule turned off by a file nobody read is a
	// rule silently skipped), and Lint reports the refusal against RuleConfig.
	Oversize bool
	// Size is the configuration file's size on disk when Oversize is set, for the report.
	Size int64
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
	// Read one byte past the cap, the way the CLI reads a brief: the extra byte is what
	// distinguishes a file exactly at the cap from one over it, without trusting a stat that
	// a special file can lie about.
	b, err := readCapped(path, maxConfigBytes+1)
	if err != nil {
		return c
	}
	c.Source = path
	if len(b) > maxConfigBytes {
		// Refused, not truncated. Parsing the first megabyte would report a configuration
		// the project did not write, and the disable list is exactly the part a truncated
		// read would get wrong in the permissive direction.
		c.Oversize = true
		c.Size = int64(len(b))
		if fi, statErr := os.Stat(path); statErr == nil {
			c.Size = fi.Size()
		}
		return c
	}
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

// maxShownPointers caps how many declared pointers one report line renders. The list comes
// out of the repository's AGENTS.md, so its length is the project's choice and not the
// report's: 16,000 pointers rendered 896 KB of header on every run, including a clean pass.
// Rules 1 and 2 bound what they print for the same reason.
const maxShownPointers = 8

// showPointers renders a declared pointer list for the terminal: quoted, each value clipped
// to a token's length, and the list itself cut to maxShownPointers with the rest counted.
// Both places that print the list use it, the report header and rule 5's finding.
func showPointers(vals []string) string {
	shown := vals
	if len(shown) > maxShownPointers {
		shown = shown[:maxShownPointers]
	}
	clipped := make([]string, len(shown))
	for i, v := range shown {
		clipped[i] = clipToken(v)
	}
	s := strings.Join(quoteAll(clipped), ", ")
	if n := len(vals) - len(shown); n > 0 {
		s += fmt.Sprintf(", and %d more", n)
	}
	return s
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

// readCapped reads at most limit bytes of a file. The limit is the point of it: this is a
// file from the repository under review, so its size is not ours to choose.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// describe renders the configuration for the report header, so the reader can see which
// expectation a rule was judged against.
func (c Config) describe() string {
	if c.Oversize {
		return fmt.Sprintf("not read: %d bytes, over the %d byte cap", c.Size, maxConfigBytes)
	}
	var parts []string
	switch {
	case len(c.Standards) > 0:
		parts = append(parts, fmt.Sprintf("brief-standards: %d declared (%s)", len(c.Standards), showPointers(c.Standards)))
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

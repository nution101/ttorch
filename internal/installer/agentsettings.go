package installer

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/harness"
)

// settingKeys are the frontmatter keys of a managed agent that belong to the user, not to
// ttorch: which model the agent runs on and how hard it thinks. A file that differs from the
// shipped version only in these keys is upgraded in place with the keys the user changed
// carried over (see manifest.ReconcileSettings), instead of being kept stale beside a
// .ttorch-new.
//
// This list must stay exactly these two keys, and must not grow into a general "preserve
// frontmatter" mechanism. The installed agent files define what the trust-gate reviewers do.
// The .ttorch-new safety exists so that a local edit to one of them never silently survives
// an update: the user has to look at the new version. Carrying tools:, permissionMode:, or
// any key that bears prompt text (description:, the body) would let such an edit persist
// across every future update of a reviewer. model and effort choose which model runs the
// shipped instructions and at what effort; they cannot change the instructions, and each
// carried value is validated (validSetting) so only a plain `model: <id>` or
// `effort: <level>` line can ever be carried.
var settingKeys = []string{"model", "effort"}

// agentSettings implements manifest.Settings for the files installed under dir, the user's
// agents directory. Other managed files (skills, commands, hooks) carry no settings.
type agentSettings struct{ dir string }

func (a agentSettings) covers(dest string) bool {
	rel, err := filepath.Rel(a.dir, dest)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// Normalize drops the setting lines from b's frontmatter. A file with no frontmatter has no
// settings and normalizes to itself.
func (a agentSettings) Normalize(dest string, b []byte) ([]byte, bool) {
	if !a.covers(dest) {
		return nil, false
	}
	lines, end := frontmatter(b)
	if end < 0 {
		return b, true
	}
	var out strings.Builder
	for i, l := range lines {
		if i > 0 && i < end && settingKey(l) != "" {
			continue
		}
		out.WriteString(l)
	}
	return []byte(out.String()), true
}

// Values returns the validated settings in b's frontmatter.
func (a agentSettings) Values(dest string, b []byte) (map[string]string, error) {
	lines, end := frontmatter(b)
	vals := map[string]string{}
	if end < 0 {
		return vals, nil
	}
	found, err := settingLines(lines, end)
	if err != nil {
		return nil, err
	}
	for k, l := range found {
		v := unquote(strings.TrimSpace(strings.TrimPrefix(l, k+":")))
		if err := validSetting(k, v); err != nil {
			return nil, err
		}
		vals[k] = v
	}
	return vals, nil
}

// Apply rewrites onto's setting lines to vals. Each line is written in the canonical
// `key: value` form, replacing the existing line in place or, when onto lacks the key,
// inserted just before the closing delimiter; a setting absent from vals is removed.
// Nothing outside those lines changes.
func (a agentSettings) Apply(dest string, vals map[string]string, onto []byte) ([]byte, error) {
	for k, v := range vals {
		if err := validSetting(k, v); err != nil {
			return nil, err
		}
	}
	lines, end := frontmatter(onto)
	if end < 0 {
		if len(vals) == 0 {
			return onto, nil
		}
		return nil, fmt.Errorf("no frontmatter to carry model/effort into, so they were not carried")
	}
	if _, err := settingLines(lines, end); err != nil {
		return nil, err
	}
	var out strings.Builder
	done := map[string]bool{}
	for i, l := range lines {
		if i == end {
			for _, k := range settingKeys {
				if v, ok := vals[k]; ok && !done[k] {
					out.WriteString(k + ": " + v + "\n")
				}
			}
		}
		if k := settingKey(l); i > 0 && i < end && k != "" {
			if v, ok := vals[k]; ok {
				out.WriteString(k + ": " + v + "\n")
				done[k] = true
			}
			continue
		}
		out.WriteString(l)
	}
	return []byte(out.String()), nil
}

// frontmatter splits b into lines (each keeping its "\n") and returns the index of the
// closing "---" delimiter, or -1 when b does not open with a terminated YAML frontmatter.
func frontmatter(b []byte) ([]string, int) {
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) == 0 || lines[0] != "---\n" {
		return lines, -1
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---\n" || lines[i] == "---" {
			return lines, i
		}
	}
	return lines, -1
}

// settingKey returns the setting a frontmatter line sets, or "" for any other line. Only a
// top-level key counts: an indented `model:` belongs to some nested mapping or block scalar.
func settingKey(line string) string {
	for _, k := range settingKeys {
		rest, ok := strings.CutPrefix(line, k+":")
		if ok && (rest == "" || strings.ContainsAny(rest[:1], " \t\r\n")) {
			return k
		}
	}
	return ""
}

// settingLines maps each setting present in the frontmatter to its line, refusing a key set
// twice since it is ambiguous which value was meant.
func settingLines(lines []string, end int) (map[string]string, error) {
	found := map[string]string{}
	for i := 1; i < end; i++ {
		k := settingKey(lines[i])
		if k == "" {
			continue
		}
		if _, dup := found[k]; dup {
			return nil, fmt.Errorf("%s is set more than once in the frontmatter, so it was not carried", k)
		}
		found[k] = lines[i]
	}
	return found, nil
}

// validSetting applies the same checks as `ttorch spawn --model` / `--effort`.
func validSetting(key, v string) error {
	switch key {
	case "model":
		if !harness.ValidModel(v) {
			return fmt.Errorf("model %q not carried: want a bare alias (%s) or a full model id, brackets refused",
				v, strings.Join(harness.ModelAliases, "|"))
		}
	case "effort":
		if !harness.ValidEffort(v) {
			return fmt.Errorf("effort %q not carried: want one of %s", v, strings.Join(harness.EffortLevels, "|"))
		}
	default:
		return fmt.Errorf("%s is not a carried setting", key)
	}
	return nil
}

// unquote strips one pair of matching YAML quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

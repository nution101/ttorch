// Global settings/hook merge. This is the generic mechanism by which ttorch keeps
// a small, ttorch-owned block of keys present in the shared global Claude Code
// settings file (~/.claude/settings.json) without ever clobbering keys the
// developer set themselves.
//
// Unlike file-managed content (the manifest engine, which parks conflicts as
// .ttorch-new), settings.json is a single live file many tools write, so ttorch
// merges in place and tracks ownership at *leaf* granularity in a side ledger:
// every leaf ttorch writes is recorded with the exact value written. A leaf is
// ttorch's to upgrade or remove only while its live value still equals what ttorch
// last wrote; the moment a developer changes it, ttorch leaves it alone forever.
//
// The block itself (managedGlobalSettings) is deliberately generic and minimal —
// the richer hook set flows through this same mechanism and is authored separately.
package installer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nution101/ttorch/internal/paths"
)

// noGlobalHooksEnv opts out of global settings/hook management entirely, in line
// with the other TTORCH_NO_* opt-outs (autotrust, supervisor, autoinit). It is an
// install-time switch: set it and ttorch neither merges nor removes any settings.
const noGlobalHooksEnv = "TTORCH_NO_GLOBAL_HOOKS"

// promptReminderScript is the shipped UserPromptSubmit hook (see content/hooks).
// The manifest engine installs it into p.ClaudeHooks(); the global settings entry
// below references it by absolute path. The script itself honors a separate
// runtime opt-out (TTORCH_NO_PROMPT_REMINDERS) so a developer can silence it for a
// session without uninstalling.
const promptReminderScript = "prompt-reminders.sh"

// managedGlobalSettings is the ttorch-owned block merged into the global settings
// file. The merge is generic: it installs whatever appears here, at leaf
// granularity, leaving every other key untouched. Arrays are treated as single
// leaves (owned and upgraded whole), so the UserPromptSubmit entry below is
// claimed wholesale — and if a developer already has their own UserPromptSubmit
// array, ttorch defers to it and installs nothing there.
//
// The hook command invokes the interpreter explicitly (`sh '<path>'`) rather than
// relying on an executable bit, because the manifest engine writes content files
// 0644. The path is absolute and resolved at apply time, so it is stable per
// machine and the merge stays idempotent.
func managedGlobalSettings(p paths.Paths) map[string]any {
	hookCommand := "sh " + shellSingleQuote(filepath.Join(p.ClaudeHooks(), promptReminderScript))
	return map[string]any{
		// Keep AI co-author trailers out of every Claude session globally (the
		// repo's no-AI-authorship convention), not only inside worker worktrees.
		"includeCoAuthoredBy": false,
		// A safe, advisory UserPromptSubmit hook: it surfaces cautions for
		// destructive command patterns and generic engineering reminders. It never
		// blocks (always exits 0). UserPromptSubmit takes no matcher.
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": hookCommand,
						},
					},
				},
			},
		},
	}
}

// settingsLedgerEntry records one leaf ttorch wrote: its path within settings.json
// and the canonical-JSON value written. Ownership is decided by comparing the live
// value against Value — equal means ttorch still owns it; different means the
// developer changed it and ttorch must not touch it.
type settingsLedgerEntry struct {
	Path  []string        `json:"path"`
	Value json.RawMessage `json:"value"`
}

type settingsLedger struct {
	Entries []settingsLedgerEntry `json:"entries"`
}

func (l settingsLedger) lookup(path []string) (json.RawMessage, bool) {
	want := joinPath(path)
	for _, e := range l.Entries {
		if joinPath(e.Path) == want {
			return e.Value, true
		}
	}
	return nil, false
}

func loadSettingsLedger(path string) settingsLedger {
	var l settingsLedger
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	// Normalize recorded values to compact canonical JSON. The ledger file is saved
	// with MarshalIndent, which re-indents structured (object/array) values; without
	// this, a nested leaf's recorded bytes would never byte-equal the compact
	// canonJSON(live) used in every ownership comparison, so ttorch would fail to
	// recognize — and thus to upgrade or remove — its own structured entries.
	for i := range l.Entries {
		l.Entries[i].Value = canonRaw(l.Entries[i].Value)
	}
	return l
}

func saveSettingsLedger(path string, l settingsLedger) error {
	if len(l.Entries) == 0 {
		// Own nothing: don't leave an empty ledger lying around.
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

// applyGlobalSettings merges the ttorch-managed block into the global settings file,
// clobber-safe and idempotent, and returns a human-readable note. Real I/O errors
// are returned; a malformed or non-object settings.json is never clobbered — it is
// left untouched with an explanatory note.
func applyGlobalSettings(p paths.Paths) (string, error) {
	if os.Getenv(noGlobalHooksEnv) != "" {
		return "skipped global hooks (" + noGlobalHooksEnv + " set)", nil
	}

	file := p.GlobalSettingsFile()
	original, err := os.ReadFile(file)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return "", err
	}
	existedNonEmpty := !missing && len(bytes.TrimSpace(original)) > 0

	settings := map[string]any{}
	if existedNonEmpty {
		if err := json.Unmarshal(original, &settings); err != nil {
			// Covers both invalid JSON and a valid-but-non-object top level.
			return "global settings.json is not a JSON object; left it untouched", nil
		}
		if settings == nil { // a literal `null` unmarshals to a nil map
			settings = map[string]any{}
		}
	}

	old := loadSettingsLedger(p.GlobalSettingsLedger())
	next := settingsLedger{}
	managed := managedGlobalSettings(p)

	changed := mergeManaged(settings, managed, nil, old, &next)

	// Retire leaves ttorch owned before but no longer manages, when the live value
	// is still exactly what ttorch wrote (the developer never changed it).
	if retireUnmanaged(settings, old, managedLeafPaths(managed, nil)) {
		changed = true
	}
	// Drop any objects emptied by retirement.
	if pruneEmpties(settings, parentPaths(old.Entries)) {
		changed = true
	}

	note := "global settings already up to date"
	if changed {
		// Back up the developer's original exactly once, before our first write.
		if existedNonEmpty {
			if _, err := os.Stat(p.GlobalSettingsBackup()); os.IsNotExist(err) {
				if err := atomicWrite(p.GlobalSettingsBackup(), original); err != nil {
					return "", err
				}
			}
		}
		b, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return "", err
		}
		if err := atomicWrite(file, append(b, '\n')); err != nil {
			return "", err
		}
		note = "merged ttorch-managed keys into " + file
	}

	if err := saveSettingsLedger(p.GlobalSettingsLedger(), next); err != nil {
		return "", err
	}
	return note, nil
}

// removeGlobalSettings deletes only the leaves ttorch still owns (live value equals
// the ledger), prunes objects it empties, and drops the ledger. Developer keys and
// any leaf the developer changed are left in place. The backup snapshot is kept so
// the developer can always recover their pre-ttorch settings.
func removeGlobalSettings(p paths.Paths) (string, error) {
	ledgerPath := p.GlobalSettingsLedger()
	ledger := loadSettingsLedger(ledgerPath)
	if len(ledger.Entries) == 0 {
		_ = os.Remove(ledgerPath)
		return "", nil
	}

	file := p.GlobalSettingsFile()
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		_ = os.Remove(ledgerPath)
		return "", nil
	}
	if err != nil {
		return "", err
	}

	settings := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &settings); err != nil {
			return "global settings.json is not a JSON object; left it untouched", nil
		}
		if settings == nil { // a literal `null` unmarshals to a nil map
			_ = os.Remove(ledgerPath)
			return "", nil
		}
	}

	changed := false
	for _, e := range ledger.Entries {
		if parent, key, val, ok := navigate(settings, e.Path); ok {
			if jsonEqual(canonJSON(val), e.Value) {
				delete(parent, key)
				changed = true
			}
		}
	}
	if pruneEmpties(settings, parentPaths(ledger.Entries)) {
		changed = true
	}

	if changed {
		out, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return "", err
		}
		if err := atomicWrite(file, append(out, '\n')); err != nil {
			return "", err
		}
	}
	_ = os.Remove(ledgerPath)
	if changed {
		return "removed ttorch-managed keys from " + file, nil
	}
	return "", nil
}

// mergeManaged walks the managed block against node (a settings object at prefix),
// recursing into objects and treating scalars/arrays as leaves. It adds absent
// leaves, upgrades leaves ttorch still owns, and leaves developer-owned leaves
// untouched. Every leaf ttorch ends up owning is recorded in out. It returns
// whether node changed.
func mergeManaged(node, managed map[string]any, prefix []string, old settingsLedger, out *settingsLedger) bool {
	changed := false
	for _, k := range sortedKeys(managed) {
		path := childPath(prefix, k)
		if sub, ok := managed[k].(map[string]any); ok {
			existing, present := node[k]
			child, isMap := existing.(map[string]any)
			if present && !isMap {
				continue // developer set a non-object here; leave it untouched
			}
			if !present {
				child = map[string]any{}
			}
			subChanged := mergeManaged(child, sub, path, old, out)
			if !present {
				// Only attach the new sub-object if a leaf was actually added.
				if len(child) > 0 {
					node[k] = child
					changed = true
				}
			} else if subChanged {
				changed = true
			}
			continue
		}

		// Leaf (scalar or array).
		want := canonJSON(managed[k])
		cur, present := node[k]
		if !present {
			node[k] = managed[k]
			out.Entries = append(out.Entries, settingsLedgerEntry{Path: path, Value: want})
			changed = true
			continue
		}
		recorded, owned := old.lookup(path)
		if owned && jsonEqual(canonJSON(cur), recorded) {
			// ttorch owns it and the developer hasn't changed it: upgrade in place.
			if !jsonEqual(canonJSON(cur), want) {
				node[k] = managed[k]
				changed = true
			}
			out.Entries = append(out.Entries, settingsLedgerEntry{Path: path, Value: want})
			continue
		}
		// Otherwise the developer owns it: leave it, and stop claiming it.
	}
	return changed
}

// retireUnmanaged removes leaves ttorch owned in old but that the current managed
// block no longer contains, when the live value still equals what ttorch wrote.
func retireUnmanaged(settings map[string]any, old settingsLedger, managedPaths [][]string) bool {
	keep := map[string]bool{}
	for _, p := range managedPaths {
		keep[joinPath(p)] = true
	}
	changed := false
	for _, e := range old.Entries {
		if keep[joinPath(e.Path)] {
			continue // still managed; handled by mergeManaged
		}
		if parent, key, val, ok := navigate(settings, e.Path); ok {
			if jsonEqual(canonJSON(val), e.Value) {
				delete(parent, key)
				changed = true
			}
		}
	}
	return changed
}

// pruneEmpties deletes any object at one of the given paths that has become empty,
// deepest first so inner empties clear before their parents are checked. It is used
// after leaf removals so ttorch never leaves an empty container behind.
func pruneEmpties(settings map[string]any, parents [][]string) bool {
	sort.Slice(parents, func(i, j int) bool { return len(parents[i]) > len(parents[j]) })
	changed := false
	for _, pp := range parents {
		if len(pp) == 0 {
			continue
		}
		if parent, key, val, ok := navigate(settings, pp); ok {
			if m, isMap := val.(map[string]any); isMap && len(m) == 0 {
				delete(parent, key)
				changed = true
			}
		}
	}
	return changed
}

// navigate walks path to its final key, returning the containing object, the key,
// and the value at it. ok is false if any intermediate node is missing or not an
// object, or the final key is absent.
func navigate(root map[string]any, path []string) (parent map[string]any, key string, val any, ok bool) {
	if len(path) == 0 {
		return nil, "", nil, false
	}
	node := root
	for _, step := range path[:len(path)-1] {
		next, isMap := node[step].(map[string]any)
		if !isMap {
			return nil, "", nil, false
		}
		node = next
	}
	key = path[len(path)-1]
	val, present := node[key]
	return node, key, val, present
}

// managedLeafPaths returns the path to every leaf (non-object) in the managed block.
func managedLeafPaths(managed map[string]any, prefix []string) [][]string {
	var out [][]string
	for _, k := range sortedKeys(managed) {
		path := childPath(prefix, k)
		if sub, ok := managed[k].(map[string]any); ok {
			out = append(out, managedLeafPaths(sub, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// parentPaths returns the distinct ancestor paths (length >= 1) of the given leaf
// entries — the containers that pruneEmpties should consider.
func parentPaths(entries []settingsLedgerEntry) [][]string {
	seen := map[string]bool{}
	var out [][]string
	for _, e := range entries {
		for i := 1; i < len(e.Path); i++ {
			pp := e.Path[:i]
			if key := joinPath(pp); !seen[key] {
				seen[key] = true
				out = append(out, append([]string{}, pp...))
			}
		}
	}
	return out
}

func childPath(prefix []string, k string) []string {
	return append(append([]string{}, prefix...), k)
}

func joinPath(p []string) string { return strings.Join(p, "\x00") }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// canonJSON marshals v to JSON. Map keys are emitted in sorted order, so the bytes
// are a stable canonical form usable for deep value comparison.
func canonJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func jsonEqual(a, b json.RawMessage) bool { return bytes.Equal(a, b) }

// shellSingleQuote wraps s in single quotes for safe use in a POSIX shell command,
// escaping any embedded single quote via the standard close-quote/escaped-quote/
// reopen-quote idiom. This keeps the hook command correct even when the install
// path contains a space or an apostrophe (e.g. /Users/o'brien/.claude/hooks).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// canonRaw recompacts an already-encoded JSON value into the same canonical
// (sorted-key, unindented) form canonJSON produces, so a value the ledger stored
// indented still compares byte-equal to a freshly marshaled live value. A value
// that fails to parse is returned unchanged.
func canonRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	return canonJSON(v)
}

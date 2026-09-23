// Package manifest implements ttorch's clobber-safe content reconciliation.
//
// The core guarantee: `ttorch install` / `ttorch update` lay down managed files,
// ADD newly shipped ones, UPGRADE files the user hasn't touched — and NEVER
// overwrite a file the developer edited. Edited/conflicting files are parked
// beside the target as "<name>.ttorch-new" and reported, so no local work is lost.
//
// A per-file sha256 ledger (the manifest) distinguishes "we wrote this and it's
// unchanged" (safe to upgrade) from "the user changed it" (must not clobber).
//
// A caller may also name a narrow set of per-user settings inside some files (see
// Settings). A file that differs from what was last shipped only in those settings is
// upgraded in place with the user's changed settings carried over, instead of being parked.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Suffix is appended to a target when ttorch must surface a new version without
// overwriting local content.
const Suffix = ".ttorch-new"

// Action describes what happened to a single file during reconciliation.
type Action string

const (
	Added     Action = "added"     // file was newly shipped and did not exist
	Updated   Action = "updated"   // managed file unchanged since we wrote it; upgraded in place
	Unchanged Action = "unchanged" // already byte-identical to the shipped version
	Conflict  Action = "conflict"  // user edited a managed file; new version parked as .ttorch-new
	UserFile  Action = "userfile"  // a file we don't manage already sat here; shipped copy parked as .ttorch-new
	Retired   Action = "retired"   // removed upstream and untouched locally; deleted
	Kept      Action = "kept"      // removed upstream but modified locally (or unverifiable); left in place
)

// FileResult records the outcome for one path.
type FileResult struct {
	Path   string
	Action Action
	Note   string
}

// Report aggregates the results of a reconciliation.
type Report struct {
	Results []FileResult
}

func (r *Report) add(path string, a Action, note string) {
	r.Results = append(r.Results, FileResult{Path: path, Action: a, Note: note})
}

// Append records a result. Exposed for callers (e.g. uninstall) that build a
// report outside Reconcile.
func (r *Report) Append(path string, a Action, note string) {
	r.add(path, a, note)
}

// Count returns how many files had the given action.
func (r *Report) Count(a Action) int {
	n := 0
	for _, res := range r.Results {
		if res.Action == a {
			n++
		}
	}
	return n
}

// Conflicts returns the results that need the developer's attention.
func (r *Report) Conflicts() []FileResult {
	var out []FileResult
	for _, res := range r.Results {
		if res.Action == Conflict || res.Action == UserFile || res.Action == Kept {
			out = append(out, res)
		}
	}
	return out
}

// Manifest is the on-disk ledger: absolute target path -> sha256 of the bytes
// ttorch last shipped there.
//
// Settings records, for files a Settings covers, what the shipped version looked like with
// respect to those settings. It is what lets a later update tell "only the settings differ"
// from any other edit, and which settings the user changed. A ledger written before it
// existed simply has no entry (see settingsOnly).
type Manifest struct {
	Version  string                     `json:"version"`
	Files    map[string]string          `json:"files"`
	Settings map[string]ShippedSettings `json:"settings,omitempty"`
}

// ShippedSettings is the ledger entry for a file a Settings covers.
type ShippedSettings struct {
	// Normalized is the sha256 of the shipped bytes with the settings removed.
	Normalized string `json:"normalized"`
	// Values are the settings as shipped; a setting the file did not set is absent.
	Values map[string]string `json:"values,omitempty"`
}

// New returns an empty manifest.
func New(version string) *Manifest {
	return &Manifest{Version: version, Files: map[string]string{}, Settings: map[string]ShippedSettings{}}
}

// Settings marks part of some managed files as the user's own setting rather than shipped
// content. The installer supplies it; this package stays ignorant of file formats.
type Settings interface {
	// Normalize returns b with the settings removed. ok is false when dest carries no
	// settings at all, in which case the file is reconciled exactly as without Settings.
	Normalize(dest string, b []byte) (norm []byte, ok bool)
	// Values returns the settings b sets. An error means they cannot be carried; its text
	// is shown to the user.
	Values(dest string, b []byte) (map[string]string, error)
	// Apply returns onto with its settings set to exactly vals: present values inserted or
	// replaced, any other setting removed, every other byte of onto kept.
	Apply(dest string, vals map[string]string, onto []byte) ([]byte, error)
}

// Load reads a manifest. A missing or corrupt manifest is treated as a fresh
// install (empty ledger) — which is always safe, since an empty ledger never
// causes a delete and treats every existing file as user-owned.
func Load(path string) *Manifest {
	b, err := os.ReadFile(path)
	if err != nil {
		return New("")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return New("")
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	if m.Settings == nil {
		m.Settings = map[string]ShippedSettings{}
	}
	return &m
}

// Save writes the manifest atomically, keeping a single rolling backup.
func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		_ = copyFile(path, path+".bak")
	}
	return atomicWrite(path, b, 0o644)
}

// Sha returns the hex sha256 of b.
func Sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func shaFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Sha(b), nil
}

// Reconcile applies the desired set of managed files (absolute path -> bytes)
// against the previous manifest, performing the clobber-safe writes described
// in the package doc. It returns the next manifest (to be saved) and a report.
func Reconcile(desired map[string][]byte, prev *Manifest, version string) (*Manifest, *Report, error) {
	return ReconcileSettings(desired, prev, version, nil)
}

// ReconcileSettings is Reconcile with a set of per-user settings (nil for none). A managed
// file whose only difference from the version last shipped is in those settings is
// upgraded in place: each setting the user changed (set, altered or removed) is carried
// into the new version, and every other setting takes the new version's value. If the
// settings cannot be carried, the file is parked as a Conflict, as any other edit is.
func ReconcileSettings(desired map[string][]byte, prev *Manifest, version string, s Settings) (*Manifest, *Report, error) {
	next := New(version)
	rep := &Report{}

	for _, dest := range sortedKeys(desired) {
		content := desired[dest]
		newSha := Sha(content)
		next.Files[dest] = newSha
		if s != nil {
			if norm, ok := s.Normalize(dest, content); ok {
				if vals, err := s.Values(dest, content); err == nil {
					next.Settings[dest] = ShippedSettings{Normalized: Sha(norm), Values: vals}
				}
			}
		}

		_, statErr := os.Stat(dest)
		if errors.Is(statErr, fs.ErrNotExist) {
			if err := writeManaged(dest, content); err != nil {
				return nil, nil, err
			}
			rep.add(dest, Added, "")
			continue
		}
		if statErr != nil {
			return nil, nil, statErr
		}

		cur, err := os.ReadFile(dest)
		if err != nil {
			return nil, nil, err
		}
		curSha := Sha(cur)
		prevSha := prev.Files[dest]

		switch {
		case curSha == newSha:
			rep.add(dest, Unchanged, "")
		case prevSha == "":
			// A file we don't track already exists here (user-authored). Don't touch it.
			if err := writeManaged(dest+Suffix, content); err != nil {
				return nil, nil, err
			}
			rep.add(dest, UserFile, "your file kept; shipped version at "+filepath.Base(dest)+Suffix)
		case curSha == prevSha:
			// Unchanged since ttorch wrote it -> safe to upgrade.
			if err := writeManaged(dest, content); err != nil {
				return nil, nil, err
			}
			rep.add(dest, Updated, "")
		case s != nil && settingsOnly(s, dest, cur, content, prev):
			// Only the user's settings differ -> upgrade, keeping the ones they changed.
			merged, err := carrySettings(s, dest, cur, content, prev)
			if err != nil {
				if err := writeManaged(dest+Suffix, content); err != nil {
					return nil, nil, err
				}
				rep.add(dest, Conflict, "your edits kept; new version at "+filepath.Base(dest)+Suffix+" ("+err.Error()+")")
				continue
			}
			if bytes.Equal(merged, cur) {
				rep.add(dest, Unchanged, "")
				continue
			}
			if err := writeManaged(dest, merged); err != nil {
				return nil, nil, err
			}
			rep.add(dest, Updated, "your settings kept")
		default:
			// User edited a managed file -> never clobber.
			if err := writeManaged(dest+Suffix, content); err != nil {
				return nil, nil, err
			}
			rep.add(dest, Conflict, "your edits kept; new version at "+filepath.Base(dest)+Suffix)
		}
	}

	// Files present in the previous manifest but no longer shipped.
	for _, dest := range sortedKeys(toAny(prev.Files)) {
		if _, ok := desired[dest]; ok {
			continue
		}
		cur, err := shaFile(dest)
		if errors.Is(err, fs.ErrNotExist) {
			continue // already gone
		}
		if err != nil {
			rep.add(dest, Kept, "could not verify; left in place")
			continue
		}
		if cur == prev.Files[dest] {
			if err := os.Remove(dest); err == nil {
				rep.add(dest, Retired, "")
			} else {
				rep.add(dest, Kept, "removed upstream but could not delete; left in place")
			}
		} else {
			rep.add(dest, Kept, "modified locally but removed upstream; left in place — delete manually if unwanted")
		}
	}

	return next, rep, nil
}

// settingsOnly reports whether cur differs from the version last shipped to dest only in
// s's settings. shipped is the version being installed now.
func settingsOnly(s Settings, dest string, cur, shipped []byte, prev *Manifest) bool {
	curNorm, ok := s.Normalize(dest, cur)
	if !ok {
		return false
	}
	if last, ok := prev.Settings[dest]; ok {
		return Sha(curNorm) == last.Normalized
	}
	// A ledger from before Settings existed only has the whole-file sha of what was last
	// shipped. Rebuild the candidate by putting the new version's settings onto cur: a sha
	// match proves cur differs from the last shipped bytes only in the settings, and that
	// those were shipped with the new version's values. This misses when the shipped
	// settings changed between versions, which falls back to a Conflict.
	vals, err := s.Values(dest, shipped)
	if err != nil {
		return false
	}
	cand, err := s.Apply(dest, vals, cur)
	return err == nil && Sha(cand) == prev.Files[dest]
}

// carrySettings returns shipped with the settings the user changed in cur carried over. A
// setting counts as changed when its value, or its presence, differs from what was last
// shipped; an unchanged one takes shipped's value, so a new default still reaches the user.
func carrySettings(s Settings, dest string, cur, shipped []byte, prev *Manifest) ([]byte, error) {
	mine, err := s.Values(dest, cur)
	if err != nil {
		return nil, err
	}
	next, err := s.Values(dest, shipped)
	if err != nil {
		return nil, err
	}
	last, ok := prev.Settings[dest]
	if !ok {
		last.Values = next // settingsOnly proved the legacy ledger shipped these
	}
	want := map[string]string{}
	for _, m := range []map[string]string{mine, last.Values, next} {
		for k := range m {
			src := next
			if !sameSetting(mine, last.Values, k) {
				src = mine
			}
			if v, ok := src[k]; ok {
				want[k] = v
			}
		}
	}
	return s.Apply(dest, want, shipped)
}

func sameSetting(a, b map[string]string, k string) bool {
	av, aok := a[k]
	bv, bok := b[k]
	return aok == bok && av == bv
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// writeManaged writes content to path atomically, creating parent dirs.
func writeManaged(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicWrite(path, content, 0o644)
}

func atomicWrite(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ttorch-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return atomicWrite(dst, b, 0o644)
}

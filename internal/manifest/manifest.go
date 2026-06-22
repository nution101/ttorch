// Package manifest implements ttorch's clobber-safe content reconciliation.
//
// The core guarantee: `ttorch install` / `ttorch update` lay down managed files,
// ADD newly shipped ones, UPGRADE files the user hasn't touched — and NEVER
// overwrite a file the developer edited. Edited/conflicting files are parked
// beside the target as "<name>.ttorch-new" and reported, so no local work is lost.
//
// A per-file sha256 ledger (the manifest) distinguishes "we wrote this and it's
// unchanged" (safe to upgrade) from "the user changed it" (must not clobber).
package manifest

import (
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
// ttorch last wrote there.
type Manifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

// New returns an empty manifest.
func New(version string) *Manifest {
	return &Manifest{Version: version, Files: map[string]string{}}
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
	next := New(version)
	rep := &Report{}

	for _, dest := range sortedKeys(desired) {
		content := desired[dest]
		newSha := Sha(content)
		next.Files[dest] = newSha

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

		curSha, err := shaFile(dest)
		if err != nil {
			return nil, nil, err
		}
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

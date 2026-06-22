// Package installer lays the embedded content payload into the user's global
// Claude Code / agent directories using the clobber-safe manifest engine, and
// manages the AGENTS.md managed-block + CLAUDE.md symlink convention.
package installer

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/manifest"
	"github.com/nution101/ttorch/internal/paths"
)

const (
	embedRoot    = "content"
	agentsGlobal = "assets/AGENTS.global.md"

	markerBegin = "<!-- BEGIN ttorch-managed -->"
	markerEnd   = "<!-- END ttorch-managed -->"
)

// Result bundles the manifest report with the side-effects on AGENTS.md/CLAUDE.md.
type Result struct {
	Report *manifest.Report
	Notes  []string
}

// Apply installs (or updates) the managed content. It is idempotent and never
// overwrites developer-edited files (see the manifest package).
func Apply(content fs.FS, p paths.Paths, version string) (*Result, error) {
	desired, global, err := desiredFiles(content, p)
	if err != nil {
		return nil, err
	}

	prev := manifest.Load(p.ManifestFile())
	next, rep, err := manifest.Reconcile(desired, prev, version)
	if err != nil {
		return nil, err
	}
	if err := next.Save(p.ManifestFile()); err != nil {
		return nil, err
	}
	_ = atomicWrite(p.VersionFile(), []byte(version+"\n"))

	res := &Result{Report: rep}
	if global != nil {
		note, err := applyAgentsBlock(p.GlobalAgentsMD(), global)
		if err != nil {
			return nil, err
		}
		res.Notes = append(res.Notes, note)
		res.Notes = append(res.Notes, ensureSymlink(p.GlobalClaudeMD(), "AGENTS.md"))
	}
	return res, nil
}

// desiredFiles walks the embedded payload and maps each file to its absolute
// destination(s). Skills are mirrored into both ~/.claude and ~/.agents. The
// global AGENTS guidance is returned separately (it is merged, not file-managed).
func desiredFiles(content fs.FS, p paths.Paths) (map[string][]byte, []byte, error) {
	desired := map[string][]byte{}
	var global []byte

	err := fs.WalkDir(content, embedRoot, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(fp, embedRoot+"/")
		b, err := fs.ReadFile(content, fp)
		if err != nil {
			return err
		}

		switch {
		case rel == agentsGlobal:
			global = b
		case strings.HasPrefix(rel, "skills/"):
			sub := strings.TrimPrefix(rel, "skills/")
			desired[filepath.Join(p.ClaudeSkills(), filepath.FromSlash(sub))] = b
			desired[filepath.Join(p.AgentsSkills(), filepath.FromSlash(sub))] = b
		case strings.HasPrefix(rel, "agents/"):
			sub := strings.TrimPrefix(rel, "agents/")
			desired[filepath.Join(p.ClaudeAgents(), filepath.FromSlash(sub))] = b
		case strings.HasPrefix(rel, "commands/"):
			sub := strings.TrimPrefix(rel, "commands/")
			desired[filepath.Join(p.ClaudeCommands(), filepath.FromSlash(sub))] = b
		default:
			// Other assets are not installed to disk in this milestone.
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return desired, global, nil
}

// Uninstall removes managed files ttorch still owns (sha matches the ledger),
// leaves anything the developer modified, strips the AGENTS managed block, and
// removes the CLAUDE.md symlink if it points at our AGENTS.md. State/data under
// ~/.ttorch is preserved unless purge is set.
func Uninstall(p paths.Paths, purge bool) (*Result, error) {
	prev := manifest.Load(p.ManifestFile())
	rep := &manifest.Report{}
	res := &Result{Report: rep}

	for dest, sha := range prev.Files {
		b, err := os.ReadFile(dest)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			res.Notes = append(res.Notes, "could not read "+dest+"; left in place")
			continue
		}
		if manifest.Sha(b) == sha {
			if err := os.Remove(dest); err == nil {
				rep.Append(dest, manifest.Retired, "")
			} else {
				rep.Append(dest, manifest.Kept, "could not remove")
			}
		} else {
			rep.Append(dest, manifest.Kept, "modified locally")
		}
	}

	res.Notes = append(res.Notes, stripAgentsBlock(p.GlobalAgentsMD()))
	removeSymlinkIfManaged(p.GlobalClaudeMD())
	_ = os.Remove(p.ManifestFile())
	_ = os.Remove(p.ManifestFile() + ".bak")
	_ = os.Remove(p.VersionFile())

	if purge {
		_ = os.RemoveAll(p.Home)
		res.Notes = append(res.Notes, "purged "+p.Home)
	}
	return res, nil
}

// applyAgentsBlock replaces only the ttorch-managed block in the target file,
// preserving any developer-authored content outside the markers.
func applyAgentsBlock(target string, block []byte) (string, error) {
	managed := markerBegin + "\n" + strings.TrimRight(string(block), "\n") + "\n" + markerEnd + "\n"

	existing, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		if err := atomicWrite(target, []byte(managed)); err != nil {
			return "", err
		}
		return "created " + target, nil
	}
	if err != nil {
		return "", err
	}

	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi >= 0 && ei > bi {
		updated := text[:bi] + strings.TrimRight(managed, "\n") + text[ei+len(markerEnd):]
		if err := atomicWrite(target, []byte(updated)); err != nil {
			return "", err
		}
		return "updated managed block in " + target, nil
	}
	// No block yet: append, keeping the developer's existing content.
	sep := "\n"
	if !strings.HasSuffix(text, "\n") {
		sep = "\n\n"
	}
	if err := atomicWrite(target, []byte(text+sep+managed)); err != nil {
		return "", err
	}
	return "appended managed block to " + target, nil
}

func stripAgentsBlock(target string) string {
	existing, err := os.ReadFile(target)
	if err != nil {
		return ""
	}
	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi < 0 || ei <= bi {
		return ""
	}
	updated := strings.TrimRight(text[:bi], "\n")
	tail := text[ei+len(markerEnd):]
	updated += tail
	_ = atomicWrite(target, []byte(updated))
	return "removed managed block from " + target
}

// ensureSymlink makes dst a symlink to linkTarget (a name relative to dst's dir),
// falling back to a copy when symlinks are unavailable (e.g. Windows without
// Developer Mode). An existing real (non-symlink) file is left untouched.
func ensureSymlink(dst, linkTarget string) string {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return dst + " exists as a real file; left it (review manually)"
		}
		_ = os.Remove(dst)
	}
	if err := os.Symlink(linkTarget, dst); err == nil {
		return "linked " + filepath.Base(dst) + " -> " + linkTarget
	}
	// Copy fallback.
	src := filepath.Join(filepath.Dir(dst), linkTarget)
	if b, err := os.ReadFile(src); err == nil {
		if err := atomicWrite(dst, b); err == nil {
			return "symlink unavailable; wrote " + filepath.Base(dst) + " as a synced copy"
		}
	}
	return "could not create " + filepath.Base(dst)
}

func removeSymlinkIfManaged(dst string) {
	fi, err := os.Lstat(dst)
	if err != nil {
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if t, err := os.Readlink(dst); err == nil && path.Base(t) == "AGENTS.md" {
			_ = os.Remove(dst)
		}
	}
}

func atomicWrite(p string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".ttorch-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, p)
}

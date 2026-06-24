// Package projectinit implements `ttorch init`: set up a repository to follow the
// AGENTS.md-as-source + CLAUDE.md-symlink convention and record its delivery mode
// in an ttorch-managed block, without clobbering existing developer content.
package projectinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	markerBegin = "<!-- BEGIN ttorch-managed -->"
	markerEnd   = "<!-- END ttorch-managed -->"
)

// ValidMode reports whether mode is a recognized delivery mode.
func ValidMode(mode string) bool {
	switch mode {
	case "pr", "local", "validated", "trusted":
		return true
	}
	return false
}

// ReadMode returns the delivery mode recorded in dir/AGENTS.md's ttorch-managed
// block, defaulting to "pr" when the file, the managed block, or a recognized
// "- delivery-mode:" line is absent. It is the first Go reader of the mode the
// manager has so far only consulted as prose, so a typed gate can require behavior
// per mode instead of trusting the LLM to honor it.
func ReadMode(dir string) string {
	const def = "pr"
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return def
	}
	text := string(b)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi < 0 || ei <= bi {
		return def
	}
	for _, line := range strings.Split(text[bi:ei], "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "- delivery-mode:"); ok {
			mode := strings.TrimSpace(rest)
			if ValidMode(mode) {
				return mode
			}
			return def
		}
	}
	return def
}

// Initialized reports whether dir already carries the ttorch-managed block in its
// AGENTS.md, i.e. `ttorch init` has been run there.
func Initialized(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), markerBegin)
}

// Init sets up dir: ensures AGENTS.md carries the ttorch-managed delivery-mode block
// and that CLAUDE.md symlinks to AGENTS.md. It returns human-readable notes.
func Init(dir, mode string) ([]string, error) {
	if mode == "" {
		mode = "pr"
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("unknown delivery mode %q (want: pr | local | validated | trusted)", mode)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	var notes []string
	agents := filepath.Join(dir, "AGENTS.md")
	claude := filepath.Join(dir, "CLAUDE.md")
	block := managedBlock(mode)

	if _, err := os.Stat(agents); os.IsNotExist(err) {
		body := "# Project guidance\n\nProject-specific notes for coding agents go here.\n\n" + block + "\n"
		if err := atomicWrite(agents, []byte(body)); err != nil {
			return nil, err
		}
		notes = append(notes, "created AGENTS.md (delivery-mode: "+mode+")")
	} else if err != nil {
		return nil, err
	} else {
		note, err := upsertBlock(agents, block, mode)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}

	notes = append(notes, ensureSymlink(claude, "AGENTS.md"))
	return notes, nil
}

func managedBlock(mode string) string {
	b := markerBegin + "\n" +
		"This repository is managed by ttorch. The manager reads the delivery mode below.\n\n" +
		"- delivery-mode: " + mode + "\n"
	if mode == "trusted" {
		b += "\nTrusted mode: worker output may be merged through the ttorch-review adversarial-review\n" +
			"gate (a passing verdict plus a fresh green validate, commit-pinned and enforced in Go)\n" +
			"WITHOUT a separate human approval. This is an explicit, repo-scoped decision; the default\n" +
			"is pr. Auto-merge REQUIRES a .ttorch/validate.sh on this default branch (the gate's\n" +
			"validation authority); without it, auto-merge is refused and a human approval is needed.\n" +
			"A change to the gate itself (this block or .ttorch/validate.sh) always requires a human.\n"
	}
	return b + markerEnd
}

// upsertBlock replaces the ttorch-managed block in an existing AGENTS.md, or appends
// it, preserving all developer content outside the markers.
func upsertBlock(path, block, mode string) (string, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi >= 0 && ei > bi {
		updated := text[:bi] + block + text[ei+len(markerEnd):]
		if err := atomicWrite(path, []byte(updated)); err != nil {
			return "", err
		}
		return "updated delivery-mode in AGENTS.md (delivery-mode: " + mode + ")", nil
	}
	sep := "\n"
	if !strings.HasSuffix(text, "\n") {
		sep = "\n\n"
	}
	if err := atomicWrite(path, []byte(text+sep+block+"\n")); err != nil {
		return "", err
	}
	return "added ttorch block to AGENTS.md (delivery-mode: " + mode + ")", nil
}

// ensureSymlink makes dst -> linkTarget, with a copy fallback; never clobbers an
// existing real file.
func ensureSymlink(dst, linkTarget string) string {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return filepath.Base(dst) + " already exists as a real file; left it (merge manually)"
		}
		_ = os.Remove(dst)
	}
	if err := os.Symlink(linkTarget, dst); err == nil {
		return "linked " + filepath.Base(dst) + " -> " + linkTarget
	}
	src := filepath.Join(filepath.Dir(dst), linkTarget)
	if b, err := os.ReadFile(src); err == nil {
		if err := atomicWrite(dst, b); err == nil {
			return "symlink unavailable; wrote " + filepath.Base(dst) + " as a synced copy"
		}
	}
	return "could not create " + filepath.Base(dst)
}

func atomicWrite(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ttorch-tmp-*")
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
	return os.Rename(name, path)
}

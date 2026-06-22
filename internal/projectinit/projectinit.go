// Package projectinit implements `orcha init`: set up a repository to follow the
// AGENTS.md-as-source + CLAUDE.md-symlink convention and record its delivery mode
// in an orcha-managed block, without clobbering existing developer content.
package projectinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	markerBegin = "<!-- BEGIN orcha-managed -->"
	markerEnd   = "<!-- END orcha-managed -->"
)

// ValidMode reports whether mode is a recognized delivery mode.
func ValidMode(mode string) bool {
	switch mode {
	case "pr", "local", "validated":
		return true
	}
	return false
}

// Init sets up dir: ensures AGENTS.md carries the orcha-managed delivery-mode block
// and that CLAUDE.md symlinks to AGENTS.md. It returns human-readable notes.
func Init(dir, mode string) ([]string, error) {
	if mode == "" {
		mode = "pr"
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("unknown delivery mode %q (want: pr | local | validated)", mode)
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
	return markerBegin + "\n" +
		"This repository is managed by orcha. The manager reads the delivery mode below.\n\n" +
		"- delivery-mode: " + mode + "\n" +
		markerEnd
}

// upsertBlock replaces the orcha-managed block in an existing AGENTS.md, or appends
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
	return "added orcha block to AGENTS.md (delivery-mode: " + mode + ")", nil
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".orcha-tmp-*")
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

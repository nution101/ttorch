// Package projectinit implements `ttorch init`: set up a repository to follow the
// AGENTS.md-as-source + CLAUDE.md-symlink convention and record its delivery mode
// in an ttorch-managed block, without clobbering existing developer content.
package projectinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	markerBegin = "<!-- BEGIN ttorch-managed -->"
	markerEnd   = "<!-- END ttorch-managed -->"
)

// ValidMode reports whether mode is a recognized delivery mode.
//
// The comparison is deliberately case-SENSITIVE, and this is the one place in the gate's
// path where folding would be the unsafe direction. ReadMode falls back to "pr" for anything
// it does not recognize, so "Trusted" in an AGENTS.md reads as pr: the gate is off, the PR
// path is taken, and MergeLocal refuses an auto-minted token as ungated. Folding would turn
// that typo into real trusted mode — a live auto-merge nobody asked for. Init rejects an
// unrecognized --mode outright, so the canonical spelling is enforced where the file is
// written. (orchestrator.matchesGateConfig folds both sides for the opposite reason: there,
// over-matching only costs an extra --allow-gate-change.)
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
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return "pr"
	}
	return ParseMode(string(b))
}

// ParseMode is ReadMode over the text of an AGENTS.md rather than a directory, for a caller
// that reads the file from a git ref instead of a working tree.
func ParseMode(agentsMD string) string {
	const def = "pr"
	for _, line := range managedBlockLines(agentsMD) {
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

// managedBlockLines returns the lines of text's ttorch-managed block, or nil when the block
// is absent or its markers are out of order.
func managedBlockLines(text string) []string {
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi < 0 || ei <= bi {
		return nil
	}
	return strings.Split(text[bi:ei], "\n")
}

// gateChangeApprovalKey is the per-repo policy line that decides whether a trusted merge whose
// diff changes a gate-definition file needs the lead's `ttorch approve --allow-gate-change`.
// It sits in the ttorch-managed block beside `- delivery-mode:`. `ttorch init` never writes
// it, so a repo gets the default until the lead adds it.
const gateChangeApprovalKey = "- gate-change-approval:"

// The two gate-change-approval policies. Off is the default: a trusted repo's passing verdict
// and fresh green validate authorize a gate change the way they authorize any other diff.
// Required is the opt-in that keeps the lead's --allow-gate-change.
const (
	GateChangeApprovalOff      = "off"
	GateChangeApprovalRequired = "required"
)

// ParseGateChangeApproval returns the gate-change-approval policy recorded in the
// ttorch-managed block of agentsMD, the text of an AGENTS.md, and every value in the block it
// did not recognize, in order.
//
// No block, no line, and lines that all say `off` read as GateChangeApprovalOff. A `required`
// line reads as GateChangeApprovalRequired, and so does any other value: a typo in a line
// someone added to switch the approval ON must not leave it off. Those values are returned so
// the caller can name them. A block with both `off` and `required` is required, for the same
// reason. Values are compared exactly, case included, so "Required" is unrecognized (and
// therefore required) rather than folded. A line outside the managed block is ignored.
//
// It takes the file's text rather than a directory, unlike ReadMode, because the gate that
// consumes it must read the DEFAULT BRANCH's committed AGENTS.md, never a checkout a worker
// can edit. A directory-reading variant would make the wrong read the easy one.
func ParseGateChangeApproval(agentsMD string) (policy string, unrecognized []string) {
	policy = GateChangeApprovalOff
	for _, line := range managedBlockLines(agentsMD) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), gateChangeApprovalKey)
		if !ok {
			continue
		}
		switch v := strings.TrimSpace(rest); v {
		case GateChangeApprovalOff:
		case GateChangeApprovalRequired:
			policy = GateChangeApprovalRequired
		default:
			policy = GateChangeApprovalRequired
			unrecognized = append(unrecognized, v)
		}
	}
	return policy, unrecognized
}

// autoMintMaxAgeKey is the per-repo policy line that bounds how stale a trusted,
// AUTONOMOUS auto-mint may be before the gate refuses to auto-land it and a human must
// approve instead. It is a `- auto-mint-max-age: <duration>` line in AGENTS.md (a Go
// duration such as 72h), read alongside the delivery mode.
const autoMintMaxAgeKey = "- auto-mint-max-age:"

// ReadAutoMintMaxAge returns the repo's configured maximum age for an AUTONOMOUS auto-mint
// land — the staleness bound that keeps a weeks-old trusted auto-pass from silently
// auto-landing with no human ever looking again. It reads the first
// `- auto-mint-max-age: <duration>` line in dir/AGENTS.md (anywhere in the file, so a
// developer can place it where `ttorch init` will not regenerate over it), parsing a Go
// duration. The bool is false — meaning NO bound (the default; a still-passing auto verdict
// always lands) — when the file or the line is absent, or the value is unparseable or
// non-positive. It governs ONLY the auto path: a human approval never expires by age. This
// is a READ-ONLY consumer of a human-authored policy; ttorch never writes the line.
func ReadAutoMintMaxAge(dir string) (time.Duration, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), autoMintMaxAgeKey); ok {
			d, err := time.ParseDuration(strings.TrimSpace(rest))
			if err != nil || d <= 0 {
				return 0, false
			}
			return d, true
		}
	}
	return 0, false
}

// LiveMode reports the delivery mode currently in force for the repo at dir — the
// SAME value the merge/land gate resolves via ReadMode — and whether dir could be
// read at all. ok is false when dir is missing or is not a directory; a caller (e.g.
// `ttorch project ls`) should then fall back to a stored/cached mode rather than to
// ReadMode's "pr" default, which would otherwise mask a vanished repo as an explicit
// pr. This is a thin DISPLAY helper over ReadMode; it does not change how the gate
// reads or enforces the mode.
func LiveMode(dir string) (mode string, ok bool) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return ReadMode(dir), true
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

	if _, err := os.Stat(agents); os.IsNotExist(err) {
		body := "# Project guidance\n\nProject-specific notes for coding agents go here.\n\n" + managedBlock(mode) + "\n"
		if err := atomicWrite(agents, []byte(body)); err != nil {
			return nil, err
		}
		notes = append(notes, "created AGENTS.md (delivery-mode: "+mode+")")
	} else if err != nil {
		return nil, err
	} else {
		note, err := upsertBlock(agents, mode)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}

	if mode == "trusted" {
		if b, err := os.ReadFile(agents); err == nil {
			notes = append(notes, gateChangeApprovalNote(string(b)))
		}
	}
	notes = append(notes, ensureSymlink(claude, "AGENTS.md"))
	return notes, nil
}

// gateChangeApprovalNote is the line `ttorch init` prints for a trusted repo, saying what the
// gate-change approval is set to in the AGENTS.md it just wrote and how to change it. It names
// any value ParseGateChangeApproval does not recognize, quoted, so a typo is visible at init
// rather than only at the first refused merge.
func gateChangeApprovalNote(agentsMD string) string {
	policy, unrecognized := ParseGateChangeApproval(agentsMD)
	switch {
	case len(unrecognized) > 0:
		quoted := make([]string, len(unrecognized))
		for i, v := range unrecognized {
			quoted[i] = strconv.Quote(v)
		}
		return "gate-change approval: required, because gate-change-approval has an unrecognized value (" +
			strings.Join(quoted, ", ") + "); set it to off or required"
	case policy == GateChangeApprovalRequired:
		return "gate-change approval: required (kept from the existing block); a change to the gate itself needs `ttorch approve --allow-gate-change`"
	default:
		return "gate-change approval: off (the default); a change to the reviewers or the validate step is authorized by the gate that change modifies. " +
			"Add `- gate-change-approval: required` under the delivery mode to require `ttorch approve --allow-gate-change` for it"
	}
}

// managedBlock renders the ttorch-managed block for mode. kept are policy lines carried over
// from the block being replaced (see keptBlockLines); they go directly under the delivery-mode
// line.
func managedBlock(mode string, kept ...string) string {
	b := markerBegin + "\n" +
		"This repository is managed by ttorch. The manager reads the delivery mode below.\n\n" +
		"- delivery-mode: " + mode + "\n"
	for _, line := range kept {
		b += line + "\n"
	}
	if mode == "trusted" {
		b += "\nTrusted mode: worker output may be merged through the ttorch-review adversarial-review\n" +
			"gate (a passing verdict plus a fresh green validate, commit-pinned and enforced in Go)\n" +
			"WITHOUT a separate human approval. This is an explicit, repo-scoped decision; the default\n" +
			"is pr. Auto-merge REQUIRES a .ttorch/validate.sh on this default branch (the gate's\n" +
			"validation authority); without it, auto-merge is refused and a human approval is needed.\n" +
			"A change to the gate itself (this block or .ttorch/validate.sh) is authorized the same\n" +
			"way, so a change to the reviewers or the validate step is authorized by the gate that\n" +
			"change modifies. To require a human's `ttorch approve --allow-gate-change` for those\n" +
			"changes, set gate-change-approval: required in this block on the default branch.\n"
	}
	return b + markerEnd
}

// keptBlockLines returns the lines of an existing managed block that Init carries into the
// block it regenerates: every `- gate-change-approval:` line, trimmed, in order. That line is
// the lead's decision rather than ttorch's text, so re-running `ttorch init` must not erase it:
// dropping a `required` line would silently remove the approval the lead asked for. All of
// them are kept, so a pair that disagrees, or a misspelled value, still reads as required
// afterwards.
func keptBlockLines(block string) []string {
	var kept []string
	for _, line := range strings.Split(block, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, gateChangeApprovalKey) {
			kept = append(kept, l)
		}
	}
	return kept
}

// upsertBlock replaces the ttorch-managed block in an existing AGENTS.md, or appends
// it, preserving all developer content outside the markers and the policy lines
// keptBlockLines names inside them.
func upsertBlock(path, mode string) (string, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi >= 0 && ei > bi {
		block := managedBlock(mode, keptBlockLines(text[bi:ei])...)
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
	if err := atomicWrite(path, []byte(text+sep+managedBlock(mode)+"\n")); err != nil {
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

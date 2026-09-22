package review

import (
	"path"
	"strings"
)

// Size classifies a worker's diff by change size so the trust gate can scale its reviewer
// set to the risk a diff actually carries. A docs-only or trivial change does not warrant
// the full three-dimension pass; a substantial code change does.
type Size string

const (
	// SizeDocsOnly is a diff whose every changed file is inert prose (Markdown, plain
	// text, a LICENSE-style file): no executable surface, so no security review. Agent
	// configuration spelled as Markdown (CLAUDE.md, .claude/agents/*.md) is NOT inert and
	// never qualifies. See isAgentInstruction.
	SizeDocsOnly Size = "docs-only"
	// SizeTrivial is a small single-file code change: low scope-creep risk, so scope
	// review is dropped — but it is still code, so security review is KEPT.
	SizeTrivial Size = "trivial"
	// SizeSubstantial is any other diff: the full correctness + scope + security pass.
	// It is the fail-safe default whenever classification is uncertain.
	SizeSubstantial Size = "substantial"
)

// trivialLineBudget caps the added+removed content lines a SizeTrivial change may carry.
// A single-file change above it is treated as substantial — large enough that scope creep
// is a real risk again, so the full pass applies.
const trivialLineBudget = 20

// fullReviewers is the complete three-dimension set: the substantial-change pass and the
// fail-safe default. Callers must treat the returned slices as read-only.
var fullReviewers = []string{DimensionCorrectness, DimensionScope, DimensionSecurity}

// Classify returns the change-size class and the review dimensions the trust gate must run
// for a diff, scaling the set to the risk it carries:
//
//   - docs-only (every changed file is inert prose) → {correctness, scope}. Prose can carry
//     factual or scope errors but has no executable surface, so the security reviewer is
//     dropped — never for code, only for documentation.
//   - trivial (one small code file, within the line budget) → {correctness, security}. A
//     tiny single-file change has little room for scope creep, so scope is dropped; but it
//     is still code, so security is KEPT.
//   - substantial (anything else) → the full {correctness, scope, security}.
//
// SECURITY-CRITICAL CONTRACT: files MUST be an AUTHORITATIVE, COMPLETE, UNQUOTED list of
// every path the diff touches — e.g. `git diff --name-only -z` (NUL-separated, never
// quoted regardless of core.quotePath). It must NOT be scraped from a patch body: git
// quotes paths with tabs, control characters, quotes, backslashes, or non-ASCII bytes in
// the patch text, and a scraper that drops a quoted path would let a worker hide a
// malicious code file behind a docs-only edit and so skip the security reviewer. To
// enforce that fail-closed, Classify treats an empty file list or any empty path entry as
// uncertain and returns the full set, and the caller must likewise pass the full set
// whenever the authoritative list could not be obtained. ok being false means git could
// not produce a trustworthy stat (it too forces the full set). Security review is dropped
// only for a diff with no code at all (docs-only); every code path keeps it, and so does
// every path that instructs an agent session (CLAUDE.md, AGENTS.md, SKILL.md, .claude/**,
// and the content/ subtrees the installer writes into a session's directories), which is
// configuration rather than prose however it is spelled. See isAgentInstruction.
func Classify(files []string, lines int, binary, ok bool) (Size, []string) {
	switch {
	case !ok || len(files) == 0 || hasEmpty(files):
		// Incomplete or untrustworthy file info: never reduce review — over-review.
		return SizeSubstantial, append([]string(nil), fullReviewers...)
	case allDocs(files):
		return SizeDocsOnly, []string{DimensionCorrectness, DimensionScope}
	case len(files) == 1 && !binary && lines <= trivialLineBudget:
		return SizeTrivial, []string{DimensionCorrectness, DimensionSecurity}
	default:
		return SizeSubstantial, append([]string(nil), fullReviewers...)
	}
}

// docExtensions are inert-prose file extensions (lowercased, with leading dot). A file
// with one of these has no executable surface, so a diff of only such files needs no
// security review.
var docExtensions = map[string]bool{
	".md": true, ".markdown": true, ".mdown": true, ".mkd": true,
	".rst": true, ".adoc": true, ".txt": true,
}

// docBasenames are conventional extension-less prose files (compared case-insensitively).
var docBasenames = map[string]bool{
	"LICENSE": true, "LICENCE": true, "NOTICE": true, "AUTHORS": true,
	"COPYING": true, "README": true, "CHANGELOG": true, "CONTRIBUTING": true,
}

// agentInstructionStems name agent-instruction files: prose by extension, configuration by
// effect. A session reads them and follows what they say, so a change to one changes how an
// agent behaves. SKILL is here because a skill body is executed as instructions the moment
// the skill is invoked.
//
// These are STEMS, not whole basenames, because the convention admits a qualifier between
// the name and the extension: AGENTS.global.md is the payload ttorch merges into the global
// AGENTS.md, and CLAUDE.local.md is the per-machine override. Matching whole basenames left
// both classified as prose. Only the first dot-separated component is compared, so skills.md
// and my-agents.md are unaffected.
//
// Compared with strings.EqualFold, so this is a slice rather than a map: a map lookup needs a
// normalized key, and the normalization that looked right (strings.ToUpper) is not the
// folding that decides here.
var agentInstructionStems = []string{"CLAUDE", "AGENTS", "SKILL"}

// agentInstructionDirs are directories whose contents configure an agent session wherever
// they appear: settings, hooks that run around the session, and subagent definitions that
// outrank the installed ones. Matched as whole PATH COMPONENTS at any depth, because a
// nested package may carry its own.
var agentInstructionDirs = []string{".claude", ".agents"}

// agentInstructionRoots are repository-rooted directories holding instruction files that are
// neither inside an agent directory nor named for one. ttorch embeds these and the installer
// writes them into the user's global agent directories, so a change here changes what every
// future session on the machine is told to do.
//
// The list is exactly the set of subtrees installer.desiredFiles routes to a session-visible
// destination. It was derived from that switch rather than from memory, after a first version
// anchored on agents and skills alone and left content/commands and content/assets classified
// as prose. content/assets is the one that mattered: AGENTS.global.md is merged into the
// global AGENTS.md block, so the guard covered the installed copy and left the source open.
// Two hand-maintained enumerations have already drifted once, so
// TestClassifierCoversEveryInstalledSubtree in internal/installer fails if the installer
// grows a subtree this list does not carry.
//
// content/hooks holds shell scripts, which keep the security reviewer on their extension
// alone; it is listed so the two sets stay comparable, and so a README dropped beside a hook
// is not treated as inert.
//
// Anchored at the repository root rather than matched at any depth, which keeps an unrelated
// docs/skills/ or a vendored content/agents/ out of it.
var agentInstructionRoots = [][]string{
	{"content", "agents"},
	{"content", "skills"},
	{"content", "commands"},
	{"content", "assets"},
	{"content", "hooks"},
}

// isAgentInstruction reports whether p instructs an agent rather than documenting the
// project. It exists because these paths are spelled with a prose extension, and treating
// them as inert prose let a diff of nothing but .md files classify as docs-only and drop the
// security reviewer. Dropping that reviewer is what a worker subverting the review would
// want, since it is the one that runs outside the worker's worktree, and the file it would
// most want to edit is the definition of the security reviewer itself.
//
// Every half of every comparison folds, and they all have to fold the same way. An earlier
// version folded only the basename and left the directory segment an exact byte compare, so
// `.Claude/agents/x.md` classified as docs-only and dropped the reviewer this whole check
// exists to keep. Folding is strings.EqualFold (Unicode simple folding) rather than upper- or
// lower-casing, so U+212A KELVIN SIGN and U+017F LATIN SMALL LETTER LONG S fold too. macOS is
// case-insensitive, so a committed `.Claude/` is the same directory to the session that reads
// it: the guard has to be at least as insensitive as the filesystem it defends.
func isAgentInstruction(p string) bool {
	segs := strings.Split(path.Clean(p), "/")
	if hasInstructionStem(segs[len(segs)-1]) {
		return true
	}
	for _, seg := range segs {
		for _, dir := range agentInstructionDirs {
			if strings.EqualFold(seg, dir) {
				return true
			}
		}
	}
	for _, root := range agentInstructionRoots {
		if hasFoldedPrefix(segs, root) {
			return true
		}
	}
	return false
}

// hasInstructionStem reports whether a basename names an instruction file. It compares only
// the first dot-separated component against agentInstructionStems, and only for a file that
// would otherwise read as prose, so AGENTS.md, AGENTS.global.md and AGENTS.MD all match while
// skills.md, my-agents.md and claude-usage.md do not. Restricting it to prose extensions
// keeps the rule to its purpose: a basename carrying any other extension already keeps the
// security reviewer, and this check exists only to rescue the ones that would not.
func hasInstructionStem(base string) bool {
	if !docExtensions[strings.ToLower(path.Ext(base))] {
		return false
	}
	stem, _, ok := strings.Cut(base, ".")
	if !ok {
		return false
	}
	for _, name := range agentInstructionStems {
		if strings.EqualFold(stem, name) {
			return true
		}
	}
	return false
}

// hasFoldedPrefix reports whether segs begins with prefix, comparing each component with
// strings.EqualFold. It exists because strings.HasPrefix on the joined path would be a byte
// compare — the exact one-sided-fold mistake this file has already paid for — and because a
// string prefix would also match a sibling whose name merely starts with the same letters
// (contentious/agents-notes.md against "content/agents"). A prefix must cover at least one
// directory and leave a file after it, so an exact match of the directory alone is not one.
func hasFoldedPrefix(segs, prefix []string) bool {
	if len(segs) <= len(prefix) {
		return false
	}
	for i, want := range prefix {
		if !strings.EqualFold(segs[i], want) {
			return false
		}
	}
	return true
}

// isDocFile reports whether p is an inert-prose documentation file. It is deliberately
// conservative: only well-known prose extensions and basenames qualify, so anything that
// could execute or carry configuration (.go, .sh, .yaml, .json, .html, .svg and so on) is
// treated as code and keeps the full reviewer set. Agent-instruction files are configuration
// whatever they are spelled (see isAgentInstruction), so they are checked first and never
// qualify. README.md and docs/*.md still do.
//
// WHAT THIS DOES NOT COVER: the check is by name and by location, so Markdown that is neither
// named as an instruction file nor under an instruction directory, yet is read as instructions
// by something, is still inert here. A brief a manager points a worker at, a runbook a prompt
// tells an agent to follow, an instruction file under a directory layout this list does not
// know: all classify docs-only and drop the security reviewer. The scaling rule's premise,
// that prose has no executable surface, holds for ordinary prose and is enforced by name for
// the instruction files above. It is not established for Markdown in general.
func isDocFile(p string) bool {
	if isAgentInstruction(p) {
		return false
	}
	base := path.Base(p)
	if ext := strings.ToLower(path.Ext(base)); ext != "" {
		return docExtensions[ext]
	}
	return docBasenames[strings.ToUpper(base)]
}

// allDocs reports whether every path is an inert-prose documentation file. An empty set is
// not docs-only (the caller handles it as the uncertain, fail-safe case).
func allDocs(files []string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !isDocFile(f) {
			return false
		}
	}
	return true
}

// hasEmpty reports whether any entry is the empty string — a sign the authoritative list
// was parsed incompletely, which must force the full reviewer set.
func hasEmpty(files []string) bool {
	for _, f := range files {
		if f == "" {
			return true
		}
	}
	return false
}

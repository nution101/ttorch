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
	// never qualifies — see isHarnessConfig.
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
// every path that configures an agent session (CLAUDE.md, AGENTS.md, .claude/**), which is
// configuration rather than prose however it is spelled — see isHarnessConfig.
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

// harnessConfigBasenames are agent-instruction files: prose by extension, configuration by
// effect. A session reads them from its working directory and follows what they say, so a
// change to one changes how an agent behaves. Compared with strings.EqualFold, so it is a
// slice rather than a map: a map lookup needs a normalized key, and the normalization that
// looked right (strings.ToUpper) is not the folding that decides here.
var harnessConfigBasenames = []string{"CLAUDE.md", "AGENTS.md"}

// harnessConfigDir is the directory whose contents configure an agent session: settings,
// hooks that run around the session, and subagent definitions that outrank the installed
// ones. Matched as a whole PATH COMPONENT, so a prose file that merely mentions the word
// (notes/claude-usage.md) is unaffected, and matched case-INSENSITIVELY, because the
// filesystem this runs on is.
const harnessConfigDir = ".claude"

// isHarnessConfig reports whether p configures an agent session rather than documenting the
// project. It exists because two of those paths — CLAUDE.md and .claude/agents/*.md — are
// spelled with a prose extension, and treating them as inert prose let a diff of nothing but
// .md files classify as docs-only and drop the security reviewer. Dropping that reviewer is
// what a worker subverting the review would want, since it is the one that runs outside the
// worker's worktree.
// Both halves fold, and they have to fold the same way. The first version folded only the
// basename and left the directory segment an exact byte compare, so `.Claude/agents/x.md`
// classified as docs-only and dropped the security reviewer, which is the reviewer this whole
// change exists to keep. Folding is strings.EqualFold (Unicode simple folding) rather than
// upper- or lower-casing, so U+212A KELVIN SIGN and U+017F LATIN SMALL LETTER LONG S fold too.
// macOS is case-insensitive, so a committed `.Claude/` is the same directory to the session
// that reads it: the guard has to be at least as insensitive as the filesystem it defends.
func isHarnessConfig(p string) bool {
	base := path.Base(p)
	for _, name := range harnessConfigBasenames {
		if strings.EqualFold(base, name) {
			return true
		}
	}
	for _, seg := range strings.Split(path.Clean(p), "/") {
		if strings.EqualFold(seg, harnessConfigDir) {
			return true
		}
	}
	return false
}

// isDocFile reports whether p is an inert-prose documentation file. It is deliberately
// conservative: only well-known prose extensions and basenames qualify, so anything that
// could execute or carry configuration (.go, .sh, .yaml, .json, .html, .svg, …) is treated
// as code and keeps the full reviewer set. Agent-instruction files are configuration
// whatever they are spelled (see isHarnessConfig), so they are checked first and never
// qualify — README.md and docs/*.md still do.
func isDocFile(p string) bool {
	if isHarnessConfig(p) {
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

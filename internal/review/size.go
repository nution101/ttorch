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
	// text, a LICENSE-style file): no executable surface, so no security review.
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

// Reviewers classifies diff (a unified `git diff`) by change size and returns the review
// dimensions the trust gate must run for it, scaling the set to the risk the diff carries:
//
//   - docs-only (every changed file is inert prose) → {correctness, scope}. Prose can carry
//     factual or scope errors but has no executable surface, so the security reviewer is
//     dropped — never for code, only for documentation.
//   - trivial (one small code file, within the line budget) → {correctness, security}. A
//     tiny single-file change has little room for scope creep, so scope is dropped; but it
//     is still code, so security is KEPT.
//   - substantial (anything else) → the full {correctness, scope, security}.
//
// It biases to the full set whenever the diff is uncertain to classify — an empty or
// unparseable diff, a binary blob, or any mix that is not cleanly docs-only or trivial —
// so a malformed or surprising diff is over-reviewed, never under-reviewed. The returned
// slice is a fresh copy the caller may retain. Security review is dropped only for a diff
// with no code at all (docs-only); every code path keeps it.
func Reviewers(diff string) (Size, []string) {
	files, lines, binary := parseDiffStat(diff)

	switch {
	case len(files) == 0:
		// No recognizable file changes: cannot assess size, so over-review.
		return SizeSubstantial, append([]string(nil), fullReviewers...)
	case allDocs(files):
		return SizeDocsOnly, []string{DimensionCorrectness, DimensionScope}
	case len(files) == 1 && !binary && lines <= trivialLineBudget:
		return SizeTrivial, []string{DimensionCorrectness, DimensionSecurity}
	default:
		return SizeSubstantial, append([]string(nil), fullReviewers...)
	}
}

// parseDiffStat reads a unified diff and returns the distinct paths it touches, the total
// added+removed content lines, and whether any touched file is binary. It is a heuristic
// scan — filenames come from `diff --git` headers and line counts from `+`/`-` body lines —
// sufficient to gauge change size; it does not aim to be a full patch parser.
func parseDiffStat(diff string) (files []string, lines int, binary bool) {
	seen := map[string]bool{}
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			if p := gitHeaderPath(ln); p != "" && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		case strings.HasPrefix(ln, "Binary files ") || strings.HasPrefix(ln, "GIT binary patch"):
			binary = true
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			// File headers, not content — never counted.
		case strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-"):
			lines++
		}
	}
	return files, lines, binary
}

// gitHeaderPath extracts the post-image path from a `diff --git a/<p> b/<p>` header,
// returning the part after " b/". It returns "" if the header is malformed (e.g. a path
// with spaces git would have quoted), which only nudges classification toward the
// fail-safe full set.
func gitHeaderPath(header string) string {
	i := strings.Index(header, " b/")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(header[i+len(" b/"):])
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

// isDocFile reports whether p is an inert-prose documentation file. It is deliberately
// conservative: only well-known prose extensions and basenames qualify, so anything that
// could execute or carry configuration (.go, .sh, .yaml, .json, .html, .svg, …) is treated
// as code and keeps the full reviewer set.
func isDocFile(p string) bool {
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

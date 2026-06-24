package state

import (
	"path"
	"strings"
)

// A task's Footprint is the set of repo-relative paths / directory prefixes it
// will touch. The functions here turn "do two footprints overlap?" into a
// deterministic, segment-aware computation so the orchestrator can refuse to
// dispatch two workers onto the same files (the cause of rebase churn) without
// guessing.

// footprintSegments normalizes one footprint entry into comparable slash-path
// segments: surrounding whitespace and slashes are trimmed and the path is
// cleaned, so "internal/cli/", "./internal/cli" and "internal/cli" all compare
// equal. An entry that denotes the repo root ("", ".", "/") yields no segments —
// the whole tree, which overlaps everything.
func footprintSegments(p string) []string {
	p = path.Clean(strings.TrimSpace(p))
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

// PathsOverlap reports whether two footprint paths refer to overlapping file
// trees: one path is equal to, or a directory ancestor of, the other. The
// comparison is segment-aware, so "internal/cli" never matches the sibling
// "internal/climate" the way a raw string-prefix test would. A root path
// ("", ".", "/") overlaps everything.
func PathsOverlap(a, b string) bool {
	as, bs := footprintSegments(a), footprintSegments(b)
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		if as[i] != bs[i] {
			return false
		}
	}
	// Every shared segment matched, so the shorter path is a prefix of (or equal
	// to) the longer — they overlap.
	return true
}

// FootprintOverlap returns the overlapping (a, b) path pairs between two
// footprints, in deterministic order (a's order, then b's). An empty result
// means the two footprints are disjoint. Either footprint being empty yields no
// overlap: a task that declares nothing is exempt from enforcement.
func FootprintOverlap(a, b []string) [][2]string {
	var out [][2]string
	for _, x := range a {
		for _, y := range b {
			if PathsOverlap(x, y) {
				out = append(out, [2]string{x, y})
			}
		}
	}
	return out
}

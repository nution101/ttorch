package worktree

import "strings"

// CloneRefs is the ref namespace in the lead's repository that holds what ttorch keeps for a
// task's clone: refs/ttorch/clones/<task>/<sha> for each commit imported from it, beside the
// other refs the task's clone needs held there. Every name under it is built from an id that
// passed ValidTaskID and, for an import, a sha that passed ValidObjectID.
const CloneRefs = "refs/ttorch/clones/"

// ValidTaskID reports whether id may name a task's directory under CloneRefs. It is an
// allowlist, ^[A-Za-z0-9][A-Za-z0-9._-]*$, plus the two refname rules that pattern still
// lets through: no ".." anywhere and no ".lock" at the end. The leading character must be
// alphanumeric because git check-ref-format accepts a component that starts with "-", which
// a command line can read as an option, so the allowlist does the work and git's own check
// on the ref is the second one.
func ValidTaskID(id string) bool {
	if id == "" || strings.Contains(id, "..") || strings.HasSuffix(id, ".lock") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// ValidObjectID reports whether s is a full SHA-1 or SHA-256 object id in lowercase hex.
func ValidObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

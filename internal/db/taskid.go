package db

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateTaskID reports whether id may name a new task: a single plain token that is safe to
// print on its own line, to join into a path as one component, and to pass as a command
// argument. A task id reaches all three. It is printed in `ttorch inbox` and `ttorch watch`
// output the manager reads, it names per-task files and directories under the ttorch home, and
// it is typed on command lines. A worker chooses the id of its own follow-on tasks, so the
// rule is written as what an id may be, not as a list of bad inputs:
//
//   - non-empty valid UTF-8, not "." or "..", not starting with '-';
//   - no '/' or '\', so it can never name a path of more than one component;
//   - every rune printable (unicode.IsPrint) and none a space of any kind (unicode.IsSpace).
//
// The last rule is stated as a property because a list of line breaks goes stale: U+2028 and
// U+2029 break lines in most renderers and are not control characters, and format characters
// such as a bidi override are invisible. IsPrint admits neither, and IsSpace removes the ASCII
// space it would otherwise allow.
func ValidateTaskID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("task id is empty")
	case !utf8.ValidString(id):
		return fmt.Errorf("task id %q is not valid UTF-8", id)
	case id == "." || id == "..":
		return fmt.Errorf("task id %q is a path reference, not a name", id)
	case strings.HasPrefix(id, "-"):
		return fmt.Errorf("task id %q starts with '-', which reads as a flag", id)
	case strings.ContainsAny(id, `/\`):
		return fmt.Errorf("task id %q contains a path separator", id)
	}
	if i := strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(id[i:])
		return fmt.Errorf("task id %q contains %U, a space or non-printing character", id, r)
	}
	return nil
}

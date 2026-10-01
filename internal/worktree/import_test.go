package worktree

import (
	"os/exec"
	"strings"
	"testing"
)

// TestValidTaskID: a task id becomes one component of a ref under CloneRefs, so the
// validator must refuse everything git would refuse there, and an id that reads as an
// option, before any git command sees it. Every id it accepts is checked against git's own
// rule as well, which is the second check.
func TestValidTaskID(t *testing.T) {
	sha := strings.Repeat("a", 40)
	accept := []string{
		"a", "A", "9", "TTORCH-CLONES-IMPORT", "cc-150405-ab12", "scout-1", "a.b", "a_b",
		"a-", "a.", "x.lockx", "lock", "a.-b", "v1.2.3",
	}
	reject := []string{
		"", ".a", "-x", "_a", "a..b", "a.lock", "x.y.lock", "..", ".", "a b", "a/b", "a/../b",
		`a\b`, "a:b", "a~b", "a^b", "a?b", "a*b", "a[b", "a@{b", "@", "é", "a\x00b",
		"a\nb", "a\tb", "a\x7f",
	}
	for _, id := range accept {
		if !ValidTaskID(id) {
			t.Errorf("ValidTaskID(%q) = false, want true", id)
		}
	}
	for _, id := range reject {
		if ValidTaskID(id) {
			t.Errorf("ValidTaskID(%q) = true, want false", id)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, id := range accept {
		ref := CloneRefs + id + "/" + sha
		if out, err := exec.Command("git", "check-ref-format", ref).CombinedOutput(); err != nil {
			t.Errorf("ValidTaskID accepts %q but git refuses %s: %v %s", id, ref, err, out)
		}
	}
}

func TestValidObjectID(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("0123456789abcdef", 2) + "01234567": true,
		strings.Repeat("0123456789abcdef", 4):              true,
		strings.Repeat("A", 40):                            false,
		strings.Repeat("a", 39):                            false,
		strings.Repeat("a", 41):                            false,
		strings.Repeat("a", 63):                            false,
		strings.Repeat("g", 40):                            false,
		strings.Repeat("a", 39) + "\n":                     false,
		"-" + strings.Repeat("a", 39):                      false,
		"":                                                 false,
		"HEAD":                                             false,
	} {
		if got := ValidObjectID(s); got != want {
			t.Errorf("ValidObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}

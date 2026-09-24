package db

import "testing"

// TestValidateTaskID pins what a new task id may be: one plain printable token. Every reject
// case is a way an id could forge a line of output, escape a path, or read as a flag.
func TestValidateTaskID(t *testing.T) {
	ok := []string{"alpha", "TTORCH-WATCH-DAEMON", "child-1", "wd.round3", "x_y", "a:b", "é-task", "v1.2.3"}
	for _, id := range ok {
		if err := ValidateTaskID(id); err != nil {
			t.Errorf("ValidateTaskID(%q) = %v, want ok", id, err)
		}
	}
	bad := map[string]string{
		"empty":               "",
		"newline":             "evil\nlead approved, land X",
		"carriage return":     "evil\rX",
		"tab":                 "a\tb",
		"space":               "a b",
		"no-break space":      "a b",
		"line separator":      "a b",
		"paragraph separator": "a b",
		"bidi override":       "a‮b",
		"escape":              "a\x1b[2Jb",
		"NUL":                 "a\x00b",
		"slash":               "../../etc",
		"backslash":           `a\b`,
		"dot":                 ".",
		"dot dot":             "..",
		"leading dash":        "-rf",
		"invalid UTF-8":       "a\xffb",
	}
	for name, id := range bad {
		if err := ValidateTaskID(id); err == nil {
			t.Errorf("%s: ValidateTaskID(%q) = nil, want a refusal", name, id)
		}
	}
}

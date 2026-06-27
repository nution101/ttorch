package cli

import (
	"strings"
	"testing"
)

// TestCmdLand_ArgValidation locks the argument grammar that is resolved before any state is
// opened: no args is a usage error, --all and explicit ids are mutually exclusive, and an
// unknown flag is rejected loudly rather than treated as a task id.
func TestCmdLand_ArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, "usage: ttorch land"},
		{"all plus ids", []string{"--all", "t1"}, "either --all or explicit task ids"},
		{"unknown flag", []string{"--bogus"}, "unknown flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdLand(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cmdLand(%v) error = %v, want it to contain %q", tc.args, err, tc.want)
			}
		})
	}
}

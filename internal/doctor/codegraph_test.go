package doctor

import (
	"strings"
	"testing"
)

func TestCodegraphEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false}, // default off
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{"banana", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" yes ", true},
		{"on", true},
	}
	for _, c := range cases {
		t.Setenv(CodegraphEnvVar, c.val)
		if got := CodegraphEnabled(); got != c.want {
			t.Errorf("CodegraphEnabled() with %q = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestCodegraphEnabledUnset(t *testing.T) {
	// Set then clear, since t.Setenv cannot fully unset; an empty value is treated as off.
	t.Setenv(CodegraphEnvVar, "")
	if CodegraphEnabled() {
		t.Fatal("an empty TTORCH_CODEGRAPH must be off (default off)")
	}
}

func TestReportCodegraph(t *testing.T) {
	// Feature off: an informational opt-in line; never [ok]/[absent] (no failure framing).
	var off strings.Builder
	reportCodegraph(&off, false, "", false)
	if s := off.String(); !strings.Contains(s, "[off]") || !strings.Contains(s, "set "+CodegraphEnvVar+"=1") {
		t.Errorf("off line missing opt-in guidance: %q", s)
	}

	// Off but installed: still off, and says so without implying it is active.
	var offInstalled strings.Builder
	reportCodegraph(&offInstalled, false, "/usr/bin/codegraph", true)
	if s := offInstalled.String(); !strings.Contains(s, "[off]") || !strings.Contains(s, "installed") {
		t.Errorf("off+installed line wrong: %q", s)
	}

	// Enabled + present: [ok] with the path.
	var ok strings.Builder
	reportCodegraph(&ok, true, "/usr/bin/codegraph", true)
	if s := ok.String(); !strings.Contains(s, "[ok]") || !strings.Contains(s, "/usr/bin/codegraph") {
		t.Errorf("ok line wrong: %q", s)
	}

	// Enabled + absent: [absent] plus an explicit "no error" / "stays off" note.
	var absent strings.Builder
	reportCodegraph(&absent, true, "", false)
	if s := absent.String(); !strings.Contains(s, "[absent]") ||
		!strings.Contains(s, "stays off") || !strings.Contains(s, "no error") {
		t.Errorf("absent line should reassure it is a clean no-op: %q", s)
	}
}

func TestFindCodegraphInjectable(t *testing.T) {
	orig := lookCodegraph
	t.Cleanup(func() { lookCodegraph = orig })

	lookCodegraph = func() (string, bool) { return "/fake/codegraph", true }
	if p, ok := FindCodegraph(); !ok || p != "/fake/codegraph" {
		t.Fatalf("FindCodegraph() = %q,%v; want /fake/codegraph,true", p, ok)
	}
	lookCodegraph = func() (string, bool) { return "", false }
	if p, ok := FindCodegraph(); ok || p != "" {
		t.Fatalf("FindCodegraph() = %q,%v; want \"\",false", p, ok)
	}
}

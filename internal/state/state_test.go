package state

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Equal and ancestor/descendant overlap.
		{"internal/cli", "internal/cli", true},
		{"internal/cli", "internal/cli/cli.go", true},
		{"internal/cli/cli.go", "internal/cli", true},
		{"internal", "internal/cli", true},
		// Disjoint siblings.
		{"internal/cli", "internal/orchestrator", false},
		{"internal/cli/a.go", "internal/cli/b.go", false},
		// Segment-aware: a raw string-prefix test would WRONGLY match these.
		{"internal/cli", "internal/climate", false},
		{"internal/cli", "internal/cli2", false},
		// Cleaning: trailing slashes and ./ normalize before comparison.
		{"internal/cli/", "internal/cli", true},
		{"./internal/cli", "internal/cli", true},
		{"internal//cli", "internal/cli", true},
		// A root path covers the whole tree, so it overlaps everything.
		{".", "internal/cli", true},
		{"", "internal/cli", true},
		{"/", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := PathsOverlap(c.a, c.b); got != c.want {
			t.Errorf("PathsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		// Overlap is symmetric.
		if got := PathsOverlap(c.b, c.a); got != c.want {
			t.Errorf("PathsOverlap(%q, %q) [reversed] = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

func TestFootprintOverlap(t *testing.T) {
	// Disjoint footprints -> no overlap.
	if got := FootprintOverlap([]string{"internal/cli"}, []string{"internal/orchestrator"}); len(got) != 0 {
		t.Fatalf("disjoint footprints should not overlap, got %v", got)
	}
	// An empty footprint on either side is exempt (declares nothing).
	if got := FootprintOverlap(nil, []string{"internal/cli"}); len(got) != 0 {
		t.Fatalf("empty proposed footprint must not overlap, got %v", got)
	}
	if got := FootprintOverlap([]string{"internal/cli"}, nil); len(got) != 0 {
		t.Fatalf("empty existing footprint must not overlap, got %v", got)
	}
	// Overlapping footprints report the colliding pairs, in deterministic order.
	got := FootprintOverlap(
		[]string{"internal/cli", "docs"},
		[]string{"internal/cli/cli.go", "internal/state"},
	)
	if len(got) != 1 || got[0] != [2]string{"internal/cli", "internal/cli/cli.go"} {
		t.Fatalf("overlap pairs = %v, want one pair internal/cli↔internal/cli/cli.go", got)
	}
}

// TestTaskFootprintOmitempty pins the additive/back-compat contract on the legacy
// Task shape: a Task with no footprint marshals without the key, so a legacy record
// re-serializes byte-identical. Persistence now lives in internal/db (whose tests
// cover the footprint JSON ↔ []string round-trip); this only guards the type's tags.
func TestTaskFootprintOmitempty(t *testing.T) {
	b, err := json.Marshal(Task{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "footprint") {
		t.Fatalf("a Task with no footprint must omit the key (omitempty), got %s", b)
	}
}

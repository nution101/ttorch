package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaskSessionIDRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	want := Task{
		ID: "t1", Window: "wk-t1", Worktree: "/tmp/wt", Project: "/repo",
		Harness: "claude", Kind: "ship", Created: time.Now().Truncate(time.Second),
		SessionID: "abc-123",
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID {
		t.Fatalf("SessionID round-trip: got %q, want %q", got.SessionID, want.SessionID)
	}
}

func TestManagerRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}

	// Absent: LoadManager returns (zero, false, nil).
	if m, ok, err := s.LoadManager(); err != nil || ok || m.Dir != "" {
		t.Fatalf("absent manager: got (%+v, %v, %v), want (zero, false, nil)", m, ok, err)
	}

	want := Manager{Dir: "/work/repo", SessionID: "mgr-sid-1"}
	if err := s.SaveManager(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadManager()
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("manager round-trip: got %+v, want %+v", got, want)
	}

	if err := s.RemoveManager(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadManager(); ok {
		t.Fatal("manager should be gone after RemoveManager")
	}
	// Removing a missing record is a no-op.
	if err := s.RemoveManager(); err != nil {
		t.Fatalf("removing absent manager should be a no-op: %v", err)
	}
}

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

// TestFootprintRoundTrip pins the additive/back-compat contract: a Task with no
// footprint marshals without the key (so legacy records re-save byte-identical),
// and a declared footprint round-trips intact.
func TestFootprintRoundTrip(t *testing.T) {
	b, err := json.Marshal(Task{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "footprint") {
		t.Fatalf("a Task with no footprint must omit the key (omitempty), got %s", b)
	}

	s := Store{Dir: t.TempDir()}
	want := Task{ID: "t2", Created: time.Now().Truncate(time.Second), Footprint: []string{"internal/cli", "internal/state"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("t2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Footprint, ",") != strings.Join(want.Footprint, ",") {
		t.Fatalf("footprint round-trip: got %v, want %v", got.Footprint, want.Footprint)
	}
}

func TestListSkipsManager(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(Task{ID: "t1", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveManager(Manager{Dir: "/d", SessionID: "x"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("List should return only the task, got %+v", tasks)
	}
}

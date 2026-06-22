package learnings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdd_UpsertCountsRecurrence(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if _, err := s.Add("prefer table-driven tests", "", "t1", false); err != nil {
		t.Fatal(err)
	}
	// Same lesson, different casing/spacing -> increments, not duplicates.
	e, err := s.Add("Prefer table-driven   tests", "", "t2", false)
	if err != nil {
		t.Fatal(err)
	}
	if e.Count != 2 {
		t.Fatalf("count = %d, want 2", e.Count)
	}
	entries, _ := s.Load()
	if len(entries) != 1 {
		t.Fatalf("expected 1 deduped entry, got %d", len(entries))
	}
}

func TestPromoted_ThresholdAndPin(t *testing.T) {
	entries := []Entry{
		{Text: "one-off", Count: 1},
		{Text: "recurring", Count: 2},
		{Text: "pinned-once", Count: 1, Pinned: true},
	}
	got := Promoted(entries)
	has := func(s string) bool {
		for _, e := range got {
			if e.Text == s {
				return true
			}
		}
		return false
	}
	if has("one-off") {
		t.Fatal("a single unpinned lesson must not promote")
	}
	if !has("recurring") || !has("pinned-once") {
		t.Fatalf("recurring + pinned should promote: %+v", got)
	}
	if got[0].Text != "pinned-once" {
		t.Fatalf("pinned should sort first, got %q", got[0].Text)
	}
}

func TestApply_WritesBlockOnPromotionAndPreserves(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Guide\n\nhouse rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// First sighting: in the ledger, not yet in the block.
	if _, err := Apply(dir, "always run gofmt", "", "t1", false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(agents)
	if strings.Contains(string(got), "always run gofmt") {
		t.Fatal("a one-off lesson should not appear in the block yet")
	}
	// Second sighting: promotes into the block.
	if _, err := Apply(dir, "always run gofmt", "", "t2", false); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agents)
	if !strings.Contains(string(got), "always run gofmt") {
		t.Fatal("recurring lesson should be promoted into the block")
	}
	if !strings.Contains(string(got), "house rules") {
		t.Fatal("existing content must be preserved")
	}
	if strings.Count(string(got), markerBegin) != 1 {
		t.Fatal("learnings block duplicated")
	}
}

func TestPrune_DropsStaleOneOffs(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	s.Add("stale one-off", "", "", false)
	s.Add("kept recurring", "", "", false)
	s.Add("kept recurring", "", "", false) // count 2
	s.Add("kept pinned", "", "", true)
	// Backdate everything so the age filter applies.
	entries, _ := s.Load()
	for i := range entries {
		entries[i].Updated = time.Now().Add(-48 * time.Hour)
	}
	s.save(entries)

	removed, err := s.Prune(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the stale one-off)", removed)
	}
	left, _ := s.Load()
	if len(left) != 2 {
		t.Fatalf("expected 2 kept (recurring + pinned), got %d", len(left))
	}
}

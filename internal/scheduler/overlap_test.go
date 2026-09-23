package scheduler

import (
	"testing"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/orchestrator"
)

// TestDispatchOverlapsUsesTheDispatchPassOccupancy pins the rule the dispatch pass and the
// board share: an active claim (even before its window exists) or a live window occupies its
// footprint; a cc session, an empty footprint, another repo, or a task neither active nor live
// does not.
func TestDispatchOverlapsUsesTheDispatchPassOccupancy(t *testing.T) {
	const repo, other = "/repos/app", "/repos/api"
	snap := orchestrator.NewLiveSnapshot([]db.Task{
		{ID: "claimed", Project: repo, Status: db.StatusActive, Footprint: []string{"claimed.go"}},                   // no window yet
		{ID: "windowed", Project: repo, Status: db.StatusNeedsInput, Window: "wk-w", Footprint: []string{"live.go"}}, // live window
		{ID: "gone", Project: repo, Status: db.StatusDone, Window: "wk-gone", Footprint: []string{"gone.go"}},        // window not live
		{ID: "cc", Project: repo, Status: db.StatusActive, Kind: db.KindCC, Footprint: []string{"cc.go"}},
		{ID: "elsewhere", Project: other, Status: db.StatusActive, Footprint: []string{"elsewhere.go"}},
	}, []string{"wk-w"})
	for _, tc := range []struct {
		fp   []string
		want bool
	}{
		{[]string{"claimed.go"}, true},
		{[]string{"live.go"}, true},
		{[]string{"gone.go"}, false},
		{[]string{"cc.go"}, false},
		{[]string{"elsewhere.go"}, false},
		{[]string{"fresh.go"}, false},
		{nil, false},
	} {
		if got := DispatchOverlaps(snap, db.Task{ID: "new", Project: repo, Footprint: tc.fp}); got != tc.want {
			t.Errorf("DispatchOverlaps(%v) = %v, want %v", tc.fp, got, tc.want)
		}
	}
}

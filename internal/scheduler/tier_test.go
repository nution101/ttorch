package scheduler

import (
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

func TestClassifyTier(t *testing.T) {
	cases := []struct {
		name       string
		task       db.Task
		wantModel  string
		wantEffort string
	}{
		{"scout is cheapest", db.Task{Kind: db.KindScout}, tierScoutModel, tierScoutEffort},
		{"normal ship is the middle tier", db.Task{Kind: db.KindShip, Footprint: []string{"internal/cli/cli.go"}, Title: "add a --json flag"}, tierShipModel, tierShipEffort},
		{"security footprint is top tier", db.Task{Kind: db.KindShip, Footprint: []string{"internal/crypto/keys.go"}}, tierRiskModel, tierRiskEffort},
		{"finance title is top tier", db.Task{Kind: db.KindShip, Title: "fix the payment rounding bug"}, tierRiskModel, tierRiskEffort},
		{"concurrency footprint is top tier", db.Task{Kind: db.KindShip, Footprint: []string{"internal/pool/mutex.go"}}, tierRiskModel, tierRiskEffort},
		{"migration footprint is top tier", db.Task{Kind: db.KindShip, Footprint: []string{"internal/db/migrations/0008_x.up.sql"}}, tierRiskModel, tierRiskEffort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, e := classifyTier(tc.task)
			if m != tc.wantModel || e != tc.wantEffort {
				t.Fatalf("classifyTier = (%q,%q), want (%q,%q)", m, e, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

func TestResolveDispatchTierExplicitWins(t *testing.T) {
	t.Setenv("TTORCH_MODEL", "")  // pin: no env default, so the classifier path is deterministic
	t.Setenv("TTORCH_EFFORT", "") // (precedence over env is covered by TestResolveDispatchTierEnv)

	// Both explicit → the classifier is not consulted (even for a scout).
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindScout, Model: "opus", Effort: "max"}); m != "opus" || e != "max" {
		t.Fatalf("explicit both = (%q,%q), want (opus,max)", m, e)
	}
	// Only model explicit → effort filled by the classifier (scout → its effort).
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindScout, Model: "opus"}); m != "opus" || e != tierScoutEffort {
		t.Fatalf("model-only = (%q,%q), want (opus,%s)", m, e, tierScoutEffort)
	}
	// Only effort explicit → model filled by the classifier (ship → its model).
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindShip, Effort: "low"}); m != tierShipModel || e != "low" {
		t.Fatalf("effort-only = (%q,%q), want (%s,low)", m, e, tierShipModel)
	}
	// Neither → the full classifier tier (ship → sonnet/high).
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindShip}); m != tierShipModel || e != tierShipEffort {
		t.Fatalf("neither = (%q,%q), want (%s,%s)", m, e, tierShipModel, tierShipEffort)
	}
}

// TestResolveDispatchTierEnv proves the env default (TTORCH_MODEL / TTORCH_EFFORT) sits
// BELOW an explicit per-task value but ABOVE the classifier: a global env must not be
// overridden by the classifier's cheaper guess, and an explicit row value must still win.
func TestResolveDispatchTierEnv(t *testing.T) {
	t.Setenv("TTORCH_MODEL", "opus")
	t.Setenv("TTORCH_EFFORT", "max")

	// No row value → env wins over the classifier (a normal ship would otherwise be sonnet/high).
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindShip}); m != "opus" || e != "max" {
		t.Fatalf("env default = (%q,%q), want (opus,max) — classifier must not override env", m, e)
	}
	// A scout, too: env beats the scout tier.
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindScout}); m != "opus" || e != "max" {
		t.Fatalf("env default (scout) = (%q,%q), want (opus,max)", m, e)
	}
	// An explicit row value still wins over the env.
	if m, e := resolveDispatchTier(db.Task{Kind: db.KindShip, Model: "haiku", Effort: "low"}); m != "haiku" || e != "low" {
		t.Fatalf("row over env = (%q,%q), want (haiku,low)", m, e)
	}
}

// TestFalsePositiveRiskKeywords guards the narrowed keyword list against common substrings
// that must NOT trip the expensive top tier: "author" (git authorship, everywhere in this
// repo), "scheduler", "trace", "middle", "unlock". A regression that broadened a keyword
// (e.g. back to a bare "auth"/"lock"/"race"/"sched") would over-tier innocuous work to opus.
func TestFalsePositiveRiskKeywords(t *testing.T) {
	for _, s := range []string{
		"update the author name in commits",
		"refactor internal/scheduler dispatch",
		"add a stack trace to the logger",
		"fix the middle pane layout",
		"unlock the door widget",
		"rename a variable in internal/cli",
	} {
		if matchesRisk(s) {
			t.Errorf("matchesRisk(%q) = true, want false (false positive over-tiers to opus)", s)
		}
	}
}

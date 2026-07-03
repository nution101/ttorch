package scheduler

import (
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
)

// Dispatch-time model/effort tiers. Model and effort are orthogonal dials (which brain vs
// how hard it thinks); a tier pairs a cheap-enough model with a matching effort. The pairs
// are deliberately VALID combinations — claude silently downgrades an effort a model does
// not support, and fast mode is opus-only — so, for example, we never emit (haiku,
// ultracode). Explicit per-task values always win over these defaults (see
// resolveDispatchTier).
const (
	tierScoutModel  = "haiku"
	tierScoutEffort = "medium"

	tierShipModel  = "sonnet"
	tierShipEffort = "high"

	tierRiskModel  = "opus"
	tierRiskEffort = "ultracode"
)

// riskKeywords mark a task whose blast radius earns the top tier even when its footprint is
// small: security/auth/crypto, concurrency, schema/data migrations, and money/finance paths.
// A small-but-hard change on one of these paths should not be under-powered. Matched
// lowercased, as a substring, against both the declared footprint paths and the task title.
// The list is deliberately narrow to avoid false positives that would over-tier innocuous
// work to the expensive model (e.g. "authn/authz/authoriz" rather than a bare "auth" that
// would also match "author"); over-tiering is the safe direction, but needless opus is the
// cost we are trying to cut.
var riskKeywords = []string{
	// security / secrets
	"secur", "authn", "authz", "authentic", "authoriz", "crypto",
	"secret", "password", "token", "credential",
	// concurrency
	"concurren", "goroutine", "mutex", "deadlock", "semaphore", "atomic",
	// schema / data migrations
	"migrat", "schema",
	// money / finance
	"payment", "money", "financ", "ledger", "invoice", "billing",
}

// classifyTier maps a task's complexity signals to a (model, effort) tier. It is heuristic
// policy, deliberately conservative and easily tuned: a scout (read-only investigation) gets
// the cheapest tier; a ship task on a risk-bearing path gets the top tier; every other ship
// task gets the middle tier. Footprint size is a weak proxy for difficulty, so risk paths
// bias UP and escalation-on-failure (a task re-dispatched after a failed validate/review at a
// higher tier) is the intended safety net for a mis-classified small-but-hard task.
func classifyTier(t db.Task) (model, effort string) {
	if t.Kind == db.KindScout {
		return tierScoutModel, tierScoutEffort
	}
	if isRiskyTask(t) {
		return tierRiskModel, tierRiskEffort
	}
	return tierShipModel, tierShipEffort
}

// isRiskyTask reports whether a task touches a risk-bearing path (by footprint or title),
// earning the top tier regardless of size.
func isRiskyTask(t db.Task) bool {
	if matchesRisk(t.Title) {
		return true
	}
	for _, f := range t.Footprint {
		if matchesRisk(f) {
			return true
		}
	}
	return false
}

func matchesRisk(s string) bool {
	s = strings.ToLower(s)
	for _, kw := range riskKeywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// resolveDispatchTier picks the (model, effort) to dispatch a claimed task at, with
// precedence: explicit per-task value (row) > env default (TTORCH_MODEL / TTORCH_EFFORT) >
// classifier tier > kind default (applied later by the harness). An explicit value on the
// row — from `ttorch task add` / `spawn --model/--effort`, or persisted by a prior dispatch —
// always wins; a global env default wins over the classifier; and the classifier fills only
// whichever dial is still unset. Resolving both here fixes the historical gap where the
// autonomous dispatch dropped the persisted effort (it passed "") and adds cheap-by-default
// model selection without overriding a user's explicit env.
func resolveDispatchTier(t db.Task) (model, effort string) {
	model, effort = t.Model, t.Effort
	if model == "" {
		model = harness.EnvWorkerModel()
	}
	if effort == "" {
		effort = harness.EnvWorkerEffort()
	}
	if model == "" || effort == "" {
		cm, ce := classifyTier(t)
		if model == "" {
			model = cm
		}
		if effort == "" {
			effort = ce
		}
	}
	return model, effort
}

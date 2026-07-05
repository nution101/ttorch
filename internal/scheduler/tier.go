package scheduler

import (
	"strings"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/harness"
)

// Dispatch-time model/effort tiers. Model and effort are orthogonal dials (which brain vs
// how hard it thinks); a tier pairs a model with a matching effort. The pairs are deliberately
// VALID combinations — claude silently downgrades an effort a model does not support, and fast
// mode is opus-only. Explicit per-task values always win over these defaults (see
// resolveDispatchTier).
//
// QUALITY FLOOR — code is NEVER written on a cheap model. A ship task (it writes code) starts
// at opus/high AT MINIMUM; the equivalent floor is fable/medium. Sonnet/haiku are never a
// default for code. Research (a scout, read-only investigation) MAY run on sonnet — the one
// place a cheaper model is allowed — and any follow-up that fills in or corrects that research
// climbs to opus/fable via escalation. The top (risk) tier keeps opus but caps effort at xhigh
// rather than ultracode: ultracode is xhigh reasoning PLUS a session spinning up its own
// internal sub-agent fleet, redundant with ttorch (ttorch already IS the orchestration fleet)
// and diminishing on a single scoped task. This saves money on research and on effort — never
// by writing code with a weaker model.
const (
	tierScoutModel  = "sonnet"
	tierScoutEffort = "medium"

	tierShipModel  = "opus"
	tierShipEffort = "high"

	tierRiskModel  = "opus"
	tierRiskEffort = "xhigh"

	// tierTopModel is the top of the escalation ladder, above opus. It is the most capable
	// (and priciest) model, reserved for auto-tiered work that has repeatedly FAILED at a
	// cheaper tier — never a default. See escalateModel / classifyTier.
	tierTopModel = "fable"
)

// modelLadder is the capability/cost order the escalation walks: each retry of an auto-tiered
// task bumps the model one rung up from its base tier, so the priciest models are spent only on
// work that could not be completed cheaper. Haiku is deliberately absent — the floor for any
// assigned tier is sonnet (research) and the floor for code is opus. Fable ($10/$50 in/out,
// ~2x opus) is the last rung.
var modelLadder = []string{tierScoutModel, tierShipModel, tierTopModel} // sonnet, opus, fable

// escalateModel bumps base up the ladder by `steps` rungs, clamped to the top (fable). A model
// not on the ladder (a full id or an unknown alias) is returned unchanged — escalation only
// applies to the classifier's own tiers, never to a value it did not choose.
func escalateModel(base string, steps int) string {
	i := indexOfModel(base)
	if i < 0 {
		return base
	}
	i += steps
	if i >= len(modelLadder) {
		i = len(modelLadder) - 1
	}
	return modelLadder[i]
}

func indexOfModel(m string) int {
	for i, v := range modelLadder {
		if v == m {
			return i
		}
	}
	return -1
}

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
	switch {
	case t.Kind == db.KindScout:
		model, effort = tierScoutModel, tierScoutEffort
	case isRiskyTask(t):
		model, effort = tierRiskModel, tierRiskEffort
	default:
		model, effort = tierShipModel, tierShipEffort
	}
	// Escalation-on-failure: each retry bumps the model one rung up the ladder
	// (haiku→sonnet→opus→fable), reserving the priciest models for work that has actually
	// failed at a cheaper tier. Effort stays at the tier's level. Only auto-tiered tasks
	// reach classifyTier on a retry (see resolveDispatchTier) — a user/env pin never escalates.
	if t.RetryCount > 0 {
		model = escalateModel(model, t.RetryCount)
	}
	return model, effort
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
// ResolveDispatchTier is the exported entry point to the dispatch-time tier policy, so the
// interactive spawn path (`ttorch spawn`) resolves model/effort through the SAME classifier
// the autonomous daemon uses — one cost policy, not two. It applies the full precedence
// (explicit row value > env default > classifier tier) and returns whether the result was
// classifier-derived (autoTiered), which the caller persists so a later retry may escalate.
func ResolveDispatchTier(t db.Task) (model, effort string, autoTiered bool) {
	return resolveDispatchTier(t)
}

func resolveDispatchTier(t db.Task) (model, effort string, autoTiered bool) {
	envM, envE := harness.EnvWorkerModel(), harness.EnvWorkerEffort()

	// An auto-tiered task (its model+effort were classifier-assigned, no user/env pin) re-derives
	// the tier on EVERY dispatch, so a retry escalates the model — deliberately ignoring the
	// values persisted from the prior dispatch. A global env still overrides the classifier.
	if t.AutoTiered {
		model, effort = classifyTier(t)
		if envM != "" {
			model = envM
		}
		if envE != "" {
			effort = envE
		}
		return model, effort, true
	}

	model, effort = t.Model, t.Effort
	if model == "" {
		model = envM
	}
	if effort == "" {
		effort = envE
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
	// The dispatch is auto-tiered only when BOTH dials were classifier-derived (no row pin and
	// no env on either), so a retry may escalate freely while any user/env pin stays pinned.
	autoTiered = t.Model == "" && t.Effort == "" && envM == "" && envE == ""
	return model, effort, autoTiered
}

package scheduler

// governor.go is the scheduler's load-aware dispatch backpressure (roadmap H4). The dispatch
// pass already refuses to spawn work it cannot prove safe — a declared, disjoint footprint
// within free worktree-pool capacity — but it has historically had NO governor on whether the
// MACHINE can take another concurrent heavy worker. Each dispatched worker is a full harness
// session that runs the repo's build + `make test` suites, each fanning out to NumCPU; filling
// the pool (up to TTORCH_MAX_WORKTREES, default 16) with such workers can drive system load far
// past what the host (especially one with MDM anti-virus scanning every build artifact) can bear.
//
// The governor is a SOFTER, machine-aware throttle layered on TOP of the hard pool cap and the
// disjoint-footprint invariant — never a replacement for them. It only ever causes the daemon to
// dispatch FEWER workers than capacity+overlap would allow, never more, so it cannot weaken any
// correctness gate (capacity, overlap, fail-closed occupancy). It has three independent knobs,
// each OFF on a bare struct (value 0) and populated from the env-or-default by New(); a value
// <= 0 DISABLES that knob (its off-switch), and with all three disabled the dispatch/land passes
// behave exactly as before:
//
//   - MaxActiveWorkers (TTORCH_MAX_ACTIVE_WORKERS / --max-active): the max number of heavy
//     workers allowed to run CONCURRENTLY across the fleet, counted as live tasks in the 'active'
//     state. Distinct from — and below — the worktree-pool cap: the pool cap is the hard per-repo
//     ceiling, this is a softer machine-wide throttle on how many of those slots run at once. When
//     the count is at the cap, a dispatch tick spawns NOTHING more and LOGS the deferral, leaving
//     ready tasks pending for a later tick. Default: NumCPU/2 (floored at 2 so a small machine
//     still runs real parallelism, capped at the pool size so it never exceeds the hard ceiling)
//     — a meaningful reduction from a full pool of 16 while still dispatching several workers, and
//     fully tunable (set it to 1-2 for an aggressive throttle, or a high value to effectively
//     disable it). The cap is ALWAYS enforceable (it needs only the DB-derived live set), so it is
//     the governor's load-floor guarantee even when the load-average read below is unavailable.
//
//   - LoadCeiling (TTORCH_LOAD_CEILING / --load-ceiling): when > 0, a dispatch tick whose 1-minute
//     system load average exceeds this value defers (logs + leaves tasks pending) rather than
//     adding more load. It is the ADAPTIVE complement to the fixed count cap — useful when the
//     bottleneck is I/O (e.g. AV scanning) rather than worker count. It is compared against the
//     RAW 1-minute load average, so set it relative to the host's core count (e.g. ~1.5×NumCPU).
//     OFF by default (0). It FAILS OPEN: an unreadable load average is treated as "no limit" and
//     never wedges dispatch — only the always-enforceable MaxActiveWorkers cap gates a tick whose
//     load could not be read.
//
//   - MaxLandConcurrency (TTORCH_MAX_LAND_CONCURRENCY / --max-land-concurrency): the max number of
//     gated tasks the LAND pass hands to the concurrent land pipeline PER TICK. Each landed task
//     re-runs the (offloaded) validate suite, so an unbounded burst of gated tasks could launch
//     many heavy suites at once; this bounds that fan-out. The remaining gated tasks land on later
//     ticks (re-derived from the board each tick). Default: 2. <= 0 disables the per-tick cap
//     (today's behavior — hand the whole gated set to the pipeline, which still bounds its own
//     internal fan-out at NumCPU).

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/nution101/ttorch/internal/db"
)

const (
	envMaxActiveWorkers   = "TTORCH_MAX_ACTIVE_WORKERS"
	envLoadCeiling        = "TTORCH_LOAD_CEILING"
	envMaxLandConcurrency = "TTORCH_MAX_LAND_CONCURRENCY"

	// minDefaultMaxActive floors the computed MaxActiveWorkers default so a low-core machine
	// still dispatches at least this many workers concurrently — enough that the fleet makes
	// real parallel progress, while still well below a full worktree pool.
	minDefaultMaxActive = 2

	// defaultMaxLandConcurrency bounds, by default, how many gated tasks the land pass hands to
	// the concurrent land pipeline per tick. Small so a burst of gated work cannot launch many
	// heavy validate suites at once; the rest land on subsequent ticks.
	defaultMaxLandConcurrency = 2

	// defaultLoadCeiling leaves the load-average ceiling OFF by default (the always-enforceable
	// MaxActiveWorkers cap is the on-by-default throttle; the load ceiling is an opt-in adaptive
	// layer on top). 0 (or negative) means "no load ceiling".
	defaultLoadCeiling = 0.0
)

// maxInt is the "unbounded" sentinel for the dispatch budget when MaxActiveWorkers is disabled.
const maxInt = int(^uint(0) >> 1)

// maxActiveWorkersFromEnv resolves the concurrent-worker cap from TTORCH_MAX_ACTIVE_WORKERS,
// falling back to defaultMaxActiveWorkers(poolMax) when unset or unparseable. An explicit value
// is honored verbatim — including 0 or negative, which DISABLE the cap (the off-switch) — so an
// operator can always turn the throttle off without recompiling.
func maxActiveWorkersFromEnv(poolMax int) int {
	if v := strings.TrimSpace(os.Getenv(envMaxActiveWorkers)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultMaxActiveWorkers(poolMax)
}

// defaultMaxActiveWorkers computes the default concurrent-worker cap: NumCPU/2, floored at
// minDefaultMaxActive (so a small machine still runs real parallelism) and capped at the
// worktree-pool size (so the governor's default can never EXCEED the hard pool ceiling). poolMax
// <= 0 (an unset pool, e.g. a bare struct) leaves the pool cap out of the min so the floor/NumCPU
// value stands.
func defaultMaxActiveWorkers(poolMax int) int {
	n := runtime.NumCPU() / 2
	if n < minDefaultMaxActive {
		n = minDefaultMaxActive
	}
	if poolMax > 0 && n > poolMax {
		n = poolMax
	}
	return n
}

// maxLandConcurrencyFromEnv resolves the per-tick land fan-out cap from
// TTORCH_MAX_LAND_CONCURRENCY, falling back to defaultMaxLandConcurrency when unset or
// unparseable. An explicit <= 0 DISABLES the cap (today's behavior — the land pass hands the
// whole gated set to the pipeline, which bounds its own internal fan-out).
func maxLandConcurrencyFromEnv() int {
	if v := strings.TrimSpace(os.Getenv(envMaxLandConcurrency)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultMaxLandConcurrency
}

// loadCeilingFromEnv resolves the 1-minute load-average ceiling from TTORCH_LOAD_CEILING,
// falling back to defaultLoadCeiling (OFF) when unset or unparseable. An explicit <= 0 keeps the
// ceiling disabled.
func loadCeilingFromEnv() float64 {
	if v := strings.TrimSpace(os.Getenv(envLoadCeiling)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultLoadCeiling
}

// runningWorkers counts the live tasks that are actively running a heavy worker harness — those
// in the 'active' state. That is the machine-load signal the MaxActiveWorkers cap throttles on:
// every active worker runs a full harness that fans build/test out to NumCPU. Tasks awaiting the
// manager (done/blocked/needs_input) or already torn down are not running a harness and are not
// counted (the land pass's own fan-out cap bounds the heavy validate load of done work). The
// count is deliberately status-only and kind-agnostic, so any active worker — daemon-dispatched
// or lead-driven — counts toward machine load, since the host does not care who launched it.
func runningWorkers(live []db.Task) int {
	n := 0
	for _, t := range live {
		if t.Status == db.StatusActive {
			n++
		}
	}
	return n
}

// dispatchBudget reports how many MORE workers this tick may dispatch under the MaxActiveWorkers
// cap, given the live set, and the current count of running workers (for the deferral log). When
// the cap is disabled (<= 0) the budget is maxInt — effectively unbounded, so the dispatch pass
// behaves exactly as it did before the governor existed. Otherwise it is MaxActiveWorkers minus
// the current running count, clamped at zero. This only ever bounds dispatch DOWNWARD; the pool
// cap and disjoint-footprint invariant remain the authoritative correctness gates.
func (sc *Scheduler) dispatchBudget(live []db.Task) (budget, active int) {
	active = runningWorkers(live)
	if sc.MaxActiveWorkers <= 0 {
		return maxInt, active // cap disabled — unbounded by the governor
	}
	budget = sc.MaxActiveWorkers - active
	if budget < 0 {
		budget = 0
	}
	return budget, active
}

// loadDefersDispatch reports whether the configured LoadCeiling is exceeded by the current
// 1-minute system load average (and the load it read, for the deferral log). It FAILS OPEN: when
// the ceiling is disabled (<= 0) or the load average cannot be read, it returns false so an
// unreadable loadavg never wedges dispatch — the always-enforceable MaxActiveWorkers cap remains
// the throttle in that case. loadAvg is the injectable reader (nil ⇒ systemLoadAvg).
func (sc *Scheduler) loadDefersDispatch() (over bool, load float64) {
	if sc.LoadCeiling <= 0 {
		return false, 0 // ceiling disabled
	}
	read := sc.loadAvg
	if read == nil {
		read = systemLoadAvg
	}
	load, ok := read()
	if !ok {
		return false, 0 // unreadable — fail OPEN (the hard max-active cap still applies)
	}
	return load > sc.LoadCeiling, load
}

// landCapReached reports whether the per-tick land fan-out cap (MaxLandConcurrency) has been hit
// for a tick that has already claimed claimed tasks. A cap of <= 0 disables the bound (today's
// behavior — claim the whole gated set).
func (sc *Scheduler) landCapReached(claimed int) bool {
	return sc.MaxLandConcurrency > 0 && claimed >= sc.MaxLandConcurrency
}

// systemLoadAvg returns the host's 1-minute load average and whether it could be read. It is the
// production reader behind Scheduler.loadAvg. It is portable across the platforms ttorch runs on
// — Linux via /proc/loadavg, darwin/BSD via `sysctl -n vm.loadavg` — and DEGRADES GRACEFULLY: any
// unreadable/unparseable source returns ok=false, which the governor treats as "no load limit"
// (fail open). It never errors out the caller.
func systemLoadAvg() (float64, bool) {
	switch runtime.GOOS {
	case "linux":
		return loadAvgProc()
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		return loadAvgSysctl()
	default:
		return 0, false // unknown platform — fail open
	}
}

// loadAvgProc reads the 1-minute load average from /proc/loadavg (Linux). The first whitespace
// field is the 1-minute average. Any read/parse failure returns ok=false (fail open).
func loadAvgProc() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// loadAvgSysctl reads the 1-minute load average from `sysctl -n vm.loadavg` (darwin/BSD), whose
// output looks like "{ 1.85 2.02 2.15 }". The first parseable float is the 1-minute average. Any
// exec/parse failure (sysctl missing, unexpected format) returns ok=false (fail open).
func loadAvgSysctl() (float64, bool) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, false
	}
	for _, f := range strings.Fields(string(out)) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			return v, true // first numeric token is the 1-minute average
		}
	}
	return 0, false
}

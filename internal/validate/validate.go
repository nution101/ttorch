// Package validate runs a repository's own checks (build / vet / lint / test) against
// a worker's worktree before delivery. Checks are auto-detected per ecosystem, or
// overridden by a repo-provided .ttorch/validate.sh.
//
// Trust: these are the repository's OWN commands (and any .ttorch/validate.sh),
// executed on the host with the operator's credentials. Run validation only against
// repositories and worker output you trust. Each step runs under a timeout
// (TTORCH_VALIDATE_TIMEOUT, default 10m) so a hung command cannot block indefinitely.
package validate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nution101/ttorch/internal/ciparity"
)

// Step is one named check to run in the worktree.
type Step struct {
	Name string
	Cmd  []string
}

// Result is the outcome of running a Step.
type Result struct {
	Name   string
	Passed bool
	Output string
	// Infra is set when a check could NOT complete because of a likely transient
	// infrastructure failure (e.g. the SSH connection to an offloaded build host was
	// severed mid-run) rather than the build/lint/test running and reporting a real
	// failure — and even bounded retries did not recover. Passed is still false (the gate
	// fails closed and never lets such a run merge), but Infra (and the InfraFailurePrefix
	// in Output) lets a caller tell a transient failure apart from broken code and re-prep
	// rather than treat the code as failing its checks. omitempty keeps the persisted
	// validate.json byte-identical for the common, non-infra case.
	Infra bool `json:",omitempty"`
}

// Detect returns the checks for a worktree: an explicit .ttorch/validate.sh override if
// present, else an ecosystem-appropriate default set, else nil (no checks detected).
func Detect(dir string) []Step {
	if fileExists(filepath.Join(dir, ".ttorch", "validate.sh")) {
		return []Step{{Name: "custom", Cmd: []string{"sh", ".ttorch/validate.sh"}}}
	}
	return DetectDefaults(dir)
}

// DetectDefaults returns the built-in, ecosystem-detected checks for dir, IGNORING any
// repo-provided .ttorch/validate.sh override. Detect uses these as its fallback; the
// trust gate uses them directly so a worker cannot redefine the gate's commands by
// committing a .ttorch/validate.sh on its own branch — only the fixed ttorch-defined
// steps run. Returns nil when no ecosystem is detected (the caller treats that as a
// hard block).
func DetectDefaults(dir string) []Step {
	if fileExists(filepath.Join(dir, "go.mod")) {
		return []Step{
			{Name: "build", Cmd: []string{"go", "build", "./..."}},
			{Name: "vet", Cmd: []string{"go", "vet", "./..."}},
			{Name: "fmt", Cmd: []string{"sh", "-c", `o=$(gofmt -l .); test -z "$o" || { echo "unformatted:"; echo "$o"; exit 1; }`}},
			{Name: "test", Cmd: []string{"go", "test", "./..."}},
		}
	}
	if scripts := packageScripts(filepath.Join(dir, "package.json")); scripts != nil {
		var steps []Step
		if _, ok := scripts["build"]; ok {
			steps = append(steps, Step{Name: "build", Cmd: []string{"npm", "run", "build"}})
		}
		if _, ok := scripts["lint"]; ok {
			steps = append(steps, Step{Name: "lint", Cmd: []string{"npm", "run", "lint"}})
		}
		if _, ok := scripts["test"]; ok {
			steps = append(steps, Step{Name: "test", Cmd: []string{"npm", "test"}})
		}
		return steps
	}
	return nil
}

// CIParitySteps converts CI-parity steps extracted from a repository's GitHub Actions
// workflows (see internal/ciparity) into runnable validate Steps. This lets a CI-parity
// run report per-step PASS/FAIL through the same Run/Result path as the heuristic checks,
// while being clearly distinct in source: these are the repo's actual CI commands rather
// than the ecosystem defaults from Detect/DetectDefaults.
func CIParitySteps(ciSteps []ciparity.Step) []Step {
	steps := make([]Step, 0, len(ciSteps))
	for _, s := range ciSteps {
		steps = append(steps, Step{Name: s.Label(), Cmd: s.Command()})
	}
	return steps
}

// DefaultTimeout bounds each check; override with TTORCH_VALIDATE_TIMEOUT.
const DefaultTimeout = 10 * time.Minute

func stepTimeout() time.Duration {
	if v := os.Getenv("TTORCH_VALIDATE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}

// Bounded retry of checks whose command failed to COMPLETE for a likely transient
// infrastructure reason (see classifyInfra). A genuine check failure is never retried.
const (
	// DefaultInfraRetries is how many extra attempts a likely-infra failure gets before it
	// is given up on; override with TTORCH_VALIDATE_INFRA_RETRIES.
	DefaultInfraRetries = 2
	// DefaultRetryBackoff is the pause between infra retries; override with
	// TTORCH_VALIDATE_RETRY_BACKOFF.
	DefaultRetryBackoff = 3 * time.Second
	// maxInfraRetries caps the configurable retry count so the loop is always bounded — a
	// misconfigured env value can never turn validation into a near-infinite retry storm.
	maxInfraRetries = 10
)

// InfraFailurePrefix tags the Output of a Result that failed only because its check could
// not complete (a likely transient/infra failure that survived all retries), so callers —
// and a human reading validate.json — can recognize it as distinct from broken code. Pairs
// with Result.Infra.
const InfraFailurePrefix = "[TRANSIENT/INFRA]"

// infraRetries is the bounded number of extra attempts a likely-infra failure gets.
// TTORCH_VALIDATE_INFRA_RETRIES overrides it; a missing/invalid value falls back to the
// default, a negative value is rejected, and the count is clamped to maxInfraRetries.
func infraRetries() int {
	if v := os.Getenv("TTORCH_VALIDATE_INFRA_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n > maxInfraRetries {
				return maxInfraRetries
			}
			return n
		}
	}
	return DefaultInfraRetries
}

// retryBackoff is the pause between infra retries. TTORCH_VALIDATE_RETRY_BACKOFF overrides
// it; a missing/invalid or negative value falls back to the default.
func retryBackoff() time.Duration {
	if v := os.Getenv("TTORCH_VALIDATE_RETRY_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return DefaultRetryBackoff
}

// sshTransportSignatures are substrings that indicate an ESTABLISHED ssh session carrying an
// offloaded check was SEVERED mid-run — the failure mode of a long check whose connection to
// the build host drops partway (the motivating case: a silent test, then a broken pipe).
// Matched case-insensitively. Three deliberate scoping choices keep this from masking a real
// failure (the prime directive — never retry-mask broken code; fail closed on ambiguity):
//
//   - Anchored to the ssh BINARY's diagnostics, never a bare network phrase. Generic phrases
//     like "connection refused"/"connection reset"/"lost connection" are EXCLUDED because
//     ordinary tools (a DB/HTTP client, curl, MySQL's "Lost connection to MySQL server") emit
//     them verbatim on their own failures.
//   - Limited to ESTABLISHED-session-severed signatures. Connect-TIME ssh failures
//     ("ssh: connect to host ...", kex/identification handshake errors) are EXCLUDED: an
//     offload host that is unreachable at connect time makes the build shim fall back to a
//     LOCAL run, so such errors never reach us as an offload failure — while a repo's own ssh
//     smoke check failing at connect ("service down") would, and must fail closed, not retry.
//   - classifyInfra treats these as infra ONLY when the exit code is 255 — ssh's exclusive
//     transport-failure code. A tool that actually ran and failed returns its OWN code
//     (1/2/101/...) which ssh forwards, so it is never misread as transport even when ssh
//     teardown noise appears in its output.
//
// Residual, documented tradeoff: a repo whose OWN checks use ssh against a flaky, long-lived
// session that severs mid-run (not at connect) with exit 255 is byte-for-byte indistinguishable
// from the offload transport and could be retried. This is inherent — the offload transport and
// a repo's ssh are the same binary — and is bounded (established-sever only, exit 255, flaky).
var sshTransportSignatures = []string{
	"client_loop:",              // ssh client loop aborting ("client_loop: ... Broken pipe")
	"ssh_packet_",               // ssh_packet_write_poll / ssh_packet_read, etc.
	"packet_write_wait:",        // "packet_write_wait: Connection to host port 22: Broken pipe"
	"write failed: broken pipe", // ssh transport write failed mid-stream
}

// genuineFailureMarkers indicate the underlying tool actually RAN and reported a failure,
// so a non-zero exit is a real check failure that must never be retried or masked as infra
// — even if a transport signature also appears (e.g. tests printed --- FAIL and THEN the
// connection dropped). classifyInfra checks these FIRST, so any match wins over a transport
// signature. This list biases toward FAIL-CLOSED: a marker firing only ever suppresses a
// retry, so it is safe to be generous. It is a SECONDARY guard — the exit-code gate already
// keeps a tool that ran and failed from being retried — so it need not be exhaustive, but
// covers the common runners/compilers as defense-in-depth. Matched case-sensitively against
// the stable output the tools emit; none is a substring of any transport signature, so a pure
// transport failure never trips one (in particular none matches the bare word "error", which
// would otherwise collide with "rsync error:").
var genuineFailureMarkers = []string{
	"--- FAIL",          // go test: a specific test failed
	"FAIL\t",            // go test: package result line, e.g. "FAIL\tpkg/path\t0.12s"
	"\nFAIL\n",          // go test: trailing summary
	"\nFAIL ",           // go test: trailing summary with detail
	"FAIL ",             // jest: "FAIL src/x.test.js" (and go test's leading-line FAIL)
	"panic:",            // a panic during a test/build run
	"build failed",      // generic build-tool failure
	"] Error ",          // make: "make: *** [Makefile:3: test] Error 7" (any exit code)
	"Error 1",           // make/sub-tool non-zero exit code line
	"Error 2",           //
	"FAILED",            // pytest -v ("FAILED test_x.py::t"), other runners' all-caps summaries
	"failed in ",        // pytest -q ("=== 1 failed in 0.12s ===")
	" failing",          // mocha ("1 failing")
	"Tests failed",      //
	"Test failed",       //
	"Tests:",            // jest summary block ("Tests:       1 failed, 2 passed")
	"tests failed,",     // jest/vitest aggregate ("1 of 3 tests failed,")
	"error[",            // rustc/cargo ("error[E0308]: mismatched types")
	"could not compile", // cargo ("error: could not compile `pkg` due to previous error")
	"error TS",          // tsc ("src/x.ts:1:1 - error TS2322: ...")
	"problems (",        // eslint summary ("✖ 5 problems (5 errors, 0 warnings)")
	"problem (",         // eslint summary, singular ("✖ 1 problem (1 error, 0 warnings)")
}

// classifyInfra reports whether a FAILED check's combined output + exit code looks like a
// transient infrastructure failure that should be retried, and (for logging) which signature
// matched. It is conservative and FAIL-CLOSED, in priority order:
//
//  1. Any genuineFailureMarker present => the tool ran and reported a failure => NOT infra.
//  2. An ssh established-session-severed signature AND exitCode == 255 => ssh's own transport
//     failure => infra. The 255 gate is the key tool-agnostic guard: ssh forwards a remote
//     tool's real exit code (never 255) when the tool actually runs, so a genuine failure —
//     even from a tool whose error vocabulary no genuineFailureMarker covers
//     (cargo/tsc/eslint/custom) — is never retried just because ssh teardown noise appears in
//     its output.
//  3. Anything else (an ambiguous non-zero exit, an ssh signature without a 255 exit, a
//     connect-time ssh error, or any rsync error — which has no transport-only exit code and
//     whose "code 12" covers genuine permission/transfer failures too) is treated as a real
//     failure. The retry only ever helps the clearly-transient established-sever case.
func classifyInfra(output string, exitCode int) (infra bool, reason string) {
	if hasGenuineFailureMarker(output) {
		return false, ""
	}
	if exitCode == 255 {
		low := strings.ToLower(output)
		for _, sig := range sshTransportSignatures {
			if strings.Contains(low, sig) {
				return true, sig
			}
		}
	}
	return false, ""
}

func hasGenuineFailureMarker(output string) bool {
	for _, m := range genuineFailureMarkers {
		if strings.Contains(output, m) {
			return true
		}
	}
	return false
}

// Run executes each step in dir under a per-step timeout, capturing combined output,
// and continues through all steps (so every failure is reported, not just the first).
func Run(dir string, steps []Step) []Result {
	to := stepTimeout()
	results := make([]Result, 0, len(steps))
	for _, s := range steps {
		results = append(results, runStep(dir, s, to))
	}
	return results
}

// attempt is the raw outcome of a single execution of a check's command.
type attempt struct {
	output   string
	passed   bool
	timedOut bool
	// exitCode is the command's process exit code (-1 if it was killed by a signal or could
	// not start). For offloaded checks this is the code the `make` shim propagates from ssh,
	// and ssh uses 255 exclusively for its OWN transport failures (it forwards the remote
	// command's real code otherwise) — see classifyInfra.
	exitCode int
}

// runStep executes a check, retrying ONLY when it fails for a likely transient/infra reason
// (classifyInfra) — up to infraRetries() extra attempts with a retryBackoff() pause between
// them. A passing attempt returns immediately. A genuine check failure (the tool ran and
// reported failures) and an ambiguous failure both fail closed with NO retry, so broken code
// is never retried into a pass. A timeout likewise fails closed (a genuinely hung check must
// not be masked as transient). When retries are exhausted and the final failure still looks
// like infra, the Result is marked Infra (and tagged with InfraFailurePrefix) so callers can
// distinguish a transient failure from broken code — but Passed stays false, fail-closed.
func runStep(dir string, s Step, to time.Duration) Result {
	retries := infraRetries()
	backoff := retryBackoff()

	var a attempt
	var infra bool
	for try := 0; ; try++ {
		a = runOnce(dir, s, to)
		if a.passed {
			return Result{Name: s.Name, Passed: true, Output: a.output}
		}
		if a.timedOut {
			return Result{Name: s.Name, Passed: false, Output: fmt.Sprintf("timed out after %s", to)}
		}
		var reason string
		infra, reason = classifyInfra(a.output, a.exitCode)
		if !infra || try >= retries {
			break
		}
		fmt.Fprintf(os.Stderr, "validate: check %q hit a likely transient/infra failure (%s); retrying (%d/%d)\n",
			s.Name, reason, try+1, retries)
		if backoff > 0 {
			time.Sleep(backoff)
		}
	}

	if infra {
		retryWord := "retries"
		if retries == 1 {
			retryWord = "retry"
		}
		note := fmt.Sprintf("%s validate check %q did not complete after %d %s due to a likely transient infrastructure failure (e.g. a dropped connection to the build host); the code was NOT shown to fail its checks.",
			InfraFailurePrefix, s.Name, retries, retryWord)
		if a.output != "" {
			note = note + "\n" + a.output
		}
		return Result{Name: s.Name, Passed: false, Infra: true, Output: note}
	}
	return Result{Name: s.Name, Passed: false, Output: a.output}
}

// runOnce executes a check's command exactly once under the per-step timeout, capturing
// combined output. It reports whether the process exited cleanly and whether the timeout
// fired (so the retry loop can fail a genuine hang closed rather than retry it).
func runOnce(dir string, s Step, to time.Duration) attempt {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	c := exec.CommandContext(ctx, s.Cmd[0], s.Cmd[1:]...)
	c.Dir = dir
	// Run the command in its own process group and kill the whole group on timeout,
	// so a hung command's children (servers, watchers) are reaped too.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process != nil {
			return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	exitCode := 0
	if err != nil {
		// -1 unless the process exited with a real status code (e.g. killed by a signal, or
		// the command could not start) — neither of which is ssh's 255 transport code, so
		// both correctly fail closed in classifyInfra.
		exitCode = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	return attempt{
		output:   strings.TrimRight(string(out), "\n"),
		passed:   err == nil,
		timedOut: ctx.Err() == context.DeadlineExceeded,
		exitCode: exitCode,
	}
}

// Failures returns the results that did not pass.
func Failures(results []Result) []Result {
	var f []Result
	for _, r := range results {
		if !r.Passed {
			f = append(f, r)
		}
	}
	return f
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func packageScripts(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil
	}
	return pkg.Scripts
}

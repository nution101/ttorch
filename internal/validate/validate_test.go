package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/ciparity"
)

func TestRun_ReportsEachStep(t *testing.T) {
	steps := []Step{
		{Name: "ok", Cmd: []string{"sh", "-c", "exit 0"}},
		{Name: "bad", Cmd: []string{"sh", "-c", "echo boom; exit 1"}},
		{Name: "ok2", Cmd: []string{"sh", "-c", "exit 0"}},
	}
	results := Run(t.TempDir(), steps)
	if len(results) != 3 {
		t.Fatalf("expected all 3 steps to run, got %d", len(results))
	}
	if !results[0].Passed || results[1].Passed || !results[2].Passed {
		t.Fatalf("unexpected pass/fail: %+v", results)
	}
	if results[1].Output != "boom" {
		t.Fatalf("expected captured output, got %q", results[1].Output)
	}
	if f := Failures(results); len(f) != 1 || f[0].Name != "bad" {
		t.Fatalf("Failures wrong: %+v", f)
	}
}

func TestDetect_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	steps := Detect(dir)
	names := stepNames(steps)
	want := []string{"build", "vet", "fmt", "test"}
	if len(names) != len(want) {
		t.Fatalf("go steps = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("go steps = %v, want %v", names, want)
		}
	}
}

func TestDetect_Node(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"scripts":{"test":"jest","lint":"eslint ."}}`), 0o644)
	names := stepNames(Detect(dir))
	// lint and test present; build absent.
	if len(names) != 2 {
		t.Fatalf("node steps = %v, want [lint test]", names)
	}
	if names[0] != "lint" || names[1] != "test" {
		t.Fatalf("node steps = %v, want [lint test]", names)
	}
}

func TestDetect_Override(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".ttorch"), 0o755)
	os.WriteFile(filepath.Join(dir, ".ttorch", "validate.sh"), []byte("#!/bin/sh\n"), 0o755)
	// Even with a go.mod, the explicit override wins.
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	steps := Detect(dir)
	if len(steps) != 1 || steps[0].Name != "custom" {
		t.Fatalf("override should yield a single custom step, got %+v", steps)
	}
}

func TestDetect_Empty(t *testing.T) {
	if steps := Detect(t.TempDir()); steps != nil {
		t.Fatalf("expected no steps for an unrecognized repo, got %+v", steps)
	}
}

func TestRun_Timeout(t *testing.T) {
	t.Setenv("TTORCH_VALIDATE_TIMEOUT", "300ms")
	results := Run(t.TempDir(), []Step{{Name: "slow", Cmd: []string{"sh", "-c", "sleep 5"}}})
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("slow step should fail by timeout, got %+v", results)
	}
	if !strings.Contains(results[0].Output, "timed out") {
		t.Fatalf("expected a timeout message, got %q", results[0].Output)
	}
}

func TestRun_FmtFailureNamesFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package x\nfunc  F( ){}\n"), 0o644)

	var fmtStep Step
	for _, s := range Detect(dir) {
		if s.Name == "fmt" {
			fmtStep = s
		}
	}
	if fmtStep.Name == "" {
		t.Fatal("no fmt step detected for a go.mod repo")
	}
	res := Run(dir, []Step{fmtStep})
	if res[0].Passed {
		t.Fatal("fmt should fail on a mis-formatted file")
	}
	if !strings.Contains(res[0].Output, "unformatted") || !strings.Contains(res[0].Output, "bad.go") {
		t.Fatalf("fmt failure should name the unformatted file; got %q", res[0].Output)
	}
}

func TestCIParitySteps_RunsThroughResultPath(t *testing.T) {
	ciSteps := []ciparity.Step{
		{Workflow: "ci.yml", Job: "build", Name: "ok", Run: "exit 0"},
		{Workflow: "ci.yml", Job: "build", Name: "bad", Run: "echo boom; exit 1"},
	}
	steps := CIParitySteps(ciSteps)
	if len(steps) != 2 || steps[0].Name != "ci(ci.yml/build): ok" {
		t.Fatalf("converted steps = %+v", steps)
	}
	results := Run(t.TempDir(), steps)
	if len(results) != 2 || !results[0].Passed || results[1].Passed {
		t.Fatalf("expected ok-pass/bad-fail, got %+v", results)
	}
	if f := Failures(results); len(f) != 1 || !strings.Contains(f[0].Output, "boom") {
		t.Fatalf("failure not captured: %+v", f)
	}
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// brokenPipe is the canonical transport-failure line the make-offload shim surfaces when
// the SSH connection to the build host is severed mid-run: a likely-infra failure.
const brokenPipe = `client_loop: ssh_packet_write_poll: Connection to 10.1.10.6 port 22: Broken pipe`

// countingStep returns a step whose shell body runs after recording one tally byte to a
// counter file, plus a reader for the run count — so a test can assert how many times the
// command actually executed (i.e. whether it was retried).
func countingStep(name, dir, body string) (Step, func(*testing.T) int) {
	count := filepath.Join(dir, "runs."+name)
	step := Step{Name: name, Cmd: []string{"sh", "-c", fmt.Sprintf("printf x >> %q\n%s", count, body)}}
	runs := func(t *testing.T) int {
		t.Helper()
		b, err := os.ReadFile(count)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatalf("read run counter: %v", err)
		}
		return len(b)
	}
	return step, runs
}

func TestRun_RetriesInfraFailureThenSucceeds(t *testing.T) {
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "2")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	// Fail once with a transport signature (exit 255, no test-FAIL marker), then succeed.
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo %q
exit 255`, marker, marker, brokenPipe)
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if len(res) != 1 || !res[0].Passed {
		t.Fatalf("infra failure should be retried into a pass, got %+v", res)
	}
	if res[0].Infra {
		t.Fatalf("a recovered run must not be marked Infra: %+v", res[0])
	}
	if n := runs(t); n != 2 {
		t.Fatalf("expected 1 retry (2 runs), got %d", n)
	}
}

func TestRun_ExhaustedInfraFailureMarkedAndFailsClosed(t *testing.T) {
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "2")
	dir := t.TempDir()
	body := fmt.Sprintf("echo %q\nexit 255", brokenPipe)
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %+v", res)
	}
	r := res[0]
	if r.Passed {
		t.Fatalf("a persistent infra failure must fail closed, got Passed=true: %+v", r)
	}
	if !r.Infra {
		t.Fatalf("exhausted infra failure should be marked Infra: %+v", r)
	}
	if !strings.Contains(r.Output, InfraFailurePrefix) {
		t.Fatalf("output should carry the infra prefix %q, got %q", InfraFailurePrefix, r.Output)
	}
	if !strings.Contains(r.Output, "Broken pipe") {
		t.Fatalf("output should preserve the underlying failure detail, got %q", r.Output)
	}
	if n := runs(t); n != 3 {
		t.Fatalf("expected initial + 2 retries (3 runs), got %d", n)
	}
}

func TestRun_GenuineFailureNotRetried(t *testing.T) {
	// A real test failure must fail closed immediately, even with a generous retry budget.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	body := "echo '--- FAIL: TestThing (0.00s)'\necho 'FAIL\tpkg/path\t0.01s'\nexit 1"
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("genuine failure must fail closed and not be Infra: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("genuine failure must NOT be retried; ran %d times", n)
	}
}

func TestRun_GenuineFailureWithTransportNoiseNotMasked(t *testing.T) {
	// The dangerous case: a real test failure whose output ALSO contains a transport
	// signature (tests printed --- FAIL, then the connection later dropped). The genuine
	// marker must win — never retry-masked as infra.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	body := fmt.Sprintf("echo '--- FAIL: TestSSH (0.00s)'\necho %q\nexit 255", brokenPipe)
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("a genuine failure with transport noise must not be masked as infra: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must not retry a genuine failure; ran %d times", n)
	}
}

func TestRun_AmbiguousFailureTreatedAsReal(t *testing.T) {
	// Non-zero exit with neither a transport signature nor a genuine marker: fail closed,
	// no retry-masking.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	step, runs := countingStep("lint", dir, "echo 'something went sideways'\nexit 7")

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("ambiguous failure must be treated as a real failure: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("ambiguous failure must NOT be retried; ran %d times", n)
	}
}

func TestRun_FlakyAppNetworkFailureNotMasked(t *testing.T) {
	// Regression for the masking finding: a real (flaky) check failure whose OWN output
	// contains a bare application network phrase ("Connection refused") must NOT be treated
	// as transport-infra and retried — otherwise a passing second attempt would mask the
	// failure into a green. The command here WOULD pass on a retry, so the only way this test
	// passes is if the first failure is correctly NOT retried (fails closed).
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo "Error: could not connect to server: Connection refused"
exit 1`, marker, marker)
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed {
		t.Fatalf("a flaky app-network failure must fail closed, not be retried into a pass: %+v", res[0])
	}
	if res[0].Infra {
		t.Fatalf("an app-level network failure must not be marked Infra: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must NOT retry an app-level failure; ran %d times", n)
	}
}

func TestRun_FlakyNonGoBuildFailureWithTransportDropNotMasked(t *testing.T) {
	// Regression for the second review finding: a non-Go tool (here a cargo/rustc-style
	// compile error) genuinely fails and the SSH link then drops mid-stream (exit 255), so the
	// output carries BOTH a real compiler error and a transport signature. The compiler error
	// marker must win — the step must fail closed and never be retried into a pass, even though
	// the command WOULD pass on a second run.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo "error[E0308]: mismatched types"
echo %q
exit 255`, marker, marker, brokenPipe)
	step, runs := countingStep("build", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed {
		t.Fatalf("a genuine non-Go build failure must fail closed, not be retried into a pass: %+v", res[0])
	}
	if res[0].Infra {
		t.Fatalf("a genuine compiler failure must not be marked Infra: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must NOT retry a genuine compiler failure; ran %d times", n)
	}
}

func TestRun_TransportNoiseWithToolExitCodeNotRetried(t *testing.T) {
	// The exit-code gate: ssh logged transport noise but the process returned its OWN non-255
	// code (the remote tool actually ran and failed, or the link recovered and the tool then
	// failed). ssh reserves 255 for its own transport failures, so a non-255 exit means a real
	// failure — it must fail closed with no retry, even though the command WOULD pass on retry.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo %q
echo "the tool then failed for real"
exit 1`, marker, marker, brokenPipe)
	step, runs := countingStep("test", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("an ssh signature without a 255 exit must fail closed as a real failure: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must NOT retry when exit code is not ssh's 255 transport code; ran %d times", n)
	}
}

func TestRun_FlakyRsyncFailureNotMasked(t *testing.T) {
	// Regression for the rsync finding: rsync has no transport-only exit code (code 12 also
	// covers genuine permission/transfer failures), so a check that exercises rsync and fails
	// must fail closed — never be retried into a pass — even though it WOULD pass on a retry.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo "rsync error: error in rsync protocol data stream (code 12) at io.c(228)"
exit 12`, marker, marker)
	step, runs := countingStep("deploy", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("a flaky rsync failure must fail closed, not be retried into a pass: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must NOT retry an rsync failure; ran %d times", n)
	}
}

func TestRun_FlakySshSmokeCheckConnectFailureNotMasked(t *testing.T) {
	// Regression for the connect-time finding: a repo's OWN ssh smoke check failing because its
	// target service is (flakily) down emits the same "ssh: connect to host" + exit 255 as the
	// offload transport. Because an unreachable offload host falls back to a local run instead,
	// a connect-time ssh error is NOT treated as offload-infra — so this genuine service-down
	// failure must fail closed, not be retried into a pass.
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "1ms")
	t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", "5")
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	body := fmt.Sprintf(`if [ -f %q ]; then exit 0; fi
touch %q
echo "ssh: connect to host appserver port 22: Connection refused"
exit 255`, marker, marker)
	step, runs := countingStep("smoke", dir, body)

	res := Run(dir, []Step{step})
	if res[0].Passed || res[0].Infra {
		t.Fatalf("a flaky ssh smoke-check connect failure must fail closed: %+v", res[0])
	}
	if n := runs(t); n != 1 {
		t.Fatalf("must NOT retry a connect-time ssh failure; ran %d times", n)
	}
}

func TestClassifyInfra(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		exit      int
		wantInfra bool
	}{
		// SSH established-session-severed failures (the broken-pipe family) with exit 255 =
		// ssh's own transport code: infra, retryable. These are the offload mid-run drop the
		// change exists to recover from.
		{"client_loop broken pipe", brokenPipe, 255, true},
		{"packet_write_wait", "packet_write_wait: Connection to 10.1.10.6 port 22: Broken pipe", 255, true},
		{"ssh write failed", "Write failed: Broken pipe", 255, true},

		// Connect-TIME ssh failures are NOT infra: an unreachable offload host makes the build
		// shim fall back to a local run, so these never reach us as an offload failure — while a
		// repo's OWN ssh smoke check failing at connect ("service down") would, and must fail
		// closed rather than be retried into a pass. (Regression for the connect-time finding.)
		// "closed by remote host" is also excluded: it is ambiguous between a mid-run sshd death
		// and a connect-time banner close (ssh_exchange_identification), so it fails closed.
		{"ssh connect refused not infra", "ssh: connect to host 10.1.10.6 port 22: Connection refused", 255, false},
		{"ssh connect timed out not infra", "ssh: connect to host 10.1.10.6 port 22: Connection timed out", 255, false},
		{"kex exchange not infra", "kex_exchange_identification: read: Connection reset by peer", 255, false},
		{"ssh_exchange_identification not infra", "ssh_exchange_identification: Connection closed by remote host", 255, false},
		{"closed by remote host not infra", "Connection to 10.1.10.6 closed by remote host.", 255, false},

		// rsync errors are NOT infra: rsync has no transport-only exit code (its "code 12" is a
		// catch-all that also covers genuine permission/partial-transfer failures), so a repo
		// whose own checks use rsync must fail closed rather than risk a retry-mask. (Regression
		// for the rsync finding.)
		{"rsync severed not infra", "rsync: connection unexpectedly closed (0 bytes received so far)", 12, false},
		{"rsync protocol error not infra", "rsync error: error in rsync protocol data stream (code 12) at io.c(228)", 12, false},
		{"rsync error at 255 not infra", "rsync error: error in rsync protocol data stream", 255, false},

		// THE exit-code gate (tool-agnostic guard): an ssh signature is present but the process
		// returned its OWN non-255 code — ssh forwarded a real remote exit, so the tool ran and
		// failed (or recovered the link and then failed). Must NOT be infra, regardless of the
		// transport noise in the output.
		{"ssh noise but tool exit 1", "client_loop: send disconnect: Broken pipe\nsomething failed", 1, false},
		{"ssh noise but tool exit 2", brokenPipe, 2, false},

		// Genuine tool failures: NOT infra (a genuine marker is present and wins, even at 255).
		{"genuine go test fail", "--- FAIL: TestX (0.00s)\nFAIL\tpkg\t0.1s", 1, false},
		{"genuine wins over transport noise", "--- FAIL: TestNet\n" + brokenPipe, 255, false},
		{"make error wins", "make: *** [Makefile:3: test] Error 7\n" + brokenPipe, 255, false},
		{"jest fail line", "FAIL src/db.test.js\n  Error: Connection refused\nTests: 1 failed, 0 passed", 1, false},
		{"pytest -q fail", "E   ConnectionRefusedError: Connection refused\n=== 1 failed in 0.12s ===", 1, false},
		{"mocha failing", "  1) DB inserts:\n     Error: Connection reset by peer\n  1 failing", 1, false},

		// Reviewer's "tool errored THEN ssh dropped (exit 255)" case for non-Go compilers: the
		// compiler's own error markers must win over the trailing transport signature.
		{"cargo error then drop", "error[E0308]: mismatched types\n" + brokenPipe + "\nerror: could not compile `pkg`", 255, false},
		{"tsc error then drop", "src/x.ts:1:1 - error TS2322: type mismatch\n" + brokenPipe, 255, false},
		{"eslint problems then drop", "/x.js\n  1:1  error  'a' is not defined\n✖ 3 problems (3 errors, 0 warnings)\n" + brokenPipe, 255, false},
		{"eslint single problem then drop", "✖ 1 problem (1 error, 0 warnings)\n" + brokenPipe, 255, false},

		// Regression for the masking finding: a real (possibly flaky) check whose OWN output
		// embeds a bare application/network error must NOT be read as transport-infra — those
		// phrases are not anchored to ssh/rsync, so they fail closed as real failures.
		{"app connection refused", "Error: could not connect to server: Connection refused", 1, false},
		{"mysql lost connection", "Lost connection to MySQL server during query", 1, false},
		{"app connection reset", "read tcp 1.2.3.4:5432: read: connection reset by peer", 1, false},
		{"curl smoke check refused", "curl: (7) Failed to connect to localhost port 8080: Connection refused", 7, false},
		{"no route generic", "dial tcp: connect: no route to host", 1, false},

		// Ambiguous / clean.
		{"ambiguous", "some tool said no\nexit status 7", 7, false},
		{"empty", "", 1, false},
		{"clean build error has no infra signal", "./x.go:3:2: undefined: foo", 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyInfra(tc.output, tc.exit)
			if got != tc.wantInfra {
				t.Fatalf("classifyInfra(%q, exit=%d) = %v, want %v", tc.output, tc.exit, got, tc.wantInfra)
			}
		})
	}
}

func TestInfraRetries_EnvParsing(t *testing.T) {
	cases := []struct {
		val  string
		set  bool
		want int
	}{
		{"", false, DefaultInfraRetries},
		{"0", true, 0},
		{"4", true, 4},
		{"-1", true, DefaultInfraRetries}, // negative rejected -> default
		{"abc", true, DefaultInfraRetries},
		{"9999", true, maxInfraRetries}, // clamped, never unbounded
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			if tc.set {
				t.Setenv("TTORCH_VALIDATE_INFRA_RETRIES", tc.val)
			} else {
				os.Unsetenv("TTORCH_VALIDATE_INFRA_RETRIES")
			}
			if got := infraRetries(); got != tc.want {
				t.Fatalf("infraRetries() with %q = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}

func TestRetryBackoff_EnvParsing(t *testing.T) {
	if got := retryBackoff(); got != DefaultRetryBackoff {
		os.Unsetenv("TTORCH_VALIDATE_RETRY_BACKOFF")
		if got = retryBackoff(); got != DefaultRetryBackoff {
			t.Fatalf("default backoff = %v, want %v", got, DefaultRetryBackoff)
		}
	}
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "250ms")
	if got := retryBackoff(); got.String() != "250ms" {
		t.Fatalf("backoff = %v, want 250ms", got)
	}
	t.Setenv("TTORCH_VALIDATE_RETRY_BACKOFF", "nonsense")
	if got := retryBackoff(); got != DefaultRetryBackoff {
		t.Fatalf("invalid backoff should fall back to default, got %v", got)
	}
}

func TestResult_InfraJSONOmitEmpty(t *testing.T) {
	// A non-infra Result must serialize exactly as before (no Infra key), keeping the
	// persisted validate.json byte-identical; an infra Result must round-trip the flag.
	plain, err := json.Marshal(Result{Name: "test", Passed: true, Output: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "Infra") {
		t.Fatalf("non-infra Result should omit Infra, got %s", plain)
	}
	infra := Result{Name: "test", Passed: false, Infra: true, Output: "x"}
	b, err := json.Marshal(infra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Infra":true`) {
		t.Fatalf("infra Result should serialize Infra, got %s", b)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != infra {
		t.Fatalf("round-trip mismatch: %+v vs %+v", back, infra)
	}
}

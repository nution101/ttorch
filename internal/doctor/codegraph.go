package doctor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CodegraphEnvVar is the opt-in switch for the codegraph worker code-navigation feature.
// It is DEFAULT OFF: unset (or set to a falsey value) leaves the feature disabled and
// ttorch behaves exactly as it does without codegraph. Set it to a truthy value
// (1/true/yes/on) to opt in. The feature is also evidence-gated — enabling it without
// codegraph actually installed is a clean no-op (a doctor note), never a failure.
const CodegraphEnvVar = "TTORCH_CODEGRAPH"

// lookCodegraph resolves the codegraph binary's path, indirected through a var so the
// present/absent branches are testable without depending on whether codegraph happens to
// be installed on the machine running the suite.
var lookCodegraph = func() (string, bool) {
	p, err := exec.LookPath("codegraph")
	if err != nil {
		return "", false
	}
	return p, true
}

// CodegraphEnabled reports whether the opt-in codegraph navigation feature is switched on
// via CodegraphEnvVar. Default off: an unset, empty, or falsey value is disabled.
func CodegraphEnabled() bool { return truthyEnv(os.Getenv(CodegraphEnvVar)) }

// truthyEnv reports whether an environment value reads as "on".
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// FindCodegraph reports the codegraph binary's path and whether it is installed.
// codegraph is OPTIONAL: ttorch never requires it and its absence is never an error.
func FindCodegraph() (string, bool) { return lookCodegraph() }

// reportCodegraph prints the codegraph line for the doctor report. codegraph backs the
// opt-in, default-off worker code-navigation feature, so the line is purely informational
// and never affects exit status:
//
//   - feature off          -> a one-line note that it is available and how to opt in
//   - on + codegraph present -> [ok] with the resolved path
//   - on + codegraph absent  -> [absent] plus a clear note that enabling without codegraph
//     installed is a clean no-op (navigation stays off), never a failure
func reportCodegraph(out io.Writer, enabled bool, path string, found bool) {
	switch {
	case !enabled:
		state := "not installed"
		if found {
			state = "installed"
		}
		fmt.Fprintf(out, "  [off]    %-9s — opt-in worker code-navigation (%s); set %s=1 to enable\n", "codegraph", state, CodegraphEnvVar)
	case found:
		fmt.Fprintf(out, "  [ok]     %-9s %s  (opt-in worker code-navigation enabled)\n", "codegraph", path)
	default:
		fmt.Fprintf(out, "  [absent] %-9s — %s=1 is set but codegraph is not installed; worker code-navigation stays off (no error)\n", "codegraph", CodegraphEnvVar)
	}
}

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// peerEnvSettings are the TTORCH_* variables peer.env may set: settings that tune how the fleet
// runs, none of which names a file or directory or a worker's identity. Every variable ttorch
// reads is either here or in peerEnvRefused (TestPeerEnvClassifiesEveryVariable).
var peerEnvSettings = []string{
	"TTORCH_BACKEND", "TTORCH_CODEGRAPH", "TTORCH_EFFORT", "TTORCH_GATE_TICK_BUDGET",
	"TTORCH_IDLE_NUDGE_GRACE", "TTORCH_LOAD_CEILING", "TTORCH_MANAGER_EFFORT",
	"TTORCH_MANAGER_MODEL", "TTORCH_MAX_ACTIVE_WORKERS", "TTORCH_MAX_CLAIMS_PER_TICK",
	"TTORCH_MAX_IDLE_NUDGES", "TTORCH_MAX_LAND_CONCURRENCY", "TTORCH_MAX_STALL_NUDGES",
	"TTORCH_MAX_WORKTREES", "TTORCH_MODEL", "TTORCH_NO_AUTOINIT", "TTORCH_NO_AUTOTRUST",
	"TTORCH_NO_GLOBAL_HOOKS", "TTORCH_NO_PROMPT_REMINDERS", "TTORCH_NO_STOP_REPORT",
	"TTORCH_SCHEDULER_AUTOSTART", "TTORCH_SERIALIZE_OVERLAP", "TTORCH_SKIP_SKILL_INSTALL",
	"TTORCH_STALL_AFTER", "TTORCH_STALL_NUDGE_GRACE", "TTORCH_STALL_REPEAT",
	"TTORCH_STALL_RERAISES", "TTORCH_TERMINAL", "TTORCH_TMUX_SESSION",
	"TTORCH_VALIDATE_INFRA_RETRIES", "TTORCH_VALIDATE_RETRY_BACKOFF", "TTORCH_VALIDATE_TIMEOUT",
	"TTORCH_WORKER_CLONES",
}

// TestPeerEnvRefusesEveryPathOverride: peer.env may not move anything ttorch reads or writes.
// Each path override is refused, the validate cache and claude's config file included.
func TestPeerEnvRefusesEveryPathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.env")
	for _, k := range []string{
		"TTORCH_HOME", "TTORCH_DB", "TTORCH_CLAUDE_DIR", "TTORCH_AGENTS_DIR", "TTORCH_BIN_DIR",
		"TTORCH_VALIDATE_CACHE_DIR", "TTORCH_CLAUDE_JSON",
	} {
		if err := os.WriteFile(path, []byte(k+"=/elsewhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := readPeerEnv(path); err == nil {
			t.Errorf("peer.env setting %s was accepted: %v", k, got)
		}
	}
}

// TestPeerEnvClassifiesEveryVariable reads every TTORCH_* name in ttorch's own code and the
// content it installs, and requires each to be refused in peer.env or listed in peerEnvSettings.
// A new variable that names a path then fails here until someone decides which it is, instead of
// becoming settable from peer.env by default.
func TestPeerEnvClassifiesEveryVariable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	literal := regexp.MustCompile(`"(TTORCH_[A-Z0-9_]+)"`)
	bare := regexp.MustCompile(`TTORCH_[A-Z0-9_]+`)
	found := map[string]string{}
	walk := func(dir string, match func(path string) bool, re *regexp.Regexp, group int) {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !match(path) {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				if _, ok := found[m[group]]; !ok {
					found[m[group]], _ = filepath.Rel(root, path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	goSource := func(path string) bool {
		return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
	}
	for _, dir := range []string{"cmd", "internal"} {
		walk(dir, goSource, literal, 1)
	}
	if b, err := os.ReadFile(filepath.Join(root, "content.go")); err == nil {
		for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = "content.go"
		}
	}
	// The hooks and skills ttorch installs run inside the sessions ensure-up starts.
	walk("content", func(string) bool { return true }, bare, 0)
	if len(found) < 20 {
		t.Fatalf("found only %d TTORCH_ names; the scan is not reading the source", len(found))
	}

	settings := map[string]bool{}
	for _, k := range peerEnvSettings {
		settings[k] = true
	}
	names := make([]string, 0, len(found))
	for k := range found {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch {
		case peerEnvRefused[k] && settings[k]:
			t.Errorf("%s is both refused and listed as a setting", k)
		case peerEnvRefused[k]:
			if peerEnvKey(k) {
				t.Errorf("%s is in peerEnvRefused but peerEnvKey accepts it", k)
			}
		case settings[k]:
			if !peerEnvKey(k) {
				t.Errorf("%s is listed as a setting but peerEnvKey refuses it", k)
			}
		default:
			t.Errorf("%s (read in %s) is neither refused in peer.env nor listed in peerEnvSettings: decide whether it names a path or an identity", k, found[k])
		}
	}
	for _, k := range peerEnvSettings {
		if _, ok := found[k]; !ok {
			t.Errorf("peerEnvSettings lists %s, which nothing reads any more", k)
		}
	}
	for k := range peerEnvRefused {
		if _, ok := found[k]; !ok {
			t.Errorf("peerEnvRefused lists %s, which nothing reads any more", k)
		}
	}
}

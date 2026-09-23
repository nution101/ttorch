package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nution101/ttorch/internal/livestate"
)

// workerSettingsHooks writes the worker settings for kind into a fresh worktree and returns
// the command of every hook it registered, keyed by Claude Code hook event. ok is false when
// no settings file was written.
func workerSettingsHooks(t *testing.T, kind string) (map[string][]string, bool) {
	t.Helper()
	t.Setenv("TTORCH_HOME", t.TempDir())
	wt := t.TempDir()
	if err := WriteWorkerSettings(kind, wt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json"))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("worker settings are not valid JSON: %v\n%s", err, b)
	}
	out := map[string][]string{}
	for event, groups := range s.Hooks {
		for _, g := range groups {
			if g.Matcher != nil {
				t.Errorf("%s: these events take no matcher, got %q", event, *g.Matcher)
			}
			for _, h := range g.Hooks {
				if h.Type != "command" {
					t.Errorf("%s: hook %q should be a command hook, got type %q", event, h.Command, h.Type)
				}
				out[event] = append(out[event], h.Command)
			}
		}
	}
	return out, true
}

// TestWriteWorkerSettings_ClaudeRegistersLifecycleHooks: the Claude Code worker settings wire
// UserPromptSubmit, Stop and SessionEnd to `ttorch hook <event>`, plus StopFailure, which is
// where Claude Code ends a turn that failed on an API error. The report-enforcing Stop hook
// stays as it was, beside the new one, and nothing else is registered.
func TestWriteWorkerSettings_ClaudeRegistersLifecycleHooks(t *testing.T) {
	hooks, ok := workerSettingsHooks(t, "claude")
	if !ok {
		t.Fatal("claude worker settings not written")
	}
	bin := "'" + filepath.Join(os.Getenv("TTORCH_HOME"), "bin", "ttorch") + "'"
	want := map[string][]string{
		"UserPromptSubmit": {bin + " hook turn-started || true"},
		"Stop":             {bin + " stop-hook", bin + " hook turn-ended || true"},
		"StopFailure":      {bin + " hook turn-ended || true"},
		"SessionEnd":       {bin + " hook session-ended || true"},
	}
	var events []string
	for e := range hooks {
		events = append(events, e)
	}
	sort.Strings(events)
	if got := strings.Join(events, ","); got != "SessionEnd,Stop,StopFailure,UserPromptSubmit" {
		t.Fatalf("hook events = %s, want SessionEnd,Stop,StopFailure,UserPromptSubmit", got)
	}
	for event, cmds := range want {
		if strings.Join(hooks[event], "\n") != strings.Join(cmds, "\n") {
			t.Errorf("%s hooks =\n  %s\nwant\n  %s", event, strings.Join(hooks[event], "\n  "), strings.Join(cmds, "\n  "))
		}
	}
}

// TestWriteWorkerSettings_OtherHarnessRegistersNone: a harness without lifecycle hooks gets no
// settings file (so no hooks at all) and reports no events.
func TestWriteWorkerSettings_OtherHarnessRegistersNone(t *testing.T) {
	for _, kind := range []string{"codex", ""} {
		if hooks, ok := workerSettingsHooks(t, kind); ok {
			t.Errorf("harness %q: want no settings file, got hooks %v", kind, hooks)
		}
		if evs := HooksFor(kind).Events(); len(evs) != 0 {
			t.Errorf("harness %q reports events %v, want none", kind, evs)
		}
	}
}

// TestHooksFor_ClaudeCoversEveryEvent: every lifecycle event Claude Code reports is wired to at
// least one of its hook events, so no event is claimed and then never recorded.
func TestHooksFor_ClaudeCoversEveryEvent(t *testing.T) {
	h := HooksFor("claude")
	if got := len(h.Events()); got != len(livestate.Events) {
		t.Fatalf("claude reports %d events, want all %d", got, len(livestate.Events))
	}
	for _, ev := range h.Events() {
		if len(h.NativeEvents(ev)) == 0 {
			t.Errorf("claude reports %s but wires no hook event for it", ev)
		}
	}
}

// TestReadHookRecord_OnlyForAHarnessWithTurnSignal: the same valid record on disk is a hook
// signal for a Claude Code worker and no signal for any other harness, so their liveness
// stays the pane heuristic alone.
func TestReadHookRecord_OnlyForAHarnessWithTurnSignal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.json")
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := livestate.WriteRecord(path, livestate.Record{Event: livestate.TurnStarted, TaskID: "t1", At: at}); err != nil {
		t.Fatal(err)
	}
	if r, ok := ReadHookRecord("claude", path, "t1"); !ok || r.Event != livestate.TurnStarted {
		t.Fatalf("claude: ReadHookRecord = (%+v, %v), want the turn-started record", r, ok)
	}
	for _, kind := range []string{"codex", ""} {
		if r, ok := ReadHookRecord(kind, path, "t1"); ok {
			t.Errorf("harness %q: ReadHookRecord = (%+v, true), want no hook signal", kind, r)
		}
	}
}

// oneEdgeHooks reports only some lifecycle events, to exercise reportsTurns.
type oneEdgeHooks []livestate.Event

func (h oneEdgeHooks) Events() []livestate.Event           { return h }
func (oneEdgeHooks) NativeEvents(livestate.Event) []string { return []string{"X"} }

// TestReportsTurns: a record is trusted only from a harness that reports both edges of a turn.
func TestReportsTurns(t *testing.T) {
	cases := []struct {
		events oneEdgeHooks
		want   bool
	}{
		{nil, false},
		{oneEdgeHooks{livestate.TurnStarted}, false},
		{oneEdgeHooks{livestate.TurnEnded}, false},
		{oneEdgeHooks{livestate.TurnEnded, livestate.SessionEnded}, false},
		{oneEdgeHooks{livestate.TurnStarted, livestate.TurnEnded}, true},
		{oneEdgeHooks{livestate.SessionEnded, livestate.TurnEnded, livestate.TurnStarted}, true},
	}
	for _, c := range cases {
		if got := reportsTurns(c.events); got != c.want {
			t.Errorf("reportsTurns(%v) = %v, want %v", c.events, got, c.want)
		}
	}
	if !reportsTurns(HooksFor("claude")) {
		t.Error("claude must report both edges of a turn")
	}
}

package livestate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// Pane captures used across the reconciler tests, one per class the pane heuristic knows.
const (
	paneBusy    = "✶ Working… (12s · esc to interrupt)\n│ > "
	paneIdle    = "all done\n│ > "
	paneStalled = "API Error: Response stalled mid-stream\n│ > "
	// paneUnrecognised has neither a busy marker the heuristic knows nor the input caret:
	// what a worker looks like if the harness redraws its screen in a new format.
	paneUnrecognised = "✢ Brewing… (4m 12s · ↓ 3.1k tokens)"
	// paneCaretOnly shows the input box with no busy marker the heuristic knows. The input
	// box stays on screen while the harness works, so this is also a busy worker whose busy
	// line changed format.
	paneCaretOnly = "✢ Brewing… (4m 12s · ↓ 3.1k tokens)\n│ > "
)

func rec(ev Event, at time.Time) Record { return Record{Event: ev, TaskID: "t1", At: at} }

// TestReconcile_StartedUnrecognisedPaneIsBusy: the harness said a turn started and has not
// said it ended, so the worker is busy even though the pane text matches nothing the
// heuristic knows. The pane alone reads it as not busy.
func TestReconcile_StartedUnrecognisedPaneIsBusy(t *testing.T) {
	for _, pane := range []string{paneUnrecognised, paneCaretOnly, ""} {
		if Busy(pane) {
			t.Fatalf("fixture %q must not be busy to the pane heuristic", pane)
		}
		if got := Reconcile(rec(TurnStarted, t0.Add(-time.Minute)), true, pane, t0); got != StateBusy {
			t.Errorf("started a minute ago, pane %q: got %v, want busy", pane, got)
		}
	}
}

// TestReconcile_EndedIdlePaneIsIdle: the hook and the pane agree the turn is over.
func TestReconcile_EndedIdlePaneIsIdle(t *testing.T) {
	if got := Reconcile(rec(TurnEnded, t0.Add(-time.Minute)), true, paneIdle, t0); got != StateIdle {
		t.Fatalf("ended + idle pane: got %v, want idle", got)
	}
}

// TestReconcile_NoRecordIsTodaysPaneHeuristic: with no hook signal the result is exactly
// what the pane heuristic gives, for every class of pane, and "busy" still means Busy(pane),
// which is what the watcher and `ttorch status` key on.
func TestReconcile_NoRecordIsTodaysPaneHeuristic(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want State
	}{
		{"empty", "", StateUnknown},
		{"blank lines", "\n\n  \n", StateUnknown},
		{"boxed idle prompt", "│ > Try \"edit this file\"                    │", StateIdle},
		{"bare caret", "all set\n> ", StateIdle},
		{"working", "✶ Working… (12s · esc to interrupt)", StateBusy},
		{"thinking over a caret", "Thinking about it\n│ > ", StateBusy},
		{"compacting", "Compacting conversation…", StateBusy},
		{"shell prompt after a crash", "command not found\nbrian@host ~ $ ", StateUnknown},
		{"stall at the prompt", paneStalled, StateIdle},
		{"stall without a caret", "API Error: Connection closed mid-response", StateUnknown},
		{"rate limit at the prompt", "API Error: 429 rate limit exceeded\n│ > ", StateIdle},
		{"unrecognised busy line", paneUnrecognised, StateUnknown},
		{"unrecognised busy line over a caret", paneCaretOnly, StateIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A zero record, a zero clock: nothing about the record may leak in when ok is false.
			got := Reconcile(Record{}, false, c.pane, time.Time{})
			if got != c.want {
				t.Errorf("Reconcile(no record, %q) = %v, want %v", c.pane, got, c.want)
			}
			if (got == StateBusy) != Busy(c.pane) {
				t.Errorf("Reconcile(no record, %q) busy = %v, but Busy(pane) = %v", c.pane, got == StateBusy, Busy(c.pane))
			}
			// A record that is present but was rejected by ReadRecord carries ok=false too; its
			// fields must not matter.
			if got := Reconcile(rec(TurnStarted, t0), false, c.pane, t0); got != c.want {
				t.Errorf("Reconcile(rejected started record, %q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

// TestReconcile_Precedence walks every row of the table in signals.go.
func TestReconcile_Precedence(t *testing.T) {
	fresh := t0.Add(-time.Minute)
	cases := []struct {
		name string
		rec  Record
		pane string
		want State
	}{
		// Row 1: busy text wins over any record, including one that says the turn ended
		// (a Stop the worker Stop hook blocked carries on without a new prompt).
		{"ended, busy pane", rec(TurnEnded, fresh), paneBusy, StateBusy},
		{"session ended, busy pane", rec(SessionEnded, fresh), paneBusy, StateBusy},
		{"started, busy pane", rec(TurnStarted, fresh), paneBusy, StateBusy},
		// Row 2: a stall on screen means the turn died without a Stop; the pane decides.
		{"started, stalled pane", rec(TurnStarted, fresh), paneStalled, StateIdle},
		{"started, stall without caret", rec(TurnStarted, fresh), "API Error: Connection closed mid-response", StateUnknown},
		// Row 4: a fresh start holds the worker busy over a pane that does not show it.
		{"started, idle-looking pane", rec(TurnStarted, fresh), paneIdle, StateBusy},
		{"started, unrecognised pane", rec(TurnStarted, fresh), paneUnrecognised, StateBusy},
		{"started just now", rec(TurnStarted, t0), paneUnrecognised, StateBusy},
		{"started just under the max age", rec(TurnStarted, t0.Add(-StartedMaxAge+time.Second)), paneUnrecognised, StateBusy},
		// Row 5: too old, or from the future, and the pane decides.
		{"started at the max age", rec(TurnStarted, t0.Add(-StartedMaxAge)), paneUnrecognised, StateUnknown},
		{"started long ago, idle pane", rec(TurnStarted, t0.Add(-3*time.Hour)), paneIdle, StateIdle},
		{"started in the future", rec(TurnStarted, t0.Add(time.Minute)), paneUnrecognised, StateUnknown},
		// Row 6: the turn ended; idle unless the pane shows busy text (row 1).
		{"ended, unrecognised pane", rec(TurnEnded, fresh), paneUnrecognised, StateIdle},
		{"ended, no caret", rec(TurnEnded, fresh), "", StateIdle},
		{"ended long ago, idle pane", rec(TurnEnded, t0.Add(-48*time.Hour)), paneIdle, StateIdle},
		// Row 7: the session exited; the pane decides.
		{"session ended, idle pane", rec(SessionEnded, fresh), paneIdle, StateIdle},
		{"session ended, shell prompt", rec(SessionEnded, fresh), "brian@host ~ $ ", StateUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Reconcile(c.rec, true, c.pane, t0); got != c.want {
				t.Errorf("Reconcile(%s at %v, %q) = %v, want %v", c.rec.Event, c.rec.At, c.pane, got, c.want)
			}
		})
	}
}

// TestRecordRoundTrip: a written record reads back whole, the write leaves no temporary
// file behind, and a later write replaces the earlier one by renaming a new file over it
// rather than rewriting it in place, which is what keeps a concurrent reader from ever
// seeing a partial record.
func TestRecordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "tasks", "t1", "hook.json")
	if err := WriteRecord(path, rec(TurnStarted, t0)); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadRecord(path, "t1")
	if !ok || got.Event != TurnStarted || got.TaskID != "t1" || !got.At.Equal(t0) {
		t.Fatalf("round trip = (%+v, %v), want turn-started for t1 at %v", got, ok, t0)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(path, rec(TurnEnded, t0.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadRecord(path, "t1"); !ok || got.Event != TurnEnded {
		t.Fatalf("second write should replace the first, got (%+v, %v)", got, ok)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, second) {
		t.Fatal("the second write rewrote the record in place; it should rename a new file over it")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "hook.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the record dir should hold only hook.json, has %v", names)
	}
}

// TestReadRecord_CorruptIsNoSignal: anything that is not a whole, valid record for this
// task reads as no hook signal, and so the reconciler gives the pane heuristic's answer.
// Each case is written as if it were a turn-started record, the one that could otherwise
// hold a worker busy.
func TestReadRecord_CorruptIsNoSignal(t *testing.T) {
	valid := `{"event":"turn-started","task_id":"t1","at":"2026-09-01T11:59:00Z"}`
	cases := []struct {
		name string
		body string
	}{
		{"empty file", ""},
		{"truncated", valid[:len(valid)-8]},
		{"not json", "turn-started t1"},
		{"json array", `["turn-started","t1"]`},
		{"unknown event", `{"event":"turn-paused","task_id":"t1","at":"2026-09-01T11:59:00Z"}`},
		{"no event", `{"task_id":"t1","at":"2026-09-01T11:59:00Z"}`},
		{"no timestamp", `{"event":"turn-started","task_id":"t1"}`},
		{"bad timestamp", `{"event":"turn-started","task_id":"t1","at":"yesterday"}`},
		{"another task", `{"event":"turn-started","task_id":"t2","at":"2026-09-01T11:59:00Z"}`},
		{"no task", `{"event":"turn-started","at":"2026-09-01T11:59:00Z"}`},
		{"oversized", valid[:len(valid)-1] + `,"pad":"` + strings.Repeat("x", maxRecordBytes) + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hook.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			r, ok := ReadRecord(path, "t1")
			if ok {
				t.Fatalf("ReadRecord(%q) = (%+v, true), want no hook signal", c.body, r)
			}
			for _, pane := range []string{paneUnrecognised, paneIdle, paneBusy} {
				if got, want := Reconcile(r, ok, pane, t0), paneState(pane); got != want {
					t.Errorf("corrupt record, pane %q: got %v, want the pane heuristic's %v", pane, got, want)
				}
			}
		})
	}
	// The valid body really is accepted, so the cases above fail for their own reason.
	path := filepath.Join(t.TempDir(), "hook.json")
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadRecord(path, "t1"); !ok {
		t.Fatal("the valid control body should read as a record")
	}
}

// TestReadRecord_MissingOrUnreadable: no file, a directory in its place, or an empty task
// id all read as no hook signal.
func TestReadRecord_MissingOrUnreadable(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadRecord(filepath.Join(dir, "absent.json"), "t1"); ok {
		t.Error("a missing record should be no hook signal")
	}
	if _, ok := ReadRecord(dir, "t1"); ok {
		t.Error("a directory at the record path should be no hook signal")
	}
	path := filepath.Join(dir, "hook.json")
	if err := WriteRecord(path, Record{Event: TurnStarted, TaskID: "", At: t0}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadRecord(path, ""); ok {
		t.Error("an empty task id must never match a record")
	}
}

func TestParseEvent(t *testing.T) {
	for _, e := range Events {
		if got, ok := ParseEvent(string(e)); !ok || got != e {
			t.Errorf("ParseEvent(%q) = (%q, %v)", e, got, ok)
		}
	}
	for _, s := range []string{"", "Stop", "turn_started", "TURN-STARTED"} {
		if _, ok := ParseEvent(s); ok {
			t.Errorf("ParseEvent(%q) should be unknown", s)
		}
	}
}

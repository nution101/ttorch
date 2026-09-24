package tmux

import (
	"reflect"
	"testing"
)

// TestTypeLine_TypesWithoutEnter: TypeLine checks the pane mode, types the text literally, and
// sends no Enter, so the caller can inspect the input before submitting it.
func TestTypeLine_TypesWithoutEnter(t *testing.T) {
	pinReadOnlyView(t, true)
	log := installFakeTmux(t)
	if err := TypeLine("s", "w", "hello there"); err != nil {
		t.Fatalf("TypeLine: %v", err)
	}
	want := [][]string{
		{"display-message", "-p", "-t", "s:w", "#{pane_in_mode}"},
		{"send-keys", "-c", "", "-t", "s:w", "-l", "hello there"},
	}
	if got := readInvocations(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("invocations =\n%v\nwant\n%v", got, want)
	}
}

// TestTypeLine_RefusesCopyMode: a pane in copy-mode gets nothing typed into it.
func TestTypeLine_RefusesCopyMode(t *testing.T) {
	pinReadOnlyView(t, true)
	log := installFakeTmux(t)
	t.Setenv("FAKE_DISPLAY", "1") // #{pane_in_mode} = 1
	if err := TypeLine("s", "w", "hello"); err == nil {
		t.Fatal("TypeLine into a copy-mode pane must refuse")
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "send-keys" {
			t.Fatalf("TypeLine sent keys into a copy-mode pane: %v", inv)
		}
	}
}

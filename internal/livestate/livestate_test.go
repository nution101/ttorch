package livestate

import "testing"

func TestBusy(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"empty", "", false},
		{"idle prompt", "│ > Try \"edit this file\"", false},
		{"esc to interrupt", "Compacting… (esc to interrupt)", true},
		{"working ellipsis", "✶ Working… (12s)", true},
		{"working dots", "Working... (3s · esc to cancel)", true},
		{"thinking", "Thinking about the problem", true},
		{"generating", "Generating response", true},
		{"case insensitive", "ESC TO INTERRUPT", true},
		{"substring inside larger output", "blah blah\nesc to interrupt\nmore", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Busy(c.pane); got != c.want {
				t.Errorf("Busy(%q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

func TestStalled(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"empty", "", false},
		{"idle prompt", "│ > Try \"edit this file\"", false},
		{"working", "✶ Working… (12s)", false},
		{"api stall", "API Error: Response stalled mid-stream", true},
		{"case insensitive", "api error: response STALLED MID-STREAM", true},
		{"no hyphen variant", "Response stalled mid stream, retrying", true},
		{"in larger output", "some logs\nAPI Error: Response stalled mid-stream\n│ >", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Stalled(c.pane); got != c.want {
				t.Errorf("Stalled(%q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

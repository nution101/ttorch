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
		{"compacting", "Compacting conversation…", true},
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

func TestIdle(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"empty", "", false},
		{"blank lines only", "\n\n   \n", false},
		{"boxed idle prompt", "│ > Try \"edit this file\"                    │", true},
		{"bare caret prompt", "all set\n> ", true},
		{"boxed empty caret", "╭───────────╮\n│ >         │\n╰───────────╯", true},
		{"busy is never idle", "│ > something\n✶ Working… (12s · esc to interrupt)", false},
		{"thinking is never idle", "Thinking about it\n│ > ", false},
		{"compacting is never idle", "Compacting conversation…\n│ > ", false},
		{"shell prompt is not idle", "command not found\nbrian@host ~ $ ", false},
		{"no caret at all", "some output\nmore output", false},
		{"caret only mid-line never counts", "see the diff -> here", false},
		{"idle prompt after stall error", "API Error: Response stalled mid-stream\n│ > ", true},
		{"markdown blockquote in ended turn still idle", "> a quoted line\n│ > ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Idle(c.pane); got != c.want {
				t.Errorf("Idle(%q) = %v, want %v", c.pane, got, c.want)
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
		{"stalled mid-stream", "API Error: Response stalled mid-stream", true},
		{"stalled case insensitive", "api error: response STALLED MID-STREAM", true},
		{"stalled no hyphen variant", "Response stalled mid stream, retrying", true},
		{"stalled in larger output", "some logs\nAPI Error: Response stalled mid-stream\n│ >", true},
		{"closed mid-response", "API Error: Connection closed mid-response", true},
		{"closed case insensitive", "api error: CONNECTION CLOSED MID-RESPONSE", true},
		{"closed no hyphen variant", "Connection closed mid response, retrying", true},
		{"closed in larger output", "some logs\nAPI Error: Connection closed mid-response\n│ >", true},
		{"non-stall rate limit", "API Error: 429 rate limit exceeded", false},
		{"non-stall auth", "API Error: 401 invalid x-api-key", false},
		{"non-stall request timeout", "API Error: Request timed out", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Stalled(c.pane); got != c.want {
				t.Errorf("Stalled(%q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

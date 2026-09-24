package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTmux is a stand-in tmux binary for the one function that talks to tmux directly. It logs
// each invocation's arguments (tab-separated, one per line) to $WD_FAKE_LOG, answers
// capture-pane with $WD_FAKE_CAPTURE (exiting $WD_FAKE_CAPTURE_EXIT), and succeeds silently on
// everything else.
const fakeTmux = `#!/bin/sh
{ first=1; for a in "$@"; do if [ $first = 1 ]; then printf '%s' "$a"; first=0; else printf '\t%s' "$a"; fi; done; printf '\n'; } >>"$WD_FAKE_LOG"
case "$1" in
capture-pane) printf '%s' "$WD_FAKE_CAPTURE"; exit "${WD_FAKE_CAPTURE_EXIT:-0}" ;;
esac
exit 0
`

// installFakeTmux puts fakeTmux first on PATH for this test and returns its invocation log, so
// the test can never reach a real tmux server.
func installFakeTmux(t *testing.T, capture string, captureExit string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fakeTmux), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "invocations.log")
	t.Setenv("WD_FAKE_LOG", log)
	t.Setenv("WD_FAKE_CAPTURE", capture)
	t.Setenv("WD_FAKE_CAPTURE_EXIT", captureExit)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func keysSent(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var sent []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "send-keys") {
			sent = append(sent, l)
		}
	}
	return sent
}

// TestSubmitWakeIfConfirmed: the function that presses Enter reads the pane itself and sends
// the key only when the input holds exactly the wake line at an idle prompt. Anything else, or
// a capture that fails, sends no key at all.
func TestSubmitWakeIfConfirmed(t *testing.T) {
	cut := strings.LastIndex(wakeLine[:60], " ")              // wrap at a space, as the harness does
	mid := strings.Index(wakeLine, "scheduler") + len("sche") // inside a word
	cases := []struct {
		name    string
		capture string
		exit    string
		enter   bool
	}{
		{"exactly the wake", inputPane(wakeLine), "0", true},
		{"wrapped wake", inputPane(wakeLine[:cut], wakeLine[cut+1:]), "0", true},
		{"wake split mid-word", inputPane(wakeLine[:mid], wakeLine[mid:]), "0", false},
		{"lead typed after the wake", inputPane(wakeLine + " and this"), "0", false},
		{"lead's draft only", inputPane("can you check the"), "0", false},
		{"empty input", inputPane(""), "0", false},
		{"manager went busy", strings.Replace(spinnerManagerPane, "❯ ", "❯ "+wakeLine, 1), "0", false},
		{"capture failed", "", "1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := installFakeTmux(t, c.capture, c.exit)
			err := submitWakeIfConfirmed("wd-test-session")
			sent := keysSent(t, log)
			if c.enter {
				if err != nil || len(sent) != 1 || !strings.HasSuffix(sent[0], "\t-t\twd-test-session:manager\tEnter") {
					t.Fatalf("err=%v sent=%q; want exactly one Enter to the manager window", err, sent)
				}
				return
			}
			if err == nil || len(sent) != 0 {
				t.Fatalf("err=%v sent=%q; want a refusal and no keys", err, sent)
			}
		})
	}
}

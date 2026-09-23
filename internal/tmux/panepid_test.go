package tmux

import "testing"

// PanePIDErr separates "tmux could not answer" from a pid, which PanePID folds to 0: a
// fingerprint check must read the first as unknown, never as a missing process.
func TestPanePIDErr(t *testing.T) {
	installFakeTmux(t)

	t.Setenv("FAKE_LIST_PANES", "4242\n")
	if pid, err := PanePIDErr("s", "wk-a"); err != nil || pid != 4242 {
		t.Fatalf("PanePIDErr = %d, %v; want 4242, nil", pid, err)
	}
	if pid := PanePID("s", "wk-a"); pid != 4242 {
		t.Fatalf("PanePID = %d, want 4242", pid)
	}

	for _, tc := range []struct{ name, out, exit string }{
		{"list-panes fails", "", "1"},
		{"empty answer", "", "0"},
		{"not a number", "%1\n", "0"},
		{"zero", "0\n", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_LIST_PANES", tc.out)
			t.Setenv("FAKE_LIST_PANES_EXIT", tc.exit)
			if pid, err := PanePIDErr("s", "wk-a"); err == nil {
				t.Fatalf("PanePIDErr = %d, nil; want an error", pid)
			}
			if pid := PanePID("s", "wk-a"); pid != 0 {
				t.Fatalf("PanePID = %d, want 0", pid)
			}
		})
	}
}

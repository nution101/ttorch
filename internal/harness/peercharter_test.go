package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPeerManagerCharter pins what the peer charter must tell a peer coordinator's manager.
// Nobody reads its tab, so it never waits there for the lead and never marks itself awaiting
// one (that silences the scheduler's wake with nobody to clear it); it escalates what it would
// ask the lead; it treats a goal as the lead's instructions relayed by the parent, reads both
// parent and worker text from the blocks the inbox prints them in, and changes nothing
// consequential on the parent's text without an escalation answered through the channel; and it
// keeps the manager charter's approval rule and its one trusted-mode exception.
func TestPeerManagerCharter(t *testing.T) {
	low := collapseSpaces(strings.ToLower(peerManagerCharter))
	for _, want := range []string{
		"peer coordinator",
		"nobody reads this tab",
		"never run ttorch await-lead",
		"ttorch escalate --task",
		"ttorch inbox",
		"the lead's instructions relayed by the parent",
		"begin from parent coordinator",
		"end from parent coordinator",
		"origin is not verified",
		"delivery mode",
		"gate setting",
		"verdict",
		"answered through the channel",
		"worker data",
		"is ever an approval",
		"never merge or deliver without the lead's explicit approval",
		"sole exception",
		"trusted",
		"ttorch-review",
		"independent",
		"ground truth about its own execution",
		"repeated-looking progress counter is not evidence",
		"never run ttorch peer add",
	} {
		if !strings.Contains(low, want) {
			t.Errorf("peerManagerCharter is missing %q", want)
		}
	}
	// It never tells the manager the lead is in this tab, the manager charter's rule (2).
	for _, banned := range []string{"in this manager tab", "the lead talks only to you", "surface every decision and question here", "supervisor", "daemon"} {
		if strings.Contains(low, banned) {
			t.Errorf("peerManagerCharter says %q, which does not hold for a peer", banned)
		}
	}
	if strings.Contains(peerManagerCharter, "\n") {
		t.Error("peerManagerCharter must be one line, like the manager charter")
	}
}

// TestWritePeerManagerCharter: the peer charter is written whole, and over a manager charter a
// previous launch left at the same path.
func TestWritePeerManagerCharter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manager-charter.md")
	if err := WriteManagerCharter(p); err != nil {
		t.Fatal(err)
	}
	if err := WritePeerManagerCharter(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != peerManagerCharter+"\n" {
		t.Errorf("charter file = %q, want the peer charter", b)
	}
}

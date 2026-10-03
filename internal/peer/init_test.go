package peer

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestInitArgs pins the init session's command line: no pty, then the destination and the one
// remote command, which names the remote ttorch and nothing the parent was sent.
func TestInitArgs(t *testing.T) {
	want := []string{"-T", "--", "lead@build-host", ".ttorch/bin/ttorch peer init"}
	if got := InitArgs("lead@build-host", DefaultRemoteTtorch); !reflect.DeepEqual(got, want) {
		t.Errorf("InitArgs = %q, want %q", got, want)
	}
}

// TestValidRemoteTtorch: the remote command is run by the peer's shell, so the path must be one
// plain word.
func TestValidRemoteTtorch(t *testing.T) {
	for _, ok := range []string{".ttorch/bin/ttorch", "~/.ttorch/bin/ttorch", "/opt/ttorch/bin/ttorch", "bin/ttorch-1.2"} {
		if err := ValidRemoteTtorch(ok); err != nil {
			t.Errorf("ValidRemoteTtorch(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "ttorch; id", "tt orch", "$(id)", "`id`", "a|b", "~root/x", "a\nb", "a'b", `a"b`} {
		if err := ValidRemoteTtorch(bad); err == nil {
			t.Errorf("ValidRemoteTtorch(%q) accepted", bad)
		}
	}
}

// TestRunInitAnswers: init's refusal comes back as a RemoteError, a session with nothing to read
// as a TransportError that says where ttorch was looked for, and its stderr reaches the lead.
func TestRunInitAnswers(t *testing.T) {
	ssh, _ := shim(t, `echo 'from the peer' >&2; printf '%s\n' '{"protocol":1,"verb":"init","ok":false,"error":{"code":"wrong_parent","message":"another parent"}}'; exit 1`)
	var stderr bytes.Buffer
	_, err := RunInit(context.Background(), ssh, "lead@build-host", DefaultRemoteTtorch, 20*time.Second, InitRequest{Name: "b"}, &stderr)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeWrongParent {
		t.Errorf("a refused init = %v, want a RemoteError with wrong_parent", err)
	}
	if !strings.Contains(stderr.String(), "from the peer") {
		t.Errorf("the session's stderr did not reach the lead: %q", stderr.String())
	}
	ssh, _ = shim(t, `echo 'sh: .ttorch/bin/ttorch: not found' >&2; exit 127`)
	_, err = RunInit(context.Background(), ssh, "lead@build-host", DefaultRemoteTtorch, 20*time.Second, InitRequest{Name: "b"}, &stderr)
	var te *TransportError
	if !errors.As(err, &te) || te.ExitCode != 127 || !strings.Contains(err.Error(), DefaultRemoteTtorch) {
		t.Errorf("no ttorch on the peer = %v, want a TransportError naming %s", err, DefaultRemoteTtorch)
	}
	if _, err := RunInit(context.Background(), ssh, "-oProxyCommand=x", DefaultRemoteTtorch, time.Second, InitRequest{}, &stderr); err == nil {
		t.Error("an option as the destination was run")
	}
}

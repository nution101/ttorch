package peer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pinnedArgs is the exact ssh command line of a control call. Every option is there to keep the
// call on the control key alone, with nothing of the lead's ssh setup reaching it:
//
//	-F none                     no ssh_config at all: no Host block can add a ProxyCommand, a
//	                            ControlMaster to share, an IdentityFile, a certificate or a
//	                            forwarded agent
//	BatchMode=yes               never prompt; fail instead
//	IdentitiesOnly=yes, -i key  offer the control key and nothing else
//	IdentityAgent=none          never ask an agent, so the lead's agent keys, which run anything,
//	                            are never offered
//	StrictHostKeyChecking=yes   an unknown or changed host key fails the call
//	UpdateHostKeys=no           and the call never writes known_hosts
//
// The verb is the one remote argument; the peer reads it from SSH_ORIGINAL_COMMAND.
func pinnedArgs(key, dest, verb string) []string {
	return []string{
		"-F", "none",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-i", key,
		"--", dest, verb,
	}
}

// TestClientArgsArePinned: a control call runs ssh with exactly these arguments, for every verb.
func TestClientArgsArePinned(t *testing.T) {
	c := Client{SSH: "ssh", Dest: "ttorch@build-host", Key: "/keys/build/control"}
	for _, verb := range Verbs() {
		if got, want := c.Args(verb), pinnedArgs("/keys/build/control", "ttorch@build-host", verb); !reflect.DeepEqual(got, want) {
			t.Errorf("Args(%s) = %q\nwant      %q", verb, got, want)
		}
	}
}

// shim writes an executable sh script standing in for ssh. It records its arguments, one per
// line, in <dir>/args and its stdin in <dir>/stdin, then runs body.
func shim(t *testing.T, body string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + dir + "/args'\ncat > '" + dir + "/stdin'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestClientCall: the request goes to ssh's stdin as one JSON object, ssh gets exactly the
// pinned arguments, and the result is decoded from the response.
func TestClientCall(t *testing.T) {
	ssh, dir := shim(t, `printf '%s\n' '{"protocol":1,"verb":"goal","ok":true,"result":{"event_id":7,"replayed":false}}'`)
	key := filepath.Join(t.TempDir(), "control")
	c := Client{SSH: ssh, Dest: "ttorch@build-host", Key: key}
	var got GoalResult
	if err := c.Call(context.Background(), VerbGoal, GoalRequest{ParentID: testParent, RequestID: "r1", Text: "tidy"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.EventID != 7 {
		t.Errorf("result = %+v", got)
	}
	if args := readLines(t, filepath.Join(dir, "args")); !reflect.DeepEqual(args, pinnedArgs(key, "ttorch@build-host", "goal")) {
		t.Errorf("ssh ran with %q\nwant         %q", args, pinnedArgs(key, "ttorch@build-host", "goal"))
	}
	var sent GoalRequest
	b, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &sent); err != nil || sent != (GoalRequest{ParentID: testParent, RequestID: "r1", Text: "tidy"}) {
		t.Errorf("stdin = %q (%v)", b, err)
	}
}

// TestClientAnswers: a refusal comes back as a RemoteError with the peer's code; another
// protocol major is refused without reading the result; an ssh failure with no response says what
// ssh said; every string the peer sends is escaped and capped before the caller sees it.
func TestClientAnswers(t *testing.T) {
	key := filepath.Join(t.TempDir(), "control")
	call := func(t *testing.T, body string, result any) error {
		t.Helper()
		ssh, _ := shim(t, body)
		c := Client{SSH: ssh, Dest: "ttorch@build-host", Key: key, Timeout: 20 * time.Second}
		return c.Call(context.Background(), VerbSummary, nil, result)
	}

	err := call(t, `printf '%s\n' '{"protocol":1,"verb":"goal","ok":false,"error":{"code":"wrong_parent","message":"not yours\u001b[2J"}}'; exit 1`, nil)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeWrongParent || re.Message != `not yours\x1b[2J` {
		t.Errorf("a refusal = %v (%#v), want a RemoteError carrying wrong_parent, escaped", err, re)
	}

	err = call(t, `printf '%s\n' '{"protocol":2,"verb":"summary","ok":true,"result":{}}'`, &struct{}{})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Got != 2 {
		t.Errorf("protocol 2 = %v, want a ProtocolError", err)
	}

	err = call(t, `echo 'ttorch@build-host: Permission denied (publickey).' >&2; exit 255`, nil)
	var te *TransportError
	if !errors.As(err, &te) || te.ExitCode != 255 || !strings.Contains(te.Stderr, "Permission denied (publickey)") {
		t.Errorf("an ssh failure = %v, want a TransportError with ssh's exit status and message", err)
	}
	err = call(t, `printf 'not json\033[2J\n'; printf 'garbage\033]0;x\007' >&2; exit 0`, nil)
	if !errors.As(err, &te) || strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("garbage = %v, want a TransportError with no raw control characters", err)
	}

	var res struct {
		Text string         `json:"text"`
		Map  map[string]int `json:"map"`
	}
	long := strings.Repeat("y", 3*MaxText)
	if err := call(t, `printf '%s\n' '{"protocol":1,"verb":"summary","ok":true,"result":{"text":"a\u001b]0;x\u0007b`+long+`","map":{"k\u202e":1}}}'`, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, `a\x1b]0;x\x07b`) || len(res.Text) > MaxText || res.Map["k\\u202e"] != 1 {
		t.Errorf("hostile result = %q (%d bytes), map %q; want escaped and capped", res.Text[:20], len(res.Text), res.Map)
	}

	ssh, _ := shim(t, `head -c `+strconv.Itoa(MaxResponse+10)+` /dev/zero | tr '\0' 'x'`)
	c := Client{SSH: ssh, Dest: "ttorch@build-host", Key: key, Timeout: 20 * time.Second}
	if err := c.Call(context.Background(), VerbSummary, nil, nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("an oversize answer = %v, want it refused", err)
	}
}

// TestClientRefusesBeforeRunning: a verb the peer does not serve, a destination ssh would read as
// an option, a relative key, or a body over MaxBody never starts ssh.
func TestClientRefusesBeforeRunning(t *testing.T) {
	ssh, dir := shim(t, `exit 0`)
	key := filepath.Join(t.TempDir(), "control")
	cases := []struct {
		label string
		c     Client
		verb  string
		req   any
	}{
		{"unknown verb", Client{SSH: ssh, Dest: "h", Key: key}, "approve", nil},
		{"option as destination", Client{SSH: ssh, Dest: "-oProxyCommand=sh", Key: key}, VerbVersion, nil},
		{"relative key", Client{SSH: ssh, Dest: "h", Key: "control"}, VerbVersion, nil},
		{"oversize body", Client{SSH: ssh, Dest: "h", Key: key}, VerbGoal, GoalRequest{Text: strings.Repeat("g", MaxBody)}},
	}
	for _, c := range cases {
		if err := c.c.Call(context.Background(), c.verb, c.req, nil); err == nil {
			t.Errorf("%s: the call went ahead", c.label)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); !os.IsNotExist(err) {
		t.Errorf("ssh was started by a refused call (%v)", err)
	}
}

// TestValidDest: the destinations ssh takes without a config file, and nothing it would read as
// an option or that a shell would split.
func TestValidDest(t *testing.T) {
	for _, ok := range []string{
		"build-host", "ttorch@build-host", "ttorch@build-host.example.com", "ttorch@10.1.2.3",
		"ssh://ttorch@build-host", "ssh://ttorch@build-host:2222", "ssh://build-host:22", "ssh://ttorch@[fd00::1]:22",
		"a_b@c_d",
	} {
		if err := ValidDest(ok); err != nil {
			t.Errorf("ValidDest(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-oProxyCommand=sh", "-p", "host name", "host\tname", "host\nname", "a@b@c", "@host", "user@",
		"host;id", "host$(id)", "host`id`", "host|x", "host:22", "ssh://", "ssh://host:", "ssh://host:99999",
		"ssh://host/path", "ssh://-x@host", "user@-host", "ssh://ttorch@[fd00::1", strings.Repeat("h", 256),
	} {
		if err := ValidDest(bad); err == nil {
			t.Errorf("ValidDest(%q) accepted", bad)
		}
	}
}

// alive reports whether pid is still a process this test could signal.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestClientDeadlineKillsTheTree: an ssh that never exits, and a child it started, are both
// killed at the client's deadline, and the call returns ErrTimeout instead of waiting.
func TestClientDeadlineKillsTheTree(t *testing.T) {
	ssh, dir := shim(t, `echo $$ > "${0%/ssh}/shim.pid"
sleep 300 &
echo $! > "${0%/ssh}/child.pid"
while :; do sleep 1; done`)
	key := filepath.Join(t.TempDir(), "control")
	c := Client{SSH: ssh, Dest: "ttorch@build-host", Key: key, Timeout: time.Second}
	start := time.Now()
	err := c.Call(context.Background(), VerbVersion, nil, nil)
	took := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a call to an ssh that never exits = %v, want ErrTimeout", err)
	}
	if took > 10*time.Second {
		t.Errorf("the call took %s past a 1s deadline", took)
	}
	pid := func(name string) int {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	shimPID, childPID := pid("shim.pid"), pid("child.pid")
	deadline := time.Now().Add(10 * time.Second)
	for (alive(shimPID) || alive(childPID)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(shimPID) {
		t.Errorf("the ssh process %d outlived the deadline", shimPID)
	}
	if alive(childPID) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Errorf("the child %d ssh started outlived the deadline", childPID)
	}
}

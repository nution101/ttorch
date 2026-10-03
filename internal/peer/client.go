package peer

// The parent's side of the control channel: one ssh process per call, authenticated by the
// peer's control key and nothing else. On the peer, authorized_keys pins that key to
// `ttorch peer serve` (see Serve), so the verb this client names is the only thing that runs.
//
// The ssh command line is fixed (Client.Args). It reads no ssh_config, offers only the control
// key, never asks an agent, never prompts, and refuses a host key it does not already know. The
// lead's own keys can run anything on the peer; none of them is ever offered on this channel,
// whatever the lead's ssh setup says.
//
// A call is bounded by a deadline and runs through proc.Command, so a connection that hangs is
// killed together with everything ssh started. What comes back is untrusted: it is decoded, every
// string in it escaped and capped (sanitize, the same pass Serve applies on the way out), and
// only then handed to the caller.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/proc"
)

// DefaultTimeout bounds one control call, connection included, when Client.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// MaxResponse bounds what a call reads from ssh's stdout. decisions answers every open
// escalation in one response, each body capped at MaxText, so this leaves room for thousands.
const MaxResponse = 16 << 20

// maxStderr is how much of ssh's stderr a failed call keeps: its last bytes, where ssh says why.
const maxStderr = 4 << 10

// maxDest bounds a destination.
const maxDest = 255

// ErrTimeout is a call that did not finish before its deadline. ssh and everything it started
// were killed.
var ErrTimeout = errors.New("the peer did not answer in time")

// RemoteError is a refusal the peer answered with: its code (one of the Code* constants), message
// and detail, all escaped.
type RemoteError struct {
	Verb    string
	Code    string
	Message string
	Detail  string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("the peer refused %s: %s: %s", e.Verb, e.Code, e.Message)
}

// ProtocolError is an answer in a protocol major version this binary does not speak. Nothing in
// it is read.
type ProtocolError struct{ Got int }

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("the peer speaks control protocol %d and this coordinator speaks %d; upgrade the older one", e.Got, ProtocolVersion)
}

// TransportError is a call with no response to read: ssh could not connect or authenticate, the
// peer printed something that is not a response, or ssh failed some other way. ExitCode is ssh's
// exit status (255 is ssh's own failure), -1 if it has none; Stderr is the end of what ssh wrote,
// escaped.
type TransportError struct {
	ExitCode int
	Stderr   string
	Reason   string
}

func (e *TransportError) Error() string {
	msg := e.Reason
	if e.ExitCode >= 0 {
		msg += fmt.Sprintf(" (ssh exited %d)", e.ExitCode)
	}
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// Client calls one peer's control channel.
type Client struct {
	// SSH is the ssh program to run. The CLI passes "ssh"; a test passes a stand-in.
	SSH string
	// Dest is the peer's control destination, [user@]host or ssh://[user@]host[:port]
	// (ValidDest). There is no ssh_config to resolve an alias, so it names the host itself.
	Dest string
	// Key is the absolute path of the control key's private half.
	Key string
	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Args is the ssh command line of a call to verb, after the program name. Every call uses
// exactly these options:
//
//	-F none                    read no ssh_config: no Host block can add a ProxyCommand, a
//	                           shared ControlMaster connection, another IdentityFile, a
//	                           certificate or agent forwarding
//	-o BatchMode=yes           never prompt
//	-o IdentitiesOnly=yes      offer only the key named by -i
//	-o IdentityAgent=none      never use an agent, so the lead's agent keys are never offered
//	-o StrictHostKeyChecking=yes  an unknown or changed host key fails the call
//	-o UpdateHostKeys=no       never write known_hosts
//	-i <key>                   the control key
//	-- <dest> <verb>           the verb is the one remote argument (SSH_ORIGINAL_COMMAND)
func (c Client) Args(verb string) []string {
	return []string{
		"-F", "none",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-i", c.Key,
		"--", c.Dest, verb,
	}
}

// Call sends req to verb and decodes the result into result, which may be nil to discard it. req
// nil sends {}. It returns a *RemoteError when the peer refused, a *ProtocolError for another
// protocol major, ErrTimeout (wrapped) past the deadline, and a *TransportError when there is no
// response to read.
func (c Client) Call(ctx context.Context, verb string, req, result any) error {
	if !servesVerb(verb) {
		return fmt.Errorf("%q is not a verb the control channel serves (%s)", verb, strings.Join(Verbs(), ", "))
	}
	if err := ValidDest(c.Dest); err != nil {
		return err
	}
	if !filepath.IsAbs(c.Key) {
		return fmt.Errorf("the control key path %q is not absolute", c.Key)
	}
	if c.SSH == "" {
		return errors.New("no ssh program is set")
	}
	body := []byte("{}")
	if req != nil {
		var err error
		if body, err = json.Marshal(req); err != nil {
			return err
		}
	}
	if len(body) > MaxBody {
		return fmt.Errorf("the %s request is %d bytes, over the %d byte limit", verb, len(body), MaxBody)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := proc.Command(ctx, c.SSH, c.Args(verb)...)
	cmd.Stdin = bytes.NewReader(body)
	stdout := &cappedBuffer{max: MaxResponse}
	stderr := &tailBuffer{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := proc.Run(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s on %s: %w after %s; ssh and everything it started were killed", verb, c.Dest, ErrTimeout, timeout)
	}
	if stdout.over {
		return fmt.Errorf("%s on %s: the answer is over the %d byte limit", verb, c.Dest, MaxResponse)
	}
	exit := -1
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		exit = ee.ExitCode()
	} else if runErr != nil {
		return fmt.Errorf("%s on %s: running ssh: %w", verb, c.Dest, runErr)
	} else {
		exit = 0
	}
	resp, err := readResponse(stdout.Bytes())
	if err != nil {
		return &TransportError{ExitCode: exit, Stderr: safeString(strings.TrimSpace(stderr.String())), Reason: fmt.Sprintf("%s on %s: no control response (%v)", verb, c.Dest, err)}
	}
	if resp.Protocol != ProtocolVersion {
		return &ProtocolError{Got: resp.Protocol}
	}
	if !resp.OK {
		e := &RemoteError{Verb: verb, Code: CodeInternal, Message: "the peer refused without saying why"}
		if resp.Error != nil {
			e.Code, e.Message, e.Detail = resp.Error.Code, resp.Error.Message, resp.Error.Detail
		}
		return e
	}
	if resp.Verb != verb {
		return &TransportError{ExitCode: exit, Reason: fmt.Sprintf("%s on %s: the peer answered %q", verb, c.Dest, resp.Verb)}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("%s on %s: the result does not decode: %w", verb, c.Dest, err)
	}
	return nil
}

// wireResponse is a Response as the client reads it, with the result left raw for the caller.
type wireResponse struct {
	Protocol int             `json:"protocol"`
	Verb     string          `json:"verb"`
	OK       bool            `json:"ok"`
	Result   json.RawMessage `json:"result"`
	Error    *ErrorBody      `json:"error"`
}

// readResponse reads one JSON object, passes every string in it, keys included, through
// sanitize, and only then decodes the envelope. Fields a newer peer adds within the same protocol
// are ignored.
func readResponse(raw []byte) (wireResponse, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return wireResponse{}, errors.New("the answer is not one JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return wireResponse{}, errors.New("the answer is not one JSON object")
	}
	if _, err := dec.Token(); err == nil {
		return wireResponse{}, errors.New("the answer has data after its JSON object")
	}
	clean, err := json.Marshal(sanitize(tree))
	if err != nil {
		return wireResponse{}, err
	}
	var resp wireResponse
	if err := json.Unmarshal(clean, &resp); err != nil {
		return wireResponse{}, fmt.Errorf("the answer is not a control response: %v", err)
	}
	return resp, nil
}

// Untrusted makes text that came from a peer, or was stored from one, safe to print on the
// parent: escaped if it still holds a non-printing rune or invalid UTF-8, and capped at MaxText.
// Text already escaped is left as it is, so it is not escaped twice.
func Untrusted(s string) string { return safeString(s) }

func servesVerb(verb string) bool {
	for _, v := range verbs {
		if v.name == verb {
			return true
		}
	}
	return false
}

// ValidDest refuses a destination ssh would not take as a host without a config file, or that
// it would read as an option: [user@]host, or ssh://[user@]host[:port] for a port or an IPv6
// address in brackets. A user is letters, digits and . _ -; a host is letters, digits and . _ -,
// or an IP address. Neither starts with a hyphen.
func ValidDest(dest string) error {
	if dest == "" || len(dest) > maxDest {
		return fmt.Errorf("a destination is 1 to %d bytes, got %d", maxDest, len(dest))
	}
	bad := fmt.Errorf("%q is not a destination: use [user@]host, or ssh://[user@]host[:port]", dest)
	host, uri := strings.CutPrefix(dest, "ssh://")
	if user, h, ok := strings.Cut(host, "@"); ok {
		if !validName(user) {
			return bad
		}
		host = h
	}
	if !uri {
		if !validName(host) {
			return bad
		}
		return nil
	}
	port, ip := "", false
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 || net.ParseIP(host[1:end]) == nil {
			return bad
		}
		if after := host[end+1:]; after != "" {
			p, ok := strings.CutPrefix(after, ":")
			if !ok {
				return bad
			}
			port = p
		}
		ip = true
	} else if h, p, ok := strings.Cut(host, ":"); ok {
		host, port = h, p
		if port == "" {
			return bad
		}
	}
	if !ip && !validName(host) {
		return bad
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return bad
		}
	}
	return nil
}

// validName is a user or host name: letters, digits, '.', '_' and '-', not starting with '-'.
func validName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// cappedBuffer keeps at most max bytes and records that more were offered. Write never fails, so
// ssh is not killed by a short write mid-answer; the call refuses the answer instead. The buffer
// is a field, not embedded: an embedded bytes.Buffer would promote ReadFrom, and io.Copy (which
// os/exec feeds stdout through) would call that instead of Write and read without a cap.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }

package peer

// Provisioning: `ttorch peer add` on the parent runs `ttorch peer init` on the peer over the
// lead's own interactive ssh session. That session is the lead's authority on the peer (their
// keys, their agent, a touch on a security key), not the control key's: the control key is not
// admitted until init has run. Init takes one JSON request on stdin and answers one response on
// stdout, in the same envelope the control channel uses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/nution101/ttorch/internal/proc"
)

// VerbInit names init's response. It is not a control verb: the control key cannot reach it.
const VerbInit = "init"

// DefaultRemoteTtorch is the ttorch the init session runs, relative to the home directory sshd
// starts a session in: the standard install.
const DefaultRemoteTtorch = ".ttorch/bin/ttorch"

// InitTimeout bounds the init session. The lead may have to type a passphrase or touch a key.
const InitTimeout = 10 * time.Minute

// InitRequest is what the parent asks init to do. Name is the peer's name on the parent; ParentID
// is the parent's coordinator id; ControlKey is the control key's public half as ssh-keygen wrote
// it. Force moves a peer another parent provisioned (`peer adopt --force`). Settings are the
// TTORCH_* settings written to a new peer.env, the lead's policy on the parent; an existing
// peer.env is kept as it is.
type InitRequest struct {
	Name       string            `json:"name"`
	ParentID   string            `json:"parent_id"`
	ControlKey string            `json:"control_key"`
	Force      bool              `json:"force"`
	Settings   map[string]string `json:"settings"`
}

// InitResult is what init did. AuthorizedKeys is "added" or "present"; PeerEnv is "written" or
// "kept". Binary is the absolute path the forced command names. StaleControlKeys counts other
// control keys' lines left in authorized_keys; RemovedControlKeys is the parent each removed one
// named, when a forced init removed them. Missing names the programs a fleet needs that peer.env's
// PATH does not reach.
type InitResult struct {
	CoordID            string   `json:"coord_id"`
	Name               string   `json:"name"`
	Role               string   `json:"role"`
	ParentID           string   `json:"parent_id"`
	PreviousParent     string   `json:"previous_parent"`
	Binary             string   `json:"binary"`
	AuthorizedKeys     string   `json:"authorized_keys"`
	StaleControlKeys   int      `json:"stale_control_keys"`
	RemovedControlKeys []string `json:"removed_control_keys"`
	PeerEnv            string   `json:"peer_env"`
	PeerEnvPath        string   `json:"peer_env_path"`
	Missing            []string `json:"missing"`
}

// ValidRemoteTtorch refuses a path for the remote ttorch that the remote shell would not read as
// one plain word: letters, digits and / . _ + @ -, with an optional leading ~/.
func ValidRemoteTtorch(path string) error {
	if !shellSafe(strings.TrimPrefix(path, "~/")) || len(path) > 1024 || strings.HasPrefix(path, "-") {
		return fmt.Errorf("%q is not a path the remote shell reads as one word; use letters, digits and / . _ + @ -", path)
	}
	return nil
}

// InitArgs is the ssh command line of the init session, after the program name. Unlike a
// control call it uses the lead's own ssh setup, which is what authenticates it. -T asks for no
// pty, since stdin carries the request; ssh still prompts on the terminal if it has to.
func InitArgs(dest, remoteTtorch string) []string {
	return []string{"-T", "--", dest, remoteTtorch + " peer init"}
}

// RunInit runs init on dest over the lead's session and returns what it did. ssh's own messages
// and anything init writes to stderr go to stderr, the lead's terminal. A refusal comes back as a
// *RemoteError, and a session with no response to read as a *TransportError.
func RunInit(ctx context.Context, ssh, dest, remoteTtorch string, timeout time.Duration, req InitRequest, stderr io.Writer) (InitResult, error) {
	if err := ValidDest(dest); err != nil {
		return InitResult{}, err
	}
	if err := ValidRemoteTtorch(remoteTtorch); err != nil {
		return InitResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return InitResult{}, err
	}
	if timeout <= 0 {
		timeout = InitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := proc.Command(ctx, ssh, InitArgs(dest, remoteTtorch)...)
	cmd.Stdin = bytes.NewReader(body)
	stdout := &cappedBuffer{max: MaxResponse}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := proc.Run(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return InitResult{}, fmt.Errorf("peer init on %s: %w after %s; ssh and everything it started were killed", dest, ErrTimeout, timeout)
	}
	exit := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		exit = ee.ExitCode()
	} else if runErr != nil {
		return InitResult{}, fmt.Errorf("peer init on %s: running ssh: %w", dest, runErr)
	}
	if stdout.over {
		return InitResult{}, fmt.Errorf("peer init on %s: the answer is over the %d byte limit", dest, MaxResponse)
	}
	resp, err := readResponse(stdout.Bytes())
	if err != nil {
		return InitResult{}, &TransportError{ExitCode: exit, Reason: fmt.Sprintf("peer init on %s: no response (%v); is ttorch installed at %s on the peer?", dest, err, remoteTtorch)}
	}
	if resp.Protocol != ProtocolVersion {
		return InitResult{}, &ProtocolError{Got: resp.Protocol}
	}
	if !resp.OK {
		e := &RemoteError{Verb: VerbInit, Code: CodeInternal, Message: "peer init refused without saying why"}
		if resp.Error != nil {
			e.Code, e.Message, e.Detail = resp.Error.Code, resp.Error.Message, resp.Error.Detail
		}
		return InitResult{}, e
	}
	var out InitResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return InitResult{}, fmt.Errorf("peer init on %s: the result does not decode: %w", dest, err)
	}
	return out, nil
}

// ReadInitRequest reads init's request: one JSON object of at most MaxBody bytes with no field
// InitRequest does not name. A malformed request is refused as bad_body.
func ReadInitRequest(r io.Reader) (InitRequest, error) {
	body, err := readBody(r)
	if err != nil {
		return InitRequest{}, err
	}
	var req InitRequest
	if err := decode(body, &req); err != nil {
		return InitRequest{}, err
	}
	return req, nil
}

// Respond writes one response for verb, as Serve does: result when err is nil, the refusal
// otherwise, every string escaped and capped. It returns the exit status, 0 or 1.
func Respond(stdout, stderr io.Writer, verb string, result any, err error) int {
	resp := Response{Protocol: ProtocolVersion, Verb: verb}
	if err == nil {
		resp.OK, resp.Result = true, result
	} else {
		resp.Error = errorBody(err)
	}
	out, mErr := encode(resp)
	if mErr != nil {
		resp = Response{Protocol: ProtocolVersion, Verb: verb, Error: &ErrorBody{Code: CodeInternal, Message: "encoding the response failed"}}
		out, _ = encode(resp)
	}
	_, _ = stdout.Write(out)
	if resp.Error != nil {
		fmt.Fprintf(stderr, "ttorch peer %s: %s: %s\n", verb, resp.Error.Code, resp.Error.Message)
		return 1
	}
	return 0
}

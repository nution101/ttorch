package peer

// Provisioning: `ttorch peer add` on the parent runs `ttorch peer init` on the peer over the
// lead's own interactive ssh session. That session is the lead's authority on the peer (their
// keys, their agent, a touch on a security key), not the control key's: the control key is not
// admitted until init has run. Init takes one JSON request on stdin and answers one response on
// stdout, in the same envelope the control channel uses.

import (
	"fmt"
	"io"
)

// VerbInit names init's response. It is not a control verb: the control key cannot reach it.
const VerbInit = "init"

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
// control keys' lines left in authorized_keys. Missing names the programs a fleet needs that
// peer.env's PATH does not reach.
type InitResult struct {
	CoordID          string   `json:"coord_id"`
	Name             string   `json:"name"`
	Role             string   `json:"role"`
	ParentID         string   `json:"parent_id"`
	PreviousParent   string   `json:"previous_parent"`
	Binary           string   `json:"binary"`
	AuthorizedKeys   string   `json:"authorized_keys"`
	StaleControlKeys int      `json:"stale_control_keys"`
	PeerEnv          string   `json:"peer_env"`
	PeerEnvPath      string   `json:"peer_env_path"`
	Missing          []string `json:"missing"`
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

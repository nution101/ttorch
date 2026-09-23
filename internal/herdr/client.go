package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// TargetProtocol is the Herdr JSON API protocol generation this client was
// written against (Herdr v0.9.1 reports protocol 22 from ping). It is recorded
// for diagnostics, not enforced: the JSON API is additive, unknown response
// fields are ignored, and a method the server lacks surfaces as
// ErrUnknownMethod.
const TargetProtocol = 22

// DefaultTimeout bounds a call whose Client.Timeout is unset.
const DefaultTimeout = 10 * time.Second

// defaultMaxResponseBytes caps one response line. pane.read returns
// scrollback text, so the cap is generous; it exists so a server that never
// sends a newline cannot grow the buffer without bound.
const defaultMaxResponseBytes = 32 << 20

var (
	// ErrNoSocket means nothing is listening at the socket path: the file
	// does not exist, or it is left over from a server that has exited.
	ErrNoSocket = errors.New("herdr: no server listening on socket")
	// ErrUnknownMethod means the server does not implement the method, for
	// example an older Herdr build. Herdr reports this as an invalid_request
	// error naming the method as an unknown variant.
	ErrUnknownMethod = errors.New("herdr: method not supported by server")
	// ErrMismatchedID means the response line carried a different request id
	// than the one sent on the connection.
	ErrMismatchedID = errors.New("herdr: response id does not match request")
	// ErrMalformedResponse means the response line was not a well-formed
	// response envelope, or its result had an unexpected type or shape.
	ErrMalformedResponse = errors.New("herdr: malformed response")
	// ErrConnectionClosed means the server closed the connection before a
	// complete response line arrived.
	ErrConnectionClosed = errors.New("herdr: connection closed before response")
	// ErrWaitTimeout means the server's own wait timeout elapsed before the
	// agent reached a requested status (error code "timeout").
	ErrWaitTimeout = errors.New("herdr: timed out waiting for agent status")
)

// APIError is an error response from the server: {"error":{"code","message"}}.
// Codes are Herdr's own strings (not_found, invalid_request, timeout, ...).
type APIError struct {
	Method  string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr: %s: %s: %s", e.Method, e.Code, e.Message)
}

// Is maps server error codes onto the package's sentinels. An unknown method
// is only matched when the message names this call's method, so an unknown
// enum value inside params (reported with the same code and wording) stays a
// plain invalid_request.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnknownMethod:
		return e.Code == "invalid_request" &&
			strings.Contains(e.Message, "unknown variant `"+e.Method+"`")
	case ErrWaitTimeout:
		return e.Code == "timeout"
	}
	return false
}

// Client talks to one Herdr server (one Herdr session) over its Unix socket.
// Each call dials a fresh connection, writes one request line and reads one
// response line, so calls are independent and safe for concurrent use.
type Client struct {
	socketPath string

	// Timeout caps every request/response call, including a subscription's
	// handshake. The effective deadline is the earlier of this and the
	// caller's context deadline. Zero means DefaultTimeout.
	Timeout time.Duration

	maxResponseBytes int
	nextID           atomic.Uint64
}

// New returns a client for the server listening at socketPath. It does not
// connect; the first call does.
func New(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SocketPath is the socket this client dials.
func (c *Client) SocketPath() string { return c.socketPath }

// DefaultSocketPath resolves the socket the way the herdr CLI does when no
// --session flag is given: HERDR_SOCKET_PATH, then HERDR_SESSION, then the
// default session.
func DefaultSocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	return SessionSocketPath(os.Getenv("HERDR_SESSION"))
}

// SessionSocketPath is the socket of the named Herdr session, or of the
// default session when name is empty. Herdr keeps sessions under its config
// directory: $XDG_CONFIG_HOME/herdr when set, otherwise ~/.config/herdr.
func SessionSocketPath(name string) string {
	dir := configDir()
	if name != "" {
		dir = filepath.Join(dir, "sessions", name)
	}
	return filepath.Join(dir, "herdr.sock")
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "herdr")
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "herdr")
	}
	return filepath.Join(os.TempDir(), "herdr")
}

// Pong is the ping result.
type Pong struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Ping checks the server is reachable and reports its version and protocol.
func (c *Client) Ping(ctx context.Context) (Pong, error) {
	var p Pong
	err := c.call(ctx, c.timeout(), "ping", nil, "pong", &p)
	return p, err
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c *Client) maxLine() int {
	if c.maxResponseBytes > 0 {
		return c.maxResponseBytes
	}
	return defaultMaxResponseBytes
}

// call runs one request/response exchange on a fresh connection, bounded by
// the earlier of ctx's deadline and bound. want is the result's "type"
// discriminator; out, when non-nil, receives the decoded result object.
func (c *Client) call(ctx context.Context, bound time.Duration, method string, params any, want string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := bindDeadline(ctx, conn)
	defer stop()
	return c.exchange(ctx, conn, bufio.NewReader(conn), method, params, want, out)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("%w: %s", ErrNoSocket, c.socketPath)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("herdr: dial %s: %w", c.socketPath, ctxErr)
	}
	return nil, fmt.Errorf("herdr: dial %s: %w", c.socketPath, err)
}

// bindDeadline makes conn's reads and writes fail once ctx is done: the
// deadline covers expiry, and the AfterFunc covers cancellation.
func bindDeadline(ctx context.Context, conn net.Conn) (stop func() bool) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	return context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type envelope struct {
	ID     *string         `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) exchange(ctx context.Context, conn net.Conn, r *bufio.Reader, method string, params any, want string, out any) error {
	if params == nil {
		// Herdr's request enum is adjacently tagged, so params must be
		// present even for methods that take none.
		params = struct{}{}
	}
	id := "ttorch_" + strconv.FormatUint(c.nextID.Add(1), 10)
	line, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("herdr: %s: encode request: %w", method, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return ioError(ctx, method, err)
	}
	resp, err := readLine(r, c.maxLine())
	if err != nil {
		return ioError(ctx, method, err)
	}
	return decodeResponse(resp, id, method, want, out)
}

// decodeResponse validates one response line against the request it answers.
func decodeResponse(line []byte, id, method, want string, out any) error {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrMalformedResponse, method, err)
	}
	if env.ID == nil {
		return fmt.Errorf("%w: %s: response has no id", ErrMalformedResponse, method)
	}
	if env.Error != nil {
		// Herdr answers a request it cannot parse (an unknown method, or
		// params that fail validation) with an empty id, because it never
		// learned the id. Each connection carries exactly one request, so an
		// empty-id error can only be about this one.
		if *env.ID != id && *env.ID != "" {
			return fmt.Errorf("%w: %s: sent %q, got %q", ErrMismatchedID, method, id, *env.ID)
		}
		return &APIError{Method: method, Code: env.Error.Code, Message: env.Error.Message}
	}
	if *env.ID != id {
		return fmt.Errorf("%w: %s: sent %q, got %q", ErrMismatchedID, method, id, *env.ID)
	}
	if len(env.Result) == 0 || bytes.Equal(env.Result, []byte("null")) {
		return fmt.Errorf("%w: %s: response has neither result nor error", ErrMalformedResponse, method)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(env.Result, &head); err != nil {
		return fmt.Errorf("%w: %s: result: %v", ErrMalformedResponse, method, err)
	}
	if head.Type != want {
		return fmt.Errorf("%w: %s: result type %q, want %q", ErrMalformedResponse, method, head.Type, want)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("%w: %s: result: %v", ErrMalformedResponse, method, err)
		}
	}
	return nil
}

// readLine reads one newline-terminated line of at most max bytes, without
// the newline. A line cut off by EOF is an error, not a short line.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max+1 {
			return nil, fmt.Errorf("%w: response line exceeds %d bytes", ErrMalformedResponse, max)
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, ErrConnectionClosed
		default:
			return nil, err
		}
	}
}

// ioError classifies a read or write failure. A deadline error is reported as
// the context's error (a Timeout-derived deadline shows up as
// context.DeadlineExceeded); a reset or broken pipe means the server went
// away mid-call, and a locally closed connection means the same to the
// reader.
func ioError(ctx context.Context, method string, err error) error {
	switch {
	case errors.Is(err, ErrConnectionClosed), errors.Is(err, ErrMalformedResponse):
		return fmt.Errorf("herdr: %s: %w", method, err)
	case errors.Is(err, os.ErrDeadlineExceeded):
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("herdr: %s: %w", method, ctxErr)
		}
		// The conn deadline can fire a hair before the context records it.
		return fmt.Errorf("herdr: %s: %w", method, context.DeadlineExceeded)
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, net.ErrClosed):
		return fmt.Errorf("herdr: %s: %w: %v", method, ErrConnectionClosed, err)
	}
	return fmt.Errorf("herdr: %s: %w", method, err)
}

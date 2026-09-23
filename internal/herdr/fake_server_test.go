package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeServer is an in-process stand-in for a Herdr server: a Unix-socket
// listener speaking newline-delimited JSON. Each accepted connection reads
// request lines and hands them to the handler registered for the method. A
// method with no handler gets the response Herdr gives a request it cannot
// parse: an invalid_request error with an empty id naming the unknown variant.
type fakeServer struct {
	t    *testing.T
	path string
	ln   *net.UnixListener

	mu       sync.Mutex
	handlers map[string]fakeHandler
	requests []fakeRequest
	conns    []net.Conn
	closed   bool
	wg       sync.WaitGroup
}

type fakeRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	// Line is the raw request line as received, without the newline.
	Line string `json:"-"`
}

// fakeHandler answers one request. It may write any number of lines, close
// the connection, or return without writing to leave the caller waiting.
type fakeHandler func(c *fakeConn, req fakeRequest)

type fakeConn struct {
	net.Conn
	t *testing.T
}

// shortSocketDir returns a fresh directory with a short path. t.TempDir
// embeds the test name, and macOS caps a Unix socket path at 104 bytes.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "herdr.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, path: path, ln: ln, handlers: map[string]fakeHandler{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func (s *fakeServer) client() *Client { return New(s.path) }

func (s *fakeServer) handle(method string, h fakeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// received returns every request the server has decoded so far.
func (s *fakeServer) received() []fakeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeRequest(nil), s.requests...)
}

func (s *fakeServer) close() {
	s.ln.Close()
	s.mu.Lock()
	s.closed = true
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *fakeServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns = append(s.conns, conn)
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *fakeServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	fc := &fakeConn{Conn: conn, t: s.t}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var req fakeRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			fc.line(`{"id":"","error":{"code":"invalid_request","message":"invalid request: bad json"}}`)
			continue
		}
		req.Line = sc.Text()
		s.mu.Lock()
		s.requests = append(s.requests, req)
		h := s.handlers[req.Method]
		s.mu.Unlock()
		if h == nil {
			fc.line(fmt.Sprintf(`{"id":"","error":{"code":"invalid_request","message":"invalid request: unknown variant `+"`%s`"+`, expected one of `+"`ping`"+`"}}`, req.Method))
			continue
		}
		h(fc, req)
	}
}

// line writes one raw line. Write errors are ignored: several tests close the
// client side on purpose.
func (c *fakeConn) line(s string) {
	_, _ = c.Write([]byte(s + "\n"))
}

// result writes a success response for id with result as the result object.
func (c *fakeConn) result(id string, result any) {
	b, err := json.Marshal(map[string]any{"id": id, "result": result})
	if err != nil {
		c.t.Errorf("fake server: marshal result: %v", err)
		return
	}
	c.line(string(b))
}

// fail writes an error response for id.
func (c *fakeConn) fail(id, code, message string) {
	b, _ := json.Marshal(map[string]any{"id": id, "error": map[string]string{"code": code, "message": message}})
	c.line(string(b))
}

// reply returns a handler that answers every request with result.
func reply(result any) fakeHandler {
	return func(c *fakeConn, req fakeRequest) { c.result(req.ID, result) }
}

// replyOK answers with Herdr's bare {"type":"ok"} result.
var replyOK = reply(map[string]any{"type": "ok"})

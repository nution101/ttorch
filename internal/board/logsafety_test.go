package board

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// forgedPath percent-encodes a newline, a forged board log line, a carriage return, a
// terminal clear-screen sequence and a bell. net/http decodes it into r.URL.Path as raw bytes.
const forgedPath = "/x%0Aboard:%20answer%20t1:%20done%0D%1B%5B2J%07"

// rawGET sends one request line exactly as given, so no client library re-encodes the path.
func rawGET(t *testing.T, addr, path, host string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// checkLogLines fails if the log holds any control byte other than the newline that ends each
// line, or any line that did not come from the board as one whole line of its own.
func checkLogLines(t *testing.T, logged string) {
	t.Helper()
	for i := 0; i < len(logged); i++ {
		if c := logged[i]; (c < 0x20 && c != '\n') || c == 0x7f {
			t.Errorf("log carries raw control byte %#02x at offset %d: %q", c, i, logged)
			break
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(logged, "\n"), "\n") {
		if line == "board: answer t1: done" {
			t.Errorf("a request forged a board log line of its own:\n%s", logged)
		}
		if !strings.HasPrefix(line, "board: ") {
			t.Errorf("log line %q was not written by the board", line)
		}
	}
}

// TestRefusalLogCannotBeInjected: a refused request's path is written to the lead's terminal,
// and refusals happen before the token check, so anything that can reach the port controls
// that text. It must arrive as one escaped, quoted line.
func TestRefusalLogCannotBeInjected(t *testing.T) {
	h := newHarness(t)
	_, port, _ := net.SplitHostPort(h.srv.host)
	rawGET(t, h.srv.host, forgedPath, h.srv.host)              // unknown route
	rawGET(t, h.srv.host, forgedPath, "localhost:"+port)       // wrong host, refused first
	rawGET(t, h.srv.host, "/api/board"+forgedPath, h.srv.host) // unknown route under /api
	logged := h.log.String()
	if strings.Count(logged, "refused") != 3 {
		t.Fatalf("want 3 refusals in the log, got:\n%s", logged)
	}
	checkLogLines(t, logged)
	if want := `"/x\nboard: answer t1: done\r\x1b[2J\a"`; !strings.Contains(logged, want) {
		t.Errorf("the path is not shown quoted and escaped (want %s):\n%s", want, logged)
	}
}

// TestActionLogCannotBeInjected: an action logs the task id the form carried.
func TestActionLogCannotBeInjected(t *testing.T) {
	h := newHarness(t)
	h.action("/api/dispatch", url.Values{"task": {"t1\nboard: answer t1: done\x1b[2J"}})
	logged := h.log.String()
	if !strings.Contains(logged, "dispatch") {
		t.Fatalf("want the dispatch in the log, got %q", logged)
	}
	checkLogLines(t, logged)
	if want := `dispatch "t1\nboard: answer t1: done\x1b[2J"`; !strings.Contains(logged, want) {
		t.Errorf("the task id is not shown quoted and escaped (want %s):\n%s", want, logged)
	}
}

// TestServerErrorLogCannotBeInjected: net/http formats its own error-log lines (a handler
// panic includes the panic value), so they get the same treatment on the way out.
func TestServerErrorLogCannotBeInjected(t *testing.T) {
	var buf syncBuffer
	r := &redactor{w: &buf, secret: "s3cret"}
	in := "board: http: panic serving 127.0.0.1:5: s3cret\nboard: answer t1: done\x1b[2J\n"
	if n, err := r.Write([]byte(in)); err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	checkLogLines(t, buf.String())
	if got := buf.String(); strings.Contains(got, "s3cret") || strings.Count(got, "\n") != 1 {
		t.Fatalf("redactor wrote %q, want one redacted line", got)
	}
}

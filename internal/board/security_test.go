package board

import (
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/db"
)

// answerForm is a well-formed answer for a waiting task, so a refusal in these tests can only
// come from the security check under test.
func (h *harness) waitingAnswerForm() url.Values {
	h.t.Helper()
	h.addTask(db.Task{ID: "t1", Window: "wk-t1", Status: db.StatusActive})
	q := h.ask("t1", "which base?")
	return url.Values{"task": {"t1"}, "question": {strconv.FormatInt(q, 10)}, "answer": {"main"}}
}

func TestAuthorizedRequestsAreServed(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	if r := h.page(); r.code != http.StatusOK || !strings.Contains(r.body, "Pending decisions") {
		t.Fatalf("page with the token: %d %q", r.code, r.body)
	}
	if r := h.sections(); r.code != http.StatusOK || !strings.Contains(r.body, "Live workers") {
		t.Fatalf("sections with the token: %d %q", r.code, r.body)
	}
	if r := h.action("/api/answer", form); r.code != http.StatusOK || h.fleet.sendCount() != 1 {
		t.Fatalf("answer with token + origin: %d %q, sends=%d", r.code, r.body, h.fleet.sendCount())
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	for _, r := range []request{
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/?token="},
		{method: http.MethodGet, path: "/api/board"},
		{method: http.MethodPost, path: "/api/answer", origin: h.base, form: form},
		{method: http.MethodPost, path: "/api/dispatch", origin: h.base, form: url.Values{"task": {"t1"}}},
		{method: http.MethodPost, path: "/api/gate-prep", origin: h.base, form: url.Values{"task": {"t1"}, "event": {"1"}}},
	} {
		got := h.do(r)
		if got.code != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: status %d, want 401", r.method, r.path, got.code)
		}
		if strings.Contains(got.body, "Pending decisions") || strings.Contains(got.body, "which base?") {
			t.Errorf("%s %s with no token leaked board content: %q…", r.method, r.path, snippet(got.body))
		}
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("an unauthenticated answer reached the worker (%d sends)", n)
	}
}

func TestWrongTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	wrong := strings.Repeat("0", len(h.srv.token))
	prefix := h.srv.token[:len(h.srv.token)-1] // one character short
	withFormToken := url.Values{}
	for k, v := range form {
		withFormToken[k] = v
	}
	withFormToken.Set("token", wrong)
	for _, r := range []request{
		{method: http.MethodGet, path: "/?token=" + wrong},
		{method: http.MethodGet, path: "/?token=" + prefix},
		{method: http.MethodGet, path: "/?token=" + h.srv.token + "0"},
		{method: http.MethodGet, path: "/api/board", headerToken: wrong},
		// The page's own URL token does not authenticate the feed: it wants the header.
		{method: http.MethodGet, path: "/api/board?token=" + h.srv.token},
		{method: http.MethodPost, path: "/api/answer", origin: h.base, headerToken: wrong, form: form},
		{method: http.MethodPost, path: "/api/answer", origin: h.base, form: withFormToken},
		// An action never takes the token from its URL, so a link cannot carry one.
		{method: http.MethodPost, path: "/api/answer?token=" + h.srv.token, origin: h.base, form: form},
	} {
		if got := h.do(r); got.code != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d, want 401", r.method, r.path, got.code)
		}
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("a wrong-token answer reached the worker (%d sends)", n)
	}
}

func TestActionAcceptsTheTokenAsAFormField(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	form.Set("token", h.srv.token)
	if got := h.do(request{method: http.MethodPost, path: "/api/answer", origin: h.base, form: form}); got.code != http.StatusOK {
		t.Fatalf("answer with the token as a form field: %d %q", got.code, got.body)
	}
	if n := h.fleet.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func TestWrongHostIsRefused(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	_, port, _ := net.SplitHostPort(h.srv.host)
	for _, host := range []string{
		"localhost:" + port,    // same socket, different name: a rebinding page would send this
		"evil.example:" + port, // DNS rebinding to 127.0.0.1
		"127.0.0.1",            // no port
		"127.0.0.1:1",          // another port
		"[::1]:" + port,
		"127.0.0.1:" + port + ".evil.example",
	} {
		for _, r := range []request{
			{method: http.MethodGet, path: "/?token=" + h.srv.token, host: host},
			{method: http.MethodGet, path: "/api/board", headerToken: h.srv.token, host: host},
			{method: http.MethodPost, path: "/api/answer", headerToken: h.srv.token, origin: "http://" + host, form: form, host: host},
		} {
			if got := h.do(r); got.code != http.StatusForbidden {
				t.Errorf("Host %q, %s %s: status %d, want 403", host, r.method, r.path, got.code)
			}
		}
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("a wrong-Host answer reached the worker (%d sends)", n)
	}
}

func TestCrossOriginActionsAreRefused(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	_, port, _ := net.SplitHostPort(h.srv.host)
	cases := []struct {
		name string
		r    request
	}{
		{"no Origin", request{}},
		{"foreign Origin", request{origin: "http://evil.example"}},
		{"localhost Origin", request{origin: "http://localhost:" + port}},
		{"https Origin", request{origin: "https://" + h.srv.host}},
		{"null Origin", request{origin: "null"}},
		{"cross-site fetch metadata", request{origin: h.base, secFetchSite: "cross-site"}},
		{"same-site fetch metadata", request{origin: h.base, secFetchSite: "same-site"}},
		{"text/plain body", request{origin: h.base, contentType: "text/plain"}},
		{"multipart body", request{origin: h.base, contentType: "multipart/form-data; boundary=x"}},
	}
	for _, tc := range cases {
		r := tc.r
		r.method, r.path, r.headerToken, r.form = http.MethodPost, "/api/answer", h.srv.token, form
		if got := h.do(r); got.code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, got.code)
		}
	}
	if n := h.fleet.sendCount(); n != 0 {
		t.Fatalf("a cross-origin answer reached the worker (%d sends)", n)
	}
}

func TestNoCORSHeadersAndNoPreflight(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	resps := []response{
		h.do(request{method: http.MethodOptions, path: "/api/answer", origin: "http://evil.example"}),
		h.do(request{method: http.MethodOptions, path: "/api/answer", origin: h.base, headerToken: h.srv.token}),
		h.page(),
		h.sections(),
		h.action("/api/answer", form),
		h.do(request{method: http.MethodGet, path: "/", origin: "http://evil.example"}),
	}
	if resps[0].code != http.StatusMethodNotAllowed || resps[1].code != http.StatusMethodNotAllowed {
		t.Fatalf("OPTIONS preflight: %d / %d, want 405", resps[0].code, resps[1].code)
	}
	for i, r := range resps {
		for k := range r.header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("response %d carries CORS header %s: %v", i, k, r.header[k])
			}
		}
	}
}

func TestListenBindsLoopbackOnly(t *testing.T) {
	h := newHarness(t)
	host, _, err := net.SplitHostPort(h.srv.host)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("board bound %q, want 127.0.0.1", h.srv.host)
	}
	if !strings.HasPrefix(h.srv.URL(), "http://127.0.0.1:") {
		t.Fatalf("URL %q does not point at 127.0.0.1", h.srv.URL())
	}
}

func TestTokenIsFreshAndRandom(t *testing.T) {
	a, b := newHarness(t), newHarness(t)
	for _, tok := range []string{a.srv.token, b.srv.token} {
		if raw, err := hex.DecodeString(tok); err != nil || len(raw) != 32 {
			t.Fatalf("token %q is not 32 random bytes in hex", tok)
		}
	}
	if a.srv.token == b.srv.token {
		t.Fatal("two boards generated the same token")
	}
	// The other board's token does not open this one.
	if got := a.do(request{method: http.MethodGet, path: "/?token=" + b.srv.token}); got.code != http.StatusUnauthorized {
		t.Fatalf("board A accepted board B's token: %d", got.code)
	}
}

func TestTokenNeverReachesTheLog(t *testing.T) {
	h := newHarness(t)
	form := h.waitingAnswerForm()
	_, port, _ := net.SplitHostPort(h.srv.host)
	h.page()
	h.sections()
	h.action("/api/answer", form)
	h.action("/api/answer", form) // a repeat: logged as nothing to do
	h.do(request{method: http.MethodGet, path: "/?token=" + h.srv.token, host: "localhost:" + port})
	h.do(request{method: http.MethodPost, path: "/api/answer?token=" + h.srv.token, origin: "http://evil.example", form: form})
	h.do(request{method: http.MethodGet, path: "/nope?token=" + h.srv.token})
	h.action("/api/dispatch", url.Values{"task": {h.srv.token}}) // echoes caller text into the log
	logged := h.log.String()
	if !strings.Contains(logged, "refused") {
		t.Fatalf("expected refusals in the log, got %q", logged)
	}
	if strings.Contains(logged, h.srv.token) {
		t.Fatalf("the token reached the log:\n%s", logged)
	}
}

func TestRedactorStripsTheToken(t *testing.T) {
	var buf syncBuffer
	r := &redactor{w: &buf, secret: "s3cret"}
	in := []byte("GET /?token=s3cret failed; again s3cret")
	n, err := r.Write(in)
	if err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := buf.String(); strings.Contains(got, "s3cret") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("redactor wrote %q", got)
	}
}

// snippet shortens a response body for a failure message.
func snippet(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

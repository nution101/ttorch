// Package board serves `ttorch board`: one local web page that shows the decisions waiting
// on the lead (escalated gates, workers asking a question, tasks awaiting approval) and the
// fleet's state (live workers, recent completions, queued backlog), refreshed live.
//
// The page can take three actions, none of which grants authority: answer a worker that is
// waiting on input (through the same Send that `ttorch send` uses), dispatch a pending
// backlog task (through the same claim and spawn the scheduler uses), and re-run a task's
// gate prep (the same TrustPrep that `ttorch trust prep` runs). It never approves and never
// merges; see routes.
//
// It binds to 127.0.0.1 only and every request must carry a token generated at start: in
// the URL for the page, and as a header or form field on each action. Requests whose Host
// is not exactly 127.0.0.1:<port> are refused, which defeats DNS rebinding, and actions
// must come from the page's own origin. No CORS headers are ever set.
//
// Because the page's token is in its URL, it stays in the browser's history and in the
// terminal scrollback where `ttorch board` printed it; it is good only until that board
// process exits, since each start generates a new one.
package board

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/review"
)

//go:embed page.html
var pageFS embed.FS

// TokenHeader is the request header an action may carry the token in. The form field
// "token" is accepted too, so a plain form post authenticates the same way.
const TokenHeader = "X-Ttorch-Token"

// maxFormBytes bounds an action's request body. An answer is a few lines of text; nothing
// the page sends comes close.
const maxFormBytes = 64 << 10

// Fleet is the orchestrator surface the board reads live state from and acts through.
// *orchestrator.Manager satisfies it; tests substitute a fake. Every method is one the CLI
// already drives, so the board reuses those paths rather than reimplementing them.
//
// It deliberately has no Approve, MergeLocal or Land method. The board cannot reach an
// approval or a merge through this interface, and the package does not import
// internal/approval; a test holds both facts in place.
type Fleet interface {
	// TaskState is the live worker state `ttorch status` shows: working, idle or gone.
	TaskState(t db.Task) string
	// Send types a message into a worker's pane: the same call `ttorch send` makes.
	Send(taskID, text string) error
	// TrustPrep stages the adversarial-review inputs: the same call `ttorch trust prep` makes.
	TrustPrep(taskID string) (string, error)
	// ReviewersFor names the reviewers the prepared inputs call for.
	ReviewersFor(taskID string) []string
	// SpawnAutonomous dispatches an already-claimed task: the scheduler's dispatch call.
	SpawnAutonomous(taskID, projectPath string, scout bool, rawCmd string, footprint []string, forceOverlap bool, effort, model string) (db.Task, error)
}

// Config wires a Server to the state it reads and the fleet it acts through.
type Config struct {
	Store *db.Store
	Fleet Fleet
	// Mode resolves a repo's delivery mode (pr|local|validated|trusted). Nil treats every
	// repo as pr, where the board lists nothing as awaiting approval.
	Mode func(repo string) string
	// ApprovalValid reports whether a task holds an unexpired approval. Nil reads as "no
	// approval". It is a function rather than a path so this package never touches the
	// approval store itself.
	ApprovalValid func(taskID string) bool
	// SerializeOverlap mirrors the scheduler's TTORCH_SERIALIZE_OVERLAP switch: when true a
	// dispatch whose footprint overlaps a live worker is refused instead of run in parallel.
	SerializeOverlap bool
	// Log receives the board's diagnostic lines. The token never reaches it: lines name the
	// request path without its query, and the HTTP server's own error log is redacted.
	Log io.Writer
	// Poll is how often the page refreshes. Zero means DefaultPoll.
	Poll time.Duration
}

// DefaultPoll is the page's refresh interval.
const DefaultPoll = 3 * time.Second

// Server is one board session: its token, its bound address and its handlers.
type Server struct {
	cfg   Config
	token string
	host  string // "127.0.0.1:<port>", set by Listen; every request's Host must equal it
	tmpl  *template.Template
	now   func() time.Time

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // per-task action locks

	actedMu sync.Mutex
	acted   map[string]bool // decisions this process acted on, by marker type and key
}

// New builds a Server with a fresh random token. It does not bind; call Listen.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Fleet == nil {
		return nil, errors.New("board: a store and a fleet are required")
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("page.html").Funcs(templateFuncs).ParseFS(pageFS, "page.html")
	if err != nil {
		return nil, fmt.Errorf("board: parsing the page template: %w", err)
	}
	return &Server{
		cfg: cfg, token: tok, tmpl: tmpl, now: time.Now,
		locks: map[string]*sync.Mutex{}, acted: map[string]bool{},
	}, nil
}

// newToken returns 32 random bytes, hex-encoded.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("board: generating the access token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Listen binds a free port on the IPv4 loopback address and records the exact host a
// request must name. It never binds a wildcard or external address.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("board: binding 127.0.0.1: %w", err)
	}
	s.host = ln.Addr().String()
	return ln, nil
}

// URL is the page address the lead opens, token included. It is printed once to the
// lead's terminal and nowhere else.
func (s *Server) URL() string {
	return "http://" + s.host + "/?token=" + s.token
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts it down.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A dispatch waits for the worker's harness to come up, which can take a while.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
		ErrorLog:     log.New(&redactor{w: s.cfg.Log, secret: s.token}, "board: ", 0),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// route is one registered endpoint.
type route struct {
	method  string
	action  bool // a state-changing POST: needs the same-origin check
	handler func(http.ResponseWriter, *http.Request)
}

// routes is the board's complete surface. There is no approve, merge or land route, and
// there must not be one. `ttorch approve` refuses to run without an interactive terminal
// (see internal/cli/approveguard.go) so that an approval is the lead's own act at a
// keyboard; a button here would be a way round that guard, reachable by anything that can
// drive a browser. For a task awaiting approval the page shows the exact command the lead
// runs instead. Merging follows from an approval (or a trusted verdict) through the land
// path, so the board has no reason to reach it either.
func (s *Server) routes() map[string]route {
	return map[string]route{
		"/":              {method: http.MethodGet, handler: s.handlePage},
		"/api/board":     {method: http.MethodGet, handler: s.handleSections},
		"/api/answer":    {method: http.MethodPost, action: true, handler: s.handleAnswer},
		"/api/dispatch":  {method: http.MethodPost, action: true, handler: s.handleDispatch},
		"/api/gate-prep": {method: http.MethodPost, action: true, handler: s.handleGatePrep},
	}
}

// Handler returns the board's HTTP handler: the security checks, then the route.
func (s *Server) Handler() http.Handler {
	routes := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		// Host first: a request that names any other host is not for this board, whatever
		// else it carries. This is the DNS-rebinding defence, so it runs before the token
		// check and before any route is looked up.
		if s.host == "" || r.Host != s.host {
			s.refuse(w, r, http.StatusForbidden, "wrong host")
			return
		}
		rt, ok := routes[r.URL.Path]
		if !ok {
			s.refuse(w, r, http.StatusNotFound, "no such route")
			return
		}
		if r.Method != rt.method {
			w.Header().Set("Allow", rt.method)
			s.refuse(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if rt.action {
			if !s.sameOrigin(r) {
				s.refuse(w, r, http.StatusForbidden, "cross-origin action")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
			if err := r.ParseForm(); err != nil {
				s.refuse(w, r, http.StatusBadRequest, "unreadable form")
				return
			}
		}
		if !s.authorized(r, rt) {
			s.refuse(w, r, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		rt.handler(w, r)
	})
}

// authorized checks the request's token. The page takes it from the URL; the sections feed
// takes it from the header; an action takes it from the header or its form body, never from
// its URL, so an action link cannot carry it.
func (s *Server) authorized(r *http.Request, rt route) bool {
	var got string
	switch {
	case r.URL.Path == "/":
		got = r.URL.Query().Get("token")
	case rt.action:
		got = r.Header.Get(TokenHeader)
		if got == "" {
			got = r.PostForm.Get("token")
		}
	default:
		got = r.Header.Get(TokenHeader)
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// sameOrigin requires an action to come from the page itself. Browsers send Origin on every
// POST, so a missing one is refused rather than trusted; Sec-Fetch-Site, when present, must
// agree; and the body must be the form encoding the page sends.
func (s *Server) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Origin") != "http://"+s.host {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == "application/x-www-form-urlencoded"
}

// setSecurityHeaders applies to every response, refusals included. The default CSP allows
// nothing; the page replaces it with one that admits only its own nonce-bearing script and
// style, so markup that slipped past escaping still could not run. No Access-Control-*
// header is ever set.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
}

// refuse answers a rejected request with a bare status and logs why, naming the path but
// never the query string (which is where the page's token travels). Refusals run before the
// token check, so the path is anyone's text: net/http has already percent-decoded it, and
// %0A or %1B arrive as real bytes. It is logged quoted, so a newline or a terminal escape
// shows as \n or \x1b inside the quotes and cannot start a line of its own.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, code int, why string) {
	s.logf("refused %s %s: %s", safeText(r.Method), review.SafeQuote(r.URL.Path), why)
	http.Error(w, http.StatusText(code), code)
}

// logf writes one board log line. It is the only way this package writes to the log, and it
// forces the result onto a single printable line (review.SafeLine), so no argument, whatever
// its origin, can put a line break or a control sequence into the lead's terminal. Text taken
// from a request should still be quoted by the caller (review.SafeQuote), so a reader can see
// where it starts and ends.
func (s *Server) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	// Belt and braces: nothing formats the token into a line, but a line that somehow held
	// it is redacted rather than written.
	line = strings.ReplaceAll(line, s.token, "[redacted]")
	fmt.Fprintf(s.cfg.Log, "board: %s\n", review.SafeLine(line))
}

// redactor is the HTTP server's error log. net/http formats those lines itself, and one can
// carry request-derived text (a handler panic includes the panic value), so each write has
// the token stripped and is forced onto one printable line, like logf.
type redactor struct {
	w      io.Writer
	secret string
}

// Write reports len(p) on success, as io.Writer requires, whatever the rewritten length.
func (r *redactor) Write(p []byte) (int, error) {
	n := len(p)
	if r.secret != "" {
		p = bytes.ReplaceAll(p, []byte(r.secret), []byte("[redacted]"))
	}
	line := review.SafeLine(string(p)) + "\n"
	if _, err := io.WriteString(r.w, line); err != nil {
		return 0, err
	}
	return n, nil
}

// pageData is what the page template renders.
type pageData struct {
	Token string
	Nonce string
	Poll  int64 // milliseconds
	Snap  Snapshot
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Snapshot(r.Context())
	if err != nil {
		s.logf("reading board state: %v", err)
		http.Error(w, "could not read the board state; see the terminal running ttorch board", http.StatusInternalServerError)
		return
	}
	nonce, err := newToken()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	nonce = nonce[:32]
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; script-src 'nonce-%s'; style-src 'nonce-%s'; connect-src 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; form-action 'none'", nonce, nonce))
	s.render(w, "page", pageData{Token: s.token, Nonce: nonce, Poll: s.cfg.Poll.Milliseconds(), Snap: snap})
}

func (s *Server) handleSections(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Snapshot(r.Context())
	if err != nil {
		s.logf("reading board state: %v", err)
		http.Error(w, "could not read the board state", http.StatusInternalServerError)
		return
	}
	s.render(w, "sections", pageData{Snap: snap})
}

// render executes a template into a buffer first, so a template error never sends half a
// page. html/template escapes every value for its context; nothing is marked safe.
func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.logf("rendering %s: %v", name, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// actionResponse is the JSON body every action answers with. Done=false with OK=true means
// the action found nothing to do (already handled, or the state moved on).
type actionResponse struct {
	OK      bool   `json:"ok"`
	Done    bool   `json:"done"`
	Message string `json:"message"`
}

func (s *Server) writeResult(w http.ResponseWriter, res Result, err error) {
	w.Header().Set("Content-Type", "application/json")
	body := actionResponse{OK: err == nil, Done: res.Done, Message: res.Message}
	code := http.StatusOK
	if err != nil {
		body.Message = err.Error()
		code = http.StatusUnprocessableEntity
		var ie inputError
		if errors.As(err, &ie) {
			code = http.StatusBadRequest
		}
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	q, err := parseEventID(r.PostForm.Get("question"))
	if err != nil {
		s.writeResult(w, Result{}, err)
		return
	}
	res, err := s.Answer(r.Context(), r.PostForm.Get("task"), q, r.PostForm.Get("answer"))
	s.logAction("answer", r.PostForm.Get("task"), res, err)
	s.writeResult(w, res, err)
}

func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	res, err := s.Dispatch(r.Context(), r.PostForm.Get("task"))
	s.logAction("dispatch", r.PostForm.Get("task"), res, err)
	s.writeResult(w, res, err)
}

func (s *Server) handleGatePrep(w http.ResponseWriter, r *http.Request) {
	ev, err := parseEventID(r.PostForm.Get("event"))
	if err != nil {
		s.writeResult(w, Result{}, err)
		return
	}
	res, err := s.GatePrep(r.Context(), r.PostForm.Get("task"), ev)
	s.logAction("gate-prep", r.PostForm.Get("task"), res, err)
	s.writeResult(w, res, err)
}

func (s *Server) logAction(action, taskID string, res Result, err error) {
	id := review.SafeQuote(taskID) // from the request's form
	switch {
	case err != nil:
		s.logf("%s %s: failed: %s", action, id, safeText(err.Error()))
	case res.Done:
		s.logf("%s %s: done", action, id)
	default:
		s.logf("%s %s: nothing to do: %s", action, id, safeText(res.Message))
	}
}

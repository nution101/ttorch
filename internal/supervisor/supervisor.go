// Package supervisor is ttorch's long-lived, zero-token watcher. It polls worker
// state and panes, and appends supervision events to the durable wake-queue that
// the manager drains. Running as a daemon (vs a one-shot) lets it hold cadence in
// memory and react quickly; durability lives in the wake-queue file, so a restart
// never loses an event.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/state"
	"github.com/nution101/ttorch/internal/tmux"
	"github.com/nution101/ttorch/internal/wake"
)

// Config tunes the polling cadence.
type Config struct {
	Poll      time.Duration
	Heartbeat time.Duration
}

// DefaultConfig returns sensible defaults (override via env in a later milestone).
func DefaultConfig() Config {
	return Config{Poll: 5 * time.Second, Heartbeat: 10 * time.Minute}
}

// Supervisor watches a tmux session's workers and emits wakes.
type Supervisor struct {
	P       paths.Paths
	Session string
	Store   state.Store
	Q       wake.Queue
	Cfg     Config

	seen          map[string]int64  // signal file -> last mtime (ns)
	paneHash      map[string]string // task id -> last pane hash
	staleCount    map[string]int    // task id -> consecutive unchanged ticks
	lastHeartbeat time.Time
	lastCheck     time.Time
	checked       map[string]bool // task id -> PR-merge already reported
	checkEvery    time.Duration
	now           func() time.Time
}

// New builds a Supervisor using the standard layout.
func New(p paths.Paths) *Supervisor {
	return &Supervisor{
		P:             p,
		Session:       tmux.SessionName(),
		Store:         state.Store{Dir: p.StateDir()},
		Q:             wake.Queue{Path: p.WakeQueue()},
		Cfg:           DefaultConfig(),
		seen:          map[string]int64{},
		paneHash:      map[string]string{},
		staleCount:    map[string]int{},
		lastHeartbeat: time.Now(),
		lastCheck:     time.Now(),
		checked:       map[string]bool{},
		checkEvery:    60 * time.Second,
		now:           time.Now,
	}
}

// Run owns the supervisor process until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.release()

	t := time.NewTicker(s.Cfg.Poll)
	defer t.Stop()

	// Watch the state dir so turn-end/status writes wake the supervisor instantly,
	// on top of the periodic sweep. If the watcher can't start, we degrade to
	// polling only (the channels stay nil and never fire).
	var events chan fsnotify.Event
	var errsCh chan error
	if w, err := fsnotify.NewWatcher(); err == nil {
		defer w.Close()
		_ = os.MkdirAll(s.P.StateDir(), 0o755)
		if err := w.Add(s.P.StateDir()); err == nil {
			events = w.Events
			errsCh = w.Errors
		}
	}

	s.beat()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.beat()
			s.tick()
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				s.scanSignals()
			}
		case _, ok := <-errsCh:
			if !ok {
				errsCh = nil
			}
		}
	}
}

// tick performs one full supervision cycle.
func (s *Supervisor) tick() {
	s.scanSignals()
	s.scanStale()
	s.scanChecks()
	s.heartbeat()
}

// scanChecks polls armed PR checks (rate-limited) and emits a wake when a task's
// PR is merged. Requires the gh CLI.
func (s *Supervisor) scanChecks() {
	if _, err := exec.LookPath("gh"); err != nil {
		return
	}
	if s.now().Sub(s.lastCheck) < s.checkEvery {
		return
	}
	s.lastCheck = s.now()
	tasks, _ := s.Store.List()
	for _, t := range tasks {
		if t.PR == "" || s.checked[t.ID] {
			continue
		}
		out, err := exec.Command("gh", "pr", "view", t.PR, "--json", "state", "-q", ".state").Output()
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(out)) == "MERGED" {
			_ = s.Q.Append("check", t.ID, "PR merged: "+t.PR)
			s.checked[t.ID] = true
		}
	}
}

// scanSignals turns new turn-end/status writes into signal wakes (idempotent:
// each file fires once per modification).
func (s *Supervisor) scanSignals() {
	entries, _ := os.ReadDir(s.P.StateDir())
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".turn-ended") && !strings.HasSuffix(name, ".status") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime().UnixNano()
		if mt > s.seen[name] {
			s.seen[name] = mt
			id := strings.TrimSuffix(strings.TrimSuffix(name, ".turn-ended"), ".status")
			_ = s.Q.Append("signal", id, name)
		}
	}
}

// scanStale emits a wake when a worker's pane stops changing and shows no busy
// indicator for two consecutive sweeps.
func (s *Supervisor) scanStale() {
	if !tmux.Available() {
		return
	}
	tasks, _ := s.Store.List()
	for _, task := range tasks {
		if task.Kind == "cc" {
			continue
		}
		out, err := tmux.CapturePane(s.Session, task.Window, 6)
		if err != nil {
			continue
		}
		if busy(out) {
			s.staleCount[task.ID] = 0
			s.paneHash[task.ID] = hash(out)
			continue
		}
		h := hash(out)
		if s.paneHash[task.ID] == h {
			s.staleCount[task.ID]++
			if s.staleCount[task.ID] == 2 {
				_ = s.Q.Append("stale", task.ID, task.Window)
			}
		} else {
			s.paneHash[task.ID] = h
			s.staleCount[task.ID] = 0
		}
	}
}

// heartbeat emits a periodic review wake.
func (s *Supervisor) heartbeat() {
	if s.now().Sub(s.lastHeartbeat) >= s.Cfg.Heartbeat {
		_ = s.Q.Append("heartbeat", "", "")
		s.lastHeartbeat = s.now()
	}
}

func (s *Supervisor) beat() { _ = touch(s.P.Beacon()) }

func (s *Supervisor) acquire() error {
	if pid, ok := Running(s.P); ok && pid != os.Getpid() {
		return &AlreadyRunningError{PID: pid}
	}
	if err := os.MkdirAll(filepath.Dir(s.P.PIDFile()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.P.PIDFile(), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func (s *Supervisor) release() { _ = os.Remove(s.P.PIDFile()) }

// AlreadyRunningError reports a live supervisor holding the singleton.
type AlreadyRunningError struct{ PID int }

func (e *AlreadyRunningError) Error() string {
	return "supervisor already running (pid " + strconv.Itoa(e.PID) + ")"
}

// PID returns the recorded supervisor PID, if any.
func PID(p paths.Paths) (int, bool) {
	b, err := os.ReadFile(p.PIDFile())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// Alive reports whether a process is running.
func Alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// Running reports the live supervisor PID, if one holds the singleton.
func Running(p paths.Paths) (int, bool) {
	pid, ok := PID(p)
	if !ok || !Alive(pid) {
		return 0, false
	}
	return pid, true
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func busy(pane string) bool {
	low := strings.ToLower(pane)
	for _, m := range []string{"esc to interrupt", "working…", "working...", "thinking", "generating"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

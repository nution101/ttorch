// Package supervisor is ttorch's long-lived, zero-token watcher. It polls worker
// state and panes, and appends supervision events to the durable wake-queue that
// the manager drains. Running as a daemon (vs a one-shot) lets it hold cadence in
// memory and react quickly; durability lives in the wake-queue file, so a restart
// never loses an event.
//
// On an actionable event (a worker turn-end, an idle/stalled worker, or a merged
// PR) — and on the periodic heartbeat as a backstop — it also pokes the manager
// window so the queue is actually drained. Without that poke the manager is dead
// between turns: it has no way to learn a worker went idle. The supervisor only
// pokes; the manager still drains and acts on its own turn.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	Poll         time.Duration
	Heartbeat    time.Duration
	PokeCooldown time.Duration // minimum interval between manager pokes (debounce)
}

// DefaultConfig returns sensible defaults (override via env in a later milestone).
func DefaultConfig() Config {
	return Config{Poll: 5 * time.Second, Heartbeat: 10 * time.Minute, PokeCooldown: 25 * time.Second}
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

	// Auto-driver: deliver actionable wakes to the manager window so it actually
	// takes a turn and drains the queue. pokePending coalesces a burst of events
	// into one poke; lastPoke debounces. The two seams reach real tmux in
	// production and are swapped out in tests.
	lastPoke       time.Time
	pokePending    bool
	sendPoke       func() error             // deliver one poke to the manager window
	inspectManager func() (live, busy bool) // is a manager window present, and is it mid-generation?

	lock *os.File // held while this process owns the singleton (nil otherwise)
}

// New builds a Supervisor using the standard layout.
func New(p paths.Paths) *Supervisor {
	s := &Supervisor{
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
	// Default auto-driver seams drive the real manager window via tmux, reusing the
	// shared helpers. Tests override them to count pokes and simulate the manager's
	// presence/busy state without a live tmux.
	s.sendPoke = func() error { return tmux.SendLine(s.Session, managerWindow, pokeDirective) }
	s.inspectManager = func() (live, busy bool) {
		if !tmux.Available() || !tmux.WindowExists(s.Session, managerWindow) {
			return false, false
		}
		out, err := tmux.CapturePane(s.Session, managerWindow, 6)
		if err != nil {
			// Can't read the pane: assume busy so we wait for a clean idle window
			// rather than risk interrupting a generating manager.
			return true, true
		}
		return true, Busy(out)
	}
	return s
}

// Run owns the supervisor process until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		// Losing the singleton race to a live supervisor is not an error: this
		// duplicate daemon simply has no work to do, so it exits quietly (the
		// winner runs). Any other failure is real and surfaces to the caller.
		var running *AlreadyRunningError
		if errors.As(err, &running) {
			return nil
		}
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
	s.flushPoke() // retry any poke the guards deferred (manager was busy / within cooldown)
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
			s.requestPoke() // a PR merged — actionable
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
			// A turn boundary means a worker just went idle — actionable, so drive the
			// manager. A bare ".status" write is not a turn end and never pokes.
			if strings.HasSuffix(name, ".turn-ended") {
				s.requestPoke()
			}
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
		if Busy(out) {
			s.staleCount[task.ID] = 0
			s.paneHash[task.ID] = hash(out)
			continue
		}
		h := hash(out)
		if s.paneHash[task.ID] == h {
			s.staleCount[task.ID]++
			if s.staleCount[task.ID] == 2 {
				_ = s.Q.Append("stale", task.ID, task.Window)
				s.requestPoke() // a worker went idle — actionable
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
		s.requestPoke() // backstop: drive the manager even if an event was missed (same debounce)
	}
}

// managerWindow is the tmux window name the manager session runs in (created by the
// orchestrator). The auto-driver pokes this window.
const managerWindow = "manager"

// pokeDirective is the one-line instruction the auto-driver types into the manager
// window when a worker goes idle/finished/merged (or the heartbeat fires). It tells
// the manager to take its turn; the manager — not the supervisor — drains the
// wake-queue and advances the backlog.
const pokeDirective = "ttorch wake: a worker is idle/finished/merged — run ttorch status, drain wakes, advance the backlog (land/answer/dispatch)."

// autodriveDisabled reports whether the operator opted out of auto-driving the
// manager (TTORCH_NO_AUTODRIVE). When set, the supervisor still queues wakes; it
// just never pokes, leaving the manager to be driven by hand.
func autodriveDisabled() bool { return os.Getenv("TTORCH_NO_AUTODRIVE") != "" }

// requestPoke records that the manager owes a turn (a worker went idle/finished/
// merged, or the heartbeat fired) and tries to deliver the poke immediately. A poke
// the guards defer (within the cooldown, or the manager busy) stays pending and is
// retried by flushPoke on the next tick, so a burst of events collapses into one
// poke once the manager is idle.
func (s *Supervisor) requestPoke() {
	if autodriveDisabled() {
		return
	}
	s.pokePending = true
	s.flushPoke()
}

// flushPoke delivers a pending poke when every guard allows it: auto-driving is on,
// the cooldown since the last poke has elapsed (debounce), a manager window exists,
// and it is idle — never interrupt a generating manager (reuses Busy). It is
// best-effort: a send-keys failure leaves the poke pending for the next tick and is
// never propagated, so a tmux hiccup can't crash the supervisor. pokePending is
// cleared only on a successful send, which is what coalesces a burst into one poke.
func (s *Supervisor) flushPoke() {
	if autodriveDisabled() {
		s.pokePending = false
		return
	}
	if !s.pokePending {
		return
	}
	if s.now().Sub(s.lastPoke) < s.Cfg.PokeCooldown {
		return // debounce: within the cooldown, stay pending
	}
	live, busy := s.inspectManager()
	if !live {
		return // no manager to wake yet; stay pending in case one appears
	}
	if busy {
		return // mid-generation; coalesce and wait for an idle window
	}
	if err := s.sendPoke(); err != nil {
		return // best-effort; stay pending and retry next tick
	}
	s.lastPoke = s.now()
	s.pokePending = false
}

func (s *Supervisor) beat() { _ = touch(s.P.Beacon()) }

// maxAcquireAttempts bounds the retry that handles the pid file being unlinked or
// replaced between our open and our lock (a departing supervisor's release racing
// our start). A handful of attempts is far more than any real start/stop overlap
// needs.
const maxAcquireAttempts = 5

// acquire claims the supervisor singleton with an exclusive advisory lock (flock)
// on the PID file, held for the lifetime of this process. The kernel arbitrates the
// lock, so at most one process can hold it at a time, and — crucially — it is
// released automatically when the holder exits, even on a crash. That makes the
// claim immune to the races a bare PID file suffers: there is no check-then-write
// window for a second daemon to slip through (the old Running()-then-WriteFile, the
// double-start race), no stale file to reclaim by hand (a dead holder's lock is
// already gone), and no need to second-guess a recorded pid against liveness or
// worry about pid reuse — the lock, not the file contents, is the source of truth.
//
// flock binds to the inode, not the path, so after winning the lock we confirm our
// descriptor still names the file at the path. If a departing supervisor's release()
// unlinked it between our open and our lock, we would otherwise be holding a live
// lock on an orphaned inode that daemon.pid no longer points at — invisible to
// Running()/stop/status, and free for the next start to claim a fresh file and run a
// second supervisor. On that mismatch we drop the orphan and retry against the live
// file.
//
// The pid is written into the file purely for observability, so Running(),
// `ttorch daemon status`, and `ttorch daemon stop` can find and signal the live
// supervisor exactly as before.
func (s *Supervisor) acquire() error {
	if s.lock != nil {
		return nil // this process already holds the singleton
	}
	path := s.P.PIDFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for attempt := 0; attempt < maxAcquireAttempts; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				pid, _ := PID(s.P) // best-effort: name the live holder if it recorded its pid
				return &AlreadyRunningError{PID: pid}
			}
			return err
		}
		if !lockedLiveFile(f, path) {
			// We locked an inode that is no longer the file at the path (a departing
			// supervisor unlinked it). Drop the orphan and retry against the live file.
			_ = f.Close()
			continue
		}
		// We hold the lock on the live pid file. Record our pid, truncating any dead
		// predecessor's, so the rest of ttorch can find and signal us.
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
			_ = f.Close()
			return err
		}
		s.lock = f // keep the fd open to hold the lock for our lifetime
		return nil
	}
	return errors.New("supervisor: could not acquire the lock (pid file kept being replaced)")
}

// lockedLiveFile reports whether f still refers to the file currently at path. flock
// is held on the inode, not the path, so a descriptor whose path was unlinked between
// open and lock points at an orphaned inode; this also catches a path replaced by a
// different inode. acquire uses it so it never writes its pid to a file that
// daemon.pid no longer names.
func lockedLiveFile(f *os.File, path string) bool {
	onDisk, err := os.Stat(path)
	if err != nil {
		return false
	}
	held, err := f.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(held, onDisk)
}

// release relinquishes the singleton: it drops the PID file (only while it still
// names this process, so a successor that has already taken over is left untouched)
// and closes the locked descriptor, which releases the advisory lock. With no
// contention the file names us and is removed, exactly as before.
func (s *Supervisor) release() {
	if s.lock == nil {
		return
	}
	if pid, ok := PID(s.P); !ok || pid == os.Getpid() {
		_ = os.Remove(s.P.PIDFile())
	}
	_ = s.lock.Close() // releases the flock
	s.lock = nil
}

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

// Start launches the supervisor as a detached background process that runs
// `<this-binary> daemon run`, logging to p.DaemonLog(). It is the single shared
// start path behind `ttorch supervise`, `ttorch daemon start`, and the automatic
// ensure-on-spawn. It is idempotent: if a supervisor already holds the singleton
// it launches nothing and returns that pid with started=false.
func Start(p paths.Paths) (pid int, started bool, err error) {
	if pid, ok := Running(p); ok {
		return pid, false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false, err
	}
	if err := os.MkdirAll(p.Home, 0o755); err != nil {
		return 0, false, err
	}
	logf, err := os.OpenFile(p.DaemonLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer logf.Close()
	c := exec.Command(exe, "daemon", "run")
	c.Env = append(os.Environ(), "TTORCH_DAEMON=1")
	c.Stdout = logf
	c.Stderr = logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, false, err
	}
	pid = c.Process.Pid
	_ = c.Process.Release()
	return pid, true, nil
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

// Busy reports whether a captured pane shows a harness busy indicator (the worker
// is mid-turn). It is the shared heuristic behind both the supervisor's
// stale-detection and `ttorch status`, so the two never disagree.
func Busy(pane string) bool {
	low := strings.ToLower(pane)
	for _, m := range []string{"esc to interrupt", "working…", "working...", "thinking", "generating"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
